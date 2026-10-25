//go:build linux

package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// ipam hands out a subnet's addresses, one per container, as IP address
// management does for a Docker network. Each address in use has a file in
// dir, named after the address and holding the container's ID. Whoever
// reserved it, the container's runt run or its monitor, keeps the file
// locked with flock for as long as the container runs. The kernel drops the
// lock when that process exits, however it exits, so an address whose file
// nobody has locked is free to take, even if its owner died without removing
// the file. Taking an address nobody has used creates its file, which only
// one process can do, so two containers starting at once never get the same
// address. CNI's host-local plugin keeps a file per address in the same way.
type ipam struct {
	dir    string
	subnet *net.IPNet
}

// reservation is an address held for a container.
type reservation struct {
	ip   net.IP
	path string
	file *os.File
}

// reserve holds the lowest free address in the subnet for container id. The
// first two addresses are the subnet's own and the gateway's, and the last
// is the broadcast address.
func (p *ipam) reserve(id string) (*reservation, error) {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", p.dir, err)
	}
	ones, bits := p.subnet.Mask.Size()
	size := uint64(1) << (bits - ones)
	for n := uint64(2); n+1 < size; n++ {
		r, err := p.tryReserve(subnetAddress(p.subnet, uint32(n)), id)
		if err != nil {
			return nil, fmt.Errorf("reserve an address: %w", err)
		}
		if r != nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no free addresses left in %s", p.subnet)
}

// tryReserve holds ip for container id, or returns nil if it is taken.
func (p *ipam) tryReserve(ip net.IP, id string) (*reservation, error) {
	path := filepath.Join(p.dir, ip.String())
	for {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			file, err = createLocked(path, id)
			if errors.Is(err, os.ErrExist) {
				// Someone took it just now; see whether they still hold it.
				continue
			}
			if err != nil {
				return nil, err
			}
			return &reservation{ip: ip, path: path, file: file}, nil
		}
		if err != nil {
			return nil, err
		}
		if err := flock(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			file.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, nil
			}
			return nil, err
		}
		// Nobody holds the address, but its file may have been released,
		// and so removed, between opening and locking it, and perhaps
		// reserved again since, as a new file.
		if !samePath(file, path) {
			file.Close()
			continue
		}
		// Its owner died without releasing it. Take it over.
		if err := file.Truncate(0); err != nil {
			file.Close()
			return nil, err
		}
		if _, err := file.WriteAt([]byte(id), 0); err != nil {
			file.Close()
			return nil, err
		}
		return &reservation{ip: ip, path: path, file: file}, nil
	}
}

// createLocked creates the file at path for a new reservation, holding id.
// The file is written and locked under a temporary name and then linked to
// path, which fails if path exists, so nobody ever sees it unlocked.
func createLocked(path, id string) (*os.File, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".new-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if _, err = file.WriteString(id); err == nil {
		if err = flock(file, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			err = os.Link(file.Name(), path)
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// samePath reports whether file is still the file at path.
func samePath(file *os.File, path string) bool {
	opened, err := file.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	return err == nil && os.SameFile(opened, named)
}

// release gives the address back. The file is removed before it is
// unlocked, so a process that opened it just before can tell, once it has
// the lock, that the file no longer stands for the address.
func (r *reservation) release() error {
	err := os.Remove(r.path)
	r.file.Close()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release address %s: %w", r.ip, err)
	}
	return nil
}

// inUse reports whether any container holds an address in the subnet: any
// reservation file that somebody has locked.
func (p *ipam) inUse() (bool, error) {
	entries, err := os.ReadDir(p.dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if net.ParseIP(entry.Name()) == nil {
			continue
		}
		file, err := os.Open(filepath.Join(p.dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		err = flock(file, syscall.LOCK_SH|syscall.LOCK_NB)
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

// subnetAddress returns the nth address in an IPv4 subnet, counting the
// subnet's own as 0.
func subnetAddress(subnet *net.IPNet, n uint32) net.IP {
	return binary.BigEndian.AppendUint32(nil, binary.BigEndian.Uint32(subnet.IP.To4())+n)
}

// macAddress makes a MAC address from an IPv4 address, as Docker does: 02:42
// (02 marks it locally administered) and then the address's 4 bytes. A
// container that gets an address another container had also gets its MAC
// address, so the ARP caches of whatever talked to the other container
// stay right.
func macAddress(ip net.IP) net.HardwareAddr {
	return append(net.HardwareAddr{0x02, 0x42}, ip.To4()...)
}
