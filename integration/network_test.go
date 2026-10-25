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

// Network tests, as feature checks (see the top of features_test.go): the
// bridge network, published ports, DNS files, and user-defined networks.
//
// Most checks have something connect to a "greeting server", which answers
// each connection with the address it came from. That shows both that the
// connection got through and what NAT, if any, did to it on the way. For
// how to read these tests, see the top of isolation_test.go.

const (
	bridgeName    = "runt0"
	bridgeGateway = "10.88.0.1"
	// greetingPort is where the test's servers listen. Each tells a client
	// the address it sees the connection come from.
	greetingPort = 7000
	// udpGreetingPort is where the test's UDP servers listen.
	udpGreetingPort = 7001
	// The stand-in for the outside world (see startOutside), on a
	// documentation range nobody routes. The host's end of its veth pair
	// mustn't start with runt, as runt's bridges do: the rule that keeps
	// networks apart would take it for one.
	outsideNamespace = "runt-feature-outside"
	outsideHostEnd   = "outside-veth"
	outsideHostIP    = "198.51.100.1"
	outsideServerIP  = "198.51.100.2"
)

// greetingServer is a shell command that serves greetings, with Perl:
// perl-base is in every Debian and Ubuntu system, and so in the rootfs.
// fmt.Sprintf fills the port number into the script where %d is.
var greetingServer = fmt.Sprintf(`exec perl -MIO::Socket::INET -e '
$s = IO::Socket::INET->new(LocalPort => %d, Listen => 5, ReuseAddr => 1) or die "listen: $!";
while ($c = $s->accept) { print $c "you are " . $c->peerhost . "\n"; close $c }'`, greetingPort)

// udpGreetingServer is greetingServer over UDP: it answers each datagram.
var udpGreetingServer = fmt.Sprintf(`perl -MIO::Socket::INET -e '
$s = IO::Socket::INET->new(LocalPort => %d, Proto => "udp") or die "bind: $!";
while ($s->recv($m, 512)) { $s->send("you are " . $s->peerhost . "\n") }'`, udpGreetingPort)

// tcpGreetingScript is a bash script that connects to a greeting server at
// host and port with bash's /dev/tcp, trying for a few seconds while the
// server starts, and prints the greeting.
func tcpGreetingScript(host string, port int) string {
	return fmt.Sprintf(`for i in $(seq 50); do exec 3<>/dev/tcp/%s/%d && break; sleep 0.1; done 2>/dev/null; cat <&3`, host, port)
}

// udpGreetingScript is a bash script that sends datagrams to a UDP greeting
// server at host and port with bash's /dev/udp until one is answered, and
// prints the answer.
func udpGreetingScript(host string, port int) string {
	return fmt.Sprintf(`exec 3<>/dev/udp/%s/%d; for i in $(seq 20); do echo hi >&3; if read -t 0.3 line <&3; then echo "$line"; exit 0; fi; done; exit 1`, host, port)
}

// greetingFrom has container connect to a greeting server at host and
// returns the greeting.
func (r *runtTester) greetingFrom(container, host string) (string, error) {
	return r.greetingFromPort(container, host, greetingPort)
}

func (r *runtTester) greetingFromPort(container, host string, port int) (string, error) {
	output, err := r.runt("exec", container, "/bin/bash", "-c", tcpGreetingScript(host, port))
	return strings.TrimSpace(output), err
}

