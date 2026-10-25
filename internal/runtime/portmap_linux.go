//go:build linux

package runtime

// Port publishing (-p): how a server inside a container is reached at a port
// of the host's. For how to read the Go here, see doc.go.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// publishedPort is a container's port published on the host, as Docker
// publishes one. Connections from elsewhere reach the container through a
// DNAT rule, which rewrites their destination from the host port to the
// container's address and port, so the container sees who they really come
// from. The kernel applies the rule as the packets arrive, before routing
// them on through the bridge.
//
// Connections from the host to its loopback addresses, and from containers,
// which the rules leave out, reach a proxy instead: the supervisor listens
// on the host port, and opens a connection of its own to the container for
// each one, as Docker's docker-proxy does. The rules can't serve these: the
// kernel won't route a packet from 127.0.0.1 out of the loopback interface,
// and a container's packets rewritten to another container would get
// replies straight from it, through the bridge, which it wouldn't recognize.
//
// Listening on the port also holds it, so that two containers can't publish
// the same one, or a program on the host take it. It is the only way to tell
// which rules are in use: the rules outlive a supervisor that is killed, but
// its sockets don't, so whoever holds a port may remove any rules for it.
//
// A port is TCP or UDP, so only one of listener and packets is set, both
// from the net package: a net.Listener accepts TCP connections, and a
// *net.UDPConn sends and receives UDP datagrams.
type publishedPort struct {
	mapping  PortMapping
	rule     iptablesRule
	listener net.Listener
	packets  *net.UDPConn
}

// publish publishes the container's ports, as each of ports says. If one
// fails, those already published stay in n.ports, for the caller to undo
// with leave.
func (n *containerNetwork) publish(ports []PortMapping) error {
	for _, mapping := range ports {
		port, err := publishPort(mapping, n.reservation.ip)
		if err != nil {
			return err
		}
		n.ports = append(n.ports, port)
	}
	return nil
}

// publishPort publishes mapping, for the container at ip. The order matters:
//
//  1. Listen on the host port. This fails if anything else holds the port,
//     and with port 0 the system picks one.
//  2. Remove any rules a dead container left for the port, which only the
//     holder of the port may do.
//  3. Add the DNAT rule.
//  4. Start relaying what the proxy gets, in a goroutine of its own, which
//     runs until the port is closed.
func publishPort(mapping PortMapping, ip net.IP) (*publishedPort, error) {
	host := mapping.HostIP
	if host == "" {
		host = "0.0.0.0"
	}
	// JoinHostPort makes "0.0.0.0:8080" (and would add brackets around an
	// IPv6 address).
	address := net.JoinHostPort(host, strconv.Itoa(mapping.HostPort))
	port := &publishedPort{mapping: mapping}
	if mapping.Protocol == "udp" {
		// "udp4" and "tcp4" ask for IPv4 sockets only.
		conn, err := net.ListenPacket("udp4", address)
		if err != nil {
			return nil, fmt.Errorf("publish port %d/udp: %w", mapping.ContainerPort, err)
		}
		// ListenPacket returns an interface that could hold several kinds
		// of socket. For "udp4" it is always a *net.UDPConn, and the type
		// assertions take that out, and the *net.UDPAddr its address is,
		// to learn the port actually bound.
		port.packets = conn.(*net.UDPConn)
		port.mapping.HostPort = conn.LocalAddr().(*net.UDPAddr).Port
	} else {
		listener, err := net.Listen("tcp4", address)
		if err != nil {
			return nil, fmt.Errorf("publish port %d/tcp: %w", mapping.ContainerPort, err)
		}
		port.listener = listener
		port.mapping.HostPort = listener.Addr().(*net.TCPAddr).Port
	}
	hostPort := strconv.Itoa(port.mapping.HostPort)
	// Rules for the same port and protocol, at an address that overlaps
	// ours, are stale: if their owner were alive, it would hold the port,
	// and listening above would have failed. Rules for the same port at
	// another single address belong to whoever listens there.
	err := removeStaleDNAT(func(rule dnatRule) bool {
		return rule.protocol == mapping.Protocol && rule.port == hostPort &&
			(rule.hostIP == "" || mapping.HostIP == "" || rule.hostIP == mapping.HostIP)
	})
	if err != nil {
		port.close()
		return nil, err
	}
	// The rule, as on iptables' command line, for example:
	// -d 127.0.0.1 ! -i runt+ -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.88.0.2:80
	// That is: packets for this host address (if one was given), not coming
	// from one of runt's bridges, for this protocol and port, get their
	// destination rewritten to the container's address and port.
	var spec []string
	if mapping.HostIP != "" {
		spec = append(spec, "-d", mapping.HostIP)
	}
	backend := net.JoinHostPort(ip.String(), strconv.Itoa(mapping.ContainerPort))
	spec = append(spec, "!", "-i", allBridges, "-p", mapping.Protocol, "-m", mapping.Protocol, "--dport", hostPort,
		"-j", "DNAT", "--to-destination", backend)
	rule := iptablesRule{"nat", portsChain, spec}
	if err := rule.add(); err != nil {
		port.close()
		return nil, err
	}
	port.rule = rule
	if port.listener != nil {
		go proxyTCP(port.listener, backend)
	} else {
		go proxyUDP(port.packets, backend)
	}
	return port, nil
}

