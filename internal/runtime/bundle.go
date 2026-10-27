package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// OCI runtime bundles (github.com/opencontainers/runtime-spec): a directory
// with a config.json describing the container, and usually its root
// filesystem. This is what runc runs, and what containerd and Podman hand to
// it.
//
// runt reads the parts of config.json that map onto what it can do (see
// LoadBundle). It always creates every namespace and applies its own
// security settings (capabilities, seccomp, masked paths, no_new_privs), so
// the fields for those are ignored.
//
// The structs below mirror config.json's layout: each field's json tag is
// its key in the file, and json.Unmarshal fills in the fields whose keys it
// finds, leaving the rest at their zero values. A field the file may leave
// out entirely is a pointer (*specProcess, *int64), which stays nil if it
// is missing, so runt can tell "not given" from "given as zero". This file
// has no build constraint, so its tests run anywhere. For how to read the Go
// here, see doc.go.

// runtimeSpec is the part of config.json runt reads and writes.
type runtimeSpec struct {
	OCIVersion string       `json:"ociVersion"`
	Process    *specProcess `json:"process,omitempty"`
	Root       *specRoot    `json:"root,omitempty"`
	Hostname   string       `json:"hostname,omitempty"`
	Mounts     []specMount  `json:"mounts,omitempty"`
	Linux      *specLinux   `json:"linux,omitempty"`
}

type specProcess struct {
	Terminal        bool     `json:"terminal,omitempty"`
	User            User     `json:"user"`
	Args            []string `json:"args"`
	Env             []string `json:"env,omitempty"`
	Cwd             string   `json:"cwd"`
	NoNewPrivileges bool     `json:"noNewPrivileges,omitempty"`
	ApparmorProfile string   `json:"apparmorProfile,omitempty"`
}

type specRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

type specMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type specLinux struct {
	UIDMappings []specIDMapping `json:"uidMappings,omitempty"`
	GIDMappings []specIDMapping `json:"gidMappings,omitempty"`
	Namespaces  []specNamespace `json:"namespaces,omitempty"`
	Resources   *specResources  `json:"resources,omitempty"`
}

type specIDMapping struct {
	ContainerID uint32 `json:"containerID"`
	HostID      uint32 `json:"hostID"`
	Size        uint32 `json:"size"`
}

type specNamespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// specResources uses anonymous struct types, written right in the field
// declarations, for its parts, since nothing else needs to name them.
type specResources struct {
	Memory *struct {
		Limit *int64 `json:"limit,omitempty"`
	} `json:"memory,omitempty"`
	CPU *struct {
		Quota  *int64  `json:"quota,omitempty"`
		Period *uint64 `json:"period,omitempty"`
	} `json:"cpu,omitempty"`
	Pids *struct {
		Limit int64 `json:"limit"`
	} `json:"pids,omitempty"`
}

