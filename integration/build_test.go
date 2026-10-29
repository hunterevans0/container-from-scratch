//go:build integration && linux

package integration

// These tests check building images (ROADMAP.md's stage 8): runt build
// with the Dockerfile's instructions, multi-stage builds, .dockerignore,
// the build cache, and runt tag and runt push. They print what they show,
// as the other feature tests do (see features_test.go), and run with make
// features FEATURE=Build, Cache, or Push.
//
// The base image Dockerfiles start FROM is the rootfs, served by a registry
// of the test's own and pulled into runt's store. Images are pushed to
// another such registry. Since the build cache would keep the base image's
// layers, these tests empty it (runt builder prune) when they are done.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildRegistry is a registry the build tests pull their base image from
// and push to. Its blobs, manifests included, are files in a directory, to
// begin with an image layout's, and it serves any repository. With login
// set, it wants registryUser and registryPassword (Basic authentication).
type buildRegistry struct {
	server *httptest.Server
	host   string
	blobs  string
	login  bool

	mu sync.Mutex
	// manifests maps REPOSITORY:TAG to a manifest's digest, and types
	// manifests' digests to their media types.
	manifests map[string]string
	types     map[string]string
	uploads   int
}

// newBuildRegistry starts a registry whose blobs are in layout's.
func newBuildRegistry(t *testing.T, layout string, login bool) *buildRegistry {
	reg := &buildRegistry{blobs: filepath.Join(layout, "blobs/sha256"), login: login, manifests: map[string]string{}, types: map[string]string{}}
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	reg.host = strings.TrimPrefix(reg.server.URL, "http://")
	t.Cleanup(reg.server.Close)
	return reg
}

// blobFile is where the registry keeps the blob with digest. filepath.Base
// keeps a digest from naming anything outside the directory.
func (reg *buildRegistry) blobFile(digest string) string {
	return filepath.Join(reg.blobs, filepath.Base(strings.TrimPrefix(digest, "sha256:")))
}

