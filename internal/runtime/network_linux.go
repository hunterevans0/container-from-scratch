//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// The bridge network works like Docker's default one. The host has a
// bridge, runt0, a virtual switch with the network's first address, which is
// every container's gateway. Each container gets a veth pair, two virtual
// interfaces joined like the ends of a cable: one end is eth0 in the
// container's network namespace, with an address of the container's own, and
// the other end is plugged into the bridge. So containers reach each other
// through the bridge, and the host through its address. To reach anything
// further, the host routes their packets on, rewriting the source address to
// its own (NAT), so that replies come back to the host, which passes them
// on to the container.
//
// The bridge and the NAT rules are made the first time a container needs
// them and left in place, as Docker leaves docker0. A container's veth pair
// and address last as long as the container runs.

const (
	// bridgeName is the bridge on the host, as docker0 is for Docker.
	bridgeName = "runt0"
	// containerInterface is the container's end of its veth pair.
	containerInterface = "eth0"
	// networkStateRoot holds the bridge network's lock and its address
	// reservations (see ipam). Like StateRoot it is on a tmpfs, since the
	// bridge and the containers are gone after a reboot too.
	networkStateRoot = "/run/runt-net"
)

// bridgeSubnet is the bridge network's range of addresses, the same as
// Podman's default network. Docker's is 172.17.0.0/16.
var bridgeSubnet = &net.IPNet{IP: net.IP{10, 88, 0, 0}, Mask: net.CIDRMask(16, 32)}

// bridgeGateway is the bridge's own address.
func bridgeGateway() net.IP {
	return subnetAddress(bridgeSubnet, 1)
}

func bridgeIPAM() *ipam {
	return &ipam{dir: filepath.Join(networkStateRoot, NetworkBridge), subnet: bridgeSubnet}
}

// initNetwork is how init configures the container's end of the veth pair.
type initNetwork struct {
	// Address is the container's address, with the subnet's prefix
	// length, as in 10.88.0.2/16.
	Address string `json:"address"`
	Gateway string `json:"gateway"`
}

// containerNetwork is a container's place on the bridge network.
type containerNetwork struct {
	reservation *reservation
	// hostVeth is the host's end of the container's veth pair.
	hostVeth string
}

// joinBridgeNetwork sets up the bridge and NAT if they aren't already, and
// reserves an address for container id. The veth pair comes later, once the
// container's network namespace exists (see connect).
func joinBridgeNetwork(id string) (*containerNetwork, error) {
	if err := setupBridge(); err != nil {
		return nil, err
	}
	reservation, err := bridgeIPAM().reserve(id)
	if err != nil {
		return nil, err
	}
	// Interface names have at most 15 characters.
	return &containerNetwork{reservation: reservation, hostVeth: "veth" + id[:8]}, nil
}

// address is the container's address, as eth0 gets it.
func (n *containerNetwork) address() *net.IPNet {
	return &net.IPNet{IP: n.reservation.ip, Mask: bridgeSubnet.Mask}
}

// initConfig is what init needs to configure eth0.
func (n *containerNetwork) initConfig() *initNetwork {
	return &initNetwork{Address: n.address().String(), Gateway: bridgeGateway().String()}
}

// connect makes the container's veth pair: eth0 in the network namespace of
// pid, the container's init, and the other end plugged into the bridge.
// Init configures eth0 itself (see configureNetwork).
func (n *containerNetwork) connect(pid int) error {
	conn, err := openNetlink()
	if err != nil {
		return err
	}
	defer conn.Close()
	bridge, err := net.InterfaceByName(bridgeName)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", bridgeName, err)
	}
	if err := conn.createVeth(n.hostVeth, bridge.Index, containerInterface, macAddress(n.reservation.ip), pid); err != nil {
		return fmt.Errorf("create veth pair %s: %w", n.hostVeth, err)
	}
	return nil
}

