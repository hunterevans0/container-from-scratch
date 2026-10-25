//go:build integration && linux

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Image tests: an image's configuration, and pulling from a registry served
// by the test itself. For how to read these tests, see the top of
// isolation_test.go.

// TestImageConfig runs an image whose configuration sets the command,
// environment, working directory, and user, first as it is and then with
// each one overridden on the command line.
func TestImageConfig(t *testing.T) {
	r := newRuntTester(t)
	b := newLayoutBuilder(t)
	base, baseDiff := b.addBaseLayer(r)
	b.writeImage("test", map[string]any{
		"Entrypoint": []string{"/bin/sh", "-c"},
		"Cmd":        []string{"id -un; pwd; echo FOO=$FOO HOME=$HOME"},
		"Env":        []string{"PATH=/usr/bin:/bin", "FOO=from-image"},
		"WorkingDir": "/srv/app",
		"User":       "daemon",
	}, []map[string]any{base}, []string{baseDiff})

	r.removeAtEnd("runt-config")
	output, err := r.runt("run", "--name", "runt-config", "--image", b.dir+":test")
	if err != nil {
		t.Fatalf("runt run: %v\n%s", err, output)
	}
	// Ubuntu's daemon user has /usr/sbin as its home.
	if want := "daemon\n/srv/app\nFOO=from-image HOME=/usr/sbin\n"; output != want {
		t.Errorf("with the image's configuration, got:\n%s\nwant:\n%s", output, want)
	}

	r.removeAtEnd("runt-config-2")
	output, err = r.runt("run", "--name", "runt-config-2", "--image", b.dir+":test",
		"-e", "FOO=override", "-u", "root", "-w", "/tmp", "--", "id -un; pwd; echo FOO=$FOO")
	if err != nil {
		t.Fatalf("runt run with overrides: %v\n%s", err, output)
	}
	if want := "root\n/tmp\nFOO=override\n"; output != want {
		t.Errorf("with -e, -u, -w, and a command, got:\n%s\nwant:\n%s", output, want)
	}

	r.removeAtEnd("runt-config-3")
	output, err = r.runt("run", "--name", "runt-config-3", "--image", b.dir+":test", "--entrypoint", "/bin/echo", "--", "replaced")
	if err != nil || output != "replaced\n" {
		t.Errorf("runt run --entrypoint: %v\n%s", err, output)
	}
}

