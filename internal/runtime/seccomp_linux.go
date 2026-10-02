//go:build linux && (amd64 || arm64)

package runtime

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	seccompSetModeFilter  = 1
	seccompFilterFlagSync = 1 // SECCOMP_FILTER_FLAG_TSYNC
)

// sockFprog mirrors struct sock_fprog.
type sockFprog struct {
	length uint16
	filter *bpfInstruction
}

// installSeccomp attaches the syscall filter to this process. It cannot be
// removed afterwards and is inherited by everything the container runs. An
// unprivileged process may only install a filter once no_new_privs is set.
func installSeccomp() error {
	program := buildSeccompFilter(nativeSeccompPolicy)
	fprog := sockFprog{length: uint16(len(program)), filter: &program[0]}
	// TSYNC applies the filter to every thread of the Go runtime at once.
	// On failure it can return the ID of a thread that could not be synced
	// instead of an errno.
	result, _, errno := syscall.Syscall(sysSeccomp, seccompSetModeFilter, seccompFilterFlagSync, uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		return fmt.Errorf("install seccomp filter: %w", errno)
	}
	if result != 0 {
		return fmt.Errorf("install seccomp filter: could not sync thread %d", result)
	}
	return nil
}
