//go:build linux

package runtime

// Bridge networks, and how a container joins one. For an overview of the
// package, and how to read the Go here, see doc.go.

// import lists the packages this file uses: encoding/binary for numbers as
// bytes, encoding/json for saved network configurations, errors and fmt for
// errors and formatting, net for IP addresses and interfaces, os for files,
// path/filepath for building paths, slices, strconv, and strings for slice
// and string handling, and syscall for raw system calls and error numbers.
import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// A bridge network works like a Docker one. The host has a bridge, a virtual
// switch with the network's first address, which is every container's
// gateway. Each container gets a veth pair, two virtual interfaces joined
// like the ends of a cable: one end is eth0 in the container's network
// namespace, with an address of the container's own, and the other end is
// plugged into the bridge. So containers reach each other through the
// bridge, and the host through its address. To reach anything further, the
// host routes their packets on, rewriting the source address to its own
// (NAT), so that replies come back to the host, which passes them on to the
// container.
//
// There is the default network, on the bridge runt0, and any made with runt
// network create (see networks_linux.go), each with a bridge of its own.
// Containers on different networks can't reach each other.
//
// A network's bridge and NAT rules are made the first time a container needs
// them and left in place, as Docker leaves docker0. A container's veth pair,
// address, and published ports last as long as the container runs.
//
// How a container gets onto a network, in the order supervise (in
// container_linux.go) does it:
//
//  1. joinNetwork, before the container exists: make sure the network's
//     bridge and iptables rules are there, reserve an address for the
//     container, and publish its ports.
//  2. connect, once init (the container's first process) has been started
//     in a new network namespace: make the veth pair, with one end inside
//     that namespace, and start the container's DNS server if its network
//     has one.
//  3. configureNetwork, which init runs inside the container: give eth0 its
//     address and a default route. Init does this itself because only a
//     process inside the namespace sees eth0 by that name.
//  4. Leaving, once the container has exited (see supervise): undo 1 and 2.

// const declares constants. Several can be grouped in parentheses.
const (
	// defaultBridge is the default network's bridge, as docker0 is for
	// Docker.
	defaultBridge = "runt0"
	// allBridges matches every bridge runt makes, in iptables rules: the
	// default network's, and runt- followed by the network's ID for the
	// others. In iptables, a + at the end of an interface name matches any
	// name that starts with what comes before it.
	allBridges = "runt+"
	// containerInterface is the container's end of its veth pair.
	containerInterface = "eth0"
	// networkStateRoot holds the networks' lock and a directory per network
	// with its address reservations (see ipam). Like StateRoot it is on a
	// tmpfs, since the bridges and the containers are gone after a reboot
	// too.
	networkStateRoot = "/run/runt-net"
)

// defaultSubnet is the default network's range of addresses, the same as
// Podman's default network. Docker's is 172.17.0.0/16. It is a var rather
// than a const because Go constants can only be numbers, strings, and
// booleans, not structs. A net.IPNet is an address and a mask: /16 means
// the first 16 bits of the address name the network, and the other 16 tell
// its 65536 addresses apart.
var defaultSubnet = &net.IPNet{IP: net.IP{10, 88, 0, 0}, Mask: net.CIDRMask(16, 32)}

// bridgeNetwork is a network of containers on a bridge.
type bridgeNetwork struct {
	name   string
	bridge string
	subnet *net.IPNet
	// dns is set on networks made with runt network create, whose
	// containers find each other by name through runt's DNS server (see
	// dnsServer). Containers on the default network don't, as on Docker's.
	dns bool
}

// ipam is the network's address book: which addresses are taken, and by
// which containers (see ipam_linux.go). Each network keeps its own, in a
// directory named after it.
func (n *bridgeNetwork) ipam() *ipam {
	return &ipam{dir: filepath.Join(networkStateRoot, n.name), subnet: n.subnet}
}

// initNetwork is how init configures the container's end of the veth pair.
// The supervisor sends it to init as JSON, which is what the tags are for.
type initNetwork struct {
	// Address is the container's address, with the subnet's prefix
	// length, as in 10.88.0.2/16.
	Address string `json:"address"`
	Gateway string `json:"gateway"`
}

