//go:build linux

package runtime

// Bridge networks, and how a container joins one. For an overview of the
// package, and how to read the Go here, see doc.go.

// import lists the packages this file uses: errors and fmt for errors and
// formatting, net for IP addresses and interfaces, os for files, path/filepath
// for building paths, strings for string handling, and syscall for raw
// system calls and error numbers.
import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
//  4. leave, once the container has exited: undo 1 and 2.

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

// defaultNetwork returns the built-in network. Fields left out of a struct
// literal, here dns, get their zero value, false.
func defaultNetwork() *bridgeNetwork {
	return &bridgeNetwork{name: NetworkBridge, bridge: defaultBridge, subnet: defaultSubnet}
}

// gateway is the bridge's own address: the first in the subnet after the
// subnet's own (10.88.0.1). Containers send everything not for their own
// network there, for the host to route on.
func (n *bridgeNetwork) gateway() net.IP {
	return subnetAddress(n.subnet, 1)
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
	network := defaultNetwork()
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
		if network, err = loadNetwork(name); err != nil {
			return nil, err
		}
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
	// it are a dead container's. The anonymous function tells
	// removeStaleDNAT which rules to remove: those whose destination is this
	// address.
	err = removeStaleDNAT(func(rule dnatRule) bool { return rule.toIP == reservation.ip.String() })
	if err == nil {
		err = n.publish(ports)
	}
	if err != nil {
		// Give back whatever was taken: the address, and any ports
		// published before the one that failed.
		n.leave()
		return nil, err
	}
	return n, nil
}

// address is the container's address, as eth0 gets it: the reserved address
// with the network's mask, which tells the container which addresses are on
// its own network and reachable directly, through the bridge.
func (n *containerNetwork) address() *net.IPNet {
	return &net.IPNet{IP: n.reservation.ip, Mask: n.network.subnet.Mask}
}

// initConfig is what init needs to configure eth0.
func (n *containerNetwork) initConfig() *initNetwork {
	return &initNetwork{Address: n.address().String(), Gateway: n.network.gateway().String()}
}

// portMappings are the container's published ports, with the host ports
// the system picked, for state.json and runt ps.
func (n *containerNetwork) portMappings() []PortMapping {
	var mappings []PortMapping
	for _, port := range n.ports {
		mappings = append(mappings, port.mapping)
	}
	return mappings
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
	// One request makes both ends: the host's, plugged into the bridge,
	// and eth0, made straight in init's network namespace, with a MAC
	// address made from the container's IP address (see macAddress).
	if err := conn.createVeth(n.hostVeth, bridge.Index, containerInterface, macAddress(n.reservation.ip), pid); err != nil {
		return fmt.Errorf("create veth pair %s: %w", n.hostVeth, err)
	}
	if n.network.dns {
		if n.dns, err = startDNS(pid, n.network.name); err != nil {
			return err
		}
	}
	return nil
}

// leave unpublishes the container's ports, unplugs it from the bridge, and
// gives its address back. It keeps going past errors, so that one thing
// failing doesn't leave the rest behind, and returns them all together.
func (n *containerNetwork) leave() error {
	var errs []error
	if n.dns != nil {
		n.dns.close()
	}
	for _, port := range n.ports {
		errs = append(errs, port.close())
	}
	if conn, err := openNetlink(); err != nil {
		errs = append(errs, err)
	} else {
		// The pair also goes when the container's network namespace does,
		// after its last process has exited, so it may be gone already:
		// ENODEV, "no such device", is fine.
		if err := conn.deleteLink(n.hostVeth); err != nil && !errors.Is(err, syscall.ENODEV) {
			errs = append(errs, fmt.Errorf("delete veth %s: %w", n.hostVeth, err))
		}
		conn.Close()
	}
	// Only now, so that the next container to get the address can't find
	// this one's veth still plugged in, or its port rules still in place.
	errs = append(errs, n.reservation.release())
	// errors.Join makes one error of several, leaving out the nils. If all
	// of them are nil, so is the result.
	return errors.Join(errs...)
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
	gateway := n.gateway()
	if err := conn.createBridge(n.bridge, macAddress(gateway)); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("create bridge %s: %w", n.bridge, err)
	}
	bridge, err := net.InterfaceByName(n.bridge)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", n.bridge, err)
	}
	// Giving the bridge an address makes the host a member of the
	// network, which containers reach it through, and use as their gateway.
	if err := conn.addAddress(bridge.Index, &net.IPNet{IP: gateway, Mask: n.subnet.Mask}); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("give bridge %s address %s: %w", n.bridge, gateway, err)
	}
	if err := conn.setUp(bridge.Index); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", n.bridge, err)
	}
	if err := enableForwarding(); err != nil {
		return err
	}
	return n.setupIptables()
}

// enableForwarding lets the host route packets from one interface to
// another, here from the bridge to the outside world and back. Docker turns
// it on the same way, and leaves it on. Files under /proc/sys are kernel
// settings: writing 1 to this one turns forwarding on.
func enableForwarding() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	if current, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(current)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("turn on IP forwarding: %w", err)
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

// sharedRules are the iptables rules all networks share. Each is a table, a
// chain, and the rule, as iptables takes it on its command line.
func sharedRules() []iptablesRule {
	return []iptablesRule{
		// Nothing passes from one network to another, unless a rule ahead
		// of this one lets it through: the networks' own (see rules) only
		// let through what stays on the network or leaves for the outside.
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
}

// rules are the network's iptables rules, each a table, a chain, and the
// rule.
func (n *bridgeNetwork) rules() []iptablesRule {
	subnet := n.subnet.String()
	return []iptablesRule{
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
}

// setupIptables adds whichever of the shared rules and the network's rules
// are missing. They go first in their chains, ahead of any rules that would
// drop the packets. The shared rules go in first, so the network's come
// ahead of the one that isolates networks: each rule inserted at the top
// pushes the earlier ones down.
func (n *bridgeNetwork) setupIptables() error {
	if err := ensureChain("nat", portsChain); err != nil {
		return err
	}
	// append(a, b...) joins two slices; the ... passes b's elements one by
	// one.
	for _, rule := range append(sharedRules(), n.rules()...) {
		if err := rule.insert(); err != nil {
			return err
		}
	}
	return nil
}

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
	if err := conn.addAddress(link.Index, &net.IPNet{IP: ip, Mask: subnet.Mask}); err != nil {
		return fmt.Errorf("give %s address %s: %w", containerInterface, network.Address, err)
	}
	if err := conn.setUp(link.Index); err != nil {
		return fmt.Errorf("bring up %s: %w", containerInterface, err)
	}
	// The default route: anything not on the container's own network goes
	// to the gateway, the host's end of the bridge.
	if err := conn.addDefaultRoute(gateway); err != nil {
		return fmt.Errorf("add default route via %s: %w", gateway, err)
	}
	return nil
}