// close unpublishes the port. The rule goes before the port is let go, so
// that it can't remove rules another container has made for the port since.
// Closing the socket also ends the proxy's goroutine, whose next attempt to
// accept or read fails with net.ErrClosed.
func (p *publishedPort) close() error {
	var err error
	// spec is nil until the rule has been added (see publishPort).
	if p.rule.spec != nil {
		err = p.rule.remove()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	if p.packets != nil {
		p.packets.Close()
	}
	return err
}

// dnatRule is a published port's rule, as iptables -S lists it.
type dnatRule struct {
	// spec is the rule, after -A and the chain.
	spec []string
	// hostIP is empty for a rule for all of the host's addresses.
	hostIP, protocol, port string
	// toIP is the container's address.
	toIP string
}

// parseDNATRule parses a line of iptables -S RUNT, such as
// "-A RUNT -d 127.0.0.1/32 ! -i runt+ -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.88.0.2:80".
// It walks the words, and takes the one after each option it cares about.
// The second result says whether the line was a rule of the RUNT chain at
// all; iptables -S also lists the chain itself, as "-N RUNT".
func parseDNATRule(line string) (dnatRule, bool) {
	// strings.Fields splits at runs of spaces.
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "-A" || fields[1] != portsChain {
		return dnatRule{}, false
	}
	// fields[2:] is a slice of the words from the third on.
	rule := dnatRule{spec: fields[2:]}
	for i := 2; i+1 < len(fields); i++ {
		switch value := fields[i+1]; fields[i] {
		case "-d":
			// iptables lists a single address as 127.0.0.1/32.
			rule.hostIP = strings.TrimSuffix(value, "/32")
		case "-p":
			rule.protocol = value
		case "--dport":
			rule.port = value
		case "--to-destination":
			rule.toIP, _, _ = strings.Cut(value, ":")
		}
	}
	return rule, true
}

// removeStaleDNAT removes the published ports' rules that match: rules left
// behind by containers whose supervisors were killed. match is a function
// the caller passes, which says yes or no to each rule.
func removeStaleDNAT(match func(dnatRule) bool) error {
	output, err := runIptables("-t", "nat", "-S", portsChain)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(output, "\n") {
		if rule, ok := parseDNATRule(line); ok && match(rule) {
			if err := (iptablesRule{"nat", portsChain, rule.spec}).remove(); err != nil {
				return err
			}
		}
	}
	return nil
}

// proxyTCP relays each connection listener accepts to backend, until the
// listener is closed. Each connection gets a goroutine of its own, so one
// slow client doesn't hold up the others.
func proxyTCP(listener net.Listener, backend string) {
	for {
		// Accept waits for the next connection.
		client, err := listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// Out of file descriptors, say. Let some close.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			defer client.Close()
			server, err := net.DialTimeout("tcp4", backend, 5*time.Second)
			if err != nil {
				// Nothing listens in the container (yet). Closing the
				// client's connection tells it so.
				return
			}
			defer server.Close()
			// A WaitGroup counts goroutines still running: Add(2) for the
			// two below, Done as each ends, and Wait until both have.
			var copies sync.WaitGroup
			// Each direction ends on its own, as when one side has sent all
			// it has to send but still waits for the reply. io.Copy reads
			// from one connection and writes to the other until the first
			// is done, and CloseWrite then tells the other side that
			// nothing more is coming, while still letting it reply.
			relay := func(to, from net.Conn) {
				defer copies.Done()
				io.Copy(to, from)
				to.(*net.TCPConn).CloseWrite()
			}
			copies.Add(2)
			go relay(server, client)
			go relay(client, server)
			copies.Wait()
		}()
	}
}

// udpIdleTimeout is how long the UDP proxy remembers a client that has had
// no replies, as Docker's does.
const udpIdleTimeout = 90 * time.Second

// proxyUDP relays datagrams that conn receives to backend, and the replies
// back, until conn is closed. UDP has no connections, so it makes one of its
// own per client: a socket connected to backend, whose replies go back to
// the client.
//
// clients maps each client's address and port to its socket. Two kinds of
// goroutine use it at once, this one and one per client reading replies, so
// a mutex (lock) makes them take turns: whoever holds the lock has the map
// to itself until it unlocks it.
func proxyUDP(conn *net.UDPConn, backend string) {
	var lock sync.Mutex
	clients := map[netip.AddrPort]*net.UDPConn{}
	// When the port is closed, close every client's socket too, which ends
	// their goroutines.
	defer func() {
		lock.Lock()
		defer lock.Unlock()
		for _, upstream := range clients {
			upstream.Close()
		}
	}()
	backendAddress, err := net.ResolveUDPAddr("udp4", backend)
	if err != nil {
		return
	}
	// 65535 bytes is as big as a UDP datagram can be.
	buffer := make([]byte, 65535)
	for {
		n, client, err := conn.ReadFromUDPAddrPort(buffer)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			continue
		}
		lock.Lock()
		upstream := clients[client]
		if upstream == nil {
			// A client we haven't heard from, or not lately.
			if upstream, err = net.DialUDP("udp4", nil, backendAddress); err != nil {
				lock.Unlock()
				continue
			}
			clients[client] = upstream
			// Relay the container's replies back to this client, until
			// none has come for udpIdleTimeout, and then forget it.
			go func() {
				reply := make([]byte, 65535)
				for {
					upstream.SetReadDeadline(time.Now().Add(udpIdleTimeout))
					n, err := upstream.Read(reply)
					if err != nil {
						break
					}
					conn.WriteToUDPAddrPort(reply[:n], client)
				}
				lock.Lock()
				delete(clients, client)
				lock.Unlock()
				upstream.Close()
			}()
		}
		lock.Unlock()
		// buffer[:n] is the part of the buffer the datagram filled.
		upstream.Write(buffer[:n])
	}
}
