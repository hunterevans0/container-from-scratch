//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
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

// defaultWinsize is the size a pseudo-terminal gets when there is no
// terminal to copy the size from.
var defaultWinsize = winsize{rows: 24, cols: 80}

// winsize mirrors struct winsize from <asm-generic/termios.h>.
type winsize struct {
	rows, cols, xpixel, ypixel uint16
}

// ioctl makes an ioctl call on file. It goes through SyscallConn rather than
// Fd, which would switch the file to blocking mode and break read deadlines.
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

// isTerminal reports whether file is a terminal.
func isTerminal(file *os.File) bool {
	var termios syscall.Termios
	return ioctl(file, syscall.TCGETS, unsafe.Pointer(&termios)) == nil
}

func getWinsize(file *os.File) (winsize, error) {
	var size winsize
	err := ioctl(file, syscall.TIOCGWINSZ, unsafe.Pointer(&size))
	return size, err
}

// setWinsize sets a pseudo-terminal's size. The kernel sends SIGWINCH to its
// foreground process group, so programs like vim redraw.
func setWinsize(file *os.File, size winsize) error {
	return ioctl(file, syscall.TIOCSWINSZ, unsafe.Pointer(&size))
}

// openPTY makes a new pseudo-terminal pair from /dev/ptmx. Neither end
// becomes this process's controlling terminal.
func openPTY() (master, slave *os.File, err error) {
	masterFD, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master = os.NewFile(uintptr(masterFD), "/dev/ptmx")
	// A new slave is locked until the master unlocks it (unlockpt), and its
	// number says which /dev/pts entry it is (ptsname).
	var unlock int32
	if err := ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("unlock pseudo-terminal: %w", err)
	}
	var number uint32
	if err := ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&number)); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("get pseudo-terminal number: %w", err)
	}
	path := fmt.Sprintf("/dev/pts/%d", number)
	slaveFD, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	return master, os.NewFile(uintptr(slaveFD), path), nil
}

// consoleSocketPair makes the Unix socket a pseudo-terminal's master is sent
// over. The parent end stays a raw descriptor for sendmsg and recvmsg; the
// child end is passed to the process that makes the pseudo-terminal.
func consoleSocketPair() (parent int, child *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("create console socket: %w", err)
	}
	return fds[0], os.NewFile(uintptr(fds[1]), "console socket"), nil
}

// sendFile passes file's descriptor over a Unix socket.
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
	quiet := saved
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ECHONL
	if err := ioctl(os.Stdin, syscall.TCSETS, unsafe.Pointer(&quiet)); err != nil {
		return "", fmt.Errorf("turn off terminal echo: %w", err)
	}
	defer ioctl(os.Stdin, syscall.TCSETS, unsafe.Pointer(&saved))
	fmt.Fprint(os.Stderr, prompt)
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

// connectTerminal connects runt's own terminal to a container's
// pseudo-terminal: it gives the pseudo-terminal the same size and keeps it
// in step when the window is resized, and if interactive is set it puts the
// terminal in raw mode and sends what is typed to the container. It returns
// a function that undoes this. The caller copies the container's output.
func connectTerminal(console *os.File, interactive bool) (func(), error) {
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
			if current, err := getWinsize(source); err == nil && current.rows > 0 && current.cols > 0 {
				size = current
			}
		}
		setWinsize(console, size)
	}
	resize()
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
	if !interactive {
		return stopResizing, nil
	}

	restore, err := makeRaw(os.Stdin)
	if err != nil {
		stopResizing()
		return nil, err
	}
	// This copy ends at the first read after the container is gone, or
	// when runt exits.
	go io.Copy(console, os.Stdin)
	return func() {
		stopResizing()
		restore()
	}, nil
}