// standardMounts are the mounts runt always sets up itself, in its own way.
// A config.json may list them (runc's needs to), but not others.
var standardMounts = []specMount{
	{Destination: "/proc", Type: "proc", Source: "proc"},
	{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
	{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
	{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
	{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
	{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
	{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
}

// LoadBundle reads config.Bundle/config.json into config. Settings given on
// the command line win over the file's; the command, root filesystem, and
// environment always come from the file. Supported fields:
//
//   - root.path (relative to the bundle) and root.readonly. The container
//     uses the directory itself, as runc does, not an overlay on top of it.
//   - process.args, env, cwd, user (uid, gid, additionalGids), terminal
//     (as -t), and apparmorProfile.
//   - hostname.
//   - linux.uidMappings and gidMappings, each a single range starting at
//     container ID 0. Without them runt uses /etc/subuid and /etc/subgid.
//   - linux.resources: memory.limit, cpu.quota and period, and pids.limit.
//   - mounts: bind mounts and tmpfs mounts (see bundleMount), besides the
//     standard ones runt makes anyway.
//
// Other kinds of mount, and linux.namespaces naming an existing namespace
// to join, runt rejects, rather than quietly running the container
// differently.
func LoadBundle(config *Config) error {
	dir, err := filepath.Abs(config.Bundle)
	if err != nil {
		return err
	}
	var spec runtimeSpec
	if err := readJSONFile(filepath.Join(dir, "config.json"), &spec); err != nil {
		return fmt.Errorf("read bundle: %w", err)
	}
	if !strings.HasPrefix(spec.OCIVersion, "1.") {
		return fmt.Errorf("bundle %s: unsupported ociVersion %q", dir, spec.OCIVersion)
	}
	if spec.Root == nil || spec.Root.Path == "" {
		return fmt.Errorf("bundle %s: config.json has no root.path", dir)
	}
	if spec.Process == nil || len(spec.Process.Args) == 0 {
		return fmt.Errorf("bundle %s: config.json has no process.args", dir)
	}
	if spec.Process.Cwd != "" && !path.IsAbs(spec.Process.Cwd) {
		return fmt.Errorf("bundle %s: process.cwd %q must be absolute", dir, spec.Process.Cwd)
	}
	// runt makes the standard mounts its own way; the rest it makes as -v
	// and --tmpfs would.
	for _, mount := range spec.Mounts {
		if isStandardMount(mount.Destination) {
			continue
		}
		m, err := bundleMount(dir, mount)
		if err != nil {
			return fmt.Errorf("bundle %s: mount at %s: %w", dir, mount.Destination, err)
		}
		config.Mounts = append(config.Mounts, m)
	}

	config.Bundle = dir
	config.Rootfs = spec.Root.Path
	if !filepath.IsAbs(config.Rootfs) {
		config.Rootfs = filepath.Join(dir, config.Rootfs)
	}
	config.DirectRootfs = true
	config.ReadonlyRootfs = spec.Root.Readonly
	config.Command = spec.Process.Args
	config.Env = spec.Process.Env
	if config.Env == nil {
		config.Env = []string{}
	}
	if config.Cwd == "" {
		config.Cwd = spec.Process.Cwd
	}
	if spec.Process.User.UID != 0 || spec.Process.User.GID != 0 || len(spec.Process.User.AdditionalGIDs) > 0 {
		user := spec.Process.User
		config.User = &user
	}
	config.Tty = config.Tty || spec.Process.Terminal
	if config.AppArmorProfile == "" {
		config.AppArmorProfile = spec.Process.ApparmorProfile
	}
	if config.Hostname == "" {
		config.Hostname = spec.Hostname
	}

	if spec.Linux == nil {
		return nil
	}
	for _, namespace := range spec.Linux.Namespaces {
		if namespace.Path != "" {
			return fmt.Errorf("bundle %s: joining an existing %s namespace is not supported", dir, namespace.Type)
		}
	}
	if config.UIDs, err = specIDRange(spec.Linux.UIDMappings, "uidMappings"); err != nil {
		return fmt.Errorf("bundle %s: %w", dir, err)
	}
	if config.GIDs, err = specIDRange(spec.Linux.GIDMappings, "gidMappings"); err != nil {
		return fmt.Errorf("bundle %s: %w", dir, err)
	}
	if (config.UIDs.Size == 0) != (config.GIDs.Size == 0) {
		return fmt.Errorf("bundle %s: give both uidMappings and gidMappings, or neither", dir)
	}
	// Limits from the file apply only if none were given on the command
	// line. Each pointer is checked before it is followed: *p reads the
	// value p points to, and following a nil pointer would crash.
	if resources := spec.Linux.Resources; resources != nil && config.Resources.IsZero() {
		if resources.Memory != nil && resources.Memory.Limit != nil && *resources.Memory.Limit > 0 {
			config.Resources.Memory = *resources.Memory.Limit
		}
		if cpu := resources.CPU; cpu != nil && cpu.Quota != nil && *cpu.Quota > 0 {
			period := uint64(cpuPeriodDefault)
			if cpu.Period != nil && *cpu.Period > 0 {
				period = *cpu.Period
			}
			config.Resources.CPUs = float64(*cpu.Quota) / float64(period)
		}
		if resources.Pids != nil && resources.Pids.Limit > 0 {
			config.Resources.PidsLimit = resources.Pids.Limit
		}
	}
	return nil
}

// cpuPeriodDefault is the CPU period the runtime spec assumes when only a
// quota is given, in microseconds.
const cpuPeriodDefault = 100000

// isStandardMount reports whether destination is one of standardMounts.
// path.Clean tidies a path, as in /dev/ -> /dev, so either way matches.
func isStandardMount(destination string) bool {
	for _, mount := range standardMounts {
		if path.Clean(destination) == mount.Destination {
			return true
		}
	}
	return false
}

// bundleOptions are the options runt takes on a bind mount in config.json.
// It always makes them as bind does with rbind, and private, so those
// options change nothing.
var bundleOptions = []string{"bind", "rbind", "private", "rprivate", "rw", "ro", "nosuid", "nodev", "noexec"}

// bundleMount turns a mount in config.json, other than the standard ones,
// into a Mount: a bind mount (type bind, or an option bind or rbind), whose
// source is relative to the bundle or absolute, or a tmpfs. Their options
// are as in mount(8). Unlike with --tmpfs, a tmpfs has only the options it
// lists.
func bundleMount(dir string, mount specMount) (Mount, error) {
	m := Mount{Destination: mount.Destination}
	switch {
	case mount.Type == "tmpfs":
		options, err := tmpfsOptions(nil, mount.Options)
		if err != nil {
			return Mount{}, err
		}
		m.Type, m.Options = MountTmpfs, options
	case mount.Type == "bind" || slices.Contains(mount.Options, "bind") || slices.Contains(mount.Options, "rbind"):
		if mount.Source == "" {
			return Mount{}, errors.New("a bind mount needs a source")
		}
		m.Type, m.Source = MountBind, mount.Source
		if !filepath.IsAbs(m.Source) {
			m.Source = filepath.Join(dir, m.Source)
		}
		for _, option := range mount.Options {
			if !slices.Contains(bundleOptions, option) {
				return Mount{}, fmt.Errorf("unsupported option %q", option)
			}
			// rw and the rest are runt's anyway.
			if flag, ok := mountFlagOptions[option]; ok && flag.on {
				m.Options = append(m.Options, option)
			}
		}
	default:
		return Mount{}, fmt.Errorf("type %q is not supported; runt makes bind and tmpfs mounts, besides its standard ones (/proc, /dev, /sys, ...)", mount.Type)
	}
	return m, checkDestination(m)
}

// specIDRange turns a config.json ID mapping into the single range runt
// supports.
// A switch with nothing after the word switch tries each case's condition
// in turn, like a chain of if and else if.
func specIDRange(mappings []specIDMapping, field string) (IDRange, error) {
	switch {
	case len(mappings) == 0:
		return IDRange{}, nil
	case len(mappings) > 1 || mappings[0].ContainerID != 0 || mappings[0].Size == 0:
		return IDRange{}, fmt.Errorf("linux.%s must be a single range starting at container ID 0", field)
	}
	return IDRange{Base: int(mappings[0].HostID), Size: int(mappings[0].Size)}, nil
}

// WriteSpec writes a starting config.json into dir, as runc spec does, for a
// root filesystem at rootfs. It runs sh on a terminal as container root,
// with the root filesystem read-only, and maps the container onto the
// invoking user's /etc/subuid and /etc/subgid ranges.
func WriteSpec(dir, rootfs string) error {
	configPath := filepath.Join(dir, "config.json")
	// Refuse to overwrite one: Stat succeeds if the file is there.
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("%s already exists", configPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	user := usernsUser("")
	uids, err := subIDRange("/etc/subuid", user, false)
	if err != nil {
		return err
	}
	gids, err := subIDRange("/etc/subgid", user, false)
	if err != nil {
		return err
	}
	spec := runtimeSpec{
		OCIVersion: "1.2.0",
		Process: &specProcess{
			Terminal:        true,
			Args:            []string{"sh"},
			Env:             []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "TERM=xterm"},
			Cwd:             "/",
			NoNewPrivileges: true,
		},
		Root:     &specRoot{Path: rootfs, Readonly: true},
		Hostname: "runt",
		Mounts:   standardMounts,
		Linux: &specLinux{
			UIDMappings: []specIDMapping{{ContainerID: 0, HostID: uint32(uids.Base), Size: uint32(uids.Size)}},
			GIDMappings: []specIDMapping{{ContainerID: 0, HostID: uint32(gids.Base), Size: uint32(gids.Size)}},
			Namespaces: []specNamespace{
				{Type: "pid"}, {Type: "network"}, {Type: "ipc"}, {Type: "uts"},
				{Type: "mount"}, {Type: "cgroup"}, {Type: "user"},
			},
		},
	}
	data, err := json.MarshalIndent(spec, "", "\t")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(data, '\n'), 0o644)
}
