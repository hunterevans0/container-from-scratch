//go:build linux

package runtime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"
)

// Containers on a network made with runt network create find each other by
// name, as on a Docker network made with docker network create. Each one's
// /etc/resolv.conf names a single DNS server, at 127.0.0.11, on the
// container's own loopback interface. It answers with the address of the
// container of that name, or with that ID, on the same network, and passes
// any other question on to the host's DNS servers. This is how Docker's
// embedded DNS server works too.
//
// The server is the container's supervisor. It makes its sockets in the
// container's network namespace, but the sockets it passes questions on with
// in the host's, so it can reach the host's servers even when they are on
// the host's loopback interface, as systemd-resolved's 127.0.0.53 is.
//
// It speaks just enough DNS for this (see RFC 1035). A message is a 12-byte
// header (an ID, flags, and how many questions, answers, and other records
// follow), then the questions, then the records. A question is a name, as a
// series of labels each preceded by its length and ending with an empty
// one, then a type (1 is A, an IPv4 address) and a class (1 is IN, the
// internet). Over TCP, each message is preceded by its length.

const (
	// dnsAddress is the DNS server's address in the container, the same
	// as Docker's.
	dnsAddress = "127.0.0.11"
	// dnsTTL is how many seconds an answer may be remembered, as Docker's
	// server says.
	dnsTTL = 600
	// dnsTimeout is how long the server waits for the host's servers, and
	// for a client on TCP.
	dnsTimeout = 5 * time.Second

	dnsTypeA         = 1
	dnsClassIN       = 1
	dnsRcodeServFail = 2
)

// dnsServer is a container's DNS server.
type dnsServer struct {
	// network is the name of the container's network, which a container's
	// name may be followed by, as in web.mynet.
	network string
	// lookup returns the address of the container called name on the
	// network, or nil if there is none.
	lookup func(name string) net.IP
	// upstreams are the host's DNS servers, as host:port.
	upstreams []string
	udp       net.PacketConn
	tcp       net.Listener
}

// startDNS starts a DNS server for the container whose init is pid, on
// network.
func startDNS(pid int, network string) (*dnsServer, error) {
	s := &dnsServer{
		network:   network,
		lookup:    func(name string) net.IP { return lookupContainer(network, name) },
		upstreams: hostNameservers(),
	}
	var err error
	if s.udp, s.tcp, err = listenDNS(pid); err != nil {
		return nil, fmt.Errorf("start DNS server: %w", err)
	}
	go s.serveUDP()
	go s.serveTCP()
	return s, nil
}

func (s *dnsServer) close() {
	s.udp.Close()
	s.tcp.Close()
}

// hostNameservers are the host's DNS servers, as host:port, for the DNS
// server to pass questions on to.
func hostNameservers() []string {
	nameservers := hostResolvConf().nameservers
	if len(nameservers) == 0 {
		nameservers = fallbackNameservers
	}
	var upstreams []string
	for _, nameserver := range nameservers {
		upstreams = append(upstreams, net.JoinHostPort(nameserver, "53"))
	}
	return upstreams
}

// lookupContainer returns the address of the container on network called
// name (in any case, as DNS names are), or with that ID, if it is created
// or running.
func lookupContainer(network, name string) net.IP {
	if name == "" {
		return nil
	}
	states, err := listStates(StateRoot)
	if err != nil {
		return nil
	}
	for _, state := range states {
		if state.Network != network || state.IPAddress == "" || !(strings.EqualFold(state.Name, name) || state.ID == name) {
			continue
		}
		// One whose monitor was killed is gone, though its state says
		// otherwise.
		if initAlive(state.Pid) {
			return net.ParseIP(state.IPAddress)
		}
	}
	return nil
}

// ipFreebind is the IP_FREEBIND socket option, which lets a socket bind an
// address the system doesn't have, which the syscall package doesn't define.
const ipFreebind = 15

