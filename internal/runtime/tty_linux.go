//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Pseudo-terminals. With -t, the command's standard input, output, and error
// are the "slave" end of a pseudo-terminal, and whatever holds the "master"
// end plays the part of the terminal: what it writes is what the command
// reads as typed input, and it reads what the command prints.
//
// As in runc, the pair is made inside the container, from the container's
// own devpts instance, so the command sees /dev/pts/0 owned by container
// root, rather than one of the host's terminals. The master is handed out to
// runt over a Unix socket (SCM_RIGHTS), and the slave never leaves the
// container.
//
// Most of what is done to a terminal is done with ioctl, a system call that
// sends a device a numbered request with a pointer to a struct: TCGETS reads
// a terminal's settings into a Termios struct, TCSETS writes them back,
// TIOCGWINSZ and TIOCSWINSZ get and set its size, and so on. For how to read
// the Go here, see doc.go.

// defaultWinsize is the size a pseudo-terminal gets when there is no
// terminal to copy the size from.
var defaultWinsize = winsize{rows: 24, cols: 80}

// winsize mirrors struct winsize from <asm-generic/termios.h>.
type winsize struct {
	rows, cols, xpixel, ypixel uint16
}

// ioctl makes an ioctl call on file. It goes through SyscallConn rather than
// Fd, which would switch the file to blocking mode and break read deadlines.
// conn.Control runs the function it is given with the file's raw descriptor
// number, while making sure the file isn't closed meanwhile. arg is an
// unsafe.Pointer, a bare memory address, since each request takes a
// different struct.
func ioctl(file *os.File, request uintptr, arg unsafe.Pointer) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// isTerminal reports whether file is a terminal: only a terminal can report
// terminal settings, so asking for them succeeds only for one.
func isTerminal(file *os.File) bool {
	var termios syscall.Termios
	return ioctl(file, syscall.TCGETS, unsafe.Pointer(&termios)) == nil
}

// setWinsize sets a pseudo-terminal's size. The kernel sends SIGWINCH to its
// foreground process group, so programs like vim redraw.
func setWinsize(file *os.File, size winsize) error {
	return ioctl(file, syscall.TIOCSWINSZ, unsafe.Pointer(&size))
}

// sendFile passes file's descriptor over a Unix socket.
//
// A Unix socket can carry open files from one process to another: the
// message carries the descriptor as "ancillary data" (SCM_RIGHTS, which
// UnixRights builds), alongside a normal byte of data, and the kernel gives
// the receiving process a descriptor of its own for the same open file.
func sendFile(socket, file *os.File) error {
	conn, err := socket.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	err = conn.Control(func(fd uintptr) {
		sendErr = syscall.Sendmsg(int(fd), []byte{0}, syscall.UnixRights(int(file.Fd())), nil, 0)
	})
	if err != nil {
		return err
	}
	if sendErr != nil {
		return fmt.Errorf("send pseudo-terminal: %w", sendErr)
	}
	return nil
}

// receiveFile waits for a descriptor sent with sendFile. It returns io.EOF if
// the other end closed the socket without sending one, as init does when it
// fails to set up. The file is non-blocking, so reads from it can time out.
func receiveFile(socket int, name string) (*os.File, error) {
	// data takes the one byte of normal data, and control the ancillary
	// data, with room for one descriptor (4 bytes).
	data := make([]byte, 1)
	control := make([]byte, syscall.CmsgSpace(4))
	for {
		n, controlLen, _, _, err := syscall.Recvmsg(socket, data, control, syscall.MSG_CMSG_CLOEXEC)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("receive pseudo-terminal: %w", err)
		}
		if n == 0 {
			return nil, io.EOF
		}
		messages, err := syscall.ParseSocketControlMessage(control[:controlLen])
		if err != nil || len(messages) != 1 {
			return nil, errors.New("receive pseudo-terminal: no descriptor in message")
		}
		fds, err := syscall.ParseUnixRights(&messages[0])
		if err != nil || len(fds) != 1 {
			return nil, errors.New("receive pseudo-terminal: no descriptor in message")
		}
		if err := syscall.SetNonblock(fds[0], true); err != nil {
			syscall.Close(fds[0])
			return nil, err
		}
		return os.NewFile(uintptr(fds[0]), name), nil
	}
}

// consoleReader reads what a container writes to its pseudo-terminal. Once
// every process has closed the slave end, reading the master fails with EIO
// instead of reaching end-of-file, so EIO is turned into io.EOF.
//
// Having a Read method with this signature makes consoleReader an
// io.Reader: Go interfaces are satisfied by having the methods, with no
// declaration needed, so a consoleReader can go anywhere an io.Reader can,
// such as copyOutput.
type consoleReader struct{ master *os.File }

func (r consoleReader) Read(p []byte) (int, error) {
	n, err := r.master.Read(p)
	if errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}

// makeRaw switches a terminal to raw mode, as cfmakeraw(3) does, so every key
// goes straight through: Ctrl+C becomes a byte for the container's own
// pseudo-terminal to turn into SIGINT, instead of a signal for runt. It
// returns a function that restores the previous mode.
func makeRaw(terminal *os.File) (func(), error) {
	var saved syscall.Termios
	if err := ioctl(terminal, syscall.TCGETS, unsafe.Pointer(&saved)); err != nil {
		return nil, fmt.Errorf("read terminal mode: %w", err)
	}
	// A terminal's settings are bit flags in four fields: input (Iflag),
	// output (Oflag), local behavior such as echo and Ctrl+C (Lflag), and
	// the character format (Cflag). &^= clears the bits named on the right,
	// and |= sets them. Turned off here: translating input (such as Enter
	// to newline), output processing, echoing what is typed, line editing
	// (ICANON, which holds input until Enter), and signal keys (ISIG). VMIN
	// 1 and VTIME 0 make a read return as soon as one byte is there.
	raw := saved
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(terminal, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, fmt.Errorf("set terminal to raw mode: %w", err)
	}
	return func() { ioctl(terminal, syscall.TCSETS, unsafe.Pointer(&saved)) }, nil
}

// ReadPassword prints prompt to standard error and reads a line from the
// terminal on standard input without echoing it.
func ReadPassword(prompt string) (string, error) {
	var saved syscall.Termios
	if err := ioctl(os.Stdin, syscall.TCGETS, unsafe.Pointer(&saved)); err != nil {
		return "", errors.New("standard input is not a terminal to read a password from")
	}
	// Echo off, so the password doesn't show, but line editing on, and the
	// newline still echoed, so the cursor moves on when Enter is pressed.
	quiet := saved
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ECHONL
	if err := ioctl(os.Stdin, syscall.TCSETS, unsafe.Pointer(&quiet)); err != nil {
		return "", fmt.Errorf("turn off terminal echo: %w", err)
	}
	defer ioctl(os.Stdin, syscall.TCSETS, unsafe.Pointer(&saved))
	fmt.Fprint(os.Stderr, prompt)
	// Read a byte at a time up to the newline, so nothing after it is read
	// and lost.
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if n == 1 && b[0] != '\n' {
			line = append(line, b[0])
			continue
		}
		if n == 1 || errors.Is(err, io.EOF) {
			return strings.TrimSuffix(string(line), "\r"), nil
		}
		if err != nil {
			return "", err
		}
	}
}
