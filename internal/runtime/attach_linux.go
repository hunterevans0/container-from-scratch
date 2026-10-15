//go:build linux

package runtime

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// runt attach connects to a container that runs in the background. Whatever
// supervises the container (its monitor, or runt run in the foreground)
// listens on a Unix socket in the container's state directory. It sends the
// container's output to everyone connected, and passes what they send on to
// the container's input: its pseudo-terminal with -t, or else a pipe it holds
// open for the purpose, if the container was made with -i.
//
// Both directions carry frames: a byte for the type, a 4-byte big-endian
// length, and that much payload.

const attachSocketName = "attach.sock"

// Frame types.
const (
	// frameStdin is input for the command (client to supervisor).
	frameStdin = 'i'
	// frameResize is a terminal size: rows and columns, each a big-endian
	// uint16 (client to supervisor).
	frameResize = 'r'
	// frameStdout and frameStderr are the command's output (supervisor to
	// client). With a terminal, it is all frameStdout.
	frameStdout = 'o'
	frameStderr = 'e'
	// frameExit is the container's exit code, a big-endian uint32, sent
	// before the supervisor hangs up (supervisor to client).
	frameExit = 'x'
)

// maxFrame caps a frame's payload; output and input are sent in chunks of at
// most 32KB.
const maxFrame = 64 * 1024

// detachKeys is the key sequence that detaches runt attach from a container
// without stopping it: Ctrl+P, Ctrl+Q, as in Docker.
var detachKeys = []byte{0x10, 0x11}

func encodeFrame(kind byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func readFrame(r *bufio.Reader) (kind byte, payload []byte, err error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maxFrame {
		return 0, nil, fmt.Errorf("attach frame of %d bytes is too large", size)
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// attachHub is the supervisor's end: it accepts connections and passes data
// between them and the container.
type attachHub struct {
	listener *net.UnixListener
	path     string

	mu      sync.Mutex
	clients map[*attachClient]bool
	closed  bool
	// input is where clients' input goes, or nil if the container's input
	// isn't open to them.
	input io.Writer
	// console is the container's pseudo-terminal, to resize, if it has one.
	console *os.File
	// writers counts the goroutines sending to clients, so close can wait
	// for them to finish.
	writers sync.WaitGroup
}

type attachClient struct {
	conn net.Conn
	// frames are the encoded frames waiting to be sent. Sending never
	// blocks the container: a client that falls this far behind is
	// disconnected.
	frames chan []byte
}

// listenAttach starts accepting attach connections on the socket at path.
func listenAttach(path string) (*attachHub, error) {
	os.Remove(path)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen for attach: %w", err)
	}
	// Removing the socket is up to close.
	listener.SetUnlinkOnClose(false)
	hub := &attachHub{listener: listener, path: path, clients: map[*attachClient]bool{}}
	go hub.accept()
	return hub, nil
}

func (hub *attachHub) accept() {
	for {
		conn, err := hub.listener.Accept()
		if err != nil {
			return
		}
		client := &attachClient{conn: conn, frames: make(chan []byte, 1024)}
		hub.mu.Lock()
		if hub.closed {
			hub.mu.Unlock()
			conn.Close()
			return
		}
		hub.clients[client] = true
		hub.writers.Add(1)
		hub.mu.Unlock()
		go hub.send(client)
		go hub.receive(client)
	}
}

// send writes a client's frames to it until the hub drops it or closes.
func (hub *attachHub) send(client *attachClient) {
	defer hub.writers.Done()
	defer client.conn.Close()
	for frame := range client.frames {
		if _, err := client.conn.Write(frame); err != nil {
			hub.drop(client)
			// Drain the rest; drop has closed the channel.
			for range client.frames {
			}
			return
		}
	}
}

// receive passes a client's input and terminal size on to the container
// until the client hangs up.
func (hub *attachHub) receive(client *attachClient) {
	defer hub.drop(client)
	reader := bufio.NewReader(client.conn)
	for {
		kind, payload, err := readFrame(reader)
		if err != nil {
			return
		}
		hub.mu.Lock()
		input, console := hub.input, hub.console
		hub.mu.Unlock()
		switch kind {
		case frameStdin:
			if input != nil {
				input.Write(payload)
			}
		case frameResize:
			if console != nil && len(payload) == 4 {
				setWinsize(console, winsize{rows: binary.BigEndian.Uint16(payload), cols: binary.BigEndian.Uint16(payload[2:])})
			}
		}
	}
}

// drop disconnects a client.
func (hub *attachHub) drop(client *attachClient) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.clients[client] {
		delete(hub.clients, client)
		close(client.frames)
	}
}

// connect sets where clients' input goes and the terminal they resize.
func (hub *attachHub) connect(input io.Writer, console *os.File) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.input, hub.console = input, console
}

// broadcast sends a frame to every client.
func (hub *attachHub) broadcast(kind byte, payload []byte) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.clients) == 0 {
		return
	}
	frame := encodeFrame(kind, payload)
	for client := range hub.clients {
		select {
		case client.frames <- frame:
		default:
			delete(hub.clients, client)
			close(client.frames)
			client.conn.Close()
		}
	}
}

// output returns a writer that sends what is written to it to every client
// as frames of kind. It never fails, so it can go first in an
// io.MultiWriter.
func (hub *attachHub) output(kind byte) io.Writer {
	return hubWriter{hub, kind}
}

type hubWriter struct {
	hub  *attachHub
	kind byte
}

func (w hubWriter) Write(p []byte) (int, error) {
	for chunk := range slices.Chunk(p, maxFrame) {
		w.hub.broadcast(w.kind, chunk)
	}
	return len(p), nil
}

