package runtime

// Tests for logs.go: saving a container's output as JSON log lines. For how
// Go tests work, see "Tests" in doc.go.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/iotest"
)

// TestCopyOutput checks that copyOutput passes output through unchanged while
// writing it to the log as JSON entries, one per line, rejoining lines split
// across reads, splitting lines longer than maxLogLine, and keeping a final
// line that has no newline.
func TestCopyOutput(t *testing.T) {
	// bytes.Buffers stand in for the log file and runt's output: they
	// collect what is written to them, in memory.
	var file, passthrough bytes.Buffer
	log := &logWriter{file: &file}
	// One byte at a time, so lines arrive split across reads. OneByteReader
	// wraps a reader so each Read returns at most one byte.
	input := "one\ntwo\n" + strings.Repeat("x", maxLogLine+5) + "\nno newline"
	if err := copyOutput(iotest.OneByteReader(strings.NewReader(input)), log, "stderr", &passthrough); err != nil {
		t.Fatal(err)
	}
	if passthrough.String() != input {
		t.Error("passthrough did not get the output unchanged")
	}

	var got []string
	for _, line := range strings.Split(strings.TrimSuffix(file.String(), "\n"), "\n") {
		var entry logEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if entry.Stream != "stderr" || entry.Time.IsZero() {
			t.Errorf("entry %+v: want stream stderr and a time", entry)
		}
		got = append(got, entry.Log)
	}
	want := []string{"one\n", "two\n", strings.Repeat("x", maxLogLine), "xxxxx\n", "no newline"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	// %.20q prints at most 20 characters of a string, quoted, so the long
	// line doesn't flood the message.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %.20q, want %.20q", i, got[i], want[i])
		}
	}
}
