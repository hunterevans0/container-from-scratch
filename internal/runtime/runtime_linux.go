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

// The container's side: Init, the container's first process (PID 1 inside
// it), which builds the container's view of the system from within, and
// then runs the command and stays with it. It is runt itself, run again
// with --init by the supervisor (see supervise in container_linux.go), in
// new namespaces. Also here: turning errors into exit codes for main, and
// the steps of setupContainer: hostname, network, mounts, /proc, /sys,
// /dev, pivot_root, and the security restrictions.
//
// A namespace gives the processes in it their own copy of one part of the
// system: a mount namespace its own set of mounts, a PID namespace its own
// process numbers (starting from 1), a network namespace its own
// interfaces, and so on. Init starts out in new, empty namespaces and fills
// them in.
//
// For how to read the Go here, see doc.go.

// ExitCode returns the exit code to pass on when err is a process exiting
// unsuccessfully: the container's command for Init, init for Run, or the
// container for Attach.
//
// errors.As looks through err (and any errors it wraps) for one of the
// given type, and if it finds one, stores it in the variable it is given a
// pointer to. So each "if errors.As(...)" asks "is this, deep down, a ...?"
func ExitCode(err error) (int, bool) {
	var status statusError
	if errors.As(err, &status) {
		return int(status), true
	}
	// The exit code is the process's own, or 128 plus the signal number if
	// a signal killed it, as shells report it.
	var commandExit *commandExitError
	if errors.As(err, &commandExit) {
		if commandExit.status.Signaled() {
			return 128 + int(commandExit.status.Signal()), true
		}
		return commandExit.status.ExitStatus(), true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	// Sys returns the operating system's own form of the exit status, which
	// on Linux is a syscall.WaitStatus; the ", ok" type assertion checks
	// that.
	if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			return 128 + int(status.Signal()), true
		}
		return status.ExitStatus(), true
	}
	return exitErr.ProcessState.ExitCode(), true
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

// debugInitEnv tells the init process to wait for a debugger. It is set only
// on the init process and removed before the command runs.
const debugInitEnv = "RUNT_DEBUG_INIT"

