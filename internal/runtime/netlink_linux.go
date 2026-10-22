//go:build linux

package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// runt changes the kernel's network configuration with rtnetlink, as ip(8)
// does: it sends a request over a netlink socket and reads back an
// acknowledgement or an error number (see rtnetlink(7)). Go's standard
// library can list interfaces but not change them, and runt needs only a
// handful of requests, so it builds them by hand rather than depending on a
// netlink library, as it does its seccomp filter.
//
// A request is a 16-byte header, a fixed-size struct saying what the request
// is about (an interface, an address, a route), and attributes: each a
// length, a type, and a value, padded to a multiple of 4 bytes. An
// attribute's value can be more attributes, nested.

// Attribute types the syscall package doesn't define, from linux/if_link.h
// and linux/veth.h.
const (
	iflaInfoKind = 1
	iflaInfoData = 2
	vethInfoPeer = 1
)

// netlinkConn is a NETLINK_ROUTE socket. Its requests apply to the network
// namespace it was opened in.
type netlinkConn struct {
	fd  int
	seq uint32
}

func openNetlink() (*netlinkConn, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open netlink socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind netlink socket: %w", err)
	}
	return &netlinkConn{fd: fd}, nil
}

func (c *netlinkConn) Close() error {
	return syscall.Close(c.fd)
}

// netlinkRequest is a request being built.
type netlinkRequest struct {
	data []byte
}

// newRequest starts a request of type kind about body, which is an
// ifinfomsg, ifaddrmsg, or rtmsg. Every request asks for an
// acknowledgement, so that errors come back.
func newRequest(kind, flags uint16, body []byte) *netlinkRequest {
	r := &netlinkRequest{data: make([]byte, syscall.NLMSG_HDRLEN, 256)}
	binary.NativeEndian.PutUint16(r.data[4:], kind)
	binary.NativeEndian.PutUint16(r.data[6:], flags|syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	r.data = append(r.data, body...)
	return r
}

// attr adds an attribute.
func (r *netlinkRequest) attr(kind uint16, value []byte) {
	r.data = binary.NativeEndian.AppendUint16(r.data, uint16(syscall.SizeofRtAttr+len(value)))
	r.data = binary.NativeEndian.AppendUint16(r.data, kind)
	r.data = append(r.data, value...)
	for len(r.data)%4 != 0 {
		r.data = append(r.data, 0)
	}
}

// nested adds an attribute whose value is whatever add adds.
func (r *netlinkRequest) nested(kind uint16, add func()) {
	start := len(r.data)
	r.attr(kind, nil)
	add()
	binary.NativeEndian.PutUint16(r.data[start:], uint16(len(r.data)-start))
}

func cString(s string) []byte {
	return append([]byte(s), 0)
}

func uint32Value(n int) []byte {
	return binary.NativeEndian.AppendUint32(nil, uint32(n))
}

// ifInfo is a struct ifinfomsg: which interface (0 for one being created,
// or one named by IFLA_IFNAME), and the flags to change (change) and what
// to set them to (flags).
func ifInfo(index int, flags, change uint32) []byte {
	b := make([]byte, syscall.SizeofIfInfomsg)
	b[0] = syscall.AF_UNSPEC
	binary.NativeEndian.PutUint32(b[4:], uint32(index))
	binary.NativeEndian.PutUint32(b[8:], flags)
	binary.NativeEndian.PutUint32(b[12:], change)
	return b
}

// do sends a request and waits for the kernel's answer.
func (c *netlinkConn) do(r *netlinkRequest) error {
	c.seq++
	binary.NativeEndian.PutUint32(r.data[0:], uint32(len(r.data)))
	binary.NativeEndian.PutUint32(r.data[8:], c.seq)
	if err := syscall.Sendto(c.fd, r.data, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("send netlink request: %w", err)
	}
	buf := make([]byte, 16384)
	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read netlink reply: %w", err)
		}
		messages, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return fmt.Errorf("parse netlink reply: %w", err)
		}
		for _, message := range messages {
			if message.Header.Seq != c.seq || message.Header.Type != syscall.NLMSG_ERROR {
				continue
			}
			// A struct nlmsgerr: 0 for success, or an error number,
			// negated, followed by the request.
			if len(message.Data) < 4 {
				return errors.New("short netlink reply")
			}
			if errno := -int32(binary.NativeEndian.Uint32(message.Data)); errno != 0 {
				return syscall.Errno(errno)
			}
			return nil
		}
	}
}