// layoutRegistry serves an OCI image layout as a registry repository named
// runt-test/app, wanting a user name and password, as a private registry
// does.
//
// httptest.NewServer serves HTTP on a free local port, calling the
// anonymous function for each request: w is where the response is written,
// and req the request. http.ServeFile sends a file as the response.
func layoutRegistry(t *testing.T, layout string) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if user, password, _ := req.BasicAuth(); user != "runt" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		const repo = "/v2/runt-test/app/"
		switch {
		case req.URL.Path == "/v2/":
		case strings.HasPrefix(req.URL.Path, repo+"manifests/"):
			ref := strings.TrimPrefix(req.URL.Path, repo+"manifests/")
			var index struct {
				Manifests []struct {
					MediaType   string            `json:"mediaType"`
					Digest      string            `json:"digest"`
					Annotations map[string]string `json:"annotations"`
				} `json:"manifests"`
			}
			data, _ := os.ReadFile(filepath.Join(layout, "index.json"))
			json.Unmarshal(data, &index)
			for _, entry := range index.Manifests {
				if entry.Digest == ref || entry.Annotations["org.opencontainers.image.ref.name"] == ref {
					w.Header().Set("Content-Type", entry.MediaType)
					http.ServeFile(w, req, filepath.Join(layout, "blobs/sha256", strings.TrimPrefix(entry.Digest, "sha256:")))
					return
				}
			}
			http.NotFound(w, req)
		case strings.HasPrefix(req.URL.Path, repo+"blobs/sha256:"):
			http.ServeFile(w, req, filepath.Join(layout, "blobs/sha256", strings.TrimPrefix(req.URL.Path, repo+"blobs/sha256:")))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestPull logs in to a registry, pulls an image from it, runs it by name,
// and removes it again with rmi, which also removes its blobs and unpacked
// layers. Then run pulls it by itself.
func TestPull(t *testing.T) {
	r := newRuntTester(t)
	b := newLayoutBuilder(t)
	base, baseDiff := b.addBaseLayer(r)
	b.writeImage("v1", map[string]any{
		"Cmd": []string{"/bin/sh", "-c", "echo pulled-$GREETING"},
		"Env": []string{"PATH=/usr/bin:/bin", "GREETING=hello"},
	}, []map[string]any{base}, []string{baseDiff})
	server := layoutRegistry(t, b.dir)
	host := strings.TrimPrefix(server.URL, "http://")
	image := host + "/runt-test/app:v1"
	// Credentials go here rather than in root's ~/.docker.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Cleanup(func() { r.runt("rmi", "-f", image) })

	if output, err := r.runt("pull", image); err == nil || !strings.Contains(output, "runt login") {
		t.Errorf("runt pull without logging in: %v\n%s", err, output)
	}
	// The password goes in on runt login's standard input, as a user would
	// pipe it.
	login := exec.Command(r.binary, "login", "-u", "runt", "--password-stdin", host)
	login.Stdin = strings.NewReader("secret\n")
	if output, err := login.CombinedOutput(); err != nil || !strings.Contains(string(output), "Login Succeeded") {
		t.Fatalf("runt login: %v\n%s", err, output)
	}
	output, err := r.runt("pull", image)
	if err != nil || !strings.Contains(output, "Downloaded newer image") {
		t.Fatalf("runt pull: %v\n%s", err, output)
	}
	if output, err := r.runt("images"); err != nil || !listsImage(output, host+"/runt-test/app", "v1") {
		t.Errorf("runt images doesn't list the image: %v\n%s", err, output)
	}

	r.removeAtEnd("runt-pulled")
	if output, err := r.runt("run", "--name", "runt-pulled", "--image", image); err != nil || output != "pulled-hello\n" {
		t.Errorf("runt run --image %s: %v\n%s", image, err, output)
	}
	if output, err := r.runt("rmi", image); err == nil || !strings.Contains(output, "used by container") {
		t.Errorf("runt rmi of an image a container uses: %v\n%s", err, output)
	}
	if output, err := r.runt("rm", "runt-pulled"); err != nil {
		t.Fatalf("runt rm: %v\n%s", err, output)
	}
	if output, err := r.runt("rmi", image); err != nil || !strings.Contains(output, "Deleted: sha256:") {
		t.Fatalf("runt rmi: %v\n%s", err, output)
	}
	if output, _ := r.runt("images"); strings.Contains(output, host) {
		t.Errorf("runt images still lists the image after rmi:\n%s", output)
	}
	for _, path := range []string{"/var/lib/runt/images/blobs/sha256/" + baseLayer.digest, "/var/lib/runt/layers/*/" + baseLayer.digest} {
		if matches, _ := filepath.Glob(path); len(matches) > 0 {
			t.Errorf("runt rmi left %s behind", matches[0])
		}
	}

	// run pulls an image it doesn't have, as docker run does.
	r.removeAtEnd("runt-autopull")
	output, err = r.runt("run", "--name", "runt-autopull", "--image", image)
	if err != nil || !strings.Contains(output, "Unable to find image") || !strings.Contains(output, "pulled-hello") {
		t.Errorf("runt run of an image it hasn't pulled: %v\n%s", err, output)
	}

	if output, err := r.runt("logout", host); err != nil || !strings.Contains(output, "Removed") {
		t.Errorf("runt logout: %v\n%s", err, output)
	}
}

// listsImage reports whether runt images output has a row for repository and
// tag.
// strings.Lines yields the output a line at a time to the loop.
func listsImage(output, repository, tag string) bool {
	for line := range strings.Lines(output) {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == repository && fields[1] == tag {
			return true
		}
	}
	return false
}
