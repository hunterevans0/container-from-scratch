package runtime

// Container logs: the supervisor saves everything a container writes to
// stdout and stderr in its state directory (copyOutput), and runt logs
// prints it back (Logs). No build constraint, so the tests run anywhere. For
// how to read the Go here, see doc.go.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logFileName is the container's output log in its state directory. Like
// Docker's json-file driver, each line of output is one JSON object:
//
//	{"log":"hello\n","stream":"stdout","time":"2026-10-07T12:00:00.000000001Z"}
const logFileName = "container.log"

// maxLogLine is where a line with no newline is split into several log
// entries, as Docker does, so one huge line can't use unlimited memory.
const maxLogLine = 16 * 1024

// logEntry is one line of container output.
type logEntry struct {
	Log    string    `json:"log"`
	Stream string    `json:"stream"`
	Time   time.Time `json:"time"`
}

// logWriter appends entries to a container's log. stdout and stderr are
// copied by separate goroutines, so writes are serialized: the mutex mu lets
// only one goroutine write at a time, so their lines can't get mixed up in
// the middle. file is an io.Writer, anything that can be written to, which
// in the tests is a buffer in memory rather than a file.
type logWriter struct {
	mu   sync.Mutex
	file io.Writer
}

// write appends one entry: line, from stream ("stdout" or "stderr"), with
// the time now. json.Marshal turns the struct into JSON, with the field
// names from its tags.
func (w *logWriter) write(stream string, line []byte) error {
	data, err := json.Marshal(logEntry{Log: string(line), Stream: stream, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.file.Write(append(data, '\n'))
	return err
}

// copyOutput copies one output stream of the container into the log, a line
// at a time, until the stream is closed. If passthrough is set, output is
// also written there as soon as it arrives, without waiting for a whole
// line.
//
// Reads come in chunks of whatever size the container happened to write,
// which needn't end at a line's end. pending holds what has been read but
// not yet logged: each chunk is added to it, and then every whole line at
// its start is logged and cut off, leaving any unfinished line for the next
// chunk to complete.
func copyOutput(r io.Reader, log *logWriter, stream string, passthrough io.Writer) error {
	var pending []byte
	chunk := make([]byte, 32*1024)
	for {
		// Read waits for output, and returns how many bytes it put in
		// chunk, and an error once the stream ends (io.EOF) or fails. It
		// can return both bytes and an error, so the bytes are handled
		// first.
		n, readErr := r.Read(chunk)
		if n > 0 {
			if passthrough != nil {
				passthrough.Write(chunk[:n])
			}
			pending = append(pending, chunk[:n]...)
			for {
				// IndexByte returns where the first newline is, or -1, so
				// end is just past it, or 0 if there is none.
				end := bytes.IndexByte(pending, '\n') + 1
				if end == 0 {
					if len(pending) < maxLogLine {
						break
					}
					end = maxLogLine
				}
				if err := log.write(stream, pending[:end]); err != nil {
					return err
				}
				pending = pending[end:]
			}
		}
		if readErr != nil {
			// Keep whatever the container wrote last, even without a
			// newline.
			if len(pending) > 0 {
				if err := log.write(stream, pending); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// LogOptions control what Logs shows.
type LogOptions struct {
	// Follow keeps printing new output until the container stops.
	Follow bool
	// Timestamps puts the time of each line in front of it.
	Timestamps bool
	// Tail shows only the last Tail lines; a negative value shows all.
	Tail int
}

// Logs prints the output a container has written: its stdout to stdout and
// its stderr to stderr.
func Logs(ref string, options LogOptions, stdout, stderr io.Writer) error {
	state, err := findContainer(StateRoot, ref)
	if err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(stateDir(StateRoot, state.ID), logFileName))
	if err != nil {
		return fmt.Errorf("open log of container %s: %w", state.ID, err)
	}
	defer file.Close()

	// print shows one line of the log: it decodes the JSON entry, and
	// writes the output it holds to stdout or stderr, as the container
	// wrote it.
	print := func(line []byte) error {
		var entry logEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return fmt.Errorf("container %s has a damaged log line: %w", state.ID, err)
		}
		out := stdout
		if entry.Stream == "stderr" {
			out = stderr
		}
		if options.Timestamps {
			fmt.Fprint(out, entry.Time.Format(time.RFC3339Nano), " ")
		}
		_, err := io.WriteString(out, entry.Log)
		return err
	}

	// A bufio.Reader reads the file in large blocks and hands it out a line
	// at a time. ReadBytes('\n') returns everything up to and including the
	// next newline, or what is left and io.EOF at the end of the file.
	reader := bufio.NewReader(file)
	// Print what is there now. For --tail, keep only the last lines: tail
	// holds at most options.Tail lines, dropping the oldest as each new one
	// comes. [][]byte is a slice of byte slices: a list of lines. partial is
	// a line the monitor is still in the middle of writing.
	var tail [][]byte
	var partial []byte
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			partial = line
			break
		}
		if err != nil {
			return err
		}
		if options.Tail < 0 {
			if err := print(line); err != nil {
				return err
			}
			continue
		}
		tail = append(tail, line)
		if len(tail) > options.Tail {
			tail = tail[1:]
		}
	}
	for _, line := range tail {
		if err := print(line); err != nil {
			return err
		}
	}
	if !options.Follow {
		return nil
	}

	// Then wait for more, until the container has stopped and everything it
	// wrote has been printed. At the end of the file, ReadBytes returns
	// io.EOF, but the file can still grow: the loop checks whether the
	// container is still running, waits a little, and reads again, as
	// tail -f does.
	for {
		line, err := reader.ReadBytes('\n')
		partial = append(partial, line...)
		if err == nil {
			if err := print(partial); err != nil {
				return err
			}
			partial = nil
			continue
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		current, loadErr := loadState(StateRoot, state.ID)
		if loadErr != nil || current.Status == StatusStopped {
			// The monitor records the stop only after the log is
			// complete, so one more read catches the last lines.
			rest, _ := io.ReadAll(reader)
			for _, line := range bytes.SplitAfter(append(partial, rest...), []byte("\n")) {
				if len(bytes.TrimSpace(line)) > 0 {
					if err := print(line); err != nil {
						return err
					}
				}
			}
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}