// close stops accepting connections and hangs up on every client, telling
// them the container's exit code first if exitCode isn't negative. It waits
// a little for what is queued to reach them. Calling it again does nothing.
func (hub *attachHub) close(exitCode int) {
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return
	}
	hub.closed = true
	hub.listener.Close()
	os.Remove(hub.path)
	var exit []byte
	if exitCode >= 0 {
		exit = encodeFrame(frameExit, binary.BigEndian.AppendUint32(nil, uint32(exitCode)))
	}
	for client := range hub.clients {
		if exit != nil {
			select {
			case client.frames <- exit:
			default:
			}
		}
		delete(hub.clients, client)
		close(client.frames)
	}
	hub.mu.Unlock()

	done := make(chan struct{})
	go func() {
		hub.writers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// statusError is a container's exit code, passed on by runt attach and runt
// start -a as their own.
type statusError int

func (e statusError) Error() string { return fmt.Sprintf("container exited with code %d", int(e)) }

// Attach connects runt's input and output to a running container, as docker
// attach does: the container's output is written to runt's, and with
// options.Stdin, what runt reads is sent to the container's input if it is
// open. With a terminal, runt's own terminal is put in raw mode and its size
// passed on; Ctrl+P, Ctrl+Q detaches. Attach returns when the container
// exits, with its exit code as a statusError, or when detached.
func Attach(ref string, options AttachOptions) error {
	state, err := findContainer(StateRoot, ref)
	if err != nil {
		return err
	}
	refresh(state)
	if state.Status == StatusStopped {
		return fmt.Errorf("container %s is stopped; you can't attach to it", state.ID)
	}
	if options.Start && state.Status != StatusCreated {
		return fmt.Errorf("container %s is %s; only a created container can be started", state.ID, state.Status)
	}
	stdin := options.Stdin && state.Interactive
	if stdin && state.Tty && !isTerminal(os.Stdin) {
		return errors.New("the input device is not a terminal; attach without -i (--no-stdin), or run from a terminal")
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(stateDir(StateRoot, state.ID), attachSocketName), Net: "unix"})
	if err != nil {
		return fmt.Errorf("attach to container %s: %w", state.ID, err)
	}
	defer conn.Close()
	var sendMu sync.Mutex
	send := func(kind byte, payload []byte) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		_, err := conn.Write(encodeFrame(kind, payload))
		return err
	}

	if state.Tty {
		// Keep the container's terminal the size of ours.
		var terminal *os.File
		for _, file := range []*os.File{os.Stdin, os.Stdout} {
			if isTerminal(file) {
				terminal = file
				break
			}
		}
		if terminal != nil {
			resize := func() {
				if size, err := getWinsize(terminal); err == nil && size.rows > 0 && size.cols > 0 {
					send(frameResize, binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, size.rows), size.cols))
				}
			}
			resize()
			winch := make(chan os.Signal, 1)
			signal.Notify(winch, syscall.SIGWINCH)
			defer signal.Stop(winch)
			go func() {
				for range winch {
					resize()
				}
			}()
		}
		if stdin {
			restore, err := makeRaw(os.Stdin)
			if err != nil {
				return err
			}
			defer restore()
		}
	} else if options.SigProxy {
		// Without a terminal, Ctrl+C is a signal to runt; pass it on, as
		// docker attach does.
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
		defer signal.Stop(signals)
		go func() {
			for sig := range signals {
				syscall.Kill(state.Pid, sig.(syscall.Signal))
			}
		}()
	}

	var detachedMu sync.Mutex
	detached := false
	if stdin {
		go func() {
			filter := &detachFilter{}
			chunk := make([]byte, 32*1024)
			for {
				n, err := os.Stdin.Read(chunk)
				if n > 0 {
					data, detach := filter.filter(chunk[:n])
					if len(data) > 0 && send(frameStdin, data) != nil {
						return
					}
					if detach {
						detachedMu.Lock()
						detached = true
						detachedMu.Unlock()
						conn.Close()
						return
					}
				}
				if err != nil {
					// End of input leaves the container's input open
					// for others, as in Docker.
					return
				}
			}
		}()
	}

	if options.Start {
		// Whether the command runs or not, the exit code comes back over
		// the connection, and init's error, if any, as output.
		if err := writeStartFifo(state.ID); err != nil {
			return err
		}
	}

	reader := bufio.NewReader(conn)
	for {
		kind, payload, err := readFrame(reader)
		if err != nil {
			detachedMu.Lock()
			defer detachedMu.Unlock()
			if detached {
				return nil
			}
			return fmt.Errorf("lost the connection to container %s", state.ID)
		}
		switch kind {
		case frameStdout:
			os.Stdout.Write(payload)
		case frameStderr:
			os.Stderr.Write(payload)
		case frameExit:
			if len(payload) == 4 {
				if code := int(binary.BigEndian.Uint32(payload)); code != 0 {
					return statusError(code)
				}
			}
			return nil
		}
	}
}

// detachFilter watches input for detachKeys. A partial match is held back
// until the next key shows whether it completes the sequence.
type detachFilter struct {
	matched int
}

// filter returns the input to pass on, and whether the detach keys were
// typed; input after them is dropped.
func (f *detachFilter) filter(input []byte) ([]byte, bool) {
	var out []byte
	for _, b := range input {
		if b == detachKeys[f.matched] {
			f.matched++
			if f.matched == len(detachKeys) {
				return out, true
			}
			continue
		}
		// What was held back wasn't the sequence after all.
		out = append(out, detachKeys[:f.matched]...)
		f.matched = 0
		if b == detachKeys[0] {
			f.matched = 1
			continue
		}
		out = append(out, b)
	}
	return out, false
}
