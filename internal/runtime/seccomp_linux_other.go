//go:build linux && !amd64 && !arm64

package runtime

import (
	"fmt"
	goruntime "runtime"
)

// On Linux on any architecture but amd64 and arm64 (see the build
// constraint), runt builds, but can't install its seccomp filter, so
// containers fail to start. For how to read the Go here, see doc.go.

// sysSetns is not a system call, so the DNS server can't join a container's
// network namespace either: setns's number is in the same tables. ^ flips
// every bit of 0, giving the largest possible number, which is no system
// call: making it fails with ENOSYS.
const sysSetns = ^uintptr(0)

// installSeccomp needs a table of syscall numbers for each architecture, and
// only amd64 and arm64 have one.
func installSeccomp() error {
	return fmt.Errorf("seccomp filter is not implemented for %s", goruntime.GOARCH)
}
