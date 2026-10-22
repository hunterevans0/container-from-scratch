//go:build integration && linux

package integration

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	bridgeName    = "runt0"
	bridgeGateway = "10.88.0.1"
	// greetingPort is where the test's servers listen. Each tells a client
	// the address it sees the connection come from.
	greetingPort = 7000
	// The stand-in for the outside world (see startOutside), on a
	// documentation range nobody routes.
	outsideNamespace = "runt-feature-outside"
	outsideHostEnd   = "runt-outside"
	outsideHostIP    = "198.51.100.1"
	outsideServerIP  = "198.51.100.2"
)

// greetingServer is a shell command that serves greetings, with Perl:
// perl-base is in every Debian and Ubuntu system, and so in the rootfs.
var greetingServer = fmt.Sprintf(`exec perl -MIO::Socket::INET -e '
$s = IO::Socket::INET->new(LocalPort => %d, Listen => 5, ReuseAddr => 1) or die "listen: $!";
while ($c = $s->accept) { print $c "you are " . $c->peerhost . "\n"; close $c }'`, greetingPort)

// greetingFrom has container connect to a greeting server at ip with bash's
// /dev/tcp, trying for a few seconds while the server starts, and returns
// the greeting.
func (r *runtTester) greetingFrom(container, ip string) (string, error) {
	script := fmt.Sprintf(`for i in $(seq 50); do exec 3<>/dev/tcp/%s/%d && break; sleep 0.1; done 2>/dev/null; cat <&3`, ip, greetingPort)
	output, err := r.runt("exec", container, "/bin/bash", "-c", script)
	return strings.TrimSpace(output), err
}

// greetingFromHost connects to a greeting server at ip from the host.
func greetingFromHost(ip string) (string, error) {
	address := net.JoinHostPort(ip, fmt.Sprint(greetingPort))
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var conn net.Conn
		if conn, err = net.DialTimeout("tcp", address, time.Second); err != nil {
			continue
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		return strings.TrimSpace(line), err
	}
	return "", err
}

