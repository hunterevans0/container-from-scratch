//go:build linux

package runtime

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Tests for ipam_linux.go: handing out addresses, with reservation files in
// a temporary directory. For how Go tests work, see "Tests" in doc.go.

// testIPAM makes an address book for the subnet cidr, in a temporary
// directory.
func testIPAM(t *testing.T, cidr string) *ipam {
	t.Helper()
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return &ipam{dir: filepath.Join(t.TempDir(), "bridge"), subnet: subnet}
}

// mustReserve reserves an address for id, and stops the test unless it is
// want.
func mustReserve(t *testing.T, p *ipam, id, want string) *reservation {
	t.Helper()
	r, err := p.reserve(id)
	if err != nil {
		t.Fatalf("reserve for %s: %v", id, err)
	}
	if r.ip.String() != want {
		t.Fatalf("reserve for %s: got %s, want %s", id, r.ip, want)
	}
	return r
}

// TestReserveLowestFree checks that each container gets the lowest free
// address, including one given back by an earlier container.
func TestReserveLowestFree(t *testing.T) {
	p := testIPAM(t, "10.88.0.0/16")
	a := mustReserve(t, p, "aaaa", "10.88.0.2")
	b := mustReserve(t, p, "bbbb", "10.88.0.3")
	mustReserve(t, p, "cccc", "10.88.0.4")
	content, err := os.ReadFile(filepath.Join(p.dir, "10.88.0.3"))
	if err != nil || string(content) != "bbbb" {
		t.Errorf("reservation file for 10.88.0.3: got %q, %v; want the container ID", content, err)
	}
	if err := b.release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "10.88.0.3")); !os.IsNotExist(err) {
		t.Errorf("released address's file is still there: %v", err)
	}
	mustReserve(t, p, "dddd", "10.88.0.3")
	a.release()
	mustReserve(t, p, "eeee", "10.88.0.2")
}

// TestReserveTakesOverAbandoned covers a reservation whose owner died: its
// file is left behind, but nobody holds the lock on it.
func TestReserveTakesOverAbandoned(t *testing.T) {
	p := testIPAM(t, "10.88.0.0/16")
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "10.88.0.2"), []byte("dead-container"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustReserve(t, p, "live", "10.88.0.2")
	content, err := os.ReadFile(filepath.Join(p.dir, "10.88.0.2"))
	if err != nil || string(content) != "live" {
		t.Errorf("taken-over reservation holds %q, %v; want the new container's ID", content, err)
	}
}

// TestReserveFull checks that a subnet with no address left says so.
func TestReserveFull(t *testing.T) {
	// A /30 has 4 addresses: the subnet's, the gateway's, one for a
	// container, and the broadcast address.
	p := testIPAM(t, "192.0.2.0/30")
	mustReserve(t, p, "aaaa", "192.0.2.2")
	if r, err := p.reserve("bbbb"); err == nil {
		t.Errorf("reserved %s in a full subnet", r.ip)
	}
}

// TestInUse covers runt network rm's check: a network is in use while any
// address on it is held, but not because of a dead container's file.
func TestInUse(t *testing.T) {
	p := testIPAM(t, "10.89.0.0/24")
	if inUse, err := p.inUse(); err != nil || inUse {
		t.Errorf("before any reservation: got %v, %v; want not in use", inUse, err)
	}
	r := mustReserve(t, p, "aaaa", "10.89.0.2")
	if inUse, err := p.inUse(); err != nil || !inUse {
		t.Errorf("with an address held: got %v, %v; want in use", inUse, err)
	}
	// Closing the file without removing it, as a killed process leaves it.
	r.file.Close()
	if inUse, err := p.inUse(); err != nil || inUse {
		t.Errorf("with only an abandoned reservation: got %v, %v; want not in use", inUse, err)
	}
}

// TestMACAddress checks the MAC address made from an IP address: 02:42 and
// then the address's four bytes in hex.
func TestMACAddress(t *testing.T) {
	if got, want := macAddress(net.IP{10, 88, 1, 2}).String(), "02:42:0a:58:01:02"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
