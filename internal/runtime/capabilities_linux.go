//go:build linux

package runtime

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Capability numbers from <linux/capability.h>.
const (
	capChown          = 0
	capDacOverride    = 1
	capFowner         = 3
	capFsetid         = 4
	capKill           = 5
	capSetgid         = 6
	capSetuid         = 7
	capSetpcap        = 8
	capNetBindService = 10
	capNetRaw         = 13
	capSysChroot      = 18
	capMknod          = 27
	capAuditWrite     = 29
	capSetfcap        = 31
)

// defaultCapabilities is the set Docker leaves a container by default. It
// covers ordinary root work (file ownership, switching users, binding low
// ports, ping) but not administering the system: no CAP_SYS_ADMIN,
// CAP_NET_ADMIN, CAP_SYS_MODULE, CAP_SYS_PTRACE, and so on.
var defaultCapabilities = []uintptr{
	capChown,
	capDacOverride,
	capFowner,
	capFsetid,
	capKill,
	capSetgid,
	capSetuid,
	capSetpcap,
	capNetBindService,
	capNetRaw,
	capSysChroot,
	capMknod,
	capAuditWrite,
	capSetfcap,
}

const linuxCapabilityVersion3 = 0x20080522

// capHeader and capData mirror struct __user_cap_header_struct and
// __user_cap_data_struct. Version 3 takes two capData, for capabilities 0-31
// and 32-63.
type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// dropCapabilities reduces this process to defaultCapabilities. Capabilities
// are per thread, so every call goes through AllThreadsSyscall to cover all of
// the Go runtime's threads; that needs a binary built without cgo.
func dropCapabilities() error {
	var keep uint32
	for _, capability := range defaultCapabilities {
		keep |= 1 << capability
	}

	// The bounding set limits what any later exec can gain, including a root
	// process, which otherwise gets every capability back. Dropping from it
	// needs CAP_SETPCAP, so this comes before capset. EINVAL means we are
	// past the last capability this kernel knows.
	for capability := uintptr(0); capability < 64; capability++ {
		if capability < 32 && keep&(1<<capability) != 0 {
			continue
		}
		_, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, syscall.PR_CAPBSET_DROP, capability, 0)
		if errno == syscall.EINVAL {
			break
		}
		if errno != 0 {
			return fmt.Errorf("drop capability %d from bounding set: %w", capability, errno)
		}
	}

	// The inheritable set stays empty, as in Docker.
	header := capHeader{version: linuxCapabilityVersion3}
	data := [2]capData{{effective: keep, permitted: keep}}
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data)), 0); errno != 0 {
		return fmt.Errorf("set capabilities: %w", errno)
	}
	return nil
}
