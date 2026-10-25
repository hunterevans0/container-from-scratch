//go:build linux

package runtime

// User-defined networks: runt network create, ls, and rm, and how a network
// is found again by name when a container joins it (see joinNetwork in
// network_linux.go). For how to read the Go here, see doc.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Networks made with runt network create are kept as a JSON file each,
// <name>.json, in networkConfigRoot. Unlike their bridges and address
// reservations, which are under /run, they last across reboots, as Docker's
// do. A lock in the same directory keeps a network from being removed while
// a container is joining it (see joinNetwork).
const networkConfigRoot = "/var/lib/runt/networks"

// lockNetworks locks the networks' configuration, shared or exclusive. It
// returns a function that releases the lock. A shared lock, for reading,
// can be held by many processes at once; an exclusive one, for changing
// networks, only by one, and only while nobody holds a shared one. Callers
// usually defer the function they get back, so the lock is released when
// they return.
func lockNetworks(exclusive bool) (func(), error) {
	if err := os.MkdirAll(networkConfigRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", networkConfigRoot, err)
	}
	// The name starts with a dot so it can't clash with a network's file:
	// network names must start with a letter or digit.
	unlock, err := lockFile(filepath.Join(networkConfigRoot, ".lock"), exclusive)
	if err != nil {
		return nil, fmt.Errorf("lock networks: %w", err)
	}
	return unlock, nil
}

func networkConfigPath(name string) string {
	return filepath.Join(networkConfigRoot, name+".json")
}

// readNetwork reads the configuration of the network called name.
func readNetwork(name string) (*Network, error) {
	// A name such as ../x would lead out of networkConfigRoot. validName
	// (in state.go) is the pattern container names must match too.
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
	// fields by their tags (see Network, in runtime.go). It needs a pointer,
	// &network, to be able to change the variable.
	var network Network
	if err := json.Unmarshal(data, &network); err != nil {
		return nil, fmt.Errorf("parse network %s: %w", name, err)
	}
	return &network, nil
}

// loadNetwork returns the network called name, made with runt network
// create.
func loadNetwork(name string) (*bridgeNetwork, error) {
	network, err := readNetwork(name)
	if err != nil {
		return nil, err
	}
	return network.bridgeNetwork()
}

// bridgeNetwork turns a network's saved configuration, which keeps its
// subnet as text, into the form the rest of the code works with. Network is
// the exported type, which cmd/runt sees; bridgeNetwork is runt's own.
func (n *Network) bridgeNetwork() (*bridgeNetwork, error) {
	_, subnet, err := net.ParseCIDR(n.Subnet)
	if err != nil {
		return nil, fmt.Errorf("network %s: %w", n.Name, err)
	}
	return &bridgeNetwork{name: n.Name, bridge: n.Bridge, subnet: subnet, dns: true}, nil
}

// userNetworks reads every network made with runt network create, by name.
func userNetworks() ([]Network, error) {
	// Glob lists the files matching a pattern, as a shell's * does.
	paths, err := filepath.Glob(filepath.Join(networkConfigRoot, "*.json"))
	if err != nil {
		return nil, err
	}
	var networks []Network
	for _, path := range paths {
		network, err := readNetwork(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			return nil, err
		}
		// network is a pointer; *network is the Network it points to,
		// which append copies into the slice.
		networks = append(networks, *network)
	}
	// sort.Slice sorts with a function that says whether element i goes
	// before element j.
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	return networks, nil
}

// checkNetworkName rejects a name that is malformed or is a built-in
// network's.
func checkNetworkName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid network name %q: use letters, digits, and _.- and start with a letter or digit", name)
	}
	switch name {
	case NetworkBridge, NetworkNone, "host":
		return fmt.Errorf("%q is the name of a built-in network", name)
	}
	return nil
}

// CreateNetwork makes a network, which containers join with --network NAME,
// and on which they find each other by name. subnet is its range of
// addresses, or, if empty, the first free /24 from 10.89.0.0/24 on, as
// Podman picks. The network's bridge is made when a container first needs
// it.
//
// All it does now is choose the network's subnet, ID, and bridge name, and
// save them. The exclusive lock keeps two runt network create commands from
// choosing the same subnet, or name, at once.
func CreateNetwork(name, subnet string) (*Network, error) {
	if err := checkNetworkName(name); err != nil {
		return nil, err
	}
	unlock, err := lockNetworks(true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := os.Stat(networkConfigPath(name)); err == nil {
		return nil, fmt.Errorf("network %s already exists", name)
	}
	networks, err := userNetworks()
	if err != nil {
		return nil, err
	}
	// The subnets the new network mustn't overlap. Two networks sharing
	// addresses would confuse the host's routing: it couldn't tell which
	// bridge an address is behind.
	taken := []*net.IPNet{defaultSubnet}
	for _, network := range networks {
		if _, subnet, err := net.ParseCIDR(network.Subnet); err == nil {
			taken = append(taken, subnet)
		}
	}
	var chosen *net.IPNet
	if subnet != "" {
		if chosen, err = parseSubnet(subnet); err != nil {
			return nil, err
		}
		if other := overlapping(chosen, taken); other != nil {
			return nil, fmt.Errorf("subnet %s overlaps %s, another runt network's", chosen, other)
		}
	} else {
		// Nor any of the host's own networks, so the host can still reach
		// them. InterfaceAddrs lists the addresses of the host's interfaces,
		// each with its network's mask, such as 192.168.1.20/24; Mask makes
		// that the network itself, 192.168.1.0/24. The ", ok" form of a
		// type assertion reports whether the value holds that type, rather
		// than crashing if it doesn't.
		if addresses, err := net.InterfaceAddrs(); err == nil {
			for _, address := range addresses {
				if ipNet, ok := address.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					taken = append(taken, &net.IPNet{IP: ipNet.IP.Mask(ipNet.Mask), Mask: ipNet.Mask})
				}
			}
		}
		if chosen, err = freeSubnet(taken); err != nil {
			return nil, err
		}
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	network := Network{
		Name: name,
		ID:   id,
		// Interface names have at most 15 characters. Starting with runt
		// puts the bridge under allBridges.
		Bridge:  "runt-" + id[:10],
		Subnet:  chosen.String(),
		Created: time.Now(),
	}
	data, err := json.MarshalIndent(network, "", "  ")
	if err != nil {
		return nil, err
	}
	// Written under another name and then renamed into place, so nobody
	// ever reads a half-written file: a rename happens all at once.
	temp := networkConfigPath(name) + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("save network %s: %w", name, err)
	}
	if err := os.Rename(temp, networkConfigPath(name)); err != nil {
		return nil, fmt.Errorf("save network %s: %w", name, err)
	}
	return &network, nil
}