// containerNetwork is a container's place on a network: everything that has
// to be undone when the container leaves it.
type containerNetwork struct {
	network     *bridgeNetwork
	reservation *reservation
	// hostVeth is the host's end of the container's veth pair.
	hostVeth string
	ports    []*publishedPort
	// dns is the container's DNS server, on networks that have one, and
	// nil on the others.
	dns *dnsServer
}

// joinNetwork sets up the network called name, if its bridge and NAT aren't
// already, reserves an address on it for container id, and publishes the
// container's ports. The veth pair comes later, once the container's network
// namespace exists (see connect).
func joinNetwork(id, name string, ports []PortMapping) (*containerNetwork, error) {
	// The built-in network. Fields left out of a struct literal, here dns,
	// get their zero value, false.
	network := &bridgeNetwork{name: NetworkBridge, bridge: defaultBridge, subnet: defaultSubnet}
	if name != NetworkBridge {
		// A network made with runt network create: read its configuration
		// from disk, holding a shared lock, which many containers can hold
		// at once but runt network rm, which takes it exclusively, can't
		// get while they do.
		unlock, err := lockNetworks(false)
		if err != nil {
			return nil, err
		}
		// Until the container holds an address on the network, which keeps
		// runt network rm from removing it. Deferred, unlock runs when
		// joinNetwork returns.
		defer unlock()
		// Read the configuration of the network called name. A name such as
		// ../x would lead out of networkConfigRoot. validName (in state.go)
		// is the pattern container names must match too.
		if !validName.MatchString(name) {
			return nil, fmt.Errorf("no such network: %s", name)
		}
		data, err := os.ReadFile(networkConfigPath(name))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no such network: %s; make it with runt network create", name)
		}
		if err != nil {
			return nil, err
		}
		// json.Unmarshal fills in the struct from the JSON, matching keys to
		// fields by their tags (see Network, in runtime.go). It needs a
		// pointer, &saved, to be able to change the variable.
		var saved Network
		if err := json.Unmarshal(data, &saved); err != nil {
			return nil, fmt.Errorf("parse network %s: %w", name, err)
		}
		// Turn the saved configuration, which keeps its subnet as text, into
		// the form the rest of the code works with. Network is the exported
		// type, which cmd/runt sees; bridgeNetwork is runt's own.
		_, subnet, err := net.ParseCIDR(saved.Subnet)
		if err != nil {
			return nil, fmt.Errorf("network %s: %w", saved.Name, err)
		}
		network = &bridgeNetwork{name: saved.Name, bridge: saved.Bridge, subnet: subnet, dns: true}
	}
	if err := network.setup(); err != nil {
		return nil, err
	}
	reservation, err := network.ipam().reserve(id)
	if err != nil {
		return nil, err
	}
	// The host's end of the veth pair is named after the container. id[:8]
	// is the first 8 characters of the ID: interface names have at most 15
	// characters.
	n := &containerNetwork{network: network, reservation: reservation, hostVeth: "veth" + id[:8]}
	// The address was free, so any port rules still sending connections to
	// it are a dead container's: remove the published ports' rules whose
	// destination is this address, left behind by containers whose
	// supervisors were killed.
	output, err := runIptables("-t", "nat", "-S", portsChain)
	if err == nil {
		for _, line := range strings.Split(output, "\n") {
			if rule, ok := parseDNATRule(line); ok && rule.toIP == reservation.ip.String() {
				if err = (iptablesRule{"nat", portsChain, rule.spec}).remove(); err != nil {
					break
				}
			}
		}
	}
	if err == nil {
		// Publish the container's ports, as each of ports says. If one
		// fails, those already published stay in n.ports, to be undone
		// below. For each, the order matters:
		//
		//  1. Listen on the host port. This fails if anything else holds the
		//     port, and with port 0 the system picks one.
		//  2. Remove any rules a dead container left for the port, which
		//     only the holder of the port may do.
		//  3. Add the DNAT rule.
		//  4. Start relaying what the proxy gets, in a goroutine of its own,
		//     which runs until the port is closed.
		for _, mapping := range ports {
			host := mapping.HostIP
			if host == "" {
				host = "0.0.0.0"
			}
			// JoinHostPort makes "0.0.0.0:8080" (and would add brackets
			// around an IPv6 address).
			address := net.JoinHostPort(host, strconv.Itoa(mapping.HostPort))
			port := &publishedPort{mapping: mapping}
			if mapping.Protocol == "udp" {
				// "udp4" and "tcp4" ask for IPv4 sockets only.
				conn, listenErr := net.ListenPacket("udp4", address)
				if listenErr != nil {
					err = fmt.Errorf("publish port %d/udp: %w", mapping.ContainerPort, listenErr)
					break
				}
				// ListenPacket returns an interface that could hold several
				// kinds of socket. For "udp4" it is always a *net.UDPConn,
				// and the type assertions take that out, and the
				// *net.UDPAddr its address is, to learn the port actually
				// bound.
				port.packets = conn.(*net.UDPConn)
				port.mapping.HostPort = conn.LocalAddr().(*net.UDPAddr).Port
			} else {
				listener, listenErr := net.Listen("tcp4", address)
				if listenErr != nil {
					err = fmt.Errorf("publish port %d/tcp: %w", mapping.ContainerPort, listenErr)
					break
				}
				port.listener = listener
				port.mapping.HostPort = listener.Addr().(*net.TCPAddr).Port
			}
			hostPort := strconv.Itoa(port.mapping.HostPort)
			// Rules for the same port and protocol, at an address that
			// overlaps ours, are stale: if their owner were alive, it would
			// hold the port, and listening above would have failed. Rules
			// for the same port at another single address belong to
			// whoever listens there.
			output, err = runIptables("-t", "nat", "-S", portsChain)
			if err == nil {
				for _, line := range strings.Split(output, "\n") {
					if rule, ok := parseDNATRule(line); ok && rule.protocol == mapping.Protocol && rule.port == hostPort &&
						(rule.hostIP == "" || mapping.HostIP == "" || rule.hostIP == mapping.HostIP) {
						if err = (iptablesRule{"nat", portsChain, rule.spec}).remove(); err != nil {
							break
						}
					}
				}
			}
			if err != nil {
				port.close()
				break
			}
			// The rule, as on iptables' command line, for example:
			// -d 127.0.0.1 ! -i runt+ -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.88.0.2:80
			// That is: packets for this host address (if one was given),
			// not coming from one of runt's bridges, for this protocol and
			// port, get their destination rewritten to the container's
			// address and port.
			var spec []string
			if mapping.HostIP != "" {
				spec = append(spec, "-d", mapping.HostIP)
			}
			backend := net.JoinHostPort(n.reservation.ip.String(), strconv.Itoa(mapping.ContainerPort))
			spec = append(spec, "!", "-i", allBridges, "-p", mapping.Protocol, "-m", mapping.Protocol, "--dport", hostPort,
				"-j", "DNAT", "--to-destination", backend)
			rule := iptablesRule{"nat", portsChain, spec}
			// -A puts the rule last in its chain.
			if err = rule.run("-A"); err != nil {
				port.close()
				break
			}
			port.rule = rule
			if port.listener != nil {
				go proxyTCP(port.listener, backend)
			} else {
				go proxyUDP(port.packets, backend)
			}
			n.ports = append(n.ports, port)
		}
	}
	if err != nil {
		// Give back whatever was taken: the address, and any ports
		// published before the one that failed. Errors in doing so are
		// left out: err says what went wrong.
		if n.dns != nil {
			n.dns.udp.Close()
			n.dns.tcp.Close()
		}
		for _, port := range n.ports {
			port.close()
		}
		if conn, err := openNetlink(); err == nil {
			// Deleting either end of a veth pair deletes both.
			deleteRequest := newRequest(syscall.RTM_DELLINK, 0, ifInfo(0, 0, 0))
			deleteRequest.attr(syscall.IFLA_IFNAME, cString(n.hostVeth))
			conn.do(deleteRequest)
			conn.Close()
		}
		n.reservation.release()
		return nil, err
	}
	return n, nil
}

