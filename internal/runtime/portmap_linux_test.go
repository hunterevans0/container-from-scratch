//go:build linux

package runtime

import (
	"slices"
	"testing"
)

// Tests for portmap_linux.go that need no root: reading published ports'
// rules back from iptables -S. For how Go tests work, see "Tests" in
// doc.go.

// TestParseDNATRule checks reading a rule's host address, protocol, port,
// and container address, and its words, which removing it takes.
func TestParseDNATRule(t *testing.T) {
	line := "-A RUNT -d 127.0.0.1/32 ! -i runt+ -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.88.0.2:80"
	rule, ok := parseDNATRule(line)
	if !ok {
		t.Fatalf("parseDNATRule(%q) didn't parse", line)
	}
	if rule.hostIP != "127.0.0.1" || rule.protocol != "tcp" || rule.port != "8080" || rule.toIP != "10.88.0.2" {
		t.Errorf("got %+v; want host address 127.0.0.1, tcp port 8080, to 10.88.0.2", rule)
	}
	if want := []string{"-d", "127.0.0.1/32", "!", "-i", "runt+", "-p", "tcp", "-m", "tcp", "--dport", "8080", "-j", "DNAT", "--to-destination", "10.88.0.2:80"}; !slices.Equal(rule.spec, want) {
		t.Errorf("spec: got %q; want %q", rule.spec, want)
	}

	rule, ok = parseDNATRule("-A RUNT ! -i runt+ -p udp -m udp --dport 5353 -j DNAT --to-destination 10.89.0.3:53")
	if !ok || rule.hostIP != "" || rule.protocol != "udp" || rule.port != "5353" || rule.toIP != "10.89.0.3" {
		t.Errorf("rule for all host addresses: got %+v, %v", rule, ok)
	}
	// The chain itself, as iptables -S lists it first.
	if rule, ok := parseDNATRule("-N RUNT"); ok {
		t.Errorf("parseDNATRule(-N RUNT) = %+v; want no rule", rule)
	}
}
