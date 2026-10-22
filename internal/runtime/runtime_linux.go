//go:build linux

package runtime

import (
	"encoding/json"
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
// unsuccessfully: the container's command for Init, init for Run, or the
// container for Attach.
func ExitCode(err error) (int, bool) {
	var status statusError
	if errors.As(err, &status) {
		return int(status), true
	}
	var commandExit *commandExitError
	if errors.As(err, &commandExit) {
		return waitStatusCode(commandExit.status), true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitStatus(exitErr.ProcessState), true
}

// exitStatus returns a process's exit code, or 128 plus the signal number if
// a signal killed it, as shells report it.
func exitStatus(process *os.ProcessState) int {
	if status, ok := process.Sys().(syscall.WaitStatus); ok {
		return waitStatusCode(status)
	}
	return process.ExitCode()
}

func waitStatusCode(status syscall.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

// commandExitError is the container's command exiting unsuccessfully, as
// init saw it.
type commandExitError struct{ status syscall.WaitStatus }

func (e *commandExitError) Error() string {
	if e.status.Signaled() {
		return "command killed by " + e.status.Signal().String()
	}
	return fmt.Sprintf("command exited with code %d", e.status.ExitStatus())
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

// initConfig is what init needs to know about the container. The supervisor
// sends it as JSON on file descriptor 5.
type initConfig struct {
	// Rootfs is the directory to make the container's root: the overlay
	// mount, or the rootfs itself.
	Rootfs          string   `json:"rootfs"`
	ReadonlyRootfs  bool     `json:"readonlyRootfs,omitempty"`
	Hostname        string   `json:"hostname"`
	AppArmorProfile string   `json:"apparmorProfile,omitempty"`
	Command         []string `json:"command"`
	// Env replaces the command's environment if it is not nil.
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd,omitempty"`
	User *User    `json:"user,omitempty"`
	// UserSpec, if set, is looked up inside the container to give User.
	UserSpec string `json:"userSpec,omitempty"`
	// Tty gives the command a pseudo-terminal, whose master end init sends
	// back on file descriptor 6.
	Tty bool `json:"tty,omitempty"`
	// Network, if set, configures eth0, which the supervisor has put in
	// the container's network namespace.
	Network *initNetwork `json:"network,omitempty"`
}

// Init is the container's init process: runt run (or create) clones it into
// the new namespaces, it sets the container up, waits to be started, and then
// runs the command as its child. As PID 1 of the container it then forwards
// signals to the command and reaps orphaned processes until the command
// exits.
//
// File descriptors from the supervisor (see supervise):
//   - 3, the start FIFO: init reads one byte from it before running the
//     command. The supervisor writes it once init is ready (runt run), or
//     runt start writes it later.
//   - 4, a status pipe: init writes "ready" once the container is set up,
//     then "started" once the command is running, or "error MESSAGE".
//   - 5, the configuration pipe, with an initConfig.
//   - 6, with Tty, a Unix socket to send the pseudo-terminal's master on.
func Init() error {
	// None of these should leak into the command.
	for fd := 3; fd <= 5; fd++ {
		syscall.CloseOnExec(fd)
	}
	start := os.NewFile(3, "start")
	status := os.NewFile(4, "status")
	configFile := os.NewFile(5, "config")
	var config initConfig
	err := json.NewDecoder(configFile).Decode(&config)
	configFile.Close()
	if err == nil && len(config.Command) == 0 {
		err = errors.New("no command")
	}
	if err != nil {
		err = fmt.Errorf("read init configuration: %w", err)
		fmt.Fprintf(status, "error %v\n", err)
		return err
	}
	var consoleSocket *os.File
	if config.Tty {
		syscall.CloseOnExec(6)
		consoleSocket = os.NewFile(6, "console socket")
	}

	tty, err := setupContainer(config, consoleSocket)
	if consoleSocket != nil {
		consoleSocket.Close()
	}
	if err == nil {
		err = prepareCommand(&config)
	}
	if err != nil {
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
	// The command's PATH decides where exec.Command looks for it, so the
	// environment is replaced here rather than only in the child.
	if config.Env != nil {
		os.Clearenv()
		for _, variable := range config.Env {
			if name, value, ok := strings.Cut(variable, "="); ok {
				os.Setenv(name, value)
			}
		}
	}
	// Catch signals from here on, so runt stop and runt kill reach the
	// command instead of only init. SIGCHLD gets a channel of its own, so a
	// burst of other signals can't crowd it out; one pending notification
	// is enough, since each one reaps every child that has exited.
	children := make(chan os.Signal, 1)
	signal.Notify(children, syscall.SIGCHLD)
	signals := make(chan os.Signal, 16)
	signal.Notify(signals)

	child := exec.Command(config.Command[0], config.Command[1:]...)
	child.Dir = config.Cwd
	child.SysProcAttr = &syscall.SysProcAttr{}
	if tty != nil {
		// The pseudo-terminal becomes the command's controlling terminal,
		// in a session of its own, so Ctrl+C and job control work.
		child.Stdin, child.Stdout, child.Stderr = tty, tty, tty
		child.SysProcAttr.Setsid = true
		child.SysProcAttr.Setctty = true
		child.SysProcAttr.Ctty = 0
	} else {
		// Init has no controlling terminal (the supervisor started it in a
		// session of its own), so signals reach the command only through
		// init.
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		child.SysProcAttr.Setpgid = true
	}
	if config.User != nil {
		groups := make([]uint32, len(config.User.AdditionalGIDs))
		copy(groups, config.User.AdditionalGIDs)
		child.SysProcAttr.Credential = &syscall.Credential{Uid: config.User.UID, Gid: config.User.GID, Groups: groups}
	}
	if err := child.Start(); err != nil {
		err = &commandError{err}
		fmt.Fprintf(status, "error %v\n", err)
		return err
	}
	if tty != nil {
		tty.Close()
	}
	fmt.Fprintln(status, "started")
	status.Close()

	for {
		select {
		case <-children:
			if result, exited := reapChildren(child.Process.Pid); exited {
				if result.Exited() && result.ExitStatus() == 0 {
					return nil
				}
				return &commandExitError{result}
			}
		case sig := <-signals:
			// SIGCHLD is handled above, and the Go runtime sends itself
			// SIGURG to preempt goroutines.
			if sig == syscall.SIGCHLD || sig == syscall.SIGURG {
				continue
			}
			child.Process.Signal(sig)
		}
	}
}

// prepareCommand finishes working out how to run the command, now that init
// sees the container's own files: who to run it as, from config.UserSpec;
// HOME, if the environment is the container's own and doesn't set it; and
// the working directory, which is created if it doesn't exist, as Docker
// does.
func prepareCommand(config *initConfig) error {
	needHome := config.Env != nil && !hasEnv(config.Env, "HOME")
	if config.UserSpec != "" || needHome {
		passwd, err := readPasswd("/etc/passwd")
		if err != nil {
			return fmt.Errorf("read the container's /etc/passwd: %w", err)
		}
		var home string
		if config.UserSpec != "" {
			groups, err := readGroup("/etc/group")
			if err != nil {
				return fmt.Errorf("read the container's /etc/group: %w", err)
			}
			user, userHome, err := resolveUser(config.UserSpec, passwd, groups)
			if err != nil {
				return err
			}
			config.User, home = &user, userHome
		} else {
			uid := uint32(0)
			if config.User != nil {
				uid = config.User.UID
			}
			home = homeOf(uid, passwd)
		}
		if needHome {
			config.Env = append(config.Env, "HOME="+home)
		}
	}
	if config.Cwd != "" {
		if _, err := os.Stat(config.Cwd); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(config.Cwd, 0o755); err != nil {
				return fmt.Errorf("create working directory %s: %w", config.Cwd, err)
			}
		}
	}
	return nil
}

// reapChildren collects the exit status of every child of init that has
// exited. Besides the command, these are orphans: when a process exits, the
// kernel hands its children to PID 1 of the namespace, and each one stays a
// zombie, holding its PID, until init waits for it. It returns the command's
// status if the command was among them.
func reapChildren(command int) (status syscall.WaitStatus, exited bool) {
	for {
		var current syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &current, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		// 0 means children remain but none has exited; ECHILD means there
		// are none.
		if err != nil || pid <= 0 {
			return status, exited
		}
		if pid == command {
			status, exited = current, true
		}
	}
}

// setupContainer builds the container's view of the system and then locks
// init down, all before the command is started. With config.Tty it returns
// the slave end of the container's pseudo-terminal, having sent the master
// on consoleSocket.
func setupContainer(config initConfig, consoleSocket *os.File) (*os.File, error) {
	rootfs := config.Rootfs
	// This runs before /proc is replaced, so it still reads the host's.
	if os.Getenv(debugInitEnv) != "" {
		os.Unsetenv(debugInitEnv)
		if err := waitForDebugger(); err != nil {
			return nil, err
		}
	}
	hostname := config.Hostname
	if hostname == "" {
		hostname = "runt"
	}
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		return nil, fmt.Errorf("set hostname: %w", err)
	}
	// The new network namespace starts with only a loopback interface, and
	// it is down. On the bridge network, the supervisor has also moved in
	// eth0, its end of the container's veth pair.
	if err := bringUpLoopback(); err != nil {
		return nil, err
	}
	if config.Network != nil {
		if err := configureNetwork(config.Network); err != nil {
			return nil, err
		}
	}
	// Stop mount events from propagating back to the host. Without this, a
	// shared root mount (the systemd default) would leak our mounts out.
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return nil, fmt.Errorf("make mounts private: %w", err)
	}
	// pivot_root requires the new root to be a mount point, so bind-mount
	// rootfs onto itself.
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return nil, fmt.Errorf("bind mount rootfs %q: %w", rootfs, err)
	}
	// /proc, /sys, and /dev are set up before pivot_root: inside a user
	// namespace the kernel only allows mounting proc and sysfs while the
	// host's copies are still visible, and /dev borrows device files from
	// the host's /dev.
	if err := mountProc(rootfs); err != nil {
		return nil, err
	}
	if err := mountSys(rootfs); err != nil {
		return nil, err
	}
	if err := mountCgroup(rootfs); err != nil {
		return nil, err
	}
	if err := setupDev(rootfs); err != nil {
		return nil, err
	}
	if err := pivotRoot(rootfs); err != nil {
		return nil, err
	}
	if err := protectPaths(); err != nil {
		return nil, err
	}
	var tty *os.File
	if consoleSocket != nil {
		var err error
		if tty, err = setupConsole(consoleSocket); err != nil {
			return nil, err
		}
	}
	if config.ReadonlyRootfs {
		if err := remountRootReadonly(); err != nil {
			return nil, err
		}
	}

	// Everything above needed full privileges in the user namespace; the
	// command itself does not. The order of the steps below matters.
	if config.AppArmorProfile != "" {
		// The profile request is tied to this thread, so stay on it until
		// the command has been forked from it.
		goruntime.LockOSThread()
		if err := applyAppArmorProfile(config.AppArmorProfile); err != nil {
			return nil, err
		}
	}
	// no_new_privs comes before the seccomp filter, which an unprivileged
	// process may not install without it.
	if err := setNoNewPrivs(); err != nil {
		return nil, err
	}
	if err := dropCapabilities(); err != nil {
		return nil, err
	}
	// The filter goes last so it does not have to allow the calls above.
	if err := installSeccomp(); err != nil {
		return nil, err
	}

	return tty, nil
}

// setupConsole makes the container's pseudo-terminal, from its own /dev/ptmx,
// and sends the master end to the supervisor. The slave also appears as
// /dev/console, which is where programs write messages meant for the
// system's operator, as with runc.
func setupConsole(socket *os.File) (*os.File, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, err
	}
	defer master.Close()
	// A bind mount needs an existing file to mount over.
	if err := os.WriteFile("/dev/console", nil, 0o600); err != nil {
		slave.Close()
		return nil, fmt.Errorf("create /dev/console: %w", err)
	}
	if err := syscall.Mount(slave.Name(), "/dev/console", "", syscall.MS_BIND, ""); err != nil {
		slave.Close()
		return nil, fmt.Errorf("bind mount %s to /dev/console: %w", slave.Name(), err)
	}
	if err := sendFile(socket, master); err != nil {
		slave.Close()
		return nil, err
	}
	return slave, nil
}

// remountRootReadonly makes "/" read-only. A remount has to repeat the
// nosuid, nodev, and noexec flags the mount already has: the kernel locks
// them on mounts that came from a more privileged user namespace, and
// refuses a remount that would clear them.
func remountRootReadonly() error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return fmt.Errorf("read root mount flags: %w", err)
	}
	// ST_NOSUID, ST_NODEV, and ST_NOEXEC have the same values as the MS_
	// flags.
	locked := uintptr(stat.Flags) & (syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	if err := syscall.Mount("", "/", "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|locked, ""); err != nil {
		return fmt.Errorf("make root read-only: %w", err)
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