// greetingFromOutside runs a greeting script in the outside world's
// namespace (see startOutside) and returns the greeting.
func greetingFromOutside(script string) (string, error) {
	output, err := exec.Command("ip", "netns", "exec", outsideNamespace, "/bin/bash", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// greetingFromHost connects to a greeting server at ip from the host.
func greetingFromHost(ip string) (string, error) {
	return greetingAt(net.JoinHostPort(ip, fmt.Sprint(greetingPort)))
}

// greetingAt connects to a greeting server at address from the host,
// trying for up to 5 seconds while the server starts, and returns the line
// it sends. Inside the loop, err := declares a new err for the read,
// shadowing the outer one, which only the last failed attempt sets.
func greetingAt(address string) (string, error) {
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

// udpGreetingAt sends datagrams to a UDP greeting server at address from the
// host until one is answered, and returns the answer. UDP doesn't say
// whether a datagram arrived, so it sends one, waits 300ms for an answer,
// and tries again, up to 20 times.
func udpGreetingAt(address string) (string, error) {
	conn, err := net.Dial("udp4", address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	buffer := make([]byte, 512)
	for i := 0; i < 20; i++ {
		if _, err = conn.Write([]byte("hi\n")); err != nil {
			continue
		}
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var n int
		if n, err = conn.Read(buffer); err == nil {
			return strings.TrimSpace(string(buffer[:n])), nil
		}
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
//
// It uses the ip command: ip netns makes a named network namespace, ip link
// add ... type veth makes a veth pair with one end moved into it, and
// ip -n NAMESPACE runs a command on that namespace's interfaces.
func startOutside(t *testing.T) {
	t.Helper()
	// Whatever an earlier run left behind. Run's error is ignored: there
	// usually is nothing to delete.
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
		// /sys/class/net/<interface>/master is a symlink to the bridge an
		// interface is plugged into.
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
		// Each case pairs a container's ID with its state. state, with no
		// field name, is embedded, so s.IPAddress reaches the state's field.
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

// TestFeaturePorts checks port publishing: a container's port reached at a
// port of the host's, from elsewhere through a DNAT rule, and from the host
// itself and other containers through runt's proxy.
func TestFeaturePorts(t *testing.T) {
	explain(t, "Port publishing: -p HOSTPORT:CONTAINERPORT, by DNAT from elsewhere and a proxy from the host and containers")
	r := newRuntTester(t)
	startOutside(t)
	options := []string{"--rootfs", r.rootfs, "-p", "17000:7000", "-p", "17001:7001/udp", "-p", "7000"}
	id := r.startWith("runt-feature-ports", options, "/bin/sh", "-c", udpGreetingServer+" & "+greetingServer)
	r.start("runt-feature-ports-client", "/bin/sleep", "600")
	s := readState(t, id)

	t.Run("FromElsewhere", func(t *testing.T) {
		explain(t, "from another machine, a DNAT rule passes connections on, and the container sees who they come from")
		greeting, err := greetingFromOutside(tcpGreetingScript(outsideHostIP, 17000))
		mustSucceed(t, "the outside connecting to the host's port 17000", greeting, err, "you are "+outsideServerIP)
		greeting, err = greetingFromOutside(udpGreetingScript(outsideHostIP, 17001))
		mustSucceed(t, "the outside sending to the host's UDP port 17001", greeting, err, "you are "+outsideServerIP)
		observe("outside (%s) -> %s:17000 -> %s:7000: %q", outsideServerIP, outsideHostIP, s.IPAddress, greeting)
	})

	t.Run("FromHost", func(t *testing.T) {
		explain(t, "from the host, at 127.0.0.1, the proxy passes connections on, from the bridge's address")
		greeting, err := greetingAt("127.0.0.1:17000")
		mustSucceed(t, "the host connecting to 127.0.0.1:17000", greeting, err, "you are "+bridgeGateway)
		greeting, err = udpGreetingAt("127.0.0.1:17001")
		mustSucceed(t, "the host sending to 127.0.0.1:17001/udp", greeting, err, "you are "+bridgeGateway)
	})

	t.Run("FromContainer", func(t *testing.T) {
		explain(t, "from another container, at the host's address, the proxy passes connections on too")
		greeting, err := r.greetingFromPort("runt-feature-ports-client", bridgeGateway, 17000)
		mustSucceed(t, "a container connecting to the host's port 17000", greeting, err, "you are "+bridgeGateway)
	})

	t.Run("AnyFreePort", func(t *testing.T) {
		explain(t, "-p 7000 publishes the port at a free port the system picks, recorded in state.json")
		hostPort := 0
		for _, port := range s.Ports {
			if port.ContainerPort == 7000 && port.HostPort != 17000 {
				hostPort = port.HostPort
			}
		}
		if hostPort == 0 {
			t.Fatalf("state.json has no host port for -p 7000: %+v", s.Ports)
		}
		greeting, err := greetingAt(fmt.Sprintf("127.0.0.1:%d", hostPort))
		mustSucceed(t, fmt.Sprintf("the host connecting to the picked port %d", hostPort), greeting, err, "you are "+bridgeGateway)
		observe("-p 7000 got host port %d", hostPort)
	})

	t.Run("Ps", func(t *testing.T) {
		explain(t, "runt ps shows the published ports, as docker ps does")
		output, err := r.runt("ps")
		mustSucceed(t, "runt ps", output, err, "0.0.0.0:17000->7000/tcp", "0.0.0.0:17001->7001/udp")
	})

	t.Run("Taken", func(t *testing.T) {
		explain(t, "a port another container has published can't be published again")
		output, err := r.runOnce("runt-feature-ports-taken", "--rootfs", r.rootfs, "-p", "17000:7000", "--", "/bin/true")
		mustFail(t, "publishing port 17000 twice", output, err, "address already in use")
	})

	t.Run("NoNetwork", func(t *testing.T) {
		explain(t, "--network none has no ports to publish")
		output, err := r.runOnce("runt-feature-ports-none", "--rootfs", r.rootfs, "--network", "none", "-p", "7000", "--", "/bin/true")
		mustFail(t, "publishing a port with --network none", output, err, "--network none")
	})

	t.Run("ReleasedOnStop", func(t *testing.T) {
		explain(t, "a container's DNAT rules go, and its ports are free again, when it stops")
		output, err := r.runt("stop", "-t", "1", "runt-feature-ports")
		mustSucceed(t, "runt stop", output, err)
		rules, err := exec.Command("iptables", "-t", "nat", "-S", "RUNT").CombinedOutput()
		if err != nil {
			t.Fatalf("iptables -t nat -S RUNT: %v\n%s", err, rules)
		}
		if strings.Contains(string(rules), s.IPAddress+":") {
			t.Errorf("rules for the stopped container are still there:\n%s", rules)
		}
		listener, err := net.Listen("tcp4", ":17000")
		if err != nil {
			t.Errorf("port 17000 is still taken: %v", err)
		} else {
			listener.Close()
		}
	})
}

// hostNameservers are the DNS servers a container on the default network
// should get from the host: those not on its loopback interface.
func hostNameservers(t *testing.T) []string {
	t.Helper()
	all := resolvConfServers(t, "/etc/resolv.conf")
	// systemd-resolved's stub server stands for the servers it asks.
	if len(all) == 1 && all[0] == "127.0.0.53" {
		if _, err := os.Stat("/run/systemd/resolve/resolv.conf"); err == nil {
			all = resolvConfServers(t, "/run/systemd/resolve/resolv.conf")
		}
	}
	var nameservers []string
	for _, nameserver := range all {
		if ip := net.ParseIP(nameserver); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			nameservers = append(nameservers, nameserver)
		}
	}
	return nameservers
}

// resolvConfServers returns the servers a resolv.conf names.
func resolvConfServers(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var nameservers []string
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "nameserver" {
			nameservers = append(nameservers, fields[1])
		}
	}
	return nameservers
}

// TestFeatureDNS checks the /etc/resolv.conf and /etc/hosts runt gives every
// container.
func TestFeatureDNS(t *testing.T) {
	explain(t, "DNS: an /etc/resolv.conf and /etc/hosts of runt's in every container")
	r := newRuntTester(t)
	id := r.start("runt-feature-dns", "/bin/sleep", "600")
	s := readState(t, id)

	t.Run("ResolvConf", func(t *testing.T) {
		explain(t, "/etc/resolv.conf has the host's DNS servers, but none on the host's loopback interface")
		output, err := r.exec("runt-feature-dns", "cat /etc/resolv.conf")
		mustSucceed(t, "reading /etc/resolv.conf", output, err)
		nameservers := hostNameservers(t)
		if len(nameservers) == 0 {
			nameservers = []string{"8.8.8.8", "8.8.4.4"}
		}
		for _, nameserver := range nameservers {
			if !strings.Contains(output, "nameserver "+nameserver+"\n") {
				t.Errorf("/etc/resolv.conf is missing the host's server %s:\n%s", nameserver, output)
			}
		}
		if strings.Contains(output, "nameserver 127.") {
			t.Errorf("/etc/resolv.conf has a loopback server, which the container can't reach:\n%s", output)
		}
		observe("servers: %s", strings.Join(nameservers, ", "))
	})

	t.Run("Hosts", func(t *testing.T) {
		explain(t, "/etc/hosts has localhost, and the container's hostname at its address")
		output, err := r.exec("runt-feature-dns", "cat /etc/hosts")
		mustSucceed(t, "reading /etc/hosts", output, err, "127.0.0.1\tlocalhost\n", s.IPAddress+"\trunt\n")
		output, err = r.exec("runt-feature-dns", "getent hosts runt")
		mustSucceed(t, "looking up the container's hostname", output, err, s.IPAddress)
	})

	t.Run("Mounted", func(t *testing.T) {
		explain(t, "they are mounted over the container's own files, which the container can still change")
		output, err := r.exec("runt-feature-dns", "cut -d' ' -f5 /proc/self/mountinfo; echo '192.0.2.9 added' >> /etc/hosts && getent hosts added")
		mustSucceed(t, "listing mounts and adding to /etc/hosts", output, err, "/etc/resolv.conf\n", "/etc/hosts\n", "192.0.2.9")
	})

	t.Run("NoNetwork", func(t *testing.T) {
		explain(t, "with --network none, /etc/hosts has only localhost")
		output, err := r.runOnce("runt-feature-dns-none", "--rootfs", r.rootfs, "--network", "none", "--", "/bin/cat", "/etc/hosts")
		mustSucceed(t, "reading /etc/hosts with --network none", output, err, "127.0.0.1\tlocalhost\n")
		if strings.Contains(output, "\trunt") {
			t.Errorf("/etc/hosts names the container, which has no address:\n%s", output)
		}
	})
}

// TestFeatureUserNetworks checks networks made with runt network create:
// their containers find each other by name, and can't reach containers on
// other networks.
func TestFeatureUserNetworks(t *testing.T) {
	explain(t, "User-defined networks: runt network create, where containers find each other by name, apart from other networks")
	r := newRuntTester(t)
	const network = "runt-feature-net"
	const server, client, other = "runt-feature-nets-server", "runt-feature-nets-client", "runt-feature-nets-other"
	// Whatever an earlier run left behind. Its containers go first: a
	// network with containers on it can't be removed.
	r.runt("rm", "-f", server, client, other)
	r.runt("network", "rm", network)
	output, err := r.runt("network", "create", network)
	mustSucceed(t, "runt network create", output, err)
	// Cleanups run last first, so this runs after the containers' own.
	t.Cleanup(func() { r.runt("network", "rm", network) })
	onNetwork := []string{"--rootfs", r.rootfs, "--network", network}
	serverID := r.startWith(server, onNetwork, "/bin/sh", "-c", greetingServer)
	r.startWith(client, onNetwork, "/bin/sleep", "600")
	r.start(other, "/bin/sleep", "600")
	serverState, clientState, otherState := readState(t, serverID), readState(t, containerID(t, client)), readState(t, containerID(t, other))
	// The network's bridge, as runt network ls shows it.
	var bridge string

	t.Run("Create", func(t *testing.T) {
		explain(t, "the network gets a subnet of its own, from 10.89.0.0/24 on, and a bridge, made when a container first joins")
		output, err := r.runt("network", "ls")
		mustSucceed(t, "runt network ls", output, err, network)
		var subnet string
		for _, line := range strings.Split(output, "\n") {
			if fields := strings.Fields(line); len(fields) == 4 && fields[1] == network {
				subnet, bridge = fields[2], fields[3]
			}
		}
		_, ipNet, err := net.ParseCIDR(subnet)
		if err != nil || !strings.HasPrefix(subnet, "10.89.") {
			t.Fatalf("network %s has subnet %q, want one from 10.89.0.0/24 on:\n%s", network, subnet, output)
		}
		if !ipNet.Contains(net.ParseIP(serverState.IPAddress)) {
			t.Errorf("the server has address %s, outside its network's %s", serverState.IPAddress, subnet)
		}
		master, err := os.Readlink(filepath.Join("/sys/class/net", hostVeth(serverID), "master"))
		if err != nil || filepath.Base(master) != bridge {
			t.Errorf("the server's veth is plugged into %q (%v), want the network's bridge %s", master, err, bridge)
		}
		observe("%s: %s on %s, with the server at %s", network, subnet, bridge, serverState.IPAddress)
	})

	t.Run("ResolvConf", func(t *testing.T) {
		explain(t, "its containers' /etc/resolv.conf names runt's DNS server, at 127.0.0.11")
		output, err := r.exec(client, "cat /etc/resolv.conf")
		mustSucceed(t, "reading /etc/resolv.conf", output, err, "nameserver 127.0.0.11\n")
	})

	t.Run("ByName", func(t *testing.T) {
		explain(t, "its containers find each other by name or ID")
		for _, name := range []string{server, serverID, server + "." + network} {
			output, err := r.exec(client, "getent hosts "+name)
			mustSucceed(t, "looking up "+name, output, err, serverState.IPAddress)
		}
		greeting, err := r.greetingFrom(client, server)
		mustSucceed(t, "the client connecting to the server by name", greeting, err, "you are "+clientState.IPAddress)
		observe("%s -> %s (%s): %q", client, server, serverState.IPAddress, greeting)
	})

	t.Run("Isolated", func(t *testing.T) {
		explain(t, "a container on another network can't find them by name, or reach them")
		output, err := r.exec(other, "getent hosts "+server)
		mustFail(t, "the default network's container looking up "+server, output, err, "")
		script := fmt.Sprintf("timeout 3 bash -c 'exec 3<>/dev/tcp/%s/%d' && echo connected", serverState.IPAddress, greetingPort)
		if output, err := r.exec(other, script); err == nil {
			t.Errorf("the default network's container (%s) connected to the server at %s:\n%s", otherState.IPAddress, serverState.IPAddress, output)
		}
		rule := []string{"-C", "FORWARD", "-i", "runt+", "-o", "runt+", "-j", "DROP"}
		if output, err := exec.Command("iptables", rule...).CombinedOutput(); err != nil {
			t.Errorf("iptables %s: %v\n%s", strings.Join(rule, " "), err, output)
		}
	})

	t.Run("NAT", func(t *testing.T) {
		explain(t, "its containers reach beyond the host, as on the default network")
		startOutside(t)
		greeting, err := r.greetingFrom(client, outsideServerIP)
		mustSucceed(t, "the client connecting to the outside server", greeting, err, "you are "+outsideHostIP)
	})

	t.Run("Remove", func(t *testing.T) {
		explain(t, "runt network rm removes the network and its bridge, once no container is on it")
		output, err := r.runt("network", "rm", network)
		mustFail(t, "removing the network with containers on it", output, err, "has containers on it")
		for _, name := range []string{server, client} {
			output, err := r.runt("rm", "-f", name)
			mustSucceed(t, "runt rm -f "+name, output, err)
		}
		output, err = r.runt("network", "rm", network)
		mustSucceed(t, "runt network rm", output, err)
		if output, _ := r.runt("network", "ls"); strings.Contains(output, network) {
			t.Errorf("runt network ls still lists %s:\n%s", network, output)
		}
		if _, err := os.Stat(filepath.Join("/var/lib/runt/networks", network+".json")); err == nil {
			t.Errorf("the network's configuration is still there")
		}
		if _, err := os.Stat(filepath.Join("/sys/class/net", bridge)); bridge != "" && err == nil {
			t.Errorf("the network's bridge %s is still there", bridge)
		}
	})
}
