//go:build linux

package runtime

// cgroups (control groups) are how Linux limits and accounts for the
// resources a group of processes uses: memory, CPU time, how many processes,
// disk I/O. In cgroup version 2, they are directories under /sys/fs/cgroup.
// Making a directory makes a cgroup; writing a number to a file in it, such
// as memory.max, sets a limit; and every process listed in its cgroup.procs,
// and their children, is held to it. Each container gets a cgroup of its
// own, /sys/fs/cgroup/runt/<id>. For how to read the Go here, see doc.go.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	// cgroupParent holds one cgroup per container, as Docker's cgroupfs
	// driver does with /sys/fs/cgroup/docker.
	cgroupParent = cgroupRoot + "/runt"
	// cgroup2SuperMagic is the statfs type of a cgroup v2 filesystem.
	cgroup2SuperMagic = 0x63677270
	// cpuPeriod is the scheduling period cpu.max quotas are measured over,
	// in microseconds: 100ms, the kernel and Docker default.
	cpuPeriod = 100000
)

// cgroupControllers are the controllers runt's limits need.
var cgroupControllers = []string{"cpu", "memory", "pids", "io"}

// cgroupV2 reports whether the unified cgroup v2 hierarchy is mounted.
// statfs describes the filesystem a path is on, including a number saying
// what kind of filesystem it is.
func cgroupV2() bool {
	var stat syscall.Statfs_t
	return syscall.Statfs(cgroupRoot, &stat) == nil && stat.Type == cgroup2SuperMagic
}

// createCgroup makes the container's cgroup and writes its limits. The
// container is placed in it as it is cloned (see Run), so it is limited from
// its first instruction.
func createCgroup(id string, resources Resources) (string, error) {
	// A controller can only be used in a cgroup if every ancestor enables
	// it for its children in cgroup.subtree_control.
	if err := os.MkdirAll(cgroupParent, 0o755); err != nil {
		return "", fmt.Errorf("create cgroup %q: %w", cgroupParent, err)
	}
	available, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("read cgroup controllers: %w", err)
	}
	for _, controller := range cgroupControllers {
		if !slices.Contains(strings.Fields(string(available)), controller) {
			continue
		}
		for _, dir := range []string{cgroupRoot, cgroupParent} {
			if err := writeCgroupFile(dir, "cgroup.subtree_control", "+"+controller); err != nil {
				return "", err
			}
		}
	}

	dir := filepath.Join(cgroupParent, id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cgroup %q: %w", dir, err)
	}
	if err := setCgroupLimits(dir, resources); err != nil {
		removeCgroup(dir)
		return "", err
	}
	return dir, nil
}