// connect makes the container's veth pair: eth0 in the network namespace of
// pid, the container's init, and the other end plugged into the bridge.
// Init configures eth0 itself (see configureNetwork). On a network with
// name resolution, it also starts the container's DNS server.
func (n *containerNetwork) connect(pid int) error {
	// netlink is how a program asks the kernel to change network
	// interfaces, as the ip command does (see netlink_linux.go).
	conn, err := openNetlink()
	if err != nil {
		return err
	}
	defer conn.Close()
	// The kernel knows interfaces by number (index) as well as by name. A
	// veth's end is plugged into a bridge by giving the bridge's number.
	bridge, err := net.InterfaceByName(n.network.bridge)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", n.network.bridge, err)
	}
	// One request makes both ends, joined like the ends of a cable: the
	// host's, up and plugged into the bridge, and eth0, made straight in
	// init's network namespace, with a MAC address made from the
	// container's IP address (see macAddress). A number in an attribute is
	// 4 bytes.
	peerMAC := macAddress(n.reservation.ip)
	vethRequest := newRequest(syscall.RTM_NEWLINK, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, ifInfo(0, syscall.IFF_UP, syscall.IFF_UP))
	vethRequest.attr(syscall.IFLA_IFNAME, cString(n.hostVeth))
	vethRequest.attr(syscall.IFLA_MASTER, binary.NativeEndian.AppendUint32(nil, uint32(bridge.Index)))
	vethRequest.nested(syscall.IFLA_LINKINFO, func() {
		vethRequest.attr(iflaInfoKind, cString("veth"))
		vethRequest.nested(iflaInfoData, func() {
			// The peer is described as an interface of its own: an
			// ifinfomsg, then its attributes.
			vethRequest.nested(vethInfoPeer, func() {
				vethRequest.data = append(vethRequest.data, ifInfo(0, 0, 0)...)
				vethRequest.attr(syscall.IFLA_IFNAME, cString(containerInterface))
				vethRequest.attr(syscall.IFLA_ADDRESS, peerMAC)
				vethRequest.attr(syscall.IFLA_NET_NS_PID, binary.NativeEndian.AppendUint32(nil, uint32(pid)))
			})
		})
	})
	if err := conn.do(vethRequest); err != nil {
		return fmt.Errorf("create veth pair %s: %w", n.hostVeth, err)
	}
	if n.network.dns {
		// Start the container's DNS server, on its network. Once its
		// sockets are ready, the serving goes on in two goroutines, one for
		// UDP and one for TCP, until its sockets are closed.
		networkName := n.network.name
		// The host's DNS servers, as host:port, for the DNS server to pass
		// questions on to. On a host whose resolver is systemd-resolved,
		// /etc/resolv.conf names only its stub server at 127.0.0.53, and
		// the servers it asks are in /run/systemd/resolve/resolv.conf, as
		// Docker knows to look. The error is ignored (_): a host with no
		// resolv.conf has no servers, and gets fallbackNameservers.
		resolvData, _ := os.ReadFile("/etc/resolv.conf")
		hostConf := parseResolvConf(string(resolvData))
		if slices.Equal(hostConf.nameservers, []string{"127.0.0.53"}) {
			if data, err := os.ReadFile("/run/systemd/resolve/resolv.conf"); err == nil {
				hostConf = parseResolvConf(string(data))
			}
		}
		nameservers := hostConf.nameservers
		if len(nameservers) == 0 {
			nameservers = fallbackNameservers
		}
		var upstreams []string
		for _, nameserver := range nameservers {
			upstreams = append(upstreams, net.JoinHostPort(nameserver, "53"))
		}
		s := &dnsServer{
			network: networkName,
			// The address of the container on the network called name (in
			// any case, as DNS names are), or with that ID, if it is
			// created or running. It reads every container's state.json
			// each time, so it always sees containers as they come and go,
			// and needs nothing kept up to date.
			lookup: func(name string) net.IP {
				if name == "" {
					return nil
				}
				states, err := listStates(StateRoot)
				if err != nil {
					return nil
				}
				for _, state := range states {
					// Skip containers on other networks, stopped ones (whose
					// address is cleared), and ones with other names.
					// EqualFold compares without regard to case.
					if state.Network != networkName || state.IPAddress == "" || !(strings.EqualFold(state.Name, name) || state.ID == name) {
						continue
					}
					// One whose monitor was killed is gone, though its state
					// says otherwise.
					if initAlive(state.Pid) {
						return net.ParseIP(state.IPAddress)
					}
				}
				return nil
			},
			upstreams: upstreams,
		}
		if s.udp, s.tcp, err = listenDNS(pid); err != nil {
			return fmt.Errorf("start DNS server: %w", err)
		}
		go s.serveUDP()
		go s.serveTCP()
		n.dns = s
	}
	return nil
}

