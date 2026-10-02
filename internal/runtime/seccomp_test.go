package runtime

import (
	"encoding/binary"
	"testing"
)

// runFilter interprets program the way the kernel would for one syscall and
// returns the verdict.
func runFilter(t *testing.T, program []bpfInstruction, arch, nr uint32, arg0 uint64) uint32 {
	t.Helper()
	// struct seccomp_data: nr, arch, instruction pointer, six arguments.
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[seccompDataNr:], nr)
	binary.LittleEndian.PutUint32(data[seccompDataArch:], arch)
	binary.LittleEndian.PutUint64(data[seccompDataArg0:], arg0)

	var a uint32
	for pc := 0; pc < len(program); pc++ {
		instruction := program[pc]
		var condition bool
		switch instruction.code {
		case bpfLoadWord:
			a = binary.LittleEndian.Uint32(data[instruction.k:])
			continue
		case bpfReturn:
			return instruction.k
		case bpfJumpEqual:
			condition = a == instruction.k
		case bpfJumpGE:
			condition = a >= instruction.k
		case bpfJumpAnySet:
			condition = a&instruction.k != 0
		default:
			t.Fatalf("unknown opcode %#x at %d", instruction.code, pc)
		}
		if condition {
			pc += int(instruction.jt)
		} else {
			pc += int(instruction.jf)
		}
	}
	t.Fatal("ran off the end of the filter")
	return 0
}

func TestSeccompFilter(t *testing.T) {
	const (
		arch        = 0xc000003e
		otherArch   = 0x40000003
		read        = 0
		mount       = 165
		unshare     = 272
		clone       = 56
		clone3      = 435
		personality = 135

		eperm  = seccompErrno | errnoEPERM
		enosys = seccompErrno | errnoENOSYS
	)
	policy := seccompPolicy{
		arch:        arch,
		x32:         true,
		denied:      []uint32{mount, unshare},
		clone:       clone,
		clone3:      clone3,
		personality: personality,
	}

	tests := []struct {
		name string
		arch uint32
		nr   uint32
		arg0 uint64
		want uint32
	}{
		{"ordinary syscall", arch, read, 0, seccompAllow},
		{"highest syscall number", arch, 1000, 0, seccompAllow},
		{"first denied", arch, mount, 0, eperm},
		{"last denied", arch, unshare, 0, eperm},
		{"foreign architecture", otherArch, read, 0, seccompKillProcess},
		{"x32 read", arch, x32SyscallBit | read, 0, eperm},
		{"x32 mount", arch, x32SyscallBit | mount, 0, eperm},
		{"clone3", arch, clone3, 0, enosys},
		{"clone for fork", arch, clone, 0x11, seccompAllow},
		{"clone for a thread", arch, clone, 0x3d0f00, seccompAllow},
		{"clone with CLONE_NEWUSER", arch, clone, 0x10000011, eperm},
		{"clone with CLONE_NEWNS", arch, clone, 0x00020000, eperm},
		{"clone with CLONE_NEWNET", arch, clone, 0x40000000, eperm},
		{"personality query", arch, personality, 0xffffffff, seccompAllow},
		{"personality PER_LINUX", arch, personality, 0x0, seccompAllow},
		{"personality PER_LINUX32", arch, personality, 0x8, seccompAllow},
		{"personality UNAME26", arch, personality, 0x20000, seccompAllow},
		{"personality PER_LINUX32|UNAME26", arch, personality, 0x20008, seccompAllow},
		{"personality ADDR_NO_RANDOMIZE", arch, personality, 0x40000, eperm},
		{"personality READ_IMPLIES_EXEC", arch, personality, 0x400000, eperm},
	}
	program := buildSeccompFilter(policy)
	for _, test := range tests {
		if got := runFilter(t, program, test.arch, test.nr, test.arg0); got != test.want {
			t.Errorf("%s: verdict %#x, want %#x", test.name, got, test.want)
		}
	}

	// Without the x32 ABI, large syscall numbers get no special treatment.
	policy.x32 = false
	program = buildSeccompFilter(policy)
	if got := runFilter(t, program, arch, x32SyscallBit|read, 0); got != seccompAllow {
		t.Errorf("x32 bit with x32 off: verdict %#x, want allow", got)
	}
	if got := runFilter(t, program, arch, mount, 0); got != eperm {
		t.Errorf("mount with x32 off: verdict %#x, want EPERM", got)
	}
}