// listenDNS makes the DNS server's sockets, UDP and TCP, at dnsAddress port
// 53, in the network namespace of process pid. They are made with the
// system calls themselves rather than with net.Listen, which may first test
// what the system supports, and remember the answers for this process: it
// would be testing the container's namespace.
//
// The container's loopback interface doesn't have 127.0.0.11 yet, nor any
// address: they come once init brings it up, which hasn't happened yet.
// IP_FREEBIND binds the address regardless.
func listenDNS(pid int) (net.PacketConn, net.Listener, error) {
	var udp, tcp int
	err := inNetworkNamespace(pid, func() error {
		address := &syscall.SockaddrInet4{Port: 53, Addr: [4]byte(net.ParseIP(dnsAddress).To4())}
		var err error
		if udp, err = boundSocket(syscall.SOCK_DGRAM, address); err != nil {
			return err
		}
		if tcp, err = boundSocket(syscall.SOCK_STREAM, address); err != nil {
			syscall.Close(udp)
			return err
		}
		if err = syscall.Listen(tcp, syscall.SOMAXCONN); err != nil {
			syscall.Close(udp)
			syscall.Close(tcp)
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	// The net package's File functions take a copy of the descriptor.
	udpFile, tcpFile := os.NewFile(uintptr(udp), "dns-udp"), os.NewFile(uintptr(tcp), "dns-tcp")
	defer udpFile.Close()
	defer tcpFile.Close()
	packets, err := net.FilePacketConn(udpFile)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.FileListener(tcpFile)
	if err != nil {
		packets.Close()
		return nil, nil, err
	}
	return packets, listener, nil
}

// boundSocket makes an IPv4 socket of kind bound to address.
func boundSocket(kind int, address *syscall.SockaddrInet4) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, kind|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	err = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, ipFreebind, 1)
	if err == nil && kind == syscall.SOCK_STREAM {
		err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}
	if err == nil {
		err = syscall.Bind(fd, address)
	}
	if err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

// inNetworkNamespace runs f on a thread of its own in the network namespace
// of process pid. Sockets made there stay in that namespace wherever they
// are used from. The thread never leaves the namespace: it ends with f,
// rather than going back to run other goroutines there.
func inNetworkNamespace(pid int, f func() error) error {
	errs := make(chan error, 1)
	go func() {
		// Never unlocked, so the thread ends with this goroutine.
		goruntime.LockOSThread()
		ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/net", pid))
		if err != nil {
			errs <- err
			return
		}
		defer ns.Close()
		if _, _, errno := syscall.Syscall(sysSetns, ns.Fd(), syscall.CLONE_NEWNET, 0); errno != 0 {
			errs <- fmt.Errorf("join the container's network namespace: %w", errno)
			return
		}
		errs <- f()
	}()
	return <-errs
}

func (s *dnsServer) serveUDP() {
	buffer := make([]byte, 65535)
	for {
		n, client, err := s.udp.ReadFrom(buffer)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			continue
		}
		query := bytes.Clone(buffer[:n])
		go func() {
			if reply := s.answer(query, false); reply != nil {
				s.udp.WriteTo(reply, client)
			}
		}()
	}
}

func (s *dnsServer) serveTCP() {
	for {
		conn, err := s.tcp.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			defer conn.Close()
			for {
				conn.SetDeadline(time.Now().Add(dnsTimeout))
				query, err := readDNSMessage(conn)
				if err != nil {
					return
				}
				reply := s.answer(query, true)
				if reply == nil || writeDNSMessage(conn, reply) != nil {
					return
				}
			}
		}()
	}
}

// readDNSMessage reads a message from a TCP connection, where each comes
// after its length, in 2 bytes.
func readDNSMessage(r io.Reader) ([]byte, error) {
	var length [2]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}
	message := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(r, message); err != nil {
		return nil, err
	}
	return message, nil
}

func writeDNSMessage(w io.Writer, message []byte) error {
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(message))), message...))
	return err
}

// answer returns the reply to query, which came over TCP if tcp is set, or
// nil if it is too malformed to reply to. A container's name gets its
// address, and any other question the reply of the host's servers.
func (s *dnsServer) answer(query []byte, tcp bool) []byte {
	question, err := parseDNSQuestion(query)
	if err != nil {
		return nil
	}
	if question.class == dnsClassIN {
		if ip := s.lookup(strings.TrimSuffix(question.name, "."+s.network)); ip != nil {
			// The name exists. A container has no other kind of address,
			// so a question about any other type of record, such as AAAA
			// for an IPv6 address, gets no answers.
			var addresses []net.IP
			if question.kind == dnsTypeA {
				addresses = []net.IP{ip}
			}
			return dnsReply(query, question, 0, addresses)
		}
	}
	reply, err := s.forward(query, tcp)
	if err != nil {
		return dnsReply(query, question, dnsRcodeServFail, nil)
	}
	return reply
}

