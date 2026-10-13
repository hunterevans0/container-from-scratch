//go:build integration && linux

package integration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// output collects what a process writes, as it arrives, without ever
// blocking the test: if the process hangs, the test fails at a deadline.
type output struct {
	t   *testing.T
	mu  sync.Mutex
	buf bytes.Buffer
}

// collect starts collecting what is read from r, until it ends.
func collect(t *testing.T, r io.Reader) *output {
	o := &output{t: t}
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, err := r.Read(chunk)
			o.mu.Lock()
			o.buf.Write(chunk[:n])
			o.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// waitFor waits for text to appear in the output.
func (o *output) waitFor(text string) {
	o.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(o.String(), text); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			o.t.Fatalf("%q never appeared in the output:\n%s", text, o.String())
		}
	}
}

// terminal is a pseudo-terminal on the host for runt to run in, standing in
// for the terminal a person would type into. Its output is what runt shows.
type terminal struct {
	*output
	master *os.File
	slave  *os.File
}

// ioctl goes through SyscallConn, since Fd would switch the file to blocking
// mode under the goroutine reading it.
func ioctl(file *os.File, request uintptr, arg unsafe.Pointer) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg))
	})
	if errno != 0 {
		return errno
	}
	return nil
}

// newTerminal opens a pseudo-terminal of rows by cols and starts collecting
// what is written to it.
func newTerminal(t *testing.T, rows, cols uint16) *terminal {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	var unlock int32
	var number uint32
	if err := ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Fatal(err)
	}
	if err := ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		master.Close()
		slave.Close()
	})
	term := &terminal{output: collect(t, master), master: master, slave: slave}
	term.resize(rows, cols)
	return term
}

func (term *terminal) resize(rows, cols uint16) {
	term.t.Helper()
	size := [4]uint16{rows, cols, 0, 0}
	if err := ioctl(term.master, syscall.TIOCSWINSZ, unsafe.Pointer(&size)); err != nil {
		term.t.Fatal(err)
	}
}

// run starts runt with the terminal as its controlling terminal and its
// input and output, as a shell would.
func (term *terminal) run(binary string, args ...string) *exec.Cmd {
	term.t.Helper()
	command := exec.Command(binary, args...)
	command.Stdin, command.Stdout, command.Stderr = term.slave, term.slave, term.slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		term.t.Fatal(err)
	}
	return command
}

// exitCode waits for command and returns its exit code.
func exitCode(t *testing.T, command *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		return command.ProcessState.ExitCode()
	case <-time.After(20 * time.Second):
		command.Process.Kill()
		t.Fatal("runt did not exit")
		return 0
	}
}

// TestInitReapsOrphans checks that init, as PID 1 of the container, waits for
// processes whose parent has exited, so they don't stay behind as zombies.
func TestInitReapsOrphans(t *testing.T) {
	r := newRuntTester(t)
	// The inner shell exits straight away, so its sleep is handed to init,
	// and exits soon after.
	r.start("runt-reap", "/bin/sh", "-c", `sh -c "sleep 0.1 &"; exec sleep 3000`)
	time.Sleep(time.Second)
	// The second field of /proc/PID/stat is the command name in
	// parentheses, and the third is the state: Z for a zombie.
	output, err := r.exec("runt-reap", "cat /proc/[0-9]*/stat")
	if err != nil {
		t.Fatalf("exec: %v\n%s", err, output)
	}
	if !strings.Contains(output, "(sleep) S") {
		t.Errorf("the container's sleep 3000 isn't listed:\n%s", output)
	}
	if strings.Contains(output, ") Z ") {
		t.Errorf("the container has a zombie process:\n%s", output)
	}
}

