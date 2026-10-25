//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// runt gives every container an /etc/hosts and an /etc/resolv.conf of its
// own, as Docker does: images come without a resolv.conf, since Docker
// writes one, and the hosts file names the container's own address. The
// supervisor writes them (see etcFiles), and init mounts them over the
// container's own files (see mountEtcFiles), so the image and its layers
// are left as they are.

// defaultHostname is the container's hostname unless it is given one.
const defaultHostname = "runt"

// etcFiles are the files for a container's /etc, by path: its hostname and
// address in /etc/hosts, if it has one, and in /etc/resolv.conf, the DNS
// servers it should use.
func etcFiles(hostname string, network *containerNetwork) map[string]string {
	if hostname == "" {
		hostname = defaultHostname
	}
	var ip net.IP
	embeddedDNS := false
	if network != nil {
		ip = network.reservation.ip
		embeddedDNS = network.network.dns
	}
	return map[string]string{
		"/etc/hosts":       hostsFile(hostname, ip),
		"/etc/resolv.conf": containerResolvConf(hostResolvConf(), embeddedDNS).String(),
	}
}

// hostsFile is /etc/hosts for a container with hostname and, if it isn't
// nil, address ip, with the same entries for localhost as Docker's.
func hostsFile(hostname string, ip net.IP) string {
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	b.WriteString("fe00::0\tip6-localnet\n")
	b.WriteString("ff00::0\tip6-mcastprefix\n")
	b.WriteString("ff02::1\tip6-allnodes\n")
	b.WriteString("ff02::2\tip6-allrouters\n")
	if ip != nil {
		fmt.Fprintf(&b, "%s\t%s\n", ip, hostname)
	}
	return b.String()
}

// resolvConf is what runt uses of a resolv.conf: its DNS servers, search
// domains, and options.
type resolvConf struct {
	nameservers []string
	search      []string
	options     []string
}

// parseResolvConf parses a resolv.conf (see resolv.conf(5)).
func parseResolvConf(data string) resolvConf {
	var conf resolvConf
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			conf.nameservers = append(conf.nameservers, fields[1])
		case "search", "domain":
			// Whichever comes last wins.
			conf.search = fields[1:]
		case "options":
			conf.options = append(conf.options, fields[1:]...)
		}
	}
	return conf
}

func (c resolvConf) String() string {
	var b strings.Builder
	for _, nameserver := range c.nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", nameserver)
	}
	if len(c.search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(c.search, " "))
	}
	if len(c.options) > 0 {
		fmt.Fprintf(&b, "options %s\n", strings.Join(c.options, " "))
	}
	return b.String()
}

// hostResolvConf reads the host's DNS configuration. On a host whose
// resolver is systemd-resolved, /etc/resolv.conf names only its stub server
// at 127.0.0.53, and the servers it asks are in
// /run/systemd/resolve/resolv.conf, as Docker knows to look.
func hostResolvConf() resolvConf {
	data, _ := os.ReadFile("/etc/resolv.conf")
	conf := parseResolvConf(string(data))
	if slices.Equal(conf.nameservers, []string{"127.0.0.53"}) {
		if data, err := os.ReadFile("/run/systemd/resolve/resolv.conf"); err == nil {
			conf = parseResolvConf(string(data))
		}
	}
	return conf
}

// fallbackNameservers are the DNS servers Docker gives a container when the
// host has none that it could use: Google's.
var fallbackNameservers = []string{"8.8.8.8", "8.8.4.4"}

// containerResolvConf is /etc/resolv.conf for a container, from the host's.
// On a network with runt's DNS server, its one server is that. Otherwise it
// has the host's servers, but not those on the host's loopback interface,
// which a container can't reach, nor IPv6 ones, since containers have no
// IPv6 address. Either way, it keeps the host's search domains and options.
func containerResolvConf(host resolvConf, embeddedDNS bool) resolvConf {
	conf := resolvConf{search: host.search, options: host.options}
	if embeddedDNS {
		conf.nameservers = []string{dnsAddress}
		// Look up a name without a dot, such as a container's, as it
		// is first, not under each search domain first: those lookups
		// would go to the host's servers, and slowly fail.
		if !slices.ContainsFunc(host.options, func(option string) bool { return strings.HasPrefix(option, "ndots:") }) {
			conf.options = append(slices.Clip(host.options), "ndots:0")
		}
		return conf
	}
	for _, nameserver := range host.nameservers {
		if ip := net.ParseIP(nameserver); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			conf.nameservers = append(conf.nameservers, nameserver)
		}
	}
	if len(conf.nameservers) == 0 {
		conf.nameservers = fallbackNameservers
	}
	return conf
}

// etcStaging is where init writes the container's /etc files before
// mounting them, on the container's /dev.
const etcStaging = "/dev/.runt-etc"

// mountEtcFiles runs in init, after pivot_root: it bind-mounts files, given
// by path and content, over the container's own. They are written to a
// tmpfs of their own, which is detached once they are mounted: the bind
// mounts keep it, and nothing else shows it. A bind mount stays writable
// even if the root is made read-only, as with Docker.
//
// A file the container doesn't have is created to mount over, in its
// writable layer, unless keepRoot is set: then the root is a bundle's root
// filesystem, used as it is, which runt doesn't change, and such a file is
// left out.
func mountEtcFiles(files map[string]string, keepRoot bool) error {
	if len(files) == 0 {
		return nil
	}
	if err := os.Mkdir(etcStaging, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", etcStaging, err)
	}
	defer os.Remove(etcStaging)
	if err := syscall.Mount("tmpfs", etcStaging, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=755,size=256k"); err != nil {
		return fmt.Errorf("mount tmpfs for /etc files: %w", err)
	}
	defer syscall.Unmount(etcStaging, syscall.MNT_DETACH)
	for path, content := range files {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) && keepRoot {
			continue
		}
		source := filepath.Join(etcStaging, filepath.Base(path))
		if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		if err := mountTarget(path); err != nil {
			return fmt.Errorf("create %s to mount over: %w", path, err)
		}
		if err := syscall.Mount(source, path, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind mount %s: %w", path, err)
		}
	}
	return nil
}

// mountTarget makes sure there is a file at path to mount over. This runs
// after pivot_root, so a symlink there can only lead to a file in the
// container. One that leads nowhere, such as to systemd-resolved's file in
// /run, is replaced with an empty file, as is no file at all.
func mountTarget(path string) error {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o644)
}
