package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSubID(t *testing.T) {
	const file = `# comment
alice:100000:65536

bob:165536:1000
1001:300000:65536
alice:500000:65536
`
	tests := []struct {
		user  string
		want  IDRange
		found bool
	}{
		{"alice", IDRange{100000, 65536}, true},
		{"bob", IDRange{165536, 1000}, true},
		{"1001", IDRange{300000, 65536}, true},
		{"carol", IDRange{}, false},
	}
	for _, test := range tests {
		got, found, err := parseSubID(strings.NewReader(file), test.user)
		if err != nil {
			t.Errorf("parseSubID(%q): %v", test.user, err)
		}
		if got != test.want || found != test.found {
			t.Errorf("parseSubID(%q) = %v, %v; want %v, %v", test.user, got, found, test.want, test.found)
		}
	}
}

func TestParseSubIDInvalid(t *testing.T) {
	for _, line := range []string{"alice:abc:65536", "alice:100000:0", "alice:-1:65536", "alice:100000:x"} {
		if _, _, err := parseSubID(strings.NewReader(line), "alice"); err == nil {
			t.Errorf("parseSubID(%q) succeeded, want an error", line)
		}
	}
}

func TestSubIDRangeFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	if err := os.WriteFile(path, []byte("alice:200000:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := subIDRange(path, "alice", false)
	if err != nil || got != (IDRange{200000, 65536}) {
		t.Errorf("listed user: got %v, %v", got, err)
	}
	got, err = subIDRange(path, "bob", false)
	if err != nil || got != defaultIDRange {
		t.Errorf("unlisted default user: got %v, %v; want %v", got, err, defaultIDRange)
	}
	if _, err = subIDRange(path, "bob", true); err == nil {
		t.Error("unlisted explicit user: want an error")
	}
	got, err = subIDRange(filepath.Join(t.TempDir(), "missing"), "bob", false)
	if err != nil || got != defaultIDRange {
		t.Errorf("missing file: got %v, %v; want %v", got, err, defaultIDRange)
	}
}