// ipCommand runs ip(8) on the host.
func ipCommand(t *testing.T, args ...string) {
	t.Helper()
	if output, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

// startOutside stands in for the internet: a network namespace of its own,
// which the host reaches through a veth pair, with a greeting server at
// outsideServerIP. The host's end has outsideHostIP, so a connection from
// a container that the host masquerades comes from there.
func startOutside(t *testing.T) {
	t.Helper()
	// Whatever an earlier run left behind.
	exec.Command("ip", "netns", "del", outsideNamespace).Run()
	exec.Command("ip", "link", "del", outsideHostEnd).Run()
	ipCommand(t, "netns", "add", outsideNamespace)
	// Deleting the namespace deletes its end of the veth pair, and so both.
	t.Cleanup(func() { exec.Command("ip", "netns", "del", outsideNamespace).Run() })
	ipCommand(t, "link", "add", outsideHostEnd, "type", "veth", "peer", "name", "eth0", "netns", outsideNamespace)
	ipCommand(t, "addr", "add", outsideHostIP+"/24", "dev", outsideHostEnd)
	ipCommand(t, "link", "set", outsideHostEnd, "up")
	ipCommand(t, "-n", outsideNamespace, "addr", "add", outsideServerIP+"/24", "dev", "eth0")
	ipCommand(t, "-n", outsideNamespace, "link", "set", "eth0", "up")

	server := exec.Command("ip", "netns", "exec", outsideNamespace, "/bin/sh", "-c", greetingServer)
	if err := server.Start(); err != nil {
		t.Fatalf("start the outside server: %v", err)
	}
	t.Cleanup(func() {
		server.Process.Kill()
		server.Wait()
	})
}

// hostVeth is the host's end of container id's veth pair.
func hostVeth(id string) string {
	return "veth" + id[:8]
}

// reservationFile is where runt records that a container has address ip.
func reservationFile(ip string) string {
	return filepath.Join("/run/runt-net/bridge", ip)
}

// TestFeatureNetwork checks the bridge network: a bridge on the host with a
// veth pair per container, an address for each container, and NAT to the
// outside world, here a network namespace standing in for it.
func TestFeatureNetwork(t *testing.T) {
	explain(t, "Networking: a bridge with a veth pair per container, an address for each, and NAT to the outside")
	r := newRuntTester(t)
	idA := r.start("runt-feature-net-a", "/bin/sleep", "600")
	idB := r.start("runt-feature-net-b", "/bin/sh", "-c", greetingServer)
	a, b := readState(t, idA), readState(t, idB)
	_, subnet, _ := net.ParseCIDR("10.88.0.0/16")

	t.Run("Bridge", func(t *testing.T) {
		explain(t, "the host has a bridge, %s, at %s/16, with each container's veth plugged into it", bridgeName, bridgeGateway)
		bridge, err := net.InterfaceByName(bridgeName)
		if err != nil {
			t.Fatalf("no %s on the host: %v", bridgeName, err)
		}
		if _, err := os.Stat(filepath.Join("/sys/class/net", bridgeName, "bridge")); err != nil {
			t.Errorf("%s isn't a bridge: %v", bridgeName, err)
		}
		addresses, err := bridge.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, address := range addresses {
			found = found || address.String() == bridgeGateway+"/16"
		}
		if !found {
			t.Errorf("%s has addresses %v, want %s/16", bridgeName, addresses, bridgeGateway)
		}
		for _, id := range []string{idA, idB} {
			master, err := os.Readlink(filepath.Join("/sys/class/net", hostVeth(id), "master"))
			if err != nil {
				t.Errorf("container %s's veth %s isn't plugged into anything: %v", id, hostVeth(id), err)
			} else if filepath.Base(master) != bridgeName {
				t.Errorf("container %s's veth %s is plugged into %s, want %s", id, hostVeth(id), filepath.Base(master), bridgeName)
			}
		}
		observe("%s %s/16, with %s and %s plugged in", bridgeName, bridgeGateway, hostVeth(idA), hostVeth(idB))
	})

	t.Run("Addresses", func(t *testing.T) {
		explain(t, "each container gets an address of its own in %s, held in a reservation file", subnet)
		for _, s := range []struct {
			id string
			state
		}{{idA, a}, {idB, b}} {
			ip := net.ParseIP(s.IPAddress)
			if ip == nil || !subnet.Contains(ip) || s.IPAddress == bridgeGateway || ip.Equal(subnet.IP) {
				t.Errorf("container %s has address %q, want one of its own in %s", s.id, s.IPAddress, subnet)
				continue
			}
			if owner, err := os.ReadFile(reservationFile(s.IPAddress)); err != nil || string(owner) != s.id {
				t.Errorf("%s holds %q (%v), want container %s's ID", reservationFile(s.IPAddress), owner, err, s.id)
			}
		}
		if a.IPAddress == b.IPAddress {
			t.Errorf("both containers have %s", a.IPAddress)
		}
		observe("A is %s and B is %s", a.IPAddress, b.IPAddress)
	})

	t.Run("ContainerInterface", func(t *testing.T) {
		explain(t, "inside, eth0 has the container's address, and the default route goes through the bridge")
		output, err := r.exec("runt-feature-net-a", "hostname -I; cat /proc/net/route")
		mustSucceed(t, "reading A's addresses and routes", output, err, a.IPAddress)
		// /proc/net/route shows addresses in hex, in the kernel's (here
		// little-endian) byte order: 10.88.0.1 is 0100580A.
		defaultRoute := false
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			defaultRoute = defaultRoute || len(fields) > 2 && fields[0] == "eth0" && fields[1] == "00000000" && fields[2] == "0100580A"
		}
		if !defaultRoute {
			t.Errorf("no default route through eth0 to %s:\n%s", bridgeGateway, output)
		}
	})

	t.Run("ContainerToContainer", func(t *testing.T) {
		explain(t, "containers reach each other directly, through the bridge, without NAT")
		greeting, err := r.greetingFrom("runt-feature-net-a", b.IPAddress)
		mustSucceed(t, "A connecting to B", greeting, err, "you are "+a.IPAddress)
		observe("A -> B (%s): %q", b.IPAddress, greeting)
	})

	t.Run("HostToContainer", func(t *testing.T) {
		explain(t, "the host reaches a container at its address, from the bridge's")
		greeting, err := greetingFromHost(b.IPAddress)
		mustSucceed(t, "the host connecting to B", greeting, err, "you are "+bridgeGateway)
	})

	t.Run("NAT", func(t *testing.T) {
		explain(t, "beyond the host, a container's connections come from the host's address (masquerading)")
		startOutside(t)
		greeting, err := r.greetingFrom("runt-feature-net-a", outsideServerIP)
		mustSucceed(t, "A connecting to the outside server", greeting, err, "you are "+outsideHostIP)
		observe("A (%s) -> outside (%s): %q", a.IPAddress, outsideServerIP, greeting)
		rule := []string{"-t", "nat", "-C", "POSTROUTING", "-s", subnet.String(), "!", "-o", bridgeName, "-j", "MASQUERADE"}
		if output, err := exec.Command("iptables", rule...).CombinedOutput(); err != nil {
			t.Errorf("iptables %s: %v\n%s", strings.Join(rule, " "), err, output)
		}
	})

	t.Run("ReleasedOnStop", func(t *testing.T) {
		explain(t, "a container's veth pair and address go when it stops")
		output, err := r.runt("stop", "-t", "1", "runt-feature-net-a")
		mustSucceed(t, "runt stop", output, err)
		if _, err := os.Stat(filepath.Join("/sys/class/net", hostVeth(idA))); err == nil {
			t.Errorf("veth %s is still there", hostVeth(idA))
		}
		if _, err := os.Stat(reservationFile(a.IPAddress)); err == nil {
			t.Errorf("%s is still reserved", a.IPAddress)
		}
		if address := readState(t, idA).IPAddress; address != "" {
			t.Errorf("the stopped container's state still has address %s", address)
		}
	})

	t.Run("NoNetwork", func(t *testing.T) {
		explain(t, "--network none leaves a container only its loopback interface")
		output, err := r.runOnce("runt-feature-net-none", "--rootfs", r.rootfs, "--network", "none", "--", "/bin/ls", "/sys/class/net")
		mustSucceed(t, "runt run --network none", output, err)
		if output != "lo\n" {
			t.Errorf("interfaces: got %q, want only lo", output)
		}
	})
}