// parseSubnet parses a network's range of addresses, such as 10.90.0.0/24.
func parseSubnet(s string) (*net.IPNet, error) {
	ip, subnet, err := net.ParseCIDR(s)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet %q: %w", s, err)
	}
	if ip.To4() == nil {
		return nil, fmt.Errorf("invalid subnet %q: only IPv4 networks are supported", s)
	}
	// 10.90.0.1/24 names an address within 10.90.0.0/24, not the network
	// itself, which is probably a mistake.
	if !ip.Equal(subnet.IP) {
		return nil, fmt.Errorf("invalid subnet %q: did you mean %s?", s, subnet)
	}
	// The subnet's own address, the gateway's, and the broadcast address
	// leave the rest for containers. Size returns the mask's length (the
	// number after the /) and the address's, of which only the first is
	// needed here.
	if ones, _ := subnet.Mask.Size(); ones > 30 {
		return nil, fmt.Errorf("invalid subnet %q: it leaves no addresses for containers; use /30 or larger", s)
	}
	return subnet, nil
}

// overlapping returns the first of others that shares addresses with
// subnet, or nil. Two subnets are either apart or one is inside the other,
// in which case one contains the other's first address.
func overlapping(subnet *net.IPNet, others []*net.IPNet) *net.IPNet {
	for _, other := range others {
		if subnet.Contains(other.IP) || other.Contains(subnet.IP) {
			return other
		}
	}
	return nil
}

// freeSubnet returns the first of 10.89.0.0/24, 10.89.1.0/24, and so on up
// to 10.89.255.0/24 that overlaps none of taken.
func freeSubnet(taken []*net.IPNet) (*net.IPNet, error) {
	for i := 0; i < 256; i++ {
		candidate := &net.IPNet{IP: net.IP{10, 89, byte(i), 0}, Mask: net.CIDRMask(24, 32)}
		if overlapping(candidate, taken) == nil {
			return candidate, nil
		}
	}
	return nil, errors.New("no free subnet left in 10.89.0.0/16; give one with --subnet")
}

// ListNetworks returns the built-in networks, bridge and none, and then the
// ones made with CreateNetwork, by name.
func ListNetworks() ([]Network, error) {
	networks := []Network{
		{Name: NetworkBridge, Bridge: defaultBridge, Subnet: defaultSubnet.String()},
		{Name: NetworkNone},
	}
	unlock, err := lockNetworks(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	user, err := userNetworks()
	if err != nil {
		return nil, err
	}
	return append(networks, user...), nil
}

// RemoveNetwork removes a network made with CreateNetwork, along with its
// bridge and iptables rules, if a container ever made them. Containers that
// are created or running must leave it first.
//
// The exclusive lock waits for any container that is joining the network
// (see joinNetwork) to have reserved its address, which inUse then sees.
func RemoveNetwork(name string) error {
	if name == NetworkBridge || name == NetworkNone {
		return fmt.Errorf("the %s network is built in, and can't be removed", name)
	}
	unlock, err := lockNetworks(true)
	if err != nil {
		return err
	}
	defer unlock()
	network, err := loadNetwork(name)
	if err != nil {
		return err
	}
	inUse, err := network.ipam().inUse()
	if err != nil {
		return err
	}
	if inUse {
		return fmt.Errorf("network %s has containers on it; stop or remove them first", name)
	}
	conn, err := openNetlink()
	if err != nil {
		return err
	}
	err = conn.deleteLink(network.bridge)
	conn.Close()
	// ENODEV: no such device. No container ever joined, so there is no
	// bridge, or a reboot took it.
	if err != nil && !errors.Is(err, syscall.ENODEV) {
		return fmt.Errorf("delete bridge %s: %w", network.bridge, err)
	}
	for _, rule := range network.rules() {
		if err := rule.remove(); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(network.ipam().dir); err != nil {
		return err
	}
	return os.Remove(networkConfigPath(name))
}