// initConfig is what init needs to know about the container. The supervisor
// sends it as JSON on file descriptor 5.
type initConfig struct {
	// Rootfs is the directory to make the container's root: the overlay
	// mount, or the rootfs itself.
	Rootfs         string `json:"rootfs"`
	ReadonlyRootfs bool   `json:"readonlyRootfs,omitempty"`
	// DirectRootfs is set if Rootfs is the rootfs itself, which mustn't be
	// changed, rather than an overlay with a writable layer.
	DirectRootfs    bool     `json:"directRootfs,omitempty"`
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
	// EtcFiles are files to mount over the container's own, by path and
	// content: /etc/hosts and /etc/resolv.conf (see mountEtcFiles).
	EtcFiles map[string]string `json:"etcFiles,omitempty"`
	// Mounts are the bind and tmpfs mounts to make, in order, each bind
	// mount from a staging directory the supervisor made (see
	// mounts_linux.go).
	Mounts []Mount `json:"mounts,omitempty"`
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
//
// A file descriptor is the number a process knows an open file (or pipe,
// or socket) by. 0, 1, and 2 are standard input, output, and error; the
// supervisor's ExtraFiles become 3 and up. os.NewFile wraps a descriptor
// number in an *os.File, to read and write it like any other file.
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
	// Reading the FIFO waits until a byte is written to it: by the
	// supervisor straight away for runt run, or by runt start later.
	if _, err := start.Read(make([]byte, 1)); err != nil {
		return fmt.Errorf("wait for start: %w", err)
	}
	start.Close()

	// Make sure the command inherits only stdin, stdout, and stderr, as runc
	// does. Go opens its own files close-on-exec, but runt can inherit open
	// descriptors from whatever started it (WSL leaves some terminal
	// descriptors open, for example), and those would otherwise pass into
	// the container.
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		err = fmt.Errorf("list open files: %w", err)
		fmt.Fprintf(status, "error %v\n", err)
		return err
	}
	// /proc/self/fd has an entry per open descriptor, named by its number.
	// Marking one close-on-exec makes the kernel close it when the process
	// runs another program, so the command never sees it.
	for _, entry := range entries {
		if fd, err := strconv.Atoi(entry.Name()); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
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

	// From here until the command exits, init's only job is to be PID 1:
	// wait for signals, and either reap exited children (SIGCHLD) or pass
	// the signal on to the command. select waits on both channels at once,
	// and runs the case for whichever has a value first.
	for {
		select {
		case <-children:
			// Collect the exit status of every child of init that has
			// exited. Besides the command, these are orphans: when a process
			// exits, the kernel hands its children to PID 1 of the
			// namespace, and each one stays a zombie, holding its PID, until
			// init waits for it. result is the command's status if the
			// command was among them.
			var result syscall.WaitStatus
			exited := false
			for {
				// wait4(-1, ..., WNOHANG): collect any one child that has
				// exited, without waiting if none has.
				var current syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &current, syscall.WNOHANG, nil)
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				// 0 means children remain but none has exited; ECHILD means
				// there are none.
				if err != nil || pid <= 0 {
					break
				}
				if pid == child.Process.Pid {
					result, exited = current, true
				}
			}
			if exited {
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

// setupContainer builds the container's view of the system and then locks
// init down, all before the command is started. With config.Tty it returns
// the slave end of the container's pseudo-terminal, having sent the master
// on consoleSocket.
//
// syscall.Mount(source, target, fstype, flags, data) is the mount system
// call: mount the filesystem source, of type fstype, at target, with flags
// (MS_ constants combined with |) and filesystem-specific options in data.
// A bind mount (MS_BIND) makes an existing file or directory appear at a
// second place.
func setupContainer(config initConfig, consoleSocket *os.File) (*os.File, error) {
	rootfs := config.Rootfs
	// This runs before /proc is replaced, so it still reads the host's.
	if os.Getenv(debugInitEnv) != "" {
		os.Unsetenv(debugInitEnv)
		// Block until a tracer such as Delve attaches to this process.
		// /proc/self/status has a TracerPid line, which is 0 until a
		// debugger attaches.
	waitForDebugger:
		for {
			status, err := os.ReadFile("/proc/self/status")
			if err != nil {
				return nil, fmt.Errorf("read process status: %w", err)
			}
			for _, line := range strings.Split(string(status), "\n") {
				if pid, ok := strings.CutPrefix(line, "TracerPid:"); ok && strings.TrimSpace(pid) != "0" {
					break waitForDebugger
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	hostname := config.Hostname
	if hostname == "" {
		hostname = defaultHostname
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
	//
	// A fresh /proc, so tools like ps and top only see the processes in
	// this container's PID namespace.
	proc := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(proc, 0o555); err != nil {
		return nil, fmt.Errorf("create %q: %w", proc, err)
	}
	procFlags := uintptr(syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV)
	if err := syscall.Mount("proc", proc, "proc", procFlags, ""); err != nil {
		return nil, fmt.Errorf("mount /proc: %w", err)
	}
	// A read-only /sys. sysfs shows the network devices of the mounting
	// process's network namespace, so the container sees only its own
	// interfaces, and read-only keeps it from changing kernel settings.
	sys := filepath.Join(rootfs, "sys")
	if err := os.MkdirAll(sys, 0o555); err != nil {
		return nil, fmt.Errorf("create %q: %w", sys, err)
	}
	sysFlags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV)
	if err := syscall.Mount("sysfs", sys, "sysfs", sysFlags, ""); err != nil {
		return nil, fmt.Errorf("mount /sys: %w", err)
	}
	// The container's cgroup, mounted read-only at /sys/fs/cgroup, so
	// programs inside can read their limits and usage. Because init is in
	// its own cgroup namespace, the mount's root is the container's cgroup
	// and the host's cgroups aren't visible. Read-only stops the container
	// from raising its own limits. Only if the unified cgroup v2 hierarchy
	// is mounted: statfs describes the filesystem a path is on, including
	// a number saying what kind of filesystem it is.
	var cgroupStat syscall.Statfs_t
	if syscall.Statfs(cgroupRoot, &cgroupStat) == nil && cgroupStat.Type == cgroup2SuperMagic {
		cgroupTarget := filepath.Join(rootfs, "sys/fs/cgroup")
		cgroupFlags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV)
		if err := syscall.Mount("cgroup2", cgroupTarget, "cgroup2", cgroupFlags, ""); err != nil {
			return nil, fmt.Errorf("mount /sys/fs/cgroup: %w", err)
		}
	}
	// Replace the rootfs's /dev with a minimal one on a tmpfs, so the
	// container sees only a few safe devices instead of the host's.
	dev := filepath.Join(rootfs, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return nil, fmt.Errorf("create %q: %w", dev, err)
	}
	if err := syscall.Mount("tmpfs", dev, "tmpfs", syscall.MS_NOSUID|syscall.MS_STRICTATIME, "mode=755,size=65536k"); err != nil {
		return nil, fmt.Errorf("mount /dev: %w", err)
	}

	for _, name := range hostDevices {
		target := filepath.Join(dev, name)
		// A bind mount needs an existing file to mount over.
		if err := os.WriteFile(target, nil, 0o666); err != nil {
			return nil, fmt.Errorf("create %q: %w", target, err)
		}
		if err := syscall.Mount(filepath.Join("/dev", name), target, "", syscall.MS_BIND, ""); err != nil {
			return nil, fmt.Errorf("bind mount /dev/%s: %w", name, err)
		}
	}
	for name, dest := range devSymlinks {
		if err := os.Symlink(dest, filepath.Join(dev, name)); err != nil {
			return nil, fmt.Errorf("create /dev/%s symlink: %w", name, err)
		}
	}

	// A private devpts instance gives the container its own pseudo-terminals.
	pts := filepath.Join(dev, "pts")
	if err := os.Mkdir(pts, 0o755); err != nil {
		return nil, fmt.Errorf("create %q: %w", pts, err)
	}
	if err := syscall.Mount("devpts", pts, "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil {
		return nil, fmt.Errorf("mount /dev/pts: %w", err)
	}

	shm := filepath.Join(dev, "shm")
	if err := os.Mkdir(shm, 0o1777); err != nil {
		return nil, fmt.Errorf("create %q: %w", shm, err)
	}
	if err := syscall.Mount("shm", shm, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=1777,size=65536k"); err != nil {
		return nil, fmt.Errorf("mount /dev/shm: %w", err)
	}

	// mqueue shows the POSIX message queues of this IPC namespace.
	mqueue := filepath.Join(dev, "mqueue")
	if err := os.Mkdir(mqueue, 0o755); err != nil {
		return nil, fmt.Errorf("create %q: %w", mqueue, err)
	}
	if err := syscall.Mount("mqueue", mqueue, "mqueue", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return nil, fmt.Errorf("mount /dev/mqueue: %w", err)
	}
	// -v and --tmpfs mounts, after /dev so that one can go inside it, and
	// before pivot_root, while the supervisor's staging directories are
	// still in sight. Each of config.Mounts, as the supervisor staged them,
	// is made at its destination in rootfs, in order. A destination that
	// doesn't exist is made, in the container's writable layer, unless
	// config.DirectRootfs is set: then the root is a bundle's root
	// filesystem, which runt doesn't change.
	if len(config.Mounts) > 0 {
		// The root as it really is, symbolic links and all followed, for
		// makeMount to check paths against.
		root, err := filepath.EvalSymlinks(rootfs)
		if err != nil {
			return nil, err
		}
		for _, mount := range config.Mounts {
			if err := makeMount(root, mount, config.DirectRootfs); err != nil {
				return nil, fmt.Errorf("mount %s: %w", mount.Destination, err)
			}
		}
	}
	// pivot_root makes rootfs the new "/" and detaches the host's root
	// filesystem, so unlike chroot there is no old root left to escape back
	// into.
	if err := os.Chdir(rootfs); err != nil {
		return nil, fmt.Errorf("change directory to %q: %w", rootfs, err)
	}
	// pivot_root(".", ".") stacks the old root on top of the new one, so no
	// directory is needed to hold it (see pivot_root(2)).
	if err := syscall.PivotRoot(".", "."); err != nil {
		return nil, fmt.Errorf("pivot_root %q: %w", rootfs, err)
	}
	// Detach the old root so the host filesystem is no longer reachable.
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return nil, fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return nil, fmt.Errorf("change directory: %w", err)
	}
	// Apply maskedPaths and readonlyPaths (see security_linux.go). This runs
	// after pivot_root, and the container cannot undo it: unmounting needs
	// CAP_SYS_ADMIN, which is dropped before the command starts.
	//
	// A masked file is covered with /dev/null and a masked directory with
	// an empty read-only tmpfs. Paths this kernel does not have are
	// skipped. Mounting something over a path hides whatever was there, for
	// every process in the container's mount namespace: reading the file
	// reads /dev/null (nothing), and listing the directory lists the empty
	// tmpfs.
	for _, path := range maskedPaths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("mask %s: %w", path, err)
		}
		if info.IsDir() {
			err = syscall.Mount("tmpfs", path, "tmpfs", syscall.MS_RDONLY, "")
		} else {
			err = syscall.Mount("/dev/null", path, "", syscall.MS_BIND, "")
		}
		if err != nil {
			return nil, fmt.Errorf("mask %s: %w", path, err)
		}
	}
	// A read-only path is bind-mounted onto itself, which gives it a mount
	// of its own, and then that is remounted read-only. A bind mount
	// ignores MS_RDONLY the first time, hence the two steps.
	for _, path := range readonlyPaths {
		// MS_REC makes the bind mount take any mounts under path along too.
		err := syscall.Mount(path, path, "", syscall.MS_BIND|syscall.MS_REC, "")
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("make %s read-only: %w", path, err)
		}
		// The flags are bits, combined with |. uintptr(...) converts them to
		// the type Mount takes. nosuid, nodev, and noexec are repeated
		// because the kernel refuses a remount that would drop them.
		flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
		if err := syscall.Mount("", path, "", flags, ""); err != nil {
			return nil, fmt.Errorf("make %s read-only: %w", path, err)
		}
	}
	// Only now that the container's root is "/", so that symlinks among
	// the container's files can't lead to the host's.
	if err := mountEtcFiles(config.EtcFiles, config.DirectRootfs); err != nil {
		return nil, err
	}
	var tty *os.File
	if consoleSocket != nil {
		var err error
		if tty, err = setupConsole(consoleSocket); err != nil {
			return nil, err
		}
	}
	// Only the root itself: mounts on top of it, such as volumes, stay
	// writable, as with Docker.
	if config.ReadonlyRootfs {
		// A remount sets every flag at once, so it has to repeat the
		// nosuid, nodev, and noexec flags the mount already has, or it
		// would take them away. In a user namespace, the kernel also locks
		// them on mounts that came from a more privileged one, and refuses
		// a remount that would clear them.
		var stat syscall.Statfs_t
		if err := syscall.Statfs("/", &stat); err != nil {
			return nil, fmt.Errorf("make root read-only: %w", fmt.Errorf("read mount flags of %s: %w", "/", err))
		}
		// ST_NOSUID, ST_NODEV, and ST_NOEXEC have the same values as the MS_
		// flags.
		kept := uintptr(stat.Flags) & (syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
		if err := syscall.Mount("", "/", "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|kept, ""); err != nil {
			return nil, fmt.Errorf("make root read-only: %w", err)
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
	// process may not install without it. It makes exec stop granting
	// privileges: setuid and setgid bits and file capabilities are ignored
	// from here on, in this process and all its descendants. It is per
	// thread, so it goes through AllThreadsSyscall like the capability
	// calls. AllThreadsSyscall6 is the form for system calls that take up
	// to six arguments.
	if _, _, errno := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return nil, fmt.Errorf("set no_new_privs: %w", errno)
	}
	// Reduce this process to defaultCapabilities. Capabilities are per
	// thread, so every call goes through AllThreadsSyscall to cover all of
	// the Go runtime's threads; that needs a binary built without cgo.
	//
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
			return nil, fmt.Errorf("drop capability %d from bounding set: %w", capability, errno)
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
		return nil, fmt.Errorf("set capabilities: %w", errno)
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
	// Make a new pseudo-terminal pair from /dev/ptmx. Neither end becomes
	// this process's controlling terminal.
	//
	// Opening /dev/ptmx makes a new pair and returns its master end; the slave
	// end appears as /dev/pts/N, which is then opened by name.
	masterFD, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master := os.NewFile(uintptr(masterFD), "/dev/ptmx")
	// A new slave is locked until the master unlocks it (unlockpt), and its
	// number says which /dev/pts entry it is (ptsname).
	var unlock int32
	if err := ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlock pseudo-terminal: %w", err)
	}
	var number uint32
	if err := ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		master.Close()
		return nil, fmt.Errorf("get pseudo-terminal number: %w", err)
	}
	ptsPath := fmt.Sprintf("/dev/pts/%d", number)
	slaveFD, err := syscall.Open(ptsPath, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open %s: %w", ptsPath, err)
	}
	slave := os.NewFile(uintptr(slaveFD), ptsPath)
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

// hostDevices are bind-mounted from the host's /dev. A user namespace can't
// create device nodes with mknod, so borrowing the host's is the standard
// approach (rootless runc and Podman do the same).
var hostDevices = []string{"null", "zero", "full", "random", "urandom", "tty"}

// devSymlinks are the links every /dev has: name, and where it points.
// /proc/self/fd/N is the calling process's own descriptor N, so /dev/stdout
// leads to whatever the process's standard output is.
var devSymlinks = map[string]string{
	"fd":     "/proc/self/fd",
	"stdin":  "/proc/self/fd/0",
	"stdout": "/proc/self/fd/1",
	"stderr": "/proc/self/fd/2",
	"ptmx":   "pts/ptmx",
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
	// whose first field is the flags. The struct type is declared right in
	// the var statement, since nothing else uses it; _ names padding that
	// is never read.
	var ifr struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	// Read the interface's flags (SIOCGIFFLAGS), set IFF_UP, and write them
	// back (SIOCSIFFLAGS).
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