// serve answers one request: the API version check, a manifest or blob to
// get (or HEAD), an upload to start (POST) or finish (PUT), or a manifest
// to put.
func (reg *buildRegistry) serve(w http.ResponseWriter, req *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if user, password, _ := req.BasicAuth(); reg.login && (user != registryUser || password != registryPassword) {
		w.Header().Set("WWW-Authenticate", `Basic realm="runt-build"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if req.URL.Path == "/v2/" {
		return
	}
	if strings.HasPrefix(req.URL.Path, "/upload/") && req.Method == http.MethodPut {
		data, _ := io.ReadAll(req.Body)
		sum := sha256.Sum256(data)
		digest := req.URL.Query().Get("digest")
		if digest != "sha256:"+hex.EncodeToString(sum[:]) {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		os.WriteFile(reg.blobFile(digest), data, 0o644)
		reg.uploads++
		w.WriteHeader(http.StatusCreated)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	if strings.HasSuffix(path, "/blobs/uploads/") && req.Method == http.MethodPost {
		w.Header().Set("Location", fmt.Sprintf("/upload/%d", reg.uploads))
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if repository, ref, ok := strings.Cut(path, "/manifests/"); ok {
		if req.Method == http.MethodPut {
			data, _ := io.ReadAll(req.Body)
			sum := sha256.Sum256(data)
			digest := "sha256:" + hex.EncodeToString(sum[:])
			os.WriteFile(reg.blobFile(digest), data, 0o644)
			reg.types[digest] = req.Header.Get("Content-Type")
			reg.manifests[repository+":"+ref] = digest
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
			return
		}
		digest := ref
		if !strings.HasPrefix(ref, "sha256:") {
			digest = reg.manifests[repository+":"+ref]
		}
		if digest == "" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", reg.types[digest])
		http.ServeFile(w, req, reg.blobFile(digest))
		return
	}
	if _, digest, ok := strings.Cut(path, "/blobs/"); ok {
		// ServeFile answers HEAD as well as GET, and 404 for a blob that
		// isn't there.
		http.ServeFile(w, req, reg.blobFile(digest))
		return
	}
	http.NotFound(w, req)
}

// tag makes repository:tag name the manifest desc describes.
func (reg *buildRegistry) tag(repository, tag string, desc map[string]any) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.manifests[repository+":"+tag] = desc["digest"].(string)
	reg.types[desc["digest"].(string)] = desc["mediaType"].(string)
}

// has reports whether the registry has repository:tag.
func (reg *buildRegistry) has(repository, tag string) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.manifests[repository+":"+tag] != ""
}

// buildBase serves the rootfs as an image, pulls it into runt's store, and
// returns its name, for Dockerfiles to start FROM. Once the test is done,
// the image is removed and the build cache emptied.
func buildBase(t *testing.T, r *runtTester) string {
	t.Helper()
	b := newLayoutBuilder(t)
	base, baseDiff := b.addBaseLayer(r)
	manifest, _ := b.image(runtime.GOARCH, map[string]any{
		"Env": []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		"Cmd": []string{"/bin/sh"},
	}, []map[string]any{base}, []string{baseDiff})
	reg := newBuildRegistry(t, b.dir, false)
	reg.tag("runt-build/base", "latest", manifest)
	name := reg.host + "/runt-build/base:latest"
	r.removeImageAtEnd(name)
	t.Cleanup(func() { r.runt("builder", "prune") })
	output, err := r.runt("pull", name)
	mustSucceed(t, "runt pull of the base image", output, err, "Downloaded newer image")
	return name
}

// writeContext writes a build context: files, given as path and content
// pairs, in a new directory, which it returns. Files ending in .sh can be
// run.
func writeContext(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i+1 < len(files); i += 2 {
		path := filepath.Join(dir, files[i])
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		writeFile(t, path, files[i+1], mode)
	}
	return dir
}

// builtID returns the image ID runt build printed last.
func builtID(output string) string {
	id := ""
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(line, "Successfully built "); ok {
			id = strings.TrimSpace(rest)
		}
	}
	return id
}

// stateDirs counts the directories in /run/runt: containers, and what runt
// build makes for a step while it runs.
func stateDirs() int {
	entries, _ := os.ReadDir("/run/runt")
	return len(entries)
}

// TestFeatureBuild checks runt build: the Dockerfile's instructions,
// multi-stage builds, .dockerignore, and failing steps.
func TestFeatureBuild(t *testing.T) {
	explain(t, "Building images: runt build runs each Dockerfile step in a temporary container and saves the result as a layer")
	r := newRuntTester(t)
	baseImage := buildBase(t, r)

	// files.tar.gz, for ADD to unpack.
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	gz.Write(makeTar(t, "pkg/readme", "from an archive\n"))
	gz.Close()
	dockerfile := `# syntax=docker/dockerfile:1
ARG BASE
FROM $BASE AS build
RUN echo "built in a stage NONCE" > /artifact

# A stage with no base, only the file.
FROM scratch AS files
COPY --from=build /artifact /only

FROM ${BASE}
ARG GREETING=hello
ENV APP_HOME=/srv/app
WORKDIR $APP_HOME
COPY greet.sh conf ./
ADD files.tar.gz /opt/
COPY . /ctx/
COPY --from=files /only /srv/from-scratch-stage
RUN chmod +x greet.sh \
    && echo "$GREETING from RUN" > from-run.txt \
    && rm /srv/old
USER daemon
EXPOSE 8080
CMD ["./greet.sh"]
`
	context := writeContext(t,
		"Dockerfile", strings.ReplaceAll(dockerfile, "NONCE", nonce),
		".dockerignore", "secret.txt\n",
		"greet.sh", `#!/bin/sh
echo "$(cat from-run.txt) in $(pwd) as $(id -un)"
echo "conf: $(cat app.conf)"
echo "archive: $(cat /opt/pkg/readme)"
echo "stage: $(cat /srv/from-scratch-stage)"
echo "context: $(ls /ctx | tr '\n' ' ')"
if [ -e /srv/old ]; then echo "old: still there"; else echo "old: deleted"; fi
`,
		"conf/app.conf", "setting="+nonce,
		"secret.txt", "do not copy",
		"files.tar.gz", archive.String())
	image := "runt-feature-build:" + nonce
	r.removeImageAtEnd(image)

	var built string
	t.Run("Instructions", func(t *testing.T) {
		explain(t, "FROM, ARG, ENV, WORKDIR, COPY, ADD, RUN, USER, EXPOSE, and CMD make an image that runs as written")
		before := stateDirs()
		output, err := r.runt("build", "-t", image, "--build-arg", "BASE="+baseImage, context)
		built = mustSucceed(t, "runt build", output, err, "Step 1/", " ---> Running in ", "Successfully built ", "Successfully tagged "+image)
		if stateDirs() != before {
			t.Errorf("runt build left %d containers behind", stateDirs()-before)
		}
		observe("%d steps, image %s", strings.Count(output, "Step "), builtID(output))
		output, err = r.runOnce("runt-feature-build", "--image", image)
		mustSucceed(t, "runt run of the built image", output, err,
			"hello from RUN in /srv/app as daemon\n", "conf: setting="+nonce+"\n", "archive: from an archive\n")
		observe("the image runs: %s", strings.SplitN(output, "\n", 2)[0])
	})

	t.Run("Layers", func(t *testing.T) {
		explain(t, "a file RUN deletes from the base image is gone (an overlayfs whiteout became an OCI .wh. file)")
		output, err := r.runOnce("runt-feature-build", "--image", image)
		mustSucceed(t, "runt run of the built image", output, err, "old: deleted\n")
	})

	t.Run("MultiStage", func(t *testing.T) {
		explain(t, "COPY --from takes files from an earlier stage, even one FROM scratch")
		output, err := r.runOnce("runt-feature-build", "--image", image)
		mustSucceed(t, "runt run of the built image", output, err, "stage: built in a stage "+nonce+"\n")
	})

	t.Run("Dockerignore", func(t *testing.T) {
		explain(t, "COPY . leaves out what .dockerignore lists")
		output, err := r.runOnce("runt-feature-build", "--image", image)
		mustSucceed(t, "runt run of the built image", output, err, "context: Dockerfile conf files.tar.gz greet.sh \n")
		if strings.Contains(output, "secret.txt") {
			t.Errorf("secret.txt was copied:\n%s", output)
		}
	})

	t.Run("Config", func(t *testing.T) {
		explain(t, "ENV is in the image's environment, and ARG isn't")
		output, err := r.runOnce("runt-feature-build", "--image", image, "--entrypoint", "/usr/bin/env")
		mustSucceed(t, "runt run --entrypoint env", output, err, "APP_HOME=/srv/app\n")
		if strings.Contains(output, "GREETING") {
			t.Errorf("an ARG is in the image's environment:\n%s", output)
		}
		output, err = r.runt("images")
		if row := imageRow(output, "runt-feature-build", nonce); err != nil || row == nil || !strings.HasPrefix(builtID(built), row[2]) {
			t.Errorf("runt images doesn't list the image as %s:\n%s", builtID(built), output)
		}
	})

	t.Run("Failures", func(t *testing.T) {
		explain(t, "a failing RUN fails the build and leaves no container; a missing COPY source is reported")
		before := stateDirs()
		failing := writeContext(t, "Dockerfile", "FROM "+baseImage+"\nRUN echo failing; exit 3\n")
		output, err := r.runt("build", failing)
		mustFail(t, "runt build with a failing RUN", output, err, "returned a non-zero code: 3")
		if stateDirs() != before {
			t.Errorf("the failed build left %d containers behind", stateDirs()-before)
		}
		missing := writeContext(t, "Dockerfile", "FROM "+baseImage+"\nCOPY nothing-here /x\n")
		output, err = r.runt("build", missing)
		mustFail(t, "runt build with a missing file", output, err, "nothing-here")
		output, err = r.runt("build", writeContext(t, "Dockerfile", "FROM "+baseImage+"\nRUN --mount=type=cache,target=/x true\n"))
		mustFail(t, "runt build with RUN --mount", output, err, "BuildKit")
	})
}

// TestFeatureCache checks that runt build reuses the steps an earlier build
// ran, until something they depend on changes.
func TestFeatureCache(t *testing.T) {
	explain(t, "Build cache: unchanged steps are skipped, and a change reruns its step and every step after it")
	r := newRuntTester(t)
	baseImage := buildBase(t, r)
	// runt build shows each instruction as it runs it, so the RUN steps
	// print something the instructions don't say, with printf, to tell
	// whether they ran.
	context := writeContext(t,
		"Dockerfile", strings.ReplaceAll(`ARG BASE
FROM ${BASE}
ARG FLAVOR=plain
RUN printf 'ran %s\n' first-NONCE
COPY data.txt /data.txt
RUN printf 'ran %s %s\n' second-NONCE "$FLAVOR" && cat /data.txt > /copy-of-data
`, "NONCE", nonce),
		"data.txt", "one")
	build := func(t *testing.T, args ...string) (string, string) {
		t.Helper()
		args = append(append([]string{"build", "--build-arg", "BASE=" + baseImage}, args...), context)
		output, err := r.runt(args...)
		mustSucceed(t, "runt build", output, err, "Successfully built")
		return output, builtID(output)
	}
	// ran reports which RUN steps ran, by what they printed.
	ran := func(output string) (first, second bool) {
		return strings.Contains(output, "ran first-"+nonce), strings.Contains(output, "ran second-"+nonce)
	}
	_, firstID := build(t)

	t.Run("Unchanged", func(t *testing.T) {
		explain(t, "building again runs nothing, and makes the same image")
		output, id := build(t)
		if first, second := ran(output); first || second || strings.Count(output, "Using cache") != 3 || id != firstID {
			t.Errorf("rebuilding: RUNs ran %v %v, image %s (was %s):\n%s", first, second, id, firstID, output)
		}
		observe("%d steps from the cache, image %s both times", strings.Count(output, "Using cache"), id)
	})

	t.Run("ChangedFile", func(t *testing.T) {
		explain(t, "changing a file COPY takes reruns COPY and what follows, but not what comes before")
		writeFile(t, filepath.Join(context, "data.txt"), "two", 0o644)
		output, id := build(t)
		if first, second := ran(output); first || !second || id == firstID {
			t.Errorf("after changing data.txt: first RUN ran %v, second %v, image %s:\n%s", first, second, id, output)
		}
		// Touching it without changing it costs nothing.
		later := time.Now().Add(time.Hour)
		os.Chtimes(filepath.Join(context, "data.txt"), later, later)
		output, _ = build(t)
		if first, second := ran(output); first || second {
			t.Errorf("after touching data.txt, RUNs ran again:\n%s", output)
		}
	})

	t.Run("BuildArg", func(t *testing.T) {
		explain(t, "a different --build-arg reruns the RUN steps after its ARG, which see it in their environment")
		output, _ := build(t, "--build-arg", "FLAVOR=spicy")
		if first, second := ran(output); !first || !second || !strings.Contains(output, "ran second-"+nonce+" spicy") {
			t.Errorf("with FLAVOR=spicy, RUNs ran %v %v:\n%s", first, second, output)
		}
	})

	t.Run("NoCache", func(t *testing.T) {
		explain(t, "--no-cache runs every step")
		output, _ := build(t, "--no-cache")
		if first, second := ran(output); !first || !second || strings.Contains(output, "Using cache") {
			t.Errorf("with --no-cache:\n%s", output)
		}
	})

	t.Run("Prune", func(t *testing.T) {
		explain(t, "runt builder prune empties the cache")
		output, err := r.runt("builder", "prune")
		mustSucceed(t, "runt builder prune", output, err, "build cache entries", "Total reclaimed space")
		observe("%s", strings.SplitN(output, "\n", 2)[0])
		output, _ = build(t)
		if first, second := ran(output); !first || !second {
			t.Errorf("after pruning, RUNs ran %v %v:\n%s", first, second, output)
		}
	})
}

// TestFeaturePush checks runt tag and runt push: an image built here goes to
// a registry that wants a login, and comes back from it.
func TestFeaturePush(t *testing.T) {
	explain(t, "Pushing images: runt push uploads an image's layers, configuration, and manifest to its registry")
	r := newRuntTester(t)
	baseImage := buildBase(t, r)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	reg := newBuildRegistry(t, newLayoutBuilder(t).dir, true)
	repository := "runt-build/pushed"
	image := reg.host + "/" + repository + ":" + nonce
	r.removeImageAtEnd(image)
	context := writeContext(t,
		"Dockerfile", "FROM "+baseImage+"\nCOPY marker /marker\nCMD [\"cat\", \"/marker\"]\n",
		"marker", "pushed-"+nonce+"\n")
	output, err := r.runt("build", "-t", image, context)
	mustSucceed(t, "runt build", output, err, "Successfully tagged")

	t.Run("NeedsLogin", func(t *testing.T) {
		explain(t, "a registry that wants credentials refuses a push without them")
		output, err := r.runt("push", image)
		mustFail(t, "runt push without logging in", output, err, "runt login")
	})

	t.Run("Push", func(t *testing.T) {
		explain(t, "once logged in, each blob is uploaded, then the manifest, under the tag")
		runtLogin(t, r, reg.host, registryPassword)
		output, err := r.runt("push", image)
		mustSucceed(t, "runt push", output, err, "The push refers to repository ["+reg.host+"/"+repository+"]", ": Pushed\n", nonce+": digest: sha256:")
		if !reg.has(repository, nonce) {
			t.Errorf("the registry has no %s:%s", repository, nonce)
		}
		observe("%d blobs uploaded", reg.uploads)
	})

	other := reg.host + "/" + repository + ":other-" + nonce
	r.removeImageAtEnd(other)
	t.Run("AlreadyThere", func(t *testing.T) {
		explain(t, "pushing again, or under another name from runt tag, uploads nothing the registry has")
		output, err := r.runt("tag", image, other)
		mustSucceed(t, "runt tag", output, err)
		uploads := reg.uploads
		for _, name := range []string{image, other} {
			output, err := r.runt("push", name)
			mustSucceed(t, "runt push again", output, err, "Layer already exists")
			if strings.Contains(output, "Pushed") {
				t.Errorf("pushing %s again uploaded a layer:\n%s", name, output)
			}
		}
		if reg.uploads != uploads || !reg.has(repository, "other-"+nonce) {
			t.Errorf("pushing again uploaded %d blobs", reg.uploads-uploads)
		}
	})

	t.Run("PullBack", func(t *testing.T) {
		explain(t, "the pushed image, removed here and pulled back, runs")
		// Both names, and the build cache, which keeps the blobs too, so
		// that the pull has to download the layer the build made.
		for _, args := range [][]string{{"rmi", image, other}, {"builder", "prune"}} {
			output, err := r.runt(args...)
			mustSucceed(t, "runt "+args[0], output, err)
		}
		output, err := r.runt("pull", image)
		mustSucceed(t, "runt pull of the pushed image", output, err, "Pull complete", "Downloaded newer image")
		output, err = r.runOnce("runt-feature-push", "--image", image)
		mustSucceed(t, "runt run of the pulled image", output, err, "pushed-"+nonce+"\n")
	})
}