// forward asks the host's servers query, one after another until one
// answers, the same way the client asked, so that a reply too big for UDP
// comes back truncated, and the client asks again over TCP.
func (s *dnsServer) forward(query []byte, tcp bool) ([]byte, error) {
	err := errors.New("no DNS servers to ask")
	for _, upstream := range s.upstreams {
		var reply []byte
		if reply, err = exchangeDNS(upstream, query, tcp); err == nil {
			return reply, nil
		}
	}
	return nil, err
}

// exchangeDNS sends query to server, over TCP if tcp is set, and returns its
// reply.
func exchangeDNS(server string, query []byte, tcp bool) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}
	conn, err := net.DialTimeout(network, server, dnsTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(dnsTimeout))
	if tcp {
		if err := writeDNSMessage(conn, query); err != nil {
			return nil, err
		}
		return readDNSMessage(conn)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buffer := make([]byte, 65535)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			return nil, err
		}
		// Anything else is a late reply to some other question.
		if n >= 2 && bytes.Equal(buffer[:2], query[:2]) {
			return buffer[:n], nil
		}
	}
}

// dnsQuestion is the question a DNS query asks.
type dnsQuestion struct {
	// name is in lower case, without a dot at the end.
	name        string
	kind, class uint16
	// end is where the question ends in the query.
	end int
}

// parseDNSQuestion parses a query, which must ask one question.
func parseDNSQuestion(query []byte) (dnsQuestion, error) {
	var question dnsQuestion
	if len(query) < 12 {
		return question, errors.New("short DNS message")
	}
	if query[2]&0x80 != 0 {
		return question, errors.New("not a DNS query")
	}
	if binary.BigEndian.Uint16(query[4:]) != 1 {
		return question, errors.New("DNS query doesn't ask one question")
	}
	var labels []string
	i := 12
	for {
		if i >= len(query) {
			return question, errors.New("short DNS message")
		}
		length := int(query[i])
		i++
		if length == 0 {
			break
		}
		// Longer labels are pointers to names elsewhere in the message,
		// which a question has no need for.
		if length > 63 || i+length > len(query) {
			return question, errors.New("bad name in DNS query")
		}
		labels = append(labels, string(query[i:i+length]))
		i += length
	}
	if i+4 > len(query) {
		return question, errors.New("short DNS message")
	}
	question.name = strings.ToLower(strings.Join(labels, "."))
	question.kind = binary.BigEndian.Uint16(query[i:])
	question.class = binary.BigEndian.Uint16(query[i+2:])
	question.end = i + 4
	return question, nil
}

// dnsReply makes a reply to query, which asks question, with the response
// code rcode (0 for success) and an A record for each of addresses.
func dnsReply(query []byte, question dnsQuestion, rcode int, addresses []net.IP) []byte {
	reply := make([]byte, 12, question.end+16*len(addresses))
	copy(reply, query[:2])
	// QR: a reply. The query's opcode and RD, which asks for recursion.
	// AA: an authoritative answer. RA: recursion is available, by passing
	// questions on.
	flags := 0x8000 | binary.BigEndian.Uint16(query[2:])&0x7900 | 0x0400 | 0x0080 | uint16(rcode)
	binary.BigEndian.PutUint16(reply[2:], flags)
	binary.BigEndian.PutUint16(reply[4:], 1)
	binary.BigEndian.PutUint16(reply[6:], uint16(len(addresses)))
	reply = append(reply, query[12:question.end]...)
	for _, ip := range addresses {
		// The record's name is a pointer to the question's, which starts
		// right after the header, at byte 12.
		reply = append(reply, 0xc0, 12)
		reply = binary.BigEndian.AppendUint16(reply, dnsTypeA)
		reply = binary.BigEndian.AppendUint16(reply, dnsClassIN)
		reply = binary.BigEndian.AppendUint32(reply, dnsTTL)
		reply = binary.BigEndian.AppendUint16(reply, 4)
		reply = append(reply, ip.To4()...)
	}
	return reply
}
