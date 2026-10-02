package runtime

const sysSeccomp = 317

// Syscall numbers from <asm/unistd_64.h>. The denied list follows Docker's
// default profile for a container without extra capabilities.
var nativeSeccompPolicy = seccompPolicy{
	arch: 0xc000003e, // AUDIT_ARCH_X86_64
	x32:  true,

	clone:       56,
	clone3:      435,
	personality: 135,

	denied: []uint32{
		// Mounting and the newer mount API.
		165, // mount
		166, // umount2
		155, // pivot_root
		430, // fsopen
		431, // fsconfig
		432, // fsmount
		433, // fspick
		428, // open_tree
		429, // move_mount
		442, // mount_setattr

		// Namespaces. unshare(CLONE_NEWUSER) needs no privilege and would
		// hand back a full set of capabilities in the new namespace.
		272, // unshare
		308, // setns

		// Kernel modules, replacing the kernel, and rebooting.
		175, // init_module
		313, // finit_module
		176, // delete_module
		174, // create_module
		177, // get_kernel_syms
		178, // query_module
		246, // kexec_load
		320, // kexec_file_load
		169, // reboot

		// System-wide settings that are not namespaced, or not ours to set.
		164, // settimeofday
		227, // clock_settime
		170, // sethostname
		171, // setdomainname
		167, // swapon
		168, // swapoff
		163, // acct
		179, // quotactl
		443, // quotactl_fd
		103, // syslog
		153, // vhangup
		172, // iopl
		173, // ioperm

		// The kernel keyring is not namespaced.
		248, // add_key
		249, // request_key
		250, // keyctl

		// Inspecting or steering the kernel and other processes.
		321, // bpf
		298, // perf_event_open
		212, // lookup_dcookie
		300, // fanotify_init
		312, // kcmp
		438, // pidfd_getfd
		440, // process_madvise
		310, // process_vm_readv
		311, // process_vm_writev
		304, // open_by_handle_at

		// NUMA memory placement.
		237, // mbind
		238, // set_mempolicy
		239, // get_mempolicy
		279, // move_pages
		450, // set_mempolicy_home_node

		// Large attack surface, and a frequent source of kernel exploits.
		425, // io_uring_setup
		426, // io_uring_enter
		427, // io_uring_register
		323, // userfaultfd

		// Obsolete.
		156, // _sysctl
		139, // sysfs
		134, // uselib
		136, // ustat
		180, // nfsservctl
	},
}
