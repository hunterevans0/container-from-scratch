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

// Lifecycle tests: init's job as PID 1, terminals (run -it, run -dt, exec
// -it), signals, attach, start -ai, and /dev/stdout. The terminal tests run
// runt inside a pseudo-terminal the test makes, and type into it, as a
// person would. For how to read these tests, see the top of
// isolation_test.go.

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

// String is what has been collected so far. The mutex keeps it from
// reading the buffer while the collecting goroutine writes to it.
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
// Writing to master is typing; runt's side is slave.
//
// *output, a field with no name, is embedded: a terminal has all of
// output's fields and methods as its own, so term.waitFor and term.String
// work directly.
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

// resize sets the terminal's size, as resizing its window would. The four
// numbers are struct winsize: rows, columns, and two pixel sizes, unused.
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

// exitCode waits for command and returns its exit code. Waiting happens in
// a goroutine, which sends Wait's result on done; the select takes whichever
// comes first, that result or the 20-second timeout, so a runt that hangs
// fails the test rather than hanging it too.
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
	// The container's shell script: print its terminal and size, print the
	// size again on each resize (SIGWINCH), and exit with 3 on Ctrl+C
	// (SIGINT).
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

// TestAttach starts a shell in the background with a terminal and its input
// open (run -dit), attaches to it from a terminal, types into it, detaches
// with Ctrl+P, Ctrl+Q, and attaches again to make it exit.
func TestAttach(t *testing.T) {
	r := newRuntTester(t)
	r.startWith("runt-attach", []string{"-it", "--rootfs", r.rootfs}, "/bin/sh")

	term := newTerminal(t, 25, 90)
	attach := term.run(r.binary, "attach", "runt-attach")
	// The shell's terminal takes the attached terminal's size.
	time.Sleep(200 * time.Millisecond)
	term.master.Write([]byte("stty size; echo attached-$((6*7))\r"))
	term.waitFor("attached-42")
	term.waitFor("25 90")
	term.master.Write([]byte{0x10, 0x11})
	if code := exitCode(t, attach); code != 0 {
		t.Errorf("runt attach exited with %d after detaching; want 0\n%s", code, term.String())
	}
	if output, _ := r.runt("ps"); !strings.Contains(output, "runt-attach") {
		t.Fatalf("the container stopped when runt attach detached:\n%s", output)
	}

	term = newTerminal(t, 25, 90)
	attach = term.run(r.binary, "attach", "runt-attach")
	time.Sleep(200 * time.Millisecond)
	term.master.Write([]byte("exit 7\r"))
	if code := exitCode(t, attach); code != 7 {
		t.Errorf("runt attach exited with %d; want the shell's 7\n%s", code, term.String())
	}
}

// TestStartAttach creates a container with its input open (create -i), and
// starts it with start -ai, which connects runt's input and output to it
// before it runs, so it reads what runt is given and none of its output is
// missed.
func TestStartAttach(t *testing.T) {
	r := newRuntTester(t)
	r.removeAtEnd("runt-start-ai")
	if output, err := r.runt("create", "-i", "--name", "runt-start-ai", "--rootfs", r.rootfs, "--",
		"/bin/sh", "-c", "echo started; read line; echo got-$line >&2; exit 4"); err != nil {
		t.Fatalf("runt create -i: %v\n%s", err, output)
	}
	start := exec.Command(r.binary, "start", "-ai", "runt-start-ai")
	start.Stdin = strings.NewReader("hello\n")
	// The anonymous function is called on the spot (note the () after its
	// body), to start runt and wait for its exit code in one expression.
	var stdout, stderr strings.Builder
	start.Stdout, start.Stderr = &stdout, &stderr
	if code := func() int {
		if err := start.Start(); err != nil {
			t.Fatal(err)
		}
		return exitCode(t, start)
	}(); code != 4 {
		t.Errorf("runt start -ai exited with %d; want the command's 4", code)
	}
	if stdout.String() != "started\n" || stderr.String() != "got-hello\n" {
		t.Errorf("runt start -ai: stdout %q, stderr %q; want started, and got-hello on stderr", stdout.String(), stderr.String())
	}
}

// TestDevStdout checks that the command can open /dev/stdout and
// /dev/stderr, which lead to runt's output pipes, as nginx does for its logs.
func TestDevStdout(t *testing.T) {
	r := newRuntTester(t)
	r.removeAtEnd("runt-dev-stdout")
	output, err := r.runt("run", "--name", "runt-dev-stdout", "--rootfs", r.rootfs, "--",
		"/bin/sh", "-c", "echo out > /dev/stdout && echo err > /dev/stderr")
	if err != nil || !strings.Contains(output, "out") || !strings.Contains(output, "err") {
		t.Errorf("writing to /dev/stdout and /dev/stderr: %v\n%s", err, output)
	}
}
