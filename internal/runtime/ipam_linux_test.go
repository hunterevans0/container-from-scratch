//go:build linux

package runtime

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func testIPAM(t *testing.T, cidr string) *ipam {
	t.Helper()
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return &ipam{dir: filepath.Join(t.TempDir(), "bridge"), subnet: subnet}
}

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

func TestReserveFull(t *testing.T) {
	// A /30 has 4 addresses: the subnet's, the gateway's, one for a
	// container, and the broadcast address.
	p := testIPAM(t, "192.0.2.0/30")
	mustReserve(t, p, "aaaa", "192.0.2.2")
	if r, err := p.reserve("bbbb"); err == nil {
		t.Errorf("reserved %s in a full subnet", r.ip)
	}
}

func TestMACAddress(t *testing.T) {
	if got, want := macAddress(net.IP{10, 88, 1, 2}).String(), "02:42:0a:58:01:02"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
