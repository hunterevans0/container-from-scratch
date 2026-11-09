//go:build linux

package runtime

// What two of the protections init applies before running the container's
// command need: the parts of /proc and /sys to hide or freeze, which would
// let the container learn about or change the host, and the prctl number for
// no_new_privs. setupContainer in runtime_linux.go applies them (and ExecInit
// in exec_linux.go sets no_new_privs too). The others are in
// capabilities_linux.go, seccomp_linux.go, and apparmor_linux.go. For how to
// read the Go here, see doc.go.

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

// prSetNoNewPrivs is PR_SET_NO_NEW_PRIVS, which the syscall package doesn't
// define, from <linux/prctl.h>. prctl is a system call with many uses, each
// picked by its first argument.
const prSetNoNewPrivs = 38
