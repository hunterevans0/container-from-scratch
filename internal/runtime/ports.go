package runtime

// This file has no build constraint, unlike most networking files, because
// the runt command parses -p options on every platform, Windows included,
// before it finds out whether it can run containers. For how to read the Go
// here, see doc.go.

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// PortMapping publishes a container's port on the host, as docker run -p
// does: connections to HostPort on the host go to ContainerPort in the
// container. It is exported (capitalized) because cmd/runt makes them from
// -p options, and saved in state.json, under the names in the json tags.
type PortMapping struct {
	// HostIP is the host address to take connections at; empty means all
	// of them.
	HostIP string `json:"hostIp,omitempty"`
	// HostPort 0 means any free port, which the system picks.
	HostPort      int `json:"hostPort"`
	ContainerPort int `json:"containerPort"`
	// Protocol is "tcp" or "udp".
	Protocol string `json:"protocol"`
}

// ParsePortMapping parses a -p option, [[HOSTIP:]HOSTPORT:]CONTAINERPORT
// with an optional /tcp or /udp, as Docker takes it: 8080:80,
// 127.0.0.1:8080:80, or 53:53/udp. Without a host port, as in 80 or
// 127.0.0.1::80, the system picks a free one.
//
// It works from the outside in: split off the protocol after the /, then
// split the rest at its colons, and the number of pieces says which form it
// is. Every way out returns the mapping and an error; the caller looks only
// at the error until it is nil.
func ParsePortMapping(s string) (PortMapping, error) {
	mapping := PortMapping{Protocol: "tcp"}
	// strings.Cut splits s at the first "/": what comes before, what comes
	// after, and whether there was a "/" at all.
	spec, protocol, hasProtocol := strings.Cut(s, "/")
	if hasProtocol {
		if protocol != "tcp" && protocol != "udp" {
			return mapping, fmt.Errorf("invalid port mapping %q: the protocol must be tcp or udp", s)
		}
		mapping.Protocol = protocol
	}
	var hostIP, hostPort, containerPort string
	// A switch can declare a variable first (parts, here) and then switch on
	// an expression. Unlike in C, a case doesn't fall through into the next.
	switch parts := strings.Split(spec, ":"); len(parts) {
	case 1:
		containerPort = parts[0]
	case 2:
		hostPort, containerPort = parts[0], parts[1]
	case 3:
		hostIP, hostPort, containerPort = parts[0], parts[1], parts[2]
	default:
		return mapping, fmt.Errorf("invalid port mapping %q: use [[HOSTIP:]HOSTPORT:]CONTAINERPORT[/PROTOCOL]", s)
	}
	if hostIP != "" {
		// To4 gives the address as 4 bytes, or nil if it isn't IPv4. A
		// string that isn't an address at all gives nil from ParseIP, and
		// To4 of nil is nil too, so one check covers both.
		ip := net.ParseIP(hostIP).To4()
		if ip == nil {
			return mapping, fmt.Errorf("invalid port mapping %q: the host address must be an IPv4 address", s)
		}
		// 0.0.0.0 means all addresses, which is what no address means here.
		if !ip.IsUnspecified() {
			mapping.HostIP = ip.String()
		}
	}
	var err error
	if mapping.ContainerPort, err = parsePort(containerPort); err != nil {
		return mapping, fmt.Errorf("invalid port mapping %q: %w", s, err)
	}
	// An empty host port, as in "80" or "127.0.0.1::80", leaves HostPort 0.
	if hostPort != "" {
		if mapping.HostPort, err = parsePort(hostPort); err != nil {
			return mapping, fmt.Errorf("invalid port mapping %q: %w", s, err)
		}
	}
	return mapping, nil
}

// parsePort parses a port number. strconv.Atoi turns a string into an int,
// and fails on anything that isn't a whole number, such as a range like
// 8000-8010, which runt doesn't support.
func parsePort(s string) (int, error) {
	port, err := strconv.Atoi(s)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q is not a port number from 1 to 65535", s)
	}
	return port, nil
}

// String formats the mapping as docker ps shows it, as in
// 0.0.0.0:8080->80/tcp. A type with a String method is printed with it by
// the fmt package, as by fmt.Println.
func (m PortMapping) String() string {
	hostIP := m.HostIP
	if hostIP == "" {
		hostIP = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d->%d/%s", hostIP, m.HostPort, m.ContainerPort, m.Protocol)
}
