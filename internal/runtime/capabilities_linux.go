//go:build linux

package runtime

// Linux capabilities split root's powers into about 40 separate privileges:
// CAP_CHOWN to change a file's owner, CAP_NET_ADMIN to reconfigure the
// network, CAP_SYS_ADMIN for mounting and much else, and so on. A process
// can have some and not others. Before running the container's command, init
// drops all but the 14 Docker leaves a container, so that root inside the
// container can do ordinary root work but not administer the system. For how
// to read the Go here, see doc.go.

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
// CAP_NET_ADMIN, CAP_SYS_MODULE, CAP_SYS_PTRACE, and so on. uintptr is an
// unsigned integer the size of a pointer, the type system calls take their
// arguments as.
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
// and 32-63. Their fields are laid out in memory exactly as the kernel's C
// structs are, which is why they can be handed to the system call as they
// are.
type capHeader struct {
	version uint32
	pid     int32
}

// A process has several sets of capabilities, each a bitmask with bit N
// set for capability N: effective (what it can do now), permitted (what it
// may make effective), and inheritable (what may pass on through exec).
type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// dropCapabilities reduces this process to defaultCapabilities. Capabilities
// are per thread, so every call goes through AllThreadsSyscall to cover all of
// the Go runtime's threads; that needs a binary built without cgo.
func dropCapabilities() error {
	// The bitmask of capabilities to keep: 1 << n is a number with only
	// bit n set, and |= sets that bit in keep.
	var keep uint32
	for _, capability := range defaultCapabilities {
		keep |= 1 << capability
	}

	// The bounding set limits what any later exec can gain, including a root
	// process, which otherwise gets every capability back. Dropping from it
	// needs CAP_SETPCAP, so this comes before capset. EINVAL means we are
	// past the last capability this kernel knows.
	for capability := uintptr(0); capability < 64; capability++ {
		// Skip the ones to keep: keep & (1 << n) is non-zero if bit n is
		// set in keep.
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

	// The inheritable set stays empty, as in Docker. data is an array of two
	// capData; only the first, for capabilities 0 to 31, has anything set.
	header := capHeader{version: linuxCapabilityVersion3}
	data := [2]capData{{effective: keep, permitted: keep}}
	// A system call takes plain numbers, so the structs are passed by their
	// memory addresses: unsafe.Pointer(&header) is header's address, which
	// uintptr turns into a number. The unsafe package is how Go code steps
	// outside its type safety for this kind of thing.
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data)), 0); errno != 0 {
		return fmt.Errorf("set capabilities: %w", errno)
	}
	return nil
}
