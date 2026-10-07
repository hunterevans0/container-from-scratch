//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	goruntime "runtime"
	"strconv"
	"syscall"
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
func Exec(ref string, command []string) error {
	if len(command) == 0 {
		return errors.New("a command is required")
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

	args := []string{
		"--target", strconv.Itoa(state.Pid),
		// Every namespace: user, mount, PID, network, IPC, UTS, and cgroup.
		// Joining the user namespace makes the process root inside the
		// container, as the container's own command is.
		"--all",
		"--wdns=/",
		"--", "/proc/self/fd/3", "--exec-init", state.AppArmorProfile,
	}
	args = append(args, command...)
	nsenterCommand := exec.Command(nsenter, args...)
	nsenterCommand.Stdin = os.Stdin
	nsenterCommand.Stdout = os.Stdout
	nsenterCommand.Stderr = os.Stderr
	nsenterCommand.ExtraFiles = []*os.File{self}
	if state.Cgroup != "" {
		// Start nsenter, and so the command, in the container's cgroup, so
		// its limits apply. (nsenter's own --join-cgroup leaves stray file
		// descriptors behind in the command.)
		cgroup, err := os.Open(state.Cgroup)
		if err != nil {
			return fmt.Errorf("open cgroup of container %s: %w", state.ID, err)
		}
		defer cgroup.Close()
		nsenterCommand.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	}

	// The terminal sends Ctrl+C to the command as well as to runt, so runt
	// only has to stay alive to report the exit code. SIGTERM and SIGHUP come
	// to runt alone, so pass them on.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		signal.Stop(signals)
		close(signals)
	}()
	if err := nsenterCommand.Start(); err != nil {
		return err
	}
	go func() {
		for sig := range signals {
			if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
				nsenterCommand.Process.Signal(sig)
			}
		}
	}()
	return nsenterCommand.Wait()
}

// ExecInit runs inside the container, started by nsenter on behalf of Exec,
// as root of the container's user namespace. It applies the restrictions
// Init applies to the container's command, then replaces itself with the
// command.
func ExecInit() error {
	// Arguments, as built by Exec: --exec-init APPARMOR_PROFILE COMMAND...
	if len(os.Args) < 4 {
		return errors.New("internal exec arguments are incomplete")
	}
	apparmorProfile := os.Args[2]
	command := os.Args[3:]
	// Descriptor 3 is this binary; the command shouldn't inherit it.
	syscall.CloseOnExec(3)

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
	path, err := exec.LookPath(command[0])
	if err != nil {
		return &commandError{err}
	}
	if err := syscall.Exec(path, command, os.Environ()); err != nil {
		return &commandError{fmt.Errorf("exec %s: %w", command[0], err)}
	}
	return nil
}