// setup makes the network's bridge, gives it the gateway address, and sets
// up forwarding and NAT, whichever of these isn't done already. A lock keeps
// containers starting at the same time from doing it twice over. Each step
// may find its work already done, by an earlier container, and goes on:
// EEXIST, "already exists", isn't an error here.
func (n *bridgeNetwork) setup() error {
	// 0o700 is an octal file mode: only the owner (root) may read, write,
	// or enter the directory.
	if err := os.MkdirAll(networkStateRoot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", networkStateRoot, err)
	}
	unlock, err := lockFile(filepath.Join(networkStateRoot, ".lock"), true)
	if err != nil {
		return fmt.Errorf("lock bridge networks: %w", err)
	}
	defer unlock()

	conn, err := openNetlink()
	if err != nil {
		return err
	}
	defer conn.Close()
	// The gateway is the bridge's own address: the first in the subnet
	// after the subnet's own (10.88.0.1). Containers send everything not
	// for their own network there, for the host to route on. An IPv4
	// address is 4 bytes, which read as one 32-bit number (big-endian, most
	// significant byte first) can simply have 1 added.
	gateway := net.IP(binary.BigEndian.AppendUint32(nil, binary.BigEndian.Uint32(n.subnet.IP.To4())+1))
	// Make the bridge, a virtual switch, with a fixed MAC address.
	// Otherwise a bridge takes the lowest MAC address of the interfaces
	// plugged into it, which changes as containers come and go, and the
	// containers' ARP caches would point at an address the gateway no
	// longer has.
	bridgeRequest := newRequest(syscall.RTM_NEWLINK, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, ifInfo(0, 0, 0))
	bridgeRequest.attr(syscall.IFLA_IFNAME, cString(n.bridge))
	bridgeRequest.attr(syscall.IFLA_ADDRESS, macAddress(gateway))
	bridgeRequest.nested(syscall.IFLA_LINKINFO, func() {
		bridgeRequest.attr(iflaInfoKind, cString("bridge"))
	})
	if err := conn.do(bridgeRequest); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("create bridge %s: %w", n.bridge, err)
	}
	bridge, err := net.InterfaceByName(n.bridge)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", n.bridge, err)
	}
	// Giving the bridge an address makes the host a member of the
	// network, which containers reach it through, and use as their gateway.
	// The kernel adds a route to the address's subnet through the bridge.
	address := &net.IPNet{IP: gateway, Mask: n.subnet.Mask}
	prefix, _ := address.Mask.Size()
	// struct ifaddrmsg: family, prefix length, flags, scope, and interface.
	body := make([]byte, syscall.SizeofIfAddrmsg)
	body[0] = syscall.AF_INET
	body[1] = byte(prefix)
	binary.NativeEndian.PutUint32(body[4:], uint32(bridge.Index))
	addressRequest := newRequest(syscall.RTM_NEWADDR, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, body)
	addressRequest.attr(syscall.IFA_LOCAL, address.IP.To4())
	addressRequest.attr(syscall.IFA_ADDRESS, address.IP.To4())
	if err := conn.do(addressRequest); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("give bridge %s address %s: %w", n.bridge, gateway, err)
	}
	if err := conn.do(newRequest(syscall.RTM_NEWLINK, 0, ifInfo(bridge.Index, syscall.IFF_UP, syscall.IFF_UP))); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", n.bridge, err)
	}
	// Let the host route packets from one interface to another, here from
	// the bridge to the outside world and back. Docker turns it on the same
	// way, and leaves it on. Files under /proc/sys are kernel settings:
	// writing 1 to this one turns forwarding on.
	const forwardPath = "/proc/sys/net/ipv4/ip_forward"
	if current, err := os.ReadFile(forwardPath); err != nil || strings.TrimSpace(string(current)) != "1" {
		if err := os.WriteFile(forwardPath, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("turn on IP forwarding: %w", err)
		}
	}
	// Add whichever of the shared rules and the network's rules are
	// missing. They go first in their chains, ahead of any rules that would
	// drop the packets. The shared rules go in first, so the network's come
	// ahead of the one that isolates networks: each rule inserted at the top
	// pushes the earlier ones down.
	//
	// First, the chain of our own for published ports, unless it is there
	// already. iptables -L lists a chain, and fails if there is no such
	// chain; -N makes a new one.
	if _, err := runIptables("-t", "nat", "-n", "-L", portsChain); err != nil {
		if _, err := runIptables("-t", "nat", "-N", portsChain); err != nil {
			return err
		}
	}
	// The rules all networks share. Each is a table, a chain, and the rule,
	// as iptables takes it on its command line.
	shared := []iptablesRule{
		// Nothing passes from one network to another, unless a rule ahead
		// of this one lets it through: the networks' own (below) only let
		// through what stays on the network or leaves for the outside.
		// -i is the interface a packet came in on, and -o the one it is
		// about to leave by.
		{"filter", "FORWARD", []string{"-i", allBridges, "-o", allBridges, "-j", "DROP"}},
		// Connections to the host's addresses go through the published
		// ports' rules: those from elsewhere, and those from the host
		// itself, except to its loopback addresses, which the ports'
		// proxies take instead. "-m addrtype --dst-type LOCAL" matches
		// packets addressed to any of the host's own addresses, and !
		// negates the test after it.
		{"nat", "PREROUTING", []string{"-m", "addrtype", "--dst-type", "LOCAL", "-j", portsChain}},
		{"nat", "OUTPUT", []string{"!", "-d", "127.0.0.0/8", "-m", "addrtype", "--dst-type", "LOCAL", "-j", portsChain}},
	}
	// The network's own rules, each a table, a chain, and the rule
	// (RemoveNetwork removes the same ones).
	subnet := n.subnet.String()
	own := []iptablesRule{
		// Masquerading: a packet from a container that leaves the host by
		// any other interface gets the host's address as its source.
		// Connection tracking remembers the change, and undoes it on the
		// replies.
		{"nat", "POSTROUTING", []string{"-s", subnet, "!", "-o", n.bridge, "-j", "MASQUERADE"}},
		// Let through what containers send each other (which the host's
		// firewall sees, if br_netfilter is loaded, as Docker loads it) and
		// what they send beyond runt's bridges, and the replies, on hosts
		// whose firewall drops forwarded packets, as Docker and ufw set it
		// up. And let through connections to published ports: those that
		// went through a DNAT rule. Connection tracking (-m conntrack) knows
		// which connection each packet belongs to, and so whether it is a
		// reply (ESTABLISHED) or its destination was rewritten (DNAT).
		{"filter", "FORWARD", []string{"-i", n.bridge, "-o", n.bridge, "-j", "ACCEPT"}},
		{"filter", "FORWARD", []string{"-i", n.bridge, "!", "-o", allBridges, "-j", "ACCEPT"}},
		{"filter", "FORWARD", []string{"-o", n.bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
		{"filter", "FORWARD", []string{"-o", n.bridge, "-m", "conntrack", "--ctstate", "DNAT", "-j", "ACCEPT"}},
	}
	// append(a, b...) joins two slices; the ... passes b's elements one by
	// one. Each rule is put first in its chain unless it is there already:
	// iptables -C (check) succeeds if it is. Checking first makes setting
	// up a network safe to repeat, which it is, for every container.
	for _, rule := range append(shared, own...) {
		if rule.run("-C") == nil {
			continue
		}
		if err := rule.run("-I"); err != nil {
			return err
		}
	}
	return nil
}

// portsChain is the nat chain for published ports' rules (see
// publishedPort), as the DOCKER chain is for Docker.
//
// iptables, briefly: the kernel passes each packet through chains of rules,
// at fixed points on its way. PREROUTING sees packets as they arrive,
// FORWARD those the host routes from one interface to another, OUTPUT those
// the host itself sends, and POSTROUTING all packets as they leave. Chains
// are grouped in tables by purpose: nat rewrites addresses, and filter lets
// packets through (ACCEPT) or drops them (DROP). The first rule in a chain
// that matches a packet decides what happens to it. A rule can also jump
// (-j) to a chain of our own, like RUNT.
const portsChain = "RUNT"

// configureNetwork runs in init: it gives eth0, which runt has moved into the
// container's network namespace, its address, brings it up, and makes the
// bridge the default route.
func configureNetwork(network *initNetwork) error {
	// ParseCIDR splits "10.88.0.2/16" into the address and its network.
	ip, subnet, err := net.ParseCIDR(network.Address)
	if err != nil {
		return fmt.Errorf("container address: %w", err)
	}
	gateway := net.ParseIP(network.Gateway)
	if gateway == nil {
		return fmt.Errorf("invalid gateway address %q", network.Gateway)
	}
	link, err := net.InterfaceByName(containerInterface)
	if err != nil {
		return fmt.Errorf("find %s: %w", containerInterface, err)
	}
	conn, err := openNetlink()
	if err != nil {
		return err
	}
	defer conn.Close()
	// Give eth0 its IPv4 address. The kernel adds a route to the address's
	// subnet through the interface.
	address := &net.IPNet{IP: ip, Mask: subnet.Mask}
	prefix, _ := address.Mask.Size()
	// struct ifaddrmsg: family, prefix length, flags, scope, and interface.
	body := make([]byte, syscall.SizeofIfAddrmsg)
	body[0] = syscall.AF_INET
	body[1] = byte(prefix)
	binary.NativeEndian.PutUint32(body[4:], uint32(link.Index))
	addressRequest := newRequest(syscall.RTM_NEWADDR, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, body)
	addressRequest.attr(syscall.IFA_LOCAL, address.IP.To4())
	addressRequest.attr(syscall.IFA_ADDRESS, address.IP.To4())
	if err := conn.do(addressRequest); err != nil {
		return fmt.Errorf("give %s address %s: %w", containerInterface, network.Address, err)
	}
	if err := conn.do(newRequest(syscall.RTM_NEWLINK, 0, ifInfo(link.Index, syscall.IFF_UP, syscall.IFF_UP))); err != nil {
		return fmt.Errorf("bring up %s: %w", containerInterface, err)
	}
	// The default route: anything not on the container's own network, with
	// no more specific route, goes to the gateway, the host's end of the
	// bridge.
	//
	// struct rtmsg: family, destination and source prefix lengths (0, so
	// any destination), TOS, table, protocol, scope, type, and flags.
	routeBody := make([]byte, syscall.SizeofRtMsg)
	routeBody[0] = syscall.AF_INET
	routeBody[4] = syscall.RT_TABLE_MAIN
	routeBody[5] = syscall.RTPROT_BOOT
	routeBody[6] = syscall.RT_SCOPE_UNIVERSE
	routeBody[7] = syscall.RTN_UNICAST
	routeRequest := newRequest(syscall.RTM_NEWROUTE, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, routeBody)
	routeRequest.attr(syscall.RTA_GATEWAY, gateway.To4())
	if err := conn.do(routeRequest); err != nil {
		return fmt.Errorf("add default route via %s: %w", gateway, err)
	}
	return nil
}
