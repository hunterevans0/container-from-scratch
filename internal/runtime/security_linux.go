//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// maskedPaths are hidden from the container: they leak information about the
// host or its hardware. The list is Docker's default.
var maskedPaths = []string{
	"/proc/asound",
	"/proc/acpi",
	"/proc/interrupts",
	"/proc/kcore",
	"/proc/keys",
	"/proc/latency_stats",
	"/proc/timer_list",
	"/proc/timer_stats",
	"/proc/sched_debug",
	"/proc/scsi",
	"/sys/firmware",
	"/sys/devices/virtual/powercap",
}

// readonlyPaths stay visible but cannot be written to: they hold kernel
// settings. The list is Docker's default.
var readonlyPaths = []string{
	"/proc/bus",
	"/proc/fs",
	"/proc/irq",
	"/proc/sys",
	"/proc/sysrq-trigger",
}

// protectPaths applies maskedPaths and readonlyPaths. It runs after
// pivot_root, and the container cannot undo it: unmounting needs
// CAP_SYS_ADMIN, which is dropped before the command starts.
func protectPaths() error {
	for _, path := range maskedPaths {
		if err := maskPath(path); err != nil {
			return fmt.Errorf("mask %s: %w", path, err)
		}
	}
	for _, path := range readonlyPaths {
		if err := readonlyPath(path); err != nil {
			return fmt.Errorf("make %s read-only: %w", path, err)
		}
	}
	return nil
}

// maskPath covers a file with /dev/null and a directory with an empty
// read-only tmpfs. Paths this kernel does not have are skipped.
func maskPath(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return syscall.Mount("tmpfs", path, "tmpfs", syscall.MS_RDONLY, "")
	}
	return syscall.Mount("/dev/null", path, "", syscall.MS_BIND, "")
}

// readonlyPath bind-mounts path onto itself, which gives it a mount of its
// own, and then remounts that read-only. A bind mount ignores MS_RDONLY the
// first time, hence the two steps.
func readonlyPath(path string) error {
	err := syscall.Mount(path, path, "", syscall.MS_BIND|syscall.MS_REC, "")
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	return syscall.Mount("", path, "", flags, "")
}

const prSetNoNewPrivs = 38

// setNoNewPrivs makes exec stop granting privileges: setuid and setgid bits
// and file capabilities are ignored from here on, in this process and all its
// descendants. It is per thread, so it goes through AllThreadsSyscall like
// the capability calls.
func setNoNewPrivs() error {
	if _, _, errno := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set no_new_privs: %w", errno)
	}
	return nil
}
