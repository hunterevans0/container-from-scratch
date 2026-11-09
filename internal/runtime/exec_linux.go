//go:build linux

package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// Exec runs command inside a running container, in all of its namespaces
// and its cgroup, with the same restrictions as the container's own command.
//
// Joining a namespace is the setns(2) system call. A Go program can't make
// it for a user or mount namespace: the kernel only allows that in a
// single-threaded process, and the Go runtime starts several threads before
// main runs. runc gets around this with C code that runs before the Go
// runtime starts. runt uses nsenter from util-linux instead, which makes the
// setns calls and then runs runt again, now inside the container, to drop
// privileges before the command runs (see ExecInit).
//
// The chain of processes, for runt exec web ls:
//
//	runt exec  ->  nsenter --target <init's PID> --all -- runt --exec-init ... ls
//	                  (joins the container's namespaces)
//	                  ->  runt --exec-init: drops privileges, then becomes ls
//
// For how to read the Go here, see doc.go.
func Exec(ref string, command []string, options ExecOptions) error {
	if len(command) == 0 {
		return errors.New("a command is required")
	}
	if options.Interactive && options.Tty && !isTerminal(os.Stdin) {
		return errors.New("the input device is not a terminal; use -t without -i, or run from a terminal")
	}
	state, err := findContainer(StateRoot, ref)
	if err != nil {
		return err
	}
	refresh(state)
	if state.Status != StatusRunning {
		return fmt.Errorf("container %s is %s; exec needs a running container", state.ID, state.Status)
	}
	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		return errors.New("exec needs nsenter, from the util-linux package")
	}
	// The runt binary isn't inside the container, so pass it in as an open
	// file. Once nsenter has joined the container's mount namespace,
	// /proc/self/fd/3 still leads to it.
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer self.Close()
	extraFiles := []*os.File{self}
	tty := ""
	consoleSocket := -1
	if options.Tty {
		// ExecInit makes the pseudo-terminal inside the container and sends
		// its master back on descriptor 4, as init does for the container's
		// own terminal.
		tty = "tty"
		// The Unix socket the pseudo-terminal's master is sent over. Our end
		// stays a raw descriptor for recvmsg; the child end is passed to the
		// process that makes the pseudo-terminal. socketpair makes two sockets
		// already connected to each other, like the two ends of a pipe that works
		// both ways.
		var fds [2]int
		if fds, err = syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0); err != nil {
			return fmt.Errorf("create console socket: %w", err)
		}
		consoleSocket = fds[0]
		child := os.NewFile(uintptr(fds[1]), "console socket")
		defer syscall.Close(consoleSocket)
		defer child.Close()
		extraFiles = append(extraFiles, child)
	}

	args := []string{
		"--target", strconv.Itoa(state.Pid),
		// Every namespace: user, mount, PID, network, IPC, UTS, and cgroup.
		// Joining the user namespace makes the process root inside the
		// container, as the container's own command is.
		"--all",
		"--wdns=/",
		"--", "/proc/self/fd/3", "--exec-init", state.AppArmorProfile, tty,
	}
	args = append(args, command...)
	// exec.Command prepares a program to run, with its arguments. Its
	// fields set up the new process before Start: Stdin, Stdout, and
	// Stderr are what it reads and writes, ExtraFiles are more open files
	// passed on as descriptors 3, 4, and so on, and SysProcAttr holds
	// Linux-specific settings for how it is made.
	nsenterCommand := exec.Command(nsenter, args...)
	if options.Interactive && !options.Tty {
		nsenterCommand.Stdin = os.Stdin
	}
	nsenterCommand.Stdout = os.Stdout
	nsenterCommand.Stderr = os.Stderr
	nsenterCommand.ExtraFiles = extraFiles
	// A session of its own, with no controlling terminal, so the command
	// can't reach runt's terminal through /dev/tty (see supervise).
	nsenterCommand.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if state.Cgroup != "" {
		// Start nsenter, and so the command, in the container's cgroup, so
		// its limits apply. (nsenter's own --join-cgroup leaves stray file
		// descriptors behind in the command.)
		cgroup, err := os.Open(state.Cgroup)
		if err != nil {
			return fmt.Errorf("open cgroup of container %s: %w", state.ID, err)
		}
		defer cgroup.Close()
		nsenterCommand.SysProcAttr.UseCgroupFD = true
		nsenterCommand.SysProcAttr.CgroupFD = int(cgroup.Fd())
	}

	// In its own session the command doesn't get the terminal's signals,
	// such as Ctrl+C, so runt passes them on. They go to the command itself:
	// nsenter would only die of them, since it doesn't pass them on.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		signal.Stop(signals)
		close(signals)
	}()
	if err := nsenterCommand.Start(); err != nil {
		return err
	}
	if options.Tty {
		extraFiles[1].Close()
	}
	// Each signal that arrives on the channel is sent on to the command.
	// sig is an os.Signal, an interface; sig.(syscall.Signal) takes out the
	// signal number it holds, which syscall.Kill wants.
	//
	// The command is found as the child of nsenter: nsenter has a single
	// child, which ExecInit replaces with the command. pid is its PID, or 0
	// if there is none. Looking through /proc/<pid>/stat for every process
	// gives each one's parent. Sscanf reads values out of a string as a
	// format says, as C's scanf does: %c a character, %d a number.
	go func() {
		for sig := range signals {
			parent := nsenterCommand.Process.Pid
			pid := 0
			stats, _ := filepath.Glob("/proc/[0-9]*/stat")
			for _, path := range stats {
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				// The fields after the command name, which is in parentheses
				// and may contain anything, start with the state and then the
				// parent.
				end := bytes.LastIndexByte(data, ')')
				if end < 0 {
					continue
				}
				var processState byte
				var ppid int
				if _, err := fmt.Sscanf(string(data[end+1:]), " %c %d", &processState, &ppid); err == nil && ppid == parent {
					pid, _ = strconv.Atoi(filepath.Base(filepath.Dir(path)))
					break
				}
			}
			if pid > 0 {
				syscall.Kill(pid, sig.(syscall.Signal))
			}
		}
	}()

	// output is closed once all of the command's terminal output has been
	// copied. A chan struct{} carries no values; closing it is the signal,
	// and <-output waits for it.
	var output chan struct{}
	var console *os.File
	if options.Tty {
		// If ExecInit fails before making the terminal, it closes the
		// socket, and nsenter's exit status says what happened.
		if console, err = receiveFile(consoleSocket, "console"); err == nil {
			defer console.Close()
			// Connect runt's own terminal to the container's pseudo-terminal:
			// give the pseudo-terminal the same size and keep it in step when the
			// window is resized, and with options.Interactive put the terminal in raw mode and
			// send what is typed to the container. Undoing this is deferred.
			//
			// The terminal to copy the size from: runt's input, or else its output,
			// whichever is a terminal.
			var source *os.File
			for _, file := range []*os.File{os.Stdin, os.Stdout} {
				if isTerminal(file) {
					source = file
					break
				}
			}
			resize := func() {
				size := defaultWinsize
				if source != nil {
					// The terminal's size, in rows and columns.
					var current winsize
					if err := ioctl(source, syscall.TIOCGWINSZ, unsafe.Pointer(&current)); err == nil && current.rows > 0 && current.cols > 0 {
						size = current
					}
				}
				setWinsize(console, size)
			}
			resize()
			// The kernel sends SIGWINCH when runt's terminal window is resized.
			// signal.Notify delivers it on the channel winch instead of applying
			// its default action, and the goroutine resizes the container's
			// pseudo-terminal each time one comes. "for range winch" loops over
			// values received from the channel, until it is closed.
			winch := make(chan os.Signal, 1)
			signal.Notify(winch, syscall.SIGWINCH)
			go func() {
				for range winch {
					resize()
				}
			}()
			stopResizing := func() {
				signal.Stop(winch)
				close(winch)
			}
			if !options.Interactive {
				defer stopResizing()
			} else if restore, err := makeRaw(os.Stdin); err != nil {
				stopResizing()
				warn(err)
			} else {
				// This copy ends at the first read after the container is gone, or
				// when runt exits.
				go io.Copy(console, os.Stdin)
				defer func() {
					stopResizing()
					restore()
				}()
			}
			output = make(chan struct{})
			go func() {
				io.Copy(os.Stdout, consoleReader{console})
				close(output)
			}()
			// The terminal has its size now; let the command start.
			syscall.Write(consoleSocket, []byte{0})
		}
	}
	err = nsenterCommand.Wait()
	if output != nil {
		// The command's last output is already waiting in the terminal.
		// Reading ends when every process using the terminal is gone, but
		// one could have been left running in the background; don't wait
		// long for it.
		console.SetReadDeadline(time.Now().Add(time.Second))
		<-output
	}
	return err
}

