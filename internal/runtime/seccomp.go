package runtime

// seccomp ("secure computing") lets a process give the kernel a filter that
// runs on every system call it, and every process it starts, makes from then
// on. The filter looks at which call it is and its arguments, and decides:
// allow it, fail it with an error, or kill the process. runt's filter blocks
// about 60 calls a container has no business making, following Docker's
// default profile.
//
// The filter is a small program for a tiny virtual machine in the kernel,
// classic BPF (Berkeley Packet Filter, first made for filtering network
// packets). It has one working register, A, and a handful of instructions:
// load a 32-bit word of the call's details into A, compare A with a
// constant and skip ahead some number of instructions depending on the
// answer, and return a verdict. There are no loops, only forward jumps, so
// the kernel knows it always finishes.
//
// This file builds that program, and has no build constraint so its tests
// run anywhere. The per-architecture lists of call numbers are in
// seccomp_linux_amd64.go and seccomp_linux_arm64.go, and installing the
// filter is in seccomp_linux.go. For how to read the Go here, see doc.go.

// bpfInstruction mirrors struct sock_filter: one instruction of the classic
// BPF program the kernel runs on every syscall once the filter is installed.
// uint16, uint8, and uint32 are unsigned numbers of 16, 8, and 32 bits.
type bpfInstruction struct {
	code uint16
	jt   uint8 // instructions to skip if a jump's condition is true
	jf   uint8 // instructions to skip if it is false
	k    uint32
}

// Classic BPF opcodes from <linux/bpf_common.h>.
const (
	bpfLoadWord   = 0x20 // BPF_LD|BPF_W|BPF_ABS: A = the 32-bit word at offset k
	bpfJumpEqual  = 0x15 // BPF_JMP|BPF_JEQ|BPF_K: A == k
	bpfJumpGE     = 0x35 // BPF_JMP|BPF_JGE|BPF_K: A >= k
	bpfJumpAnySet = 0x45 // BPF_JMP|BPF_JSET|BPF_K: A & k != 0
	bpfReturn     = 0x06 // BPF_RET|BPF_K: finish with verdict k
)

// Offsets into struct seccomp_data, which is what the program reads from.
// Syscall arguments are 64-bit; on little-endian the low half comes first.
// struct seccomp_data is: the call's number (4 bytes), the architecture
// (4 bytes), the address of the instruction making the call (8 bytes), and
// then its six arguments (8 bytes each).
const (
	seccompDataNr   = 0
	seccompDataArch = 4
	seccompDataArg0 = 16
)

// Verdicts from <linux/seccomp.h>.
const (
	seccompAllow       = 0x7fff0000
	seccompErrno       = 0x00050000 // fail the syscall with the errno in the low 16 bits
	seccompKillProcess = 0x80000000
)

// Error numbers a blocked call fails with: EPERM, "operation not
// permitted", and ENOSYS, "function not implemented".
const (
	errnoEPERM  = 1
	errnoENOSYS = 38
)

// cloneNamespaceFlags are the clone(2) flags that create namespaces:
// CLONE_NEWNS, NEWCGROUP, NEWUTS, NEWIPC, NEWUSER, NEWPID, and NEWNET.
// Combined, as one number with each flag's bit set.
const cloneNamespaceFlags = 0x7e020000

// x32SyscallBit is set in the syscall number of x32 calls (64-bit code with
// 32-bit pointers on x86-64), which would otherwise slip past a filter
// written for the normal numbers.
const x32SyscallBit = 0x40000000

// allowedPersonalities are the personality(2) values Docker allows: reading
// the current one (0xffffffff), PER_LINUX, PER_LINUX32, and both with
// UNAME26. Anything else can switch off protections like ASLR.
var allowedPersonalities = []uint32{0x0, 0x8, 0x20000, 0x20008, 0xffffffff}

// seccompPolicy describes the filter for one CPU architecture, since syscall
// numbers differ between them.
type seccompPolicy struct {
	arch uint32 // AUDIT_ARCH_* value of the native ABI
	x32  bool   // the architecture also has the x32 ABI

	denied      []uint32 // fail with EPERM
	clone       uint32   // fail with EPERM if it asks for new namespaces
	clone3      uint32   // fail with ENOSYS
	personality uint32   // fail with EPERM unless in allowedPersonalities
}

// buildSeccompFilter compiles policy into a BPF program. Everything not
// mentioned in the policy is allowed.
//
// The program it builds reads, in outline:
//
//	load arch;    if arch != native: kill
//	load nr;      if nr is x32: EPERM
//	for each denied call: if nr == it: EPERM
//	if nr == clone3: ENOSYS
//	if nr == clone:  load arg0; if it asks for namespaces: EPERM, else allow
//	if nr == personality: load arg0; if allowed: allow, else EPERM
//	allow
//
// Each "if" is a jump instruction followed by what to do if the test
// passes. A jump's jt and jf say how many instructions to skip when the
// test is true or false, so jf: 1 means "if not, skip the next instruction
// (the return), and go on to the next test".
func buildSeccompFilter(policy seccompPolicy) []bpfInstruction {
	// Two small helper functions, kept in variables, that make the most
	// common instructions.
	load := func(offset uint32) bpfInstruction { return bpfInstruction{code: bpfLoadWord, k: offset} }
	ret := func(verdict uint32) bpfInstruction { return bpfInstruction{code: bpfReturn, k: verdict} }

	// A syscall made through another ABI (such as 32-bit int 0x80) has
	// different numbers, so the tables below would not apply. Kill instead.
	program := []bpfInstruction{
		load(seccompDataArch),
		{code: bpfJumpEqual, k: policy.arch, jt: 1},
		ret(seccompKillProcess),
		load(seccompDataNr),
	}
	if policy.x32 {
		program = append(program,
			bpfInstruction{code: bpfJumpGE, k: x32SyscallBit, jf: 1},
			ret(seccompErrno|errnoEPERM),
		)
	}

	for _, nr := range policy.denied {
		program = append(program,
			bpfInstruction{code: bpfJumpEqual, k: nr, jf: 1},
			ret(seccompErrno|errnoEPERM),
		)
	}

	// clone3 passes its flags in a struct, which a filter cannot read.
	// ENOSYS makes glibc fall back to clone, whose flags are argument 0.
	// For clone, jf: 4 skips the four instructions of its block when the
	// call is something else.
	program = append(program,
		bpfInstruction{code: bpfJumpEqual, k: policy.clone3, jf: 1},
		ret(seccompErrno|errnoENOSYS),

		bpfInstruction{code: bpfJumpEqual, k: policy.clone, jf: 4},
		load(seccompDataArg0),
		bpfInstruction{code: bpfJumpAnySet, k: cloneNamespaceFlags, jf: 1},
		ret(seccompErrno|errnoEPERM),
		ret(seccompAllow),
	)

	// Skip the personality block: the load, one jump per allowed value, and
	// the two returns.
	count := len(allowedPersonalities)
	program = append(program,
		bpfInstruction{code: bpfJumpEqual, k: policy.personality, jf: uint8(count + 3)},
		load(seccompDataArg0),
	)
	for i, persona := range allowedPersonalities {
		// On a match, jump over the remaining comparisons and the EPERM.
		program = append(program, bpfInstruction{code: bpfJumpEqual, k: persona, jt: uint8(count - i)})
	}
	program = append(program,
		ret(seccompErrno|errnoEPERM),
		ret(seccompAllow),
	)

	return append(program, ret(seccompAllow))
}
