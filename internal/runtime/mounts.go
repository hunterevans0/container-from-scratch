package runtime

// Mounts on top of the container's root filesystem: bind mounts and volumes
// (-v) and tmpfs mounts (--tmpfs), as Docker takes them. This file parses
// the options and checks the mounts make sense together. It has no build
// constraint, so the parsing works, and is tested, anywhere. Making the
// mounts is in mounts_linux.go, and the volumes themselves in
// volumes_linux.go. For how to read the Go here, see doc.go.

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ParseVolume parses a -v option, SOURCE:DESTINATION[:OPTIONS], as Docker
// takes it:
//
//   - -v /host/dir:/data mounts a file or directory on the host. A relative
//     path, starting with . or .., is from the current directory.
//   - -v NAME:/data mounts the volume NAME, which is made if it doesn't
//     exist yet.
//
// OPTIONS are separated by commas: ro (read-only) or rw (the default), and
// for a volume, nocopy, for it not to be filled from the image (see
// populateVolume in mounts_linux.go).
func ParseVolume(s string) (Mount, error) {
	parts := strings.Split(s, ":")
	var options []string
	switch len(parts) {
	case 1:
		// Docker makes an anonymous volume for this, which runt doesn't.
		return Mount{}, fmt.Errorf("invalid volume %q: give a host path or a volume name before the container path, as in data:%s", s, s)
	case 2:
	case 3:
		options = strings.Split(parts[2], ",")
	default:
		return Mount{}, fmt.Errorf("invalid volume %q: use SOURCE:DESTINATION[:OPTIONS]", s)
	}
	source, destination := parts[0], parts[1]
	mount := Mount{Type: MountVolume, Source: source, Destination: destination}
	switch {
	case source == "." || source == ".." || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../"):
		// The container's monitor runs in /, so a relative path is made
		// absolute here, from where runt runs.
		absolute, err := filepath.Abs(source)
		if err != nil {
			return Mount{}, err
		}
		mount.Type, mount.Source = MountBind, absolute
	case !validName.MatchString(source):
		return Mount{}, fmt.Errorf("invalid volume %q: %q is neither a volume name (letters, digits, and _.-) nor a host path (start it with / or ./)", s, source)
	}
	for _, option := range options {
		switch {
		case option == "ro":
			mount.Options = append(mount.Options, "ro")
		case option == "rw":
		case option == "nocopy" && mount.Type == MountVolume:
			mount.Options = append(mount.Options, "nocopy")
		default:
			return Mount{}, fmt.Errorf("invalid volume %q: unsupported option %q; use ro, rw, or, for a volume, nocopy", s, option)
		}
	}
	return mount, checkDestination(mount)
}

// tmpfsDefaults are the options a --tmpfs mount has unless it says
// otherwise, as in Docker: nothing on it can be a setuid program or a
// device, or be run.
var tmpfsDefaults = []string{"nosuid", "nodev", "noexec"}

// ParseTmpfs parses a --tmpfs option, DESTINATION[:OPTIONS], as Docker takes
// it, as in /run:size=64m,mode=755. OPTIONS are separated by commas (see
// tmpfsOptions).
func ParseTmpfs(s string) (Mount, error) {
	destination, list, hasOptions := strings.Cut(s, ":")
	var given []string
	if hasOptions {
		given = strings.Split(list, ",")
	}
	options, err := tmpfsOptions(tmpfsDefaults, given)
	if err != nil {
		return Mount{}, fmt.Errorf("invalid tmpfs %q: %w", s, err)
	}
	mount := Mount{Type: MountTmpfs, Destination: destination, Options: options}
	return mount, checkDestination(mount)
}

// mountFlagOptions are the on/off options a tmpfs takes, each with the
// option it is in the end, and whether it turns that on or off: noexec
// turns noexec on, and exec turns it off.
var mountFlagOptions = map[string]struct {
	option string
	on     bool
}{
	"ro": {"ro", true}, "rw": {"ro", false},
	"nosuid": {"nosuid", true}, "suid": {"nosuid", false},
	"nodev": {"nodev", true}, "dev": {"nodev", false},
	"noexec": {"noexec", true}, "exec": {"noexec", false},
}