// ExecInit runs inside the container, started by nsenter on behalf of Exec,
// as root of the container's user namespace. It applies the restrictions
// Init applies to the container's command, then replaces itself with the
// command.
func ExecInit() error {
	// Arguments, as built by Exec: --exec-init APPARMOR_PROFILE TTY COMMAND...
	// os.Args holds the program's own name first, and then its arguments,
	// so os.Args[4:] is the command and its arguments.
	if len(os.Args) < 5 {
		return errors.New("internal exec arguments are incomplete")
	}
	apparmorProfile := os.Args[2]
	tty := os.Args[3] == "tty"
	command := os.Args[4:]
	// Descriptor 3 is this binary; the command shouldn't inherit it.
	syscall.CloseOnExec(3)
	// Look the command up first, so an error about it still goes to runt
	// exec's own stderr rather than the terminal.
	path, err := exec.LookPath(command[0])
	if err != nil {
		return &commandError{err}
	}
	if tty {
		syscall.CloseOnExec(4)
		// Make a pseudo-terminal in the container, send its master to runt
		// exec over socket, and make the slave this process's controlling
		// terminal and its standard input, output, and error.
		socket := os.NewFile(4, "console socket")
		// Make a new pseudo-terminal pair from /dev/ptmx. Neither end becomes
		// this process's controlling terminal.
		//
		// Opening /dev/ptmx makes a new pair and returns its master end; the slave
		// end appears as /dev/pts/N, which is then opened by name.
		masterFD, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open /dev/ptmx: %w", err)
		}
		master := os.NewFile(uintptr(masterFD), "/dev/ptmx")
		// A new slave is locked until the master unlocks it (unlockpt), and its
		// number says which /dev/pts entry it is (ptsname).
		var unlock int32
		if err := ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
			master.Close()
			return fmt.Errorf("unlock pseudo-terminal: %w", err)
		}
		var number uint32
		if err := ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
			master.Close()
			return fmt.Errorf("get pseudo-terminal number: %w", err)
		}
		ptsPath := fmt.Sprintf("/dev/pts/%d", number)
		slaveFD, err := syscall.Open(ptsPath, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
		if err != nil {
			master.Close()
			return fmt.Errorf("open %s: %w", ptsPath, err)
		}
		slave := os.NewFile(uintptr(slaveFD), ptsPath)
		err = sendFile(socket, master)
		master.Close()
		if err != nil {
			slave.Close()
			return err
		}
		// runt exec sends a byte once it has sized the terminal.
		if _, err := socket.Read(make([]byte, 1)); err != nil {
			slave.Close()
			return fmt.Errorf("wait for terminal: %w", err)
		}
		socket.Close()

		// A controlling terminal belongs to a session, so start one. This
		// process isn't a process group leader (nsenter is), so it may.
		if _, err := syscall.Setsid(); err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		// dup3 copies a descriptor onto a given number: here the terminal
		// onto 0, 1, and 2, standard input, output, and error.
		for fd := 0; fd <= 2; fd++ {
			if err := syscall.Dup3(int(slave.Fd()), fd, 0); err != nil {
				return fmt.Errorf("use terminal: %w", err)
			}
		}
		slave.Close()
		if err := ioctl(os.Stdin, syscall.TIOCSCTTY, unsafe.Pointer(nil)); err != nil {
			return fmt.Errorf("set controlling terminal: %w", err)
		}
	}

	if apparmorProfile != "" {
		// The profile request belongs to this thread, which must also be
		// the one that calls exec.
		goruntime.LockOSThread()
		if err := applyAppArmorProfile(apparmorProfile); err != nil {
			return err
		}
	}
	// no_new_privs makes exec stop granting privileges: setuid and setgid
	// bits and file capabilities are ignored from here on, in this process
	// and all its descendants. It is per thread, so it goes through
	// AllThreadsSyscall like the capability calls. AllThreadsSyscall6 is
	// the form for system calls that take up to six arguments.
	if _, _, errno := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set no_new_privs: %w", errno)
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
	if err := installSeccomp(); err != nil {
		return err
	}
	// Make sure the command inherits only stdin, stdout, and stderr, as runc
	// does. Go opens its own files close-on-exec, but runt can inherit open
	// descriptors from whatever started it (WSL leaves some terminal
	// descriptors open, for example), and those would otherwise pass into
	// the container.
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("list open files: %w", err)
	}
	// /proc/self/fd has an entry per open descriptor, named by its number.
	// Marking one close-on-exec makes the kernel close it when the process
	// runs another program, so the command never sees it.
	for _, entry := range entries {
		if fd, err := strconv.Atoi(entry.Name()); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
	// syscall.Exec replaces this process with the command: the same
	// process, with the same PID, namespaces, and restrictions, but running
	// the command's program. On success it never returns.
	if err := syscall.Exec(path, command, os.Environ()); err != nil {
		return &commandError{fmt.Errorf("exec %s: %w", command[0], err)}
	}
	return nil
}
