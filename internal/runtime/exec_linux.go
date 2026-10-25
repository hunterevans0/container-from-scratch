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
		var child *os.File
		if consoleSocket, child, err = consoleSocketPair(); err != nil {
			return err
		}
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
	go func() {
		for sig := range signals {
			if pid := childOf(nsenterCommand.Process.Pid); pid > 0 {
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
			disconnect, err := connectTerminal(console, options.Interactive)
			if err != nil {
				warn(err)
			} else {
				defer disconnect()
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

// childOf returns the PID of a child of process parent, or 0 if it has none.
// nsenter has a single child, which ExecInit replaces with the command.
// It looks through /proc/<pid>/stat for every process, which gives each
// one's parent. Sscanf reads values out of a string as a format says, as C's
// scanf does: %c a character, %d a number.
func childOf(parent int) int {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, path := range stats {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// The fields after the command name, which is in parentheses and
		// may contain anything, start with the state and then the parent.
		end := bytes.LastIndexByte(data, ')')
		if end < 0 {
			continue
		}
		var state byte
		var ppid int
		if _, err := fmt.Sscanf(string(data[end+1:]), " %c %d", &state, &ppid); err == nil && ppid == parent {
			pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(path)))
			return pid
		}
	}
	return 0
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
		if err := useNewTerminal(os.NewFile(4, "console socket")); err != nil {
			return err
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
	if err := setNoNewPrivs(); err != nil {
		return err
	}
	if err := dropCapabilities(); err != nil {
		return err
	}
	if err := installSeccomp(); err != nil {
		return err
	}
	if err := closeExtraFiles(); err != nil {
		return err
	}
	// syscall.Exec replaces this process with the command: the same
	// process, with the same PID, namespaces, and restrictions, but running
	// the command's program. On success it never returns.
	if err := syscall.Exec(path, command, os.Environ()); err != nil {
		return &commandError{fmt.Errorf("exec %s: %w", command[0], err)}
	}
	return nil
}

// useNewTerminal makes a pseudo-terminal in the container, sends its master
// to runt exec over socket, and makes the slave this process's controlling
// terminal and its standard input, output, and error.
func useNewTerminal(socket *os.File) error {
	master, slave, err := openPTY()
	if err != nil {
		return err
	}
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
	// dup3 copies a descriptor onto a given number: here the terminal onto
	// 0, 1, and 2, standard input, output, and error.
	for fd := 0; fd <= 2; fd++ {
		if err := syscall.Dup3(int(slave.Fd()), fd, 0); err != nil {
			return fmt.Errorf("use terminal: %w", err)
		}
	}
	slave.Close()
	if err := ioctl(os.Stdin, syscall.TIOCSCTTY, unsafe.Pointer(nil)); err != nil {
		return fmt.Errorf("set controlling terminal: %w", err)
	}
	return nil
}
