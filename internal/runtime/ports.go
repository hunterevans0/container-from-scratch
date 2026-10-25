package runtime

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// PortMapping publishes a container's port on the host, as docker run -p
// does: connections to HostPort on the host go to ContainerPort in the
// container.
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
func ParsePortMapping(s string) (PortMapping, error) {
	mapping := PortMapping{Protocol: "tcp"}
	spec, protocol, hasProtocol := strings.Cut(s, "/")
	if hasProtocol {
		if protocol != "tcp" && protocol != "udp" {
			return mapping, fmt.Errorf("invalid port mapping %q: the protocol must be tcp or udp", s)
		}
		mapping.Protocol = protocol
	}
	var hostIP, hostPort, containerPort string
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
		ip := net.ParseIP(hostIP).To4()
		if ip == nil {
			return mapping, fmt.Errorf("invalid port mapping %q: the host address must be an IPv4 address", s)
		}
		if !ip.IsUnspecified() {
			mapping.HostIP = ip.String()
		}
	}
	var err error
	if mapping.ContainerPort, err = parsePort(containerPort); err != nil {
		return mapping, fmt.Errorf("invalid port mapping %q: %w", s, err)
	}
	if hostPort != "" {
		if mapping.HostPort, err = parsePort(hostPort); err != nil {
			return mapping, fmt.Errorf("invalid port mapping %q: %w", s, err)
		}
	}
	return mapping, nil
}

func parsePort(s string) (int, error) {
	port, err := strconv.Atoi(s)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q is not a port number from 1 to 65535", s)
	}
	return port, nil
}

// String formats the mapping as docker ps shows it, as in
// 0.0.0.0:8080->80/tcp.
func (m PortMapping) String() string {
	hostIP := m.HostIP
	if hostIP == "" {
		hostIP = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d->%d/%s", hostIP, m.HostPort, m.ContainerPort, m.Protocol)
}
