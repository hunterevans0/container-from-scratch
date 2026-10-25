//go:build linux

package runtime

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
type publishedPort struct {
	mapping  PortMapping
	rule     iptablesRule
	listener net.Listener
	packets  *net.UDPConn
}

// publish publishes the container's ports, as each of ports says.
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

// publishPort publishes mapping, for the container at ip.
func publishPort(mapping PortMapping, ip net.IP) (*publishedPort, error) {
	host := mapping.HostIP
	if host == "" {
		host = "0.0.0.0"
	}
	address := net.JoinHostPort(host, strconv.Itoa(mapping.HostPort))
	port := &publishedPort{mapping: mapping}
	if mapping.Protocol == "udp" {
		conn, err := net.ListenPacket("udp4", address)
		if err != nil {
			return nil, fmt.Errorf("publish port %d/udp: %w", mapping.ContainerPort, err)
		}
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
	err := removeStaleDNAT(func(rule dnatRule) bool {
		return rule.protocol == mapping.Protocol && rule.port == hostPort &&
			(rule.hostIP == "" || mapping.HostIP == "" || rule.hostIP == mapping.HostIP)
	})
	if err != nil {
		port.close()
		return nil, err
	}
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
func (p *publishedPort) close() error {
	var err error
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
func parseDNATRule(line string) (dnatRule, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "-A" || fields[1] != portsChain {
		return dnatRule{}, false
	}
	rule := dnatRule{spec: fields[2:]}
	for i := 2; i+1 < len(fields); i++ {
		switch value := fields[i+1]; fields[i] {
		case "-d":
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
// behind by containers whose supervisors were killed.
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
// listener is closed.
func proxyTCP(listener net.Listener, backend string) {
	for {
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
				return
			}
			defer server.Close()
			var copies sync.WaitGroup
			// Each direction ends on its own, as when one side has sent all
			// it has to send but still waits for the reply.
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
func proxyUDP(conn *net.UDPConn, backend string) {
	var lock sync.Mutex
	clients := map[netip.AddrPort]*net.UDPConn{}
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
			if upstream, err = net.DialUDP("udp4", nil, backendAddress); err != nil {
				lock.Unlock()
				continue
			}
			clients[client] = upstream
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
		upstream.Write(buffer[:n])
	}
}
