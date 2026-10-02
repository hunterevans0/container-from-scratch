package runtime

const sysSeccomp = 277

// Syscall numbers from <asm-generic/unistd.h>. The denied list follows
// Docker's default profile for a container without extra capabilities; it is
// shorter than amd64's because arm64 never had the obsolete calls.
var nativeSeccompPolicy = seccompPolicy{
	arch: 0xc00000b7, // AUDIT_ARCH_AARCH64

	clone:       220,
	clone3:      435,
	personality: 92,

	denied: []uint32{
		// Mounting and the newer mount API.
		40,  // mount
		39,  // umount2
		41,  // pivot_root
		430, // fsopen
		431, // fsconfig
		432, // fsmount
		433, // fspick
		428, // open_tree
		429, // move_mount
		442, // mount_setattr

		// Namespaces. unshare(CLONE_NEWUSER) needs no privilege and would
		// hand back a full set of capabilities in the new namespace.
		97,  // unshare
		268, // setns

		// Kernel modules, replacing the kernel, and rebooting.
		105, // init_module
		273, // finit_module
		106, // delete_module
		104, // kexec_load
		294, // kexec_file_load
		142, // reboot

		// System-wide settings that are not namespaced, or not ours to set.
		170, // settimeofday
		112, // clock_settime
		161, // sethostname
		162, // setdomainname
		224, // swapon
		225, // swapoff
		89,  // acct
		60,  // quotactl
		443, // quotactl_fd
		116, // syslog
		58,  // vhangup

		// The kernel keyring is not namespaced.
		217, // add_key
		218, // request_key
		219, // keyctl

		// Inspecting or steering the kernel and other processes.
		280, // bpf
		241, // perf_event_open
		18,  // lookup_dcookie
		262, // fanotify_init
		272, // kcmp
		438, // pidfd_getfd
		440, // process_madvise
		270, // process_vm_readv
		271, // process_vm_writev
		265, // open_by_handle_at

		// NUMA memory placement.
		235, // mbind
		237, // set_mempolicy
		236, // get_mempolicy
		239, // move_pages
		450, // set_mempolicy_home_node

		// Large attack surface, and a frequent source of kernel exploits.
		425, // io_uring_setup
		426, // io_uring_enter
		427, // io_uring_register
		282, // userfaultfd

		// Obsolete.
		42, // nfsservctl
	},
}
