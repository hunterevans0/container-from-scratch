//go:build linux

package runtime

import (
	"net"
	"testing"
)

// Tests for networks_linux.go that need no root: choosing and checking
// subnets. For how Go tests work, see "Tests" in doc.go.

// mustCIDR parses a subnet, such as 10.89.0.0/24, and stops the test if it
// can't.
func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return subnet
}

// TestFreeSubnet checks that a new network gets the first /24 from
// 10.89.0.0 on that nothing else has.
func TestFreeSubnet(t *testing.T) {
	taken := []*net.IPNet{defaultSubnet, mustCIDR(t, "10.89.0.0/24"), mustCIDR(t, "10.89.1.0/25")}
	subnet, err := freeSubnet(taken)
	if err != nil {
		t.Fatal(err)
	}
	if subnet.String() != "10.89.2.0/24" {
		t.Errorf("got %s; want 10.89.2.0/24, the first not taken", subnet)
	}
	// A host network that covers them all.
	if subnet, err := freeSubnet([]*net.IPNet{mustCIDR(t, "10.0.0.0/8")}); err == nil {
		t.Errorf("got %s; want an error, with 10.89.0.0/16 all taken", subnet)
	}
}

// TestParseSubnet checks --subnet's checks: IPv4, the network's own
// address, and room for containers.
func TestParseSubnet(t *testing.T) {
	if subnet, err := parseSubnet("10.90.0.0/24"); err != nil || subnet.String() != "10.90.0.0/24" {
		t.Errorf("parseSubnet(10.90.0.0/24) = %v, %v", subnet, err)
	}
	for _, s := range []string{"10.90.0.1/24", "10.90.0.0/31", "fd00::/64", "10.90.0.0"} {
		if subnet, err := parseSubnet(s); err == nil {
			t.Errorf("parseSubnet(%q) = %s; want an error", s, subnet)
		}
	}
}

// TestOverlapping checks telling whether subnets share addresses: one
// inside the other, either way round, or apart.
func TestOverlapping(t *testing.T) {
	taken := []*net.IPNet{defaultSubnet}
	if other := overlapping(mustCIDR(t, "10.88.5.0/24"), taken); other != defaultSubnet {
		t.Errorf("10.88.5.0/24 inside 10.88.0.0/16: got %v", other)
	}
	if other := overlapping(mustCIDR(t, "10.0.0.0/8"), taken); other != defaultSubnet {
		t.Errorf("10.0.0.0/8 around 10.88.0.0/16: got %v", other)
	}
	if other := overlapping(mustCIDR(t, "10.89.0.0/24"), taken); other != nil {
		t.Errorf("10.89.0.0/24 apart from 10.88.0.0/16: got %v", other)
	}
}
