//go:build linux

package runtime

// Linux capabilities split root's powers into about 40 separate privileges:
// CAP_CHOWN to change a file's owner, CAP_NET_ADMIN to reconfigure the
// network, CAP_SYS_ADMIN for mounting and much else, and so on. A process
// can have some and not others. Before running the container's command, init
// drops all but the 14 Docker leaves a container, so that root inside the
// container can do ordinary root work but not administer the system. For how
// to read the Go here, see doc.go. The capabilities are dropped in
// setupContainer (runtime_linux.go) and ExecInit (exec_linux.go).

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
