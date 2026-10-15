package runtime

import (
	"strings"
	"testing"
)

// TestParseReference checks that references are normalized as Docker does:
// Docker Hub by default, library/ for its official images, and latest when
// there is no tag.
func TestParseReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	for text, want := range map[string][2]string{
		// The full reference, and the familiar one.
		"alpine":                          {"docker.io/library/alpine:latest", "alpine:latest"},
		"alpine:3.20":                     {"docker.io/library/alpine:3.20", "alpine:3.20"},
		"someuser/someimage:v1":           {"docker.io/someuser/someimage:v1", "someuser/someimage:v1"},
		"docker.io/library/ubuntu":        {"docker.io/library/ubuntu:latest", "ubuntu:latest"},
		"index.docker.io/library/ubuntu":  {"docker.io/library/ubuntu:latest", "ubuntu:latest"},
		"ghcr.io/owner/repo/image:1.0":    {"ghcr.io/owner/repo/image:1.0", "ghcr.io/owner/repo/image:1.0"},
		"localhost:5000/test":             {"localhost:5000/test:latest", "localhost:5000/test:latest"},
		"localhost/test:edge":             {"localhost/test:edge", "localhost/test:edge"},
		"127.0.0.1:5000/a/b":              {"127.0.0.1:5000/a/b:latest", "127.0.0.1:5000/a/b:latest"},
		"alpine@" + digest:                {"docker.io/library/alpine@" + digest, "alpine@" + digest},
		"alpine:3.20@" + digest:           {"docker.io/library/alpine@" + digest, "alpine@" + digest},
		"my-registry.example.com/x_y.z-w": {"my-registry.example.com/x_y.z-w:latest", "my-registry.example.com/x_y.z-w:latest"},
	} {
		ref, err := parseReference(text)
		if err != nil {
			t.Errorf("parseReference(%q): %v", text, err)
			continue
		}
		if ref.String() != want[0] || ref.familiar() != want[1] {
			t.Errorf("parseReference(%q) = %q, familiar %q; want %q, %q", text, ref.String(), ref.familiar(), want[0], want[1])
		}
	}
	for _, text := range []string{"", "Alpine", "alpine:", "alpine@sha256:abc", "-alpine", "alpine/", "a//b", "alpine:bad/tag", "ex ample/x"} {
		if ref, err := parseReference(text); err == nil {
			t.Errorf("parseReference(%q) = %q; want an error", text, ref)
		}
	}
}

// TestRegistryHost checks where each registry's API is reached: Docker Hub
// at its own host, and registries on this machine over plain HTTP.
func TestRegistryHost(t *testing.T) {
	for text, want := range map[string]string{
		"alpine":                     "https://registry-1.docker.io",
		"ghcr.io/a/b":                "https://ghcr.io",
		"localhost:5000/a":           "http://localhost:5000",
		"127.0.0.1:5000/a":           "http://127.0.0.1:5000",
		"registry.example.com:443/a": "https://registry.example.com:443",
	} {
		ref, err := parseReference(text)
		if err != nil {
			t.Fatal(err)
		}
		if got := registryURL(ref.registryHost()); got != want {
			t.Errorf("registry URL for %q = %q; want %q", text, got, want)
		}
	}
}
