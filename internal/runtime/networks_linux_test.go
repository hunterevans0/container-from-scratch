//go:build linux

package runtime

import (
	"net"
	"testing"
)

func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return subnet
}

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