// leave unplugs the container from the bridge and gives its address back.
func (n *containerNetwork) leave() error {
	var errs []error
	if conn, err := openNetlink(); err != nil {
		errs = append(errs, err)
	} else {
		// The pair also goes when the container's network namespace does,
		// after its last process has exited, so it may be gone already.
		if err := conn.deleteLink(n.hostVeth); err != nil && !errors.Is(err, syscall.ENODEV) {
			errs = append(errs, fmt.Errorf("delete veth %s: %w", n.hostVeth, err))
		}
		conn.Close()
	}
	// Only now, so that the next container to get the address can't find
	// this one's veth still plugged in.
	errs = append(errs, n.reservation.release())
	return errors.Join(errs...)
}

// setupBridge makes the bridge, gives it the gateway address, and sets up
// forwarding and NAT, whichever of these isn't done already. A lock keeps
// containers starting at the same time from doing it twice over.
func setupBridge() error {
	if err := os.MkdirAll(networkStateRoot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", networkStateRoot, err)
	}
	unlock, err := lockFile(filepath.Join(networkStateRoot, "lock"), true)
	if err != nil {
		return fmt.Errorf("lock bridge network: %w", err)
	}
	defer unlock()

	conn, err := openNetlink()
	if err != nil {
		return err
	}
	defer conn.Close()
	gateway := bridgeGateway()
	if err := conn.createBridge(bridgeName, macAddress(gateway)); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("create bridge %s: %w", bridgeName, err)
	}
	bridge, err := net.InterfaceByName(bridgeName)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", bridgeName, err)
	}
	if err := conn.addAddress(bridge.Index, &net.IPNet{IP: gateway, Mask: bridgeSubnet.Mask}); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("give bridge %s address %s: %w", bridgeName, gateway, err)
	}
	if err := conn.setUp(bridge.Index); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", bridgeName, err)
	}
	if err := enableForwarding(); err != nil {
		return err
	}
	return setupNAT()
}

// enableForwarding lets the host route packets from one interface to
// another, here from the bridge to the outside world and back. Docker turns
// it on the same way, and leaves it on.
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

// natRules are the iptables rules for the bridge network, each a table, a
// chain, and the rule.
func natRules() [][]string {
	subnet := bridgeSubnet.String()
	return [][]string{
		// Masquerading: a packet from a container that leaves the host by
		// any other interface gets the host's address as its source.
		// Connection tracking remembers the change, and undoes it on the
		// replies.
		{"nat", "POSTROUTING", "-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE"},
		// Let through what containers send, and the replies, on hosts whose
		// firewall drops forwarded packets, as Docker and ufw set it up.
		{"filter", "FORWARD", "-i", bridgeName, "-j", "ACCEPT"},
		{"filter", "FORWARD", "-o", bridgeName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
}

// setupNAT adds whichever of natRules are missing. They go first in their
// chains, ahead of any rules that would drop the packets.
func setupNAT() error {
	iptables, err := exec.LookPath("iptables")
	if err != nil {
		return errors.New("the bridge network needs iptables, for NAT: install it (apt install iptables), or use --network none")
	}
	for _, rule := range natRules() {
		table, chain, spec := rule[0], rule[1], rule[2:]
		// -C checks whether the rule is there. -w waits for another
		// program changing the rules to finish, rather than failing.
		check := append([]string{"-w", "-t", table, "-C", chain}, spec...)
		if exec.Command(iptables, check...).Run() == nil {
			continue
		}
		insert := append([]string{"-w", "-t", table, "-I", chain}, spec...)
		if output, err := exec.Command(iptables, insert...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %s: %w: %s", strings.Join(insert, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

// configureNetwork runs in init: it gives eth0, which runt has moved into the
// container's network namespace, its address, brings it up, and makes the
// bridge the default route.
func configureNetwork(network *initNetwork) error {
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
	if err := conn.addDefaultRoute(gateway); err != nil {
		return fmt.Errorf("add default route via %s: %w", gateway, err)
	}
	return nil
}
