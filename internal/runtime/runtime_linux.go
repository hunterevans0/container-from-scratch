//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ExitCode returns the exit code to pass on when err is a process exiting
// unsuccessfully: the container's command for Init, or init for Run.
func ExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitStatus(exitErr.ProcessState), true
}

// exitStatus returns a process's exit code, or 128 plus the signal number if
// a signal killed it, as shells report it.
func exitStatus(process *os.ProcessState) int {
	if status, ok := process.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return process.ExitCode()
}

// commandError means the container's command could not be run at all.
type commandError struct{ err error }

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

// FailureCode is the exit code for an error runt reports itself. When the
// command could not be run it is 127 if the command was not found and 126
// otherwise, as shells and Docker use; for anything else it is 1.
func FailureCode(err error) int {
	var codeErr *exitCodeError
	if errors.As(err, &codeErr) {
		return codeErr.code
	}
	var cmdErr *commandError
	if !errors.As(err, &cmdErr) {
		return 1
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return 127
	}
	return 126
}

// closeExtraFiles makes sure the command inherits only stdin, stdout, and
// stderr, as runc does. Go opens its own files close-on-exec, but runt can
// inherit open descriptors from whatever started it (WSL leaves some
// terminal descriptors open, for example), and those would otherwise pass
// into the container.
func closeExtraFiles() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("list open files: %w", err)
	}
	for _, entry := range entries {
		if fd, err := strconv.Atoi(entry.Name()); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
	return nil
}

// isTerminal reports whether file is a terminal.
func isTerminal(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

// debugInitEnv tells the init process to wait for a debugger. It is set only
// on the init process and removed before the command runs.
const debugInitEnv = "RUNT_DEBUG_INIT"

// waitForDebugger blocks until a tracer such as Delve attaches to this
// process.
func waitForDebugger() error {
	for {
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return fmt.Errorf("read process status: %w", err)
		}
		for _, line := range strings.Split(string(status), "\n") {
			if pid, ok := strings.CutPrefix(line, "TracerPid:"); ok && strings.TrimSpace(pid) != "0" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
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

// Init is the container's init process: runt run (or create) clones it into
// the new namespaces, it sets the container up, waits to be started, and then
// runs the command as its child.
//
// File descriptors from the supervisor (see supervise):
//   - 3, the start FIFO: init reads one byte from it before running the
//     command. runt run writes the byte up front; runt start writes it later.
//   - 4, a status pipe: init writes "ready" once the container is set up,
//     then "started" once the command is running, or "error MESSAGE".
func Init() error {
	// Arguments, as built by supervise: --init ROOTFS APPARMOR_PROFILE COMMAND...
	if len(os.Args) < 5 {
		return fmt.Errorf("internal init arguments are incomplete")
	}
	rootfs := os.Args[2]
	apparmorProfile := os.Args[3]
	command := os.Args[4:]

	// Neither pipe should leak into the command.
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	start := os.NewFile(3, "start")
	status := os.NewFile(4, "status")

	if err := setupContainer(rootfs, apparmorProfile); err != nil {
		fmt.Fprintf(status, "error %v\n", err)
		return err
	}
	fmt.Fprintln(status, "ready")
	if _, err := start.Read(make([]byte, 1)); err != nil {
		return fmt.Errorf("wait for start: %w", err)
	}
	start.Close()

	if err := closeExtraFiles(); err != nil {
		fmt.Fprintf(status, "error %v\n", err)
		return err
	}
	// Catch signals from here on, so runt stop and runt kill reach the
	// command instead of only init.
	signals := make(chan os.Signal, 16)
	signal.Notify(signals)

	child := exec.Command(command[0], command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	// The command gets a process group of its own. If it is attached to a
	// terminal, that group becomes the terminal's foreground group, so
	// Ctrl+C reaches the command directly and only once, and shells inside
	// the container can use job control. Otherwise signals reach it only
	// through init.
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if isTerminal(os.Stdin) {
		child.SysProcAttr = &syscall.SysProcAttr{Foreground: true, Ctty: 0}
	}
	if err := child.Start(); err != nil {
		err = &commandError{err}
		fmt.Fprintf(status, "error %v\n", err)
		return err
	}
	fmt.Fprintln(status, "started")
	status.Close()

	go func() {
		for sig := range signals {
			// SIGCHLD is init's own business, and the Go runtime sends
			// itself SIGURG to preempt goroutines.
			if sig == syscall.SIGCHLD || sig == syscall.SIGURG {
				continue
			}
			child.Process.Signal(sig)
		}
	}()
	return child.Wait()
}

// setupContainer builds the container's view of the system and then locks
// init down, all before the command is started.
func setupContainer(rootfs, apparmorProfile string) error {
	// This runs before /proc is replaced, so it still reads the host's.
	if os.Getenv(debugInitEnv) != "" {
		os.Unsetenv(debugInitEnv)
		if err := waitForDebugger(); err != nil {
			return err
		}
	}
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
	if err := mountCgroup(rootfs); err != nil {
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

	return nil
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
