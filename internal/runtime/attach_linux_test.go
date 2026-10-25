//go:build linux

package runtime

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// Tests for attach_linux.go: the detach keys, and the supervisor's hub,
// with real Unix sockets in a temporary directory. For how Go tests work,
// see "Tests" in doc.go.

// TestDetachFilter checks that Ctrl+P, Ctrl+Q is caught even across reads,
// and that a Ctrl+P not followed by Ctrl+Q is passed on. "\x10" in a string
// is the byte 0x10, Ctrl+P, and "\x11" is Ctrl+Q.
func TestDetachFilter(t *testing.T) {
	for _, test := range []struct {
		reads  []string
		want   string
		detach bool
	}{
		{[]string{"ls\r"}, "ls\r", false},
		{[]string{"a\x10\x11b"}, "a", true},
		{[]string{"a\x10", "\x11"}, "a", true},
		{[]string{"\x10x"}, "\x10x", false},
		{[]string{"\x10", "\x10\x11"}, "\x10", true},
		{[]string{"\x10", "y"}, "\x10y", false},
	} {
		filter := &detachFilter{}
		var got []byte
		detach := false
		for _, read := range test.reads {
			out, d := filter.filter([]byte(read))
			got = append(got, out...)
			if d {
				detach = true
				break
			}
		}
		if string(got) != test.want || detach != test.detach {
			t.Errorf("filter(%q) = %q, %v; want %q, %v", test.reads, got, detach, test.want, test.detach)
		}
	}
}

// syncBuffer is a bytes.Buffer safe to write from the hub's goroutines.
// It stands in for the container's input: an io.Writer, since it has a
// Write method.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAttachHub connects two clients to a hub, as runt attach would, and
// checks that both get the output and the exit code, and that input reaches
// the container.
func TestAttachHub(t *testing.T) {
	path := filepath.Join(t.TempDir(), attachSocketName)
	hub, err := listenAttach(path)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.close(-1)
	input := &syncBuffer{}
	hub.connect(input, nil)

	var conns []net.Conn
	for range 2 {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conns = append(conns, conn)
	}
	// Wait until the hub has both. The hub accepts them in a goroutine of
	// its own, so the test checks every 10ms, up to a deadline, rather than
	// assuming it already has.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		hub.mu.Lock()
		n := len(hub.clients)
		hub.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hub didn't accept both clients")
		}
	}

	if _, err := conns[0].Write(encodeFrame(frameStdin, []byte("typed"))); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); input.String() != "typed"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the container's input got %q", input.String())
		}
	}

	io.WriteString(hub.output(frameStdout), "out")
	io.WriteString(hub.output(frameStderr), "err")
	hub.close(42)
	for i, conn := range conns {
		reader := bufio.NewReader(conn)
		var got []string
		for {
			kind, payload, err := readFrame(reader)
			if err != nil {
				break
			}
			// The exit code is a number in 4 bytes; fmt.Append writes it as
			// text, "42", to compare with the others.
			if kind == frameExit {
				payload = fmt.Append(nil, binary.BigEndian.Uint32(payload))
			}
			got = append(got, string(kind), string(payload))
		}
		if want := []string{"o", "out", "e", "err", "x", "42"}; !slices.Equal(got, want) {
			t.Errorf("client %d got %q; want %q", i, got, want)
		}
	}
	if _, err := net.Dial("unix", path); err == nil {
		t.Error("the hub still accepts connections after closing")
	}
}
