//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"unsafe"
)

func Run(config Config) error {
	if len(config.Command) == 0 {
		return fmt.Errorf("command cannot be empty")
	}
	// Container UIDs/GIDs 0..Size-1 map to host IDs Base..Base+Size-1, so root
	// inside the container is an unprivileged user on the host. The ranges
	// come from /etc/subuid and /etc/subgid; scripts/make-rootfs.sh reads the
	// same files to shift the rootfs ownership to match.
	user := usernsUser(config.UsernsUser)
	explicit := config.UsernsUser != ""
	uids, err := subIDRange("/etc/subuid", user, explicit)
	if err != nil {
		return fmt.Errorf("read subordinate UIDs: %w", err)
	}
	gids, err := subIDRange("/etc/subgid", user, explicit)
	if err != nil {
		return fmt.Errorf("read subordinate GIDs: %w", err)
	}
	if err := checkRootfsOwner(config.Rootfs, uids, gids); err != nil {
		return err
	}

	args := []string{"--init", config.Rootfs, config.AppArmorProfile}
	args = append(args, config.Command...)
	command := exec.Command("/proc/self/exe", args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWNET |
			syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWCGROUP,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: uids.Base, Size: uids.Size}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: gids.Base, Size: gids.Size}},
		// Allow setgroups inside the container so tools like apt and su work.
		GidMappingsEnableSetgroups: true,
		// Become root of the new user namespace (host uid uids.Base). Without
		// this the child stays host uid 0, which is unmapped there, so it
		// would have no capabilities inside the container.
		Credential: &syscall.Credential{Uid: 0, Gid: 0},
	}
	return command.Run()
}

// checkRootfsOwner fails early with a helpful message if the rootfs has not
// been shifted into the user-namespace ID ranges.
func checkRootfsOwner(rootfs string, uids, gids IDRange) error {
	var stat syscall.Stat_t
	if err := syscall.Stat(rootfs, &stat); err != nil {
		return fmt.Errorf("stat rootfs %q: %w", rootfs, err)
	}
	if int(stat.Uid) != uids.Base || int(stat.Gid) != gids.Base {
		return fmt.Errorf("rootfs %q is owned by host %d:%d, but container root is host %d:%d; run `make rootfs` to shift its ownership", rootfs, stat.Uid, stat.Gid, uids.Base, gids.Base)
	}
	return nil
}

func Init() error {
	// Arguments, as built by Run: --init ROOTFS APPARMOR_PROFILE COMMAND...
	if len(os.Args) < 5 {
		return fmt.Errorf("internal init arguments are incomplete")
	}

	rootfs := os.Args[2]
	apparmorProfile := os.Args[3]
	command := os.Args[4:]
	if err := syscall.Sethostname([]byte("runt")); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	// The new network namespace starts with only a loopback interface, and
	// it is down.
	if err := bringUpLoopback(); err != nil {
		return err
	}
	// Stop mount events from propagating back to the host. Without this, a
	// shared root mount (the systemd default) would leak our mounts out.
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	// pivot_root requires the new root to be a mount point, so bind-mount
	// rootfs onto itself.
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs %q: %w", rootfs, err)
	}
	// /proc, /sys, and /dev are set up before pivot_root: inside a user
	// namespace the kernel only allows mounting proc and sysfs while the
	// host's copies are still visible, and /dev borrows device files from
	// the host's /dev.
	if err := mountProc(rootfs); err != nil {
		return err
	}
	if err := mountSys(rootfs); err != nil {
		return err
	}
	if err := setupDev(rootfs); err != nil {
		return err
	}
	if err := pivotRoot(rootfs); err != nil {
		return err
	}
	if err := protectPaths(); err != nil {
		return err
	}

	// Everything above needed full privileges in the user namespace; the
	// command itself does not. The order of the steps below matters.
	if apparmorProfile != "" {
		// The profile request is tied to this thread, so stay on it until
		// the command has been forked from it.
		goruntime.LockOSThread()
		if err := applyAppArmorProfile(apparmorProfile); err != nil {
			return err
		}
	}
	// no_new_privs comes before the seccomp filter, which an unprivileged
	// process may not install without it.
	if err := setNoNewPrivs(); err != nil {
		return err
	}
	if err := dropCapabilities(); err != nil {
		return err
	}
	// The filter goes last so it does not have to allow the calls above.
	if err := installSeccomp(); err != nil {
		return err
	}

	child := exec.Command(command[0], command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	return child.Run()
}