// TestTerminal runs an interactive container (run -it) in a pseudo-terminal
// and checks that the command has a terminal of its own, sized like runt's
// and resized with it, and that Ctrl+C reaches it as SIGINT.
func TestTerminal(t *testing.T) {
	r := newRuntTester(t)
	r.removeAtEnd("runt-tty")
	term := newTerminal(t, 30, 100)
	script := `tty; stty size
trap "stty size" WINCH
trap "echo got-int; exit 3" INT
echo ready
while :; do sleep 0.2; done`
	command := term.run(r.binary, "run", "-it", "--name", "runt-tty", "--rootfs", r.rootfs, "--", "/bin/sh", "-c", script)

	term.waitFor("ready")
	shown := term.String()
	// The container's own devpts has a single terminal, so it is number 0.
	if !strings.Contains(shown, "/dev/pts/0") {
		t.Errorf("tty did not print /dev/pts/0:\n%s", shown)
	}
	if !strings.Contains(shown, "30 100") {
		t.Errorf("stty size did not print the terminal's size, 30 100:\n%s", shown)
	}

	term.resize(50, 150)
	term.waitFor("50 150")

	// Ctrl+C, typed into the terminal.
	if _, err := term.master.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, command); code != 3 {
		t.Errorf("runt exited with %d; want the command's 3\n%s", code, term.String())
	}
	if !strings.Contains(term.String(), "got-int") {
		t.Errorf("the command did not get SIGINT:\n%s", term.String())
	}
	// The whole session is in the log too.
	logs, err := r.runt("logs", "runt-tty")
	if err != nil || !strings.Contains(logs, "got-int") {
		t.Errorf("runt logs: %v\n%s", err, logs)
	}
}

// TestNotATerminal checks that -it refuses to run without a terminal to read
// from, as docker does.
func TestNotATerminal(t *testing.T) {
	r := newRuntTester(t)
	output, err := r.runt("run", "-it", "--rootfs", r.rootfs, "--", "/bin/true")
	if err == nil || !strings.Contains(output, "not a terminal") {
		t.Errorf("runt run -it without a terminal: %v\n%s", err, output)
	}
}

// TestDetachedTerminal checks that a background container (run -dt) gets a
// terminal too, with its output going to the log.
func TestDetachedTerminal(t *testing.T) {
	r := newRuntTester(t)
	r.startWith("runt-dt", []string{"-t", "--rootfs", r.rootfs}, "/bin/sh", "-c", "tty; stty size; exec sleep 3000")
	var logs string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if logs, _ = r.runt("logs", "runt-dt"); strings.Contains(logs, "24 80") {
			break
		}
	}
	if !strings.Contains(logs, "/dev/pts/0") || !strings.Contains(logs, "24 80") {
		t.Errorf("runt logs: want /dev/pts/0 and the default size 24 80:\n%s", logs)
	}
}

// TestExecTerminal checks that exec -it gives the command a terminal of its
// own inside the container, sized like runt's, and passes on its exit code.
func TestExecTerminal(t *testing.T) {
	r := newRuntTester(t)
	r.start("runt-exec-tty", "/bin/sleep", "3000")
	term := newTerminal(t, 20, 70)
	command := term.run(r.binary, "exec", "-it", "runt-exec-tty", "/bin/sh", "-c", "tty; stty size; exit 5")
	if code := exitCode(t, command); code != 5 {
		t.Errorf("runt exec exited with %d; want the command's 5\n%s", code, term.String())
	}
	shown := term.String()
	if !strings.Contains(shown, "/dev/pts/") || !strings.Contains(shown, "20 70") {
		t.Errorf("want a /dev/pts terminal of size 20 70:\n%s", shown)
	}
}

// TestExecSignals checks that runt exec passes signals it gets on to the
// command, which runs in a session of its own.
func TestExecSignals(t *testing.T) {
	r := newRuntTester(t)
	r.start("runt-exec-sig", "/bin/sleep", "3000")
	command := exec.Command(r.binary, "exec", "runt-exec-sig", "/bin/sh", "-c",
		`trap "echo got-term; exit 9" TERM; echo ready; while :; do sleep 0.2; done`)
	// A pipe of our own, rather than StdoutPipe, so that waiting for runt
	// doesn't also wait for the command to close its output.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()
	command.Stdout = stdoutW
	err = command.Start()
	stdoutW.Close()
	if err != nil {
		t.Fatal(err)
	}
	stdout := collect(t, stdoutR)
	stdout.waitFor("ready")
	command.Process.Signal(syscall.SIGTERM)
	if code := exitCode(t, command); code != 9 {
		t.Errorf("runt exec exited with %d; want the command's 9 (output %q)", code, stdout)
	}
	stdout.waitFor("got-term")
}
