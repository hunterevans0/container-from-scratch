package runtime

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCopyOutput(t *testing.T) {
	var file, passthrough bytes.Buffer
	log := &logWriter{file: &file}
	// One byte at a time, so lines arrive split across reads.
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
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %.20q, want %.20q", i, got[i], want[i])
		}
	}
}