// tmpfsOptions works out a tmpfs mount's options: defaults, then given, a
// later option winning over an earlier one. Besides those in
// mountFlagOptions, it takes size=SIZE, as in 64m (see ParseBytes), and
// mode=MODE, the permissions of its root directory in octal, as in 1777 (the
// default). It returns them in a set order, with the size in bytes.
func tmpfsOptions(defaults, given []string) ([]string, error) {
	on := map[string]bool{}
	var size, mode string
	// slices.Concat joins the two lists into a new one.
	for _, option := range slices.Concat(defaults, given) {
		key, value, hasValue := strings.Cut(option, "=")
		flag, isFlag := mountFlagOptions[option]
		switch {
		case isFlag:
			on[flag.option] = flag.on
		case key == "size" && hasValue:
			bytes, err := ParseBytes(value)
			if err != nil || bytes <= 0 {
				return nil, fmt.Errorf("invalid size %q", value)
			}
			size = "size=" + strconv.FormatInt(bytes, 10)
		case key == "mode" && hasValue:
			// Base 8, at most 32 bits, as the permissions are written in
			// octal.
			bits, err := strconv.ParseUint(value, 8, 32)
			if err != nil || bits > 0o7777 {
				return nil, fmt.Errorf("invalid mode %q; give it in octal, as in 1777", value)
			}
			mode = "mode=" + strconv.FormatUint(bits, 8)
		default:
			return nil, fmt.Errorf("unsupported option %q; use size=, mode=, ro, rw, exec, noexec, suid, nosuid, dev, or nodev", option)
		}
	}
	var options []string
	for _, option := range []string{"ro", "nosuid", "nodev", "noexec"} {
		if on[option] {
			options = append(options, option)
		}
	}
	for _, option := range []string{size, mode} {
		if option != "" {
			options = append(options, option)
		}
	}
	return options, nil
}

// checkDestination checks where a mount goes in the container: an absolute
// path, other than / itself, and not in /proc or /sys, which runt fills
// with the kernel's own filesystems. Its destination is tidied up, as in
// /data/ -> /data.
func checkDestination(mount Mount) error {
	if !path.IsAbs(mount.Destination) {
		return fmt.Errorf("invalid mount destination %q: it must be an absolute path", mount.Destination)
	}
	destination := path.Clean(mount.Destination)
	if destination == "/" {
		return errors.New("invalid mount destination \"/\": the container's root can't be mounted over")
	}
	for _, kernel := range []string{"/proc", "/sys"} {
		if isUnder(destination, kernel) {
			return fmt.Errorf("invalid mount destination %q: runt makes %s itself", mount.Destination, kernel)
		}
	}
	return nil
}

// isUnder reports whether p is dir, or inside it. Both are clean paths.
func isUnder(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// hasOption reports whether a mount has option, such as ro.
func (m Mount) hasOption(option string) bool {
	return slices.Contains(m.Options, option)
}

// sortMounts checks a container's mounts against each other, and returns
// them in the order to make them in: by how deep their destinations are, so
// that /data comes before /data/cache, which then goes inside it rather
// than being hidden by it. Each destination is cleaned. Two mounts may not
// share a destination.
func sortMounts(mounts []Mount) ([]Mount, error) {
	sorted := slices.Clone(mounts)
	seen := map[string]bool{}
	for i := range sorted {
		if err := checkDestination(sorted[i]); err != nil {
			return nil, err
		}
		sorted[i].Destination = path.Clean(sorted[i].Destination)
		if seen[sorted[i].Destination] {
			return nil, fmt.Errorf("two mounts at %s", sorted[i].Destination)
		}
		seen[sorted[i].Destination] = true
	}
	// SliceStable keeps mounts of the same depth in the order given.
	depth := func(p string) int { return strings.Count(p, "/") }
	sort.SliceStable(sorted, func(i, j int) bool { return depth(sorted[i].Destination) < depth(sorted[j].Destination) })
	return sorted, nil
}

// mountCovers reports whether one of mounts is at p, a clean path, or
// above it, so that p in the container is the mount's rather than the root
// filesystem's.
func mountCovers(mounts []Mount, p string) bool {
	for _, mount := range mounts {
		if isUnder(p, path.Clean(mount.Destination)) {
			return true
		}
	}
	return false
}