// createBridge makes a bridge, a virtual switch, with a fixed MAC address.
// Otherwise a bridge takes the lowest MAC address of the interfaces plugged
// into it, which changes as containers come and go, and the containers'
// ARP caches would point at an address the gateway no longer has.
func (c *netlinkConn) createBridge(name string, mac net.HardwareAddr) error {
	r := newRequest(syscall.RTM_NEWLINK, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, ifInfo(0, 0, 0))
	r.attr(syscall.IFLA_IFNAME, cString(name))
	r.attr(syscall.IFLA_ADDRESS, mac)
	r.nested(syscall.IFLA_LINKINFO, func() {
		r.attr(iflaInfoKind, cString("bridge"))
	})
	return c.do(r)
}

// createVeth makes a veth pair, two interfaces joined like the ends of a
// cable: name, up and plugged into the bridge with index master, and peer,
// with MAC address peerMAC, in the network namespace of process peerPID.
func (c *netlinkConn) createVeth(name string, master int, peer string, peerMAC net.HardwareAddr, peerPID int) error {
	r := newRequest(syscall.RTM_NEWLINK, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, ifInfo(0, syscall.IFF_UP, syscall.IFF_UP))
	r.attr(syscall.IFLA_IFNAME, cString(name))
	r.attr(syscall.IFLA_MASTER, uint32Value(master))
	r.nested(syscall.IFLA_LINKINFO, func() {
		r.attr(iflaInfoKind, cString("veth"))
		r.nested(iflaInfoData, func() {
			// The peer is described as an interface of its own: an
			// ifinfomsg, then its attributes.
			r.nested(vethInfoPeer, func() {
				r.data = append(r.data, ifInfo(0, 0, 0)...)
				r.attr(syscall.IFLA_IFNAME, cString(peer))
				r.attr(syscall.IFLA_ADDRESS, peerMAC)
				r.attr(syscall.IFLA_NET_NS_PID, uint32Value(peerPID))
			})
		})
	})
	return c.do(r)
}

// deleteLink deletes the interface called name. Deleting either end of a
// veth pair deletes both.
func (c *netlinkConn) deleteLink(name string) error {
	r := newRequest(syscall.RTM_DELLINK, 0, ifInfo(0, 0, 0))
	r.attr(syscall.IFLA_IFNAME, cString(name))
	return c.do(r)
}

// setUp brings an interface up.
func (c *netlinkConn) setUp(index int) error {
	return c.do(newRequest(syscall.RTM_NEWLINK, 0, ifInfo(index, syscall.IFF_UP, syscall.IFF_UP)))
}

// addAddress gives an interface an IPv4 address. The kernel adds a route to
// the address's subnet through the interface.
func (c *netlinkConn) addAddress(index int, address *net.IPNet) error {
	prefix, _ := address.Mask.Size()
	// struct ifaddrmsg: family, prefix length, flags, scope, and interface.
	body := make([]byte, syscall.SizeofIfAddrmsg)
	body[0] = syscall.AF_INET
	body[1] = byte(prefix)
	binary.NativeEndian.PutUint32(body[4:], uint32(index))
	r := newRequest(syscall.RTM_NEWADDR, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, body)
	r.attr(syscall.IFA_LOCAL, address.IP.To4())
	r.attr(syscall.IFA_ADDRESS, address.IP.To4())
	return c.do(r)
}

// addDefaultRoute sends everything that has no more specific route to
// gateway.
func (c *netlinkConn) addDefaultRoute(gateway net.IP) error {
	// struct rtmsg: family, destination and source prefix lengths (0, so
	// any destination), TOS, table, protocol, scope, type, and flags.
	body := make([]byte, syscall.SizeofRtMsg)
	body[0] = syscall.AF_INET
	body[4] = syscall.RT_TABLE_MAIN
	body[5] = syscall.RTPROT_BOOT
	body[6] = syscall.RT_SCOPE_UNIVERSE
	body[7] = syscall.RTN_UNICAST
	r := newRequest(syscall.RTM_NEWROUTE, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, body)
	r.attr(syscall.RTA_GATEWAY, gateway.To4())
	return c.do(r)
}