// setCgroupLimits writes each limit that is set to its file in the cgroup
// directory dir. A limit left at zero isn't written, which leaves it
// unlimited.
func setCgroupLimits(dir string, resources Resources) error {
	if resources.Memory > 0 {
		memory := strconv.FormatInt(resources.Memory, 10)
		if err := writeCgroupFile(dir, "memory.max", memory); err != nil {
			return err
		}
		// Docker's default lets a container swap as much as its memory
		// limit. Without a swap limit it could use unlimited swap and
		// never hit its memory limit.
		if err := writeCgroupFile(dir, "memory.swap.max", memory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if resources.CPUs > 0 {
		// cpu.max is "QUOTA PERIOD": the cgroup may run for QUOTA
		// microseconds out of every PERIOD. --cpus 1.5 is a quota of
		// 150000 out of 100000, one and a half CPUs' worth.
		quota := int64(math.Round(resources.CPUs * cpuPeriod))
		// The kernel rejects quotas under 1ms.
		if quota < 1000 {
			return fmt.Errorf("--cpus %g is too small; the minimum is 0.01", resources.CPUs)
		}
		if err := writeCgroupFile(dir, "cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriod)); err != nil {
			return err
		}
	}
	if resources.PidsLimit > 0 {
		if err := writeCgroupFile(dir, "pids.max", strconv.FormatInt(resources.PidsLimit, 10)); err != nil {
			return err
		}
	}
	// io.max takes one line per device, such as "8:0 rbps=1048576", and
	// leaves the keys a line doesn't mention unchanged. ioLimits is a slice
	// of an anonymous struct type, declared right there, pairing each key
	// with its limits, so one loop can handle all four.
	ioLimits := []struct {
		key    string
		limits []DeviceLimit
	}{
		{"rbps", resources.DeviceReadBps},
		{"wbps", resources.DeviceWriteBps},
		{"riops", resources.DeviceReadIOPS},
		{"wiops", resources.DeviceWriteIOPS},
	}
	for _, io := range ioLimits {
		for _, limit := range io.limits {
			device, err := blockDeviceNumber(limit.Path)
			if err != nil {
				return err
			}
			line := fmt.Sprintf("%s %s=%d", device, io.key, limit.Rate)
			if err := writeCgroupFile(dir, "io.max", line); err != nil {
				return fmt.Errorf("%w (io.max limits whole disks, such as /dev/sda, not partitions)", err)
			}
		}
	}
	return nil
}

// blockDeviceNumber returns the "major:minor" number of the block device at
// path. The kernel knows a device by two numbers: the major number says
// which driver (8 is SCSI disks), and the minor which device of that
// driver's (0 is the first, sda). stat on a device file reports them packed
// together in Rdev, which this unpacks with shifts (>>) and masks (&).
// S_IFMT picks out the file's type from its mode, which must be S_IFBLK, a
// block device.
func blockDeviceNumber(path string) (string, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return "", fmt.Errorf("device %q: %w", path, err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return "", fmt.Errorf("%q is not a block device", path)
	}
	// The glibc encoding of dev_t: the major number is bits 8-19 and 32-43,
	// the minor number is bits 0-7 and 20-31.
	rdev := uint64(stat.Rdev)
	major := (rdev>>8)&0xfff | (rdev>>32)&^0xfff
	minor := rdev&0xff | (rdev>>12)&^0xff
	return fmt.Sprintf("%d:%d", major, minor), nil
}

// removeCgroup kills anything left in the cgroup and removes it. The
// container's processes normally die with its init, since that ends its PID
// namespace, but they can take a moment to leave the cgroup.
func removeCgroup(dir string) error {
	// cgroup.kill sends SIGKILL to every process in the cgroup (Linux 5.14+).
	// Assigning to _ says the error is ignored on purpose: an older kernel
	// has no cgroup.kill.
	_ = writeCgroupFile(dir, "cgroup.kill", "1")
	// A cgroup can only be removed once it is empty; until then rmdir fails
	// with EBUSY. "for range 50" repeats 50 times, waiting 20ms between
	// tries, a second in all.
	var err error
	for range 50 {
		if err = syscall.Rmdir(dir); err == nil || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("remove cgroup %q: %w", dir, err)
}

// writeCgroupFile writes value to the cgroup file called name in dir, which
// is how every cgroup setting is made. The error names the file and value,
// since the kernel's own error ("invalid argument") rarely says which.
func writeCgroupFile(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %q to %s: %w", value, path, err)
	}
	return nil
}

// mountCgroup mounts the container's cgroup read-only at /sys/fs/cgroup, so
// programs inside can read their limits and usage. Because init is in its
// own cgroup namespace, the mount's root is the container's cgroup and the
// host's cgroups aren't visible. Read-only stops the container from
// raising its own limits.
func mountCgroup(rootfs string) error {
	if !cgroupV2() {
		return nil
	}
	target := filepath.Join(rootfs, "sys/fs/cgroup")
	flags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV)
	if err := syscall.Mount("cgroup2", target, "cgroup2", flags, ""); err != nil {
		return fmt.Errorf("mount /sys/fs/cgroup: %w", err)
	}
	return nil
}
