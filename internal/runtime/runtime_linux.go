//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func Run(config Config) error {
	if len(config.Command) == 0 {
		return fmt.Errorf("command cannot be empty")
	}

	args := []string{"--init", config.Rootfs}
	args = append(args, config.Command...)
	command := exec.Command("/proc/self/exe", args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	}
	return command.Run()
}

func Init() error {
	if len(os.Args) < 4 {
		return fmt.Errorf("internal init arguments are incomplete")
	}

	rootfs := os.Args[2]
	command := os.Args[3:]
	if err := syscall.Sethostname([]byte("runt")); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := syscall.Chroot(rootfs); err != nil {
		return fmt.Errorf("chroot %q: %w", rootfs, err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("change directory: %w", err)
	}

	child := exec.Command(command[0], command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	return child.Run()
}