// pivotRoot makes rootfs the new "/" and detaches the host's root filesystem,
// so unlike chroot there is no old root left to escape back into.
func pivotRoot(rootfs string) error {
	if err := os.Chdir(rootfs); err != nil {
		return fmt.Errorf("change directory to %q: %w", rootfs, err)
	}
	// pivot_root(".", ".") stacks the old root on top of the new one, so no
	// directory is needed to hold it (see pivot_root(2)).
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root %q: %w", rootfs, err)
	}
	// Detach the old root so the host filesystem is no longer reachable.
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("change directory: %w", err)
	}
	return nil
}

// mountProc mounts a fresh /proc so tools like ps and top only see the
// processes in this container's PID namespace.
func mountProc(rootfs string) error {
	target := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(target, 0o555); err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV)
	if err := syscall.Mount("proc", target, "proc", flags, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	return nil
}

// mountSys mounts a read-only /sys. sysfs shows the network devices of the
// mounting process's network namespace, so the container sees only its own
// interfaces, and read-only keeps it from changing kernel settings.
func mountSys(rootfs string) error {
	target := filepath.Join(rootfs, "sys")
	if err := os.MkdirAll(target, 0o555); err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}
	flags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV)
	if err := syscall.Mount("sysfs", target, "sysfs", flags, ""); err != nil {
		return fmt.Errorf("mount /sys: %w", err)
	}
	return nil
}

// hostDevices are bind-mounted from the host's /dev. A user namespace can't
// create device nodes with mknod, so borrowing the host's is the standard
// approach (rootless runc and Podman do the same).
var hostDevices = []string{"null", "zero", "full", "random", "urandom", "tty"}

var devSymlinks = map[string]string{
	"fd":     "/proc/self/fd",
	"stdin":  "/proc/self/fd/0",
	"stdout": "/proc/self/fd/1",
	"stderr": "/proc/self/fd/2",
	"ptmx":   "pts/ptmx",
}

// setupDev replaces the rootfs's /dev with a minimal one on a tmpfs, so the
// container sees only a few safe devices instead of the host's.
func setupDev(rootfs string) error {
	dev := filepath.Join(rootfs, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", dev, err)
	}
	if err := syscall.Mount("tmpfs", dev, "tmpfs", syscall.MS_NOSUID|syscall.MS_STRICTATIME, "mode=755,size=65536k"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}

	for _, name := range hostDevices {
		target := filepath.Join(dev, name)
		// A bind mount needs an existing file to mount over.
		if err := os.WriteFile(target, nil, 0o666); err != nil {
			return fmt.Errorf("create %q: %w", target, err)
		}
		if err := syscall.Mount(filepath.Join("/dev", name), target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind mount /dev/%s: %w", name, err)
		}
	}
	for name, dest := range devSymlinks {
		if err := os.Symlink(dest, filepath.Join(dev, name)); err != nil {
			return fmt.Errorf("create /dev/%s symlink: %w", name, err)
		}
	}

	// A private devpts instance gives the container its own pseudo-terminals.
	pts := filepath.Join(dev, "pts")
	if err := os.Mkdir(pts, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", pts, err)
	}
	if err := syscall.Mount("devpts", pts, "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil {
		return fmt.Errorf("mount /dev/pts: %w", err)
	}

	shm := filepath.Join(dev, "shm")
	if err := os.Mkdir(shm, 0o1777); err != nil {
		return fmt.Errorf("create %q: %w", shm, err)
	}
	if err := syscall.Mount("shm", shm, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=1777,size=65536k"); err != nil {
		return fmt.Errorf("mount /dev/shm: %w", err)
	}

	// mqueue shows the POSIX message queues of this IPC namespace.
	mqueue := filepath.Join(dev, "mqueue")
	if err := os.Mkdir(mqueue, 0o755); err != nil {
		return fmt.Errorf("create %q: %w", mqueue, err)
	}
	if err := syscall.Mount("mqueue", mqueue, "mqueue", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /dev/mqueue: %w", err)
	}
	return nil
}

// bringUpLoopback sets the IFF_UP flag on "lo" so localhost works inside the
// container.
func bringUpLoopback() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open socket: %w", err)
	}
	defer syscall.Close(fd)

	// struct ifreq: a 16-byte interface name followed by a 24-byte union,
	// whose first field is the flags.
	var ifr struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(ifr.name[:], "lo")
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return fmt.Errorf("read loopback flags: %w", errno)
	}
	ifr.flags |= syscall.IFF_UP
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return fmt.Errorf("bring up loopback: %w", errno)
	}
	return nil
}
