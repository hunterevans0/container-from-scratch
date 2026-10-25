//go:build integration && linux

package integration

// These tests check the image and filesystem features in ROADMAP.md one at a
// time: layered filesystems, OCI image layouts and runtime bundles, registry
// pulls, the image store, and image configuration. Each check prints a line
// saying what it shows (see explain), so the output reads as a checklist.
//
// Run them on their own with make features, or one feature with, say,
// make features FEATURE=Registry. They run with make integration too. Like
// the other integration tests, they need root and make rootfs, but not the
// internet: registries are served from this process, on localhost.
//
// Each feature is one TestFeatureX function, and each of its checks a
// subtest, run with t.Run(name, func(t *testing.T) { ... }). The network
// features are in network_test.go. For how to read these tests, see the top
// of isolation_test.go.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The feature tests print what they show straight to standard output rather
// than with t.Log, so it shows without -test.v, and without a file and line
// on every line: a heading per feature, a line per check, and now and then a
// line of evidence. Checks that pass print nothing else. A check that fails
// or is skipped says so, and go test prints why once the feature is done.

// explain prints what a test shows: the heading of a feature, or a line for
// one of its checks.
//
// format and args are as for fmt.Printf: args ...any takes any number of
// values of any type. A subtest's name is its test's name, a slash, and its
// own, so a slash says this is a check rather than a feature's heading. The
// cleanup runs once the test is over, when whether it passed is known.
func explain(t *testing.T, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	check := strings.Contains(t.Name(), "/")
	if check {
		fmt.Printf("  - %s\n", text)
	} else {
		fmt.Printf("\n%s\n", text)
	}
	t.Cleanup(func() {
		switch {
		case t.Skipped():
			fmt.Println("    skipped (needs root, make build, and make rootfs)")
		case t.Failed() && check:
			fmt.Println("    FAILED (details below)")
		}
	})
}

// observe prints a piece of evidence a check found.
func observe(format string, args ...any) {
	fmt.Printf("      "+format+"\n", args...)
}

// nonce makes file contents, and so layer digests, unique to this run, so
// that layers unpacked by an earlier run are never reused. It is the time
// now, in nanoseconds, written in base 36 (digits and letters) to be short.
var nonce = strconv.FormatInt(time.Now().UnixNano(), 36)

const imageStoreDir = "/var/lib/runt/images"

// makeTar returns a tar archive of regular files, given as name and content
// pairs. Names starting with .wh. are whiteouts, as in any OCI layer.
func makeTar(t *testing.T, files ...string) []byte {
	t.Helper()
	// files alternates names and contents, so the loop takes them two at a
	// time: files[i] and files[i+1].
	var buf bytes.Buffer
	archive := tar.NewWriter(&buf)
	for i := 0; i+1 < len(files); i += 2 {
		addFile(t, archive, files[i], files[i+1])
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarLayer adds an uncompressed layer to the layout, returning its descriptor
// and diff ID.
func (b *layoutBuilder) tarLayer(data []byte) (map[string]any, string) {
	b.t.Helper()
	desc := b.blob("application/vnd.oci.image.layer.v1.tar", data)
	return desc, desc["digest"].(string)
}

// gzipLayer adds a gzipped layer to the layout, returning its descriptor and
// diff ID, the digest of the uncompressed tar.
func (b *layoutBuilder) gzipLayer(data []byte) (map[string]any, string) {
	b.t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Write(data)
	if err := gz.Close(); err != nil {
		b.t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return b.blob("application/vnd.oci.image.layer.v1.tar+gzip", compressed.Bytes()), "sha256:" + hex.EncodeToString(sum[:])
}

// blobFile is where the layout keeps the blob desc describes.
func (b *layoutBuilder) blobFile(desc map[string]any) string {
	return filepath.Join(b.dir, "blobs/sha256", hexDigest(desc))
}

// hexDigest is a descriptor's digest without its sha256: prefix. The
// descriptor is a map of any values, so .(string) takes out the string.
func hexDigest(desc map[string]any) string {
	return strings.TrimPrefix(desc["digest"].(string), "sha256:")
}

// withPlatform returns a copy of a manifest's descriptor with the platform an
// image index entry gives.
//
// Maps are shared, not copied, when assigned, so it copies desc's entries
// into a new map before adding to it.
func withPlatform(desc map[string]any, architecture string) map[string]any {
	entry := map[string]any{}
	for key, value := range desc {
		entry[key] = value
	}
	entry["platform"] = map[string]string{"os": "linux", "architecture": architecture}
	return entry
}

// otherArchitecture is a CPU architecture other than this machine's.
func otherArchitecture() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

// layerStore is where runt unpacks image layers for containers in the
// rootfs's ID range.
func (r *runtTester) layerStore() string {
	r.t.Helper()
	var stat syscall.Stat_t
	if err := syscall.Stat(r.rootfs, &stat); err != nil {
		r.t.Fatal(err)
	}
	return fmt.Sprintf("/var/lib/runt/layers/%d-%d", stat.Uid, stat.Gid)
}

// removeUnpackedAtEnd removes what runt unpacked of layer once the test is
// done. Layers are shared by every container from them, so runt itself
// only removes them on runt rmi.
func (r *runtTester) removeUnpackedAtEnd(layer map[string]any) {
	dir := filepath.Join(r.layerStore(), hexDigest(layer))
	r.t.Cleanup(func() { os.RemoveAll(dir) })
}

// runOnce runs a container called name in the foreground, with args as the
// options and command for runt run, removes it, and returns its output.
func (r *runtTester) runOnce(name string, args ...string) (string, error) {
	r.t.Helper()
	r.removeAtEnd(name)
	// The arguments: run --name NAME, then args. append(a, b...) joins the
	// two lists, and the final ... passes the result as separate arguments.
	output, err := r.runt(append([]string{"run", "--name", name}, args...)...)
	r.runt("rm", "-f", name)
	return output, err
}

// mustSucceed fails the test if a runt command failed or its output lacks any
// of wants, and returns the output. It is how most checks look at a
// command's result: mustSucceed(t, "what was done", output, err, "text the
// output must contain", ...).
func mustSucceed(t *testing.T, what string, output string, err error, wants ...string) string {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v\n%s", what, err, output)
	}
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Errorf("%s: output is missing %q:\n%s", what, want, output)
		}
	}
	return output
}

// mustFail fails the test unless a runt command failed with an error
// containing want.
func mustFail(t *testing.T, what string, output string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s succeeded; want an error containing %q:\n%s", what, want, output)
	} else if !strings.Contains(output, want) {
		t.Errorf("%s: want an error containing %q, got:\n%s", what, want, output)
	}
}

// overlayMount returns the options of container id's overlay mount on the
// host, from /proc/mounts, or nil if it isn't mounted.
func overlayMount(t *testing.T, id string) map[string]string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatal(err)
	}
	// Each line of /proc/self/mounts is: what is mounted, where, the
	// filesystem type, and its options, separated by commas.
	target := filepath.Join("/var/lib/runt/containers", id, "rootfs")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != target || fields[2] != "overlay" {
			continue
		}
		options := map[string]string{}
		for _, option := range strings.Split(fields[3], ",") {
			key, value, _ := strings.Cut(option, "=")
			options[key] = value
		}
		return options
	}
	return nil
}

// isWhiteout reports whether path is an overlayfs whiteout: a character
// device with device number 0/0.
func isWhiteout(path string) bool {
	var stat syscall.Stat_t
	return syscall.Lstat(path, &stat) == nil && stat.Mode&syscall.S_IFMT == syscall.S_IFCHR && stat.Rdev == 0
}

// containerID finds the ID of the container called name.
func containerID(t *testing.T, name string) string {
	t.Helper()
	paths, _ := filepath.Glob("/run/runt/*/state.json")
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &s) == nil && s.Name == name {
			return s.ID
		}
	}
	t.Fatalf("no container called %s", name)
	return ""
}

// TestFeatureLayers checks the layered filesystem: every container's root is
// an overlay of shared read-only layers and a writable layer of its own.
func TestFeatureLayers(t *testing.T) {
	explain(t, "Layered filesystem: shared read-only layers under a writable layer per container")

	t.Run("RootfsIsTheSharedLowerLayer", func(t *testing.T) {
		explain(t, "--rootfs is the one lower layer; each container gets its own upper layer, mounted nodev")
		r := newRuntTester(t)
		rootfs, err := filepath.Abs(r.rootfs)
		if err != nil {
			t.Fatal(err)
		}
		for i, name := range []string{"runt-feature-lower-a", "runt-feature-lower-b"} {
			id := r.start(name, "/bin/sleep", "600")
			options := overlayMount(t, id)
			if options == nil {
				t.Fatalf("%s: no overlay mounted at /var/lib/runt/containers/%s/rootfs", name, id)
			}
			if options["lowerdir"] != rootfs {
				t.Errorf("%s: lowerdir is %q, want the rootfs %s", name, options["lowerdir"], rootfs)
			}
			if want := filepath.Join("/var/lib/runt/containers", id, "upper"); options["upperdir"] != want {
				t.Errorf("%s: upperdir is %q, want %s", name, options["upperdir"], want)
			}
			if _, ok := options["nodev"]; !ok {
				t.Errorf("%s: the overlay isn't mounted nodev", name)
			}
			if i == 0 {
				observe("lowerdir=%s upperdir=%s", options["lowerdir"], options["upperdir"])
			}
		}
	})

	t.Run("WritesStayInTheContainersOwnLayer", func(t *testing.T) {
		explain(t, "a file written and a file deleted in one container change only its own upper layer")
		r := newRuntTester(t)
		idA := r.start("runt-feature-write-a", "/bin/sleep", "600")
		r.start("runt-feature-write-b", "/bin/sleep", "600")
		file := "/root/runt-feature-" + nonce

		if output, err := r.exec("runt-feature-write-a", "echo from-a > "+file+" && rm /etc/issue"); err != nil {
			t.Fatalf("A writing a file and deleting /etc/issue: %v\n%s", err, output)
		}
		if output, err := r.exec("runt-feature-write-b", "cat "+file); err == nil {
			t.Errorf("B can read A's file: %q", output)
		}
		if _, err := r.exec("runt-feature-write-b", "test -e /etc/issue"); err != nil {
			t.Error("A's deleting /etc/issue reached B")
		}
		if _, err := os.Stat(filepath.Join(r.rootfs, file)); err == nil {
			os.Remove(filepath.Join(r.rootfs, file))
			t.Error("A's file was written into the shared rootfs")
		}
		if _, err := os.Stat(filepath.Join(r.rootfs, "etc/issue")); err != nil {
			t.Errorf("A's deleting /etc/issue reached the shared rootfs: %v", err)
		}

		upper := filepath.Join("/var/lib/runt/containers", idA, "upper")
		if data, err := os.ReadFile(filepath.Join(upper, file)); err != nil || string(data) != "from-a\n" {
			t.Errorf("A's file is not in its upper layer: %v, %q", err, data)
		}
		if !isWhiteout(filepath.Join(upper, "etc/issue")) {
			t.Errorf("%s is not a whiteout", filepath.Join(upper, "etc/issue"))
		} else {
			observe("A's rm /etc/issue left a whiteout (0/0 char device) at %s", filepath.Join(upper, "etc/issue"))
		}
	})

	t.Run("WritableLayerLastsUntilRm", func(t *testing.T) {
		explain(t, "a stopped container is unmounted but keeps its writable layer until runt rm")
		r := newRuntTester(t)
		name := "runt-feature-kept"
		r.removeAtEnd(name)
		output, err := r.runt("run", "--name", name, "--rootfs", r.rootfs, "--", "/bin/sh", "-c", "echo kept > /kept")
		mustSucceed(t, "runt run", output, err)
		id := containerID(t, name)
		dir := filepath.Join("/var/lib/runt/containers", id)
		if overlayMount(t, id) != nil {
			t.Error("the overlay is still mounted after the container stopped")
		}
		if data, err := os.ReadFile(filepath.Join(dir, "upper/kept")); err != nil || string(data) != "kept\n" {
			t.Errorf("the stopped container's writable layer lost /kept: %v, %q", err, data)
		}
		if output, err := r.runt("rm", name); err != nil {
			t.Fatalf("runt rm: %v\n%s", err, output)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("runt rm left %s behind", dir)
		}
	})

	t.Run("ImageLayersAreSharedAndReadOnly", func(t *testing.T) {
		explain(t, "image layers are unpacked once, shared by its containers, never written to; OCI whiteouts become overlayfs ones")
		r := newRuntTester(t)
		b := newLayoutBuilder(t)
		base, baseDiff := b.addBaseLayer(r)
		top, topDiff := b.gzipLayer(makeTar(t,
			"feature-layer", "from-the-top-layer-"+nonce+"\n",
			"etc/.wh.issue", "",
			"srv/.wh..wh..opq", "",
			"srv/new", "new"))
		b.writeImage("layers", nil, []map[string]any{base, top}, []string{baseDiff, topDiff})
		r.removeUnpackedAtEnd(top)
		store := r.layerStore()
		topDir, baseDir := filepath.Join(store, hexDigest(top)), filepath.Join(store, hexDigest(base))

		image := []string{"--image", b.dir + ":layers"}
		idA := r.startWith("runt-feature-image-a", image, "/bin/sleep", "600")
		var before syscall.Stat_t
		if err := syscall.Stat(topDir, &before); err != nil {
			t.Fatalf("the top layer wasn't unpacked into %s: %v", topDir, err)
		}
		idB := r.startWith("runt-feature-image-b", image, "/bin/sleep", "600")
		var after syscall.Stat_t
		if err := syscall.Stat(topDir, &after); err != nil || after.Ino != before.Ino {
			t.Errorf("starting a second container unpacked the top layer again")
		}

		want := topDir + ":" + baseDir
		for _, id := range []string{idA, idB} {
			options := overlayMount(t, id)
			if options == nil || options["lowerdir"] != want {
				t.Errorf("container %s: lowerdir is %q, want %s", id, options["lowerdir"], want)
			}
		}
		observe("both containers: lowerdir=%s/{%s… (top), %s… (base)}", store, shortHex(top), shortHex(base))

		if output, err := r.exec("runt-feature-image-a", "echo a > /etc/feature-a-"+nonce); err != nil {
			t.Fatalf("writing in A: %v\n%s", err, output)
		}
		if _, err := r.exec("runt-feature-image-b", "test -e /etc/feature-a-"+nonce); err == nil {
			t.Error("B sees a file A wrote")
		}
		for _, dir := range []string{topDir, baseDir} {
			if _, err := os.Stat(filepath.Join(dir, "etc/feature-a-"+nonce)); err == nil {
				t.Errorf("A's write reached the shared layer %s", dir)
			}
		}

		output, err := r.exec("runt-feature-image-a", "cat /feature-layer; test -e /etc/issue || echo issue-gone; ls /srv")
		mustSucceed(t, "reading the image's files", output, err, "from-the-top-layer-"+nonce, "issue-gone", "new")
		if strings.Contains(output, "old") {
			t.Errorf("/srv/old from the base layer shows through the top layer's opaque /srv:\n%s", output)
		}
		if !isWhiteout(filepath.Join(topDir, "etc/issue")) {
			t.Errorf("%s/etc/issue is not a whiteout", topDir)
		}
		opaque := make([]byte, 1)
		if n, err := syscall.Getxattr(filepath.Join(topDir, "srv"), "trusted.overlay.opaque", opaque); err != nil || n != 1 || opaque[0] != 'y' {
			t.Errorf("%s/srv is not marked opaque: %v", topDir, err)
		}
	})
}

// TestFeatureOCIFormats checks OCI image layouts (--image DIR[:TAG]) and OCI
// runtime bundles (--bundle DIR, runt spec).
func TestFeatureOCIFormats(t *testing.T) {
	explain(t, "OCI formats: image layouts (--image DIR[:TAG]) and runtime bundles (--bundle, runt spec)")
	path := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	t.Run("LayoutWithGzipAndUncompressedLayers", func(t *testing.T) {
		explain(t, "a layout with an uncompressed and a gzipped layer runs, with files from both")
		r := newRuntTester(t)
		b := newLayoutBuilder(t)
		base, baseDiff := b.addBaseLayer(r)
		top, topDiff := b.gzipLayer(makeTar(t, "feature-gzip", "from-gzip-"+nonce+"\n"))
		b.writeImage("mixed", nil, []map[string]any{base, top}, []string{baseDiff, topDiff})
		r.removeUnpackedAtEnd(top)
		output, err := r.runOnce("runt-feature-mixed", "--image", b.dir+":mixed", "--", "/bin/sh", "-c", "cat /feature-gzip /srv/old")
		mustSucceed(t, "runt run --image", output, err, "from-gzip-"+nonce, "old")
	})

	t.Run("LayoutPicksImageByTag", func(t *testing.T) {
		explain(t, "DIR:TAG picks one of several images in a layout; no tag, or an unknown one, is refused")
		r := newRuntTester(t)
		b := newLayoutBuilder(t)
		base, baseDiff := b.addBaseLayer(r)
		first, _ := b.image(runtime.GOARCH, map[string]any{"Cmd": []string{"/bin/echo", "i-am-first"}, "Env": []string{path}}, []map[string]any{base}, []string{baseDiff})
		second, _ := b.image(runtime.GOARCH, map[string]any{"Cmd": []string{"/bin/echo", "i-am-second"}, "Env": []string{path}}, []map[string]any{base}, []string{baseDiff})
		b.writeIndex(tagged(first, "first"), tagged(second, "second"))
		for _, tag := range []string{"first", "second"} {
			output, err := r.runOnce("runt-feature-tag", "--image", b.dir+":"+tag)
			mustSucceed(t, "runt run --image DIR:"+tag, output, err, "i-am-"+tag)
		}
		output, err := r.runOnce("runt-feature-tag", "--image", b.dir)
		mustFail(t, "a layout with two images and no tag", output, err, "add :TAG")
		output, err = r.runOnce("runt-feature-tag", "--image", b.dir+":third")
		mustFail(t, "a tag the layout doesn't have", output, err, `no image tagged "third"`)
	})

	t.Run("LayoutFollowsPlatformIndex", func(t *testing.T) {
		explain(t, "a multi-platform index runs its linux/%s image; one without it is refused", runtime.GOARCH)
		r := newRuntTester(t)
		b := newLayoutBuilder(t)
		base, baseDiff := b.addBaseLayer(r)
		other := otherArchitecture()
		mine, _ := b.image(runtime.GOARCH, map[string]any{"Cmd": []string{"/bin/echo", "picked-" + runtime.GOARCH}, "Env": []string{path}}, []map[string]any{base}, []string{baseDiff})
		theirs, _ := b.image(other, map[string]any{"Cmd": []string{"/bin/echo", "picked-" + other}, "Env": []string{path}}, []map[string]any{base}, []string{baseDiff})
		index := map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json"}
		index["manifests"] = []any{withPlatform(theirs, other), withPlatform(mine, runtime.GOARCH)}
		multi := b.jsonBlob("application/vnd.oci.image.index.v1+json", index)
		index["manifests"] = []any{withPlatform(theirs, other)}
		foreign := b.jsonBlob("application/vnd.oci.image.index.v1+json", index)
		b.writeIndex(tagged(multi, "multi"), tagged(foreign, "foreign"))

		output, err := r.runOnce("runt-feature-platform", "--image", b.dir+":multi")
		mustSucceed(t, "runt run of a multi-platform image", output, err, "picked-"+runtime.GOARCH)
		output, err = r.runOnce("runt-feature-platform", "--image", b.dir+":foreign")
		mustFail(t, "an image only for linux/"+other, output, err, "has no linux/"+runtime.GOARCH+" variant")
	})

	t.Run("LayoutRejectsZstdLayers", func(t *testing.T) {
		explain(t, "zstd-compressed layers are refused")
		r := newRuntTester(t)
		b := newLayoutBuilder(t)
		base, baseDiff := b.addBaseLayer(r)
		zstd := b.blob("application/vnd.oci.image.layer.v1.tar+zstd", []byte("not really zstd "+nonce))
		b.writeImage("zstd", map[string]any{"Cmd": []string{"/bin/true"}}, []map[string]any{base, zstd}, []string{baseDiff, zstd["digest"].(string)})
		output, err := r.runOnce("runt-feature-zstd", "--image", b.dir+":zstd")
		mustFail(t, "an image with a zstd layer", output, err, "zstd-compressed layers are not supported")
	})

	t.Run("LayoutChecksLayerDigests", func(t *testing.T) {
		explain(t, "a layer that doesn't match its digest or diff_id is refused, and nothing is left unpacked")
		r := newRuntTester(t)
		b := newLayoutBuilder(t)
		base, baseDiff := b.addBaseLayer(r)
		store := r.layerStore()

		damaged, damagedDiff := b.tarLayer(makeTar(t, "feature-damaged", "intact-"+nonce))
		data, err := os.ReadFile(b.blobFile(damaged))
		if err != nil {
			t.Fatal(err)
		}
		data[512] ^= 1 // the file's content, just after its tar header
		if err := os.WriteFile(b.blobFile(damaged), data, 0o644); err != nil {
			t.Fatal(err)
		}
		b.writeImage("damaged", map[string]any{"Cmd": []string{"/bin/true"}}, []map[string]any{base, damaged}, []string{baseDiff, damagedDiff})
		output, err := r.runOnce("runt-feature-digest", "--image", b.dir+":damaged")
		mustFail(t, "a layer changed after its digest was taken", output, err, "does not match its digest")
		if _, err := os.Stat(filepath.Join(store, hexDigest(damaged))); err == nil {
			t.Error("the damaged layer was left in the layer store")
		}

		swapped, _ := b.gzipLayer(makeTar(t, "feature-swapped", "real-"+nonce))
		_, otherDiff := b.gzipLayer(makeTar(t, "feature-swapped", "claimed-"+nonce))
		b.writeImage("swapped", map[string]any{"Cmd": []string{"/bin/true"}}, []map[string]any{base, swapped}, []string{baseDiff, otherDiff})
		output, err = r.runOnce("runt-feature-digest", "--image", b.dir+":swapped")
		mustFail(t, "a layer whose content isn't what the configuration's diff_ids says", output, err, "does not match its diff ID")
		if _, err := os.Stat(filepath.Join(store, hexDigest(swapped))); err == nil {
			t.Error("the swapped layer was left in the layer store")
		}
	})

	t.Run("SpecWritesConfig", func(t *testing.T) {
		explain(t, "runt spec writes a starting config.json, and won't overwrite one")
		r := newRuntTester(t)
		dir := t.TempDir()
		output, err := r.runt("spec", "--bundle", dir, "--rootfs", r.rootfs)
		mustSucceed(t, "runt spec", output, err)
		var spec struct {
			OCIVersion string `json:"ociVersion"`
			Process    struct {
				Terminal bool     `json:"terminal"`
				Args     []string `json:"args"`
			} `json:"process"`
			Root struct {
				Path     string `json:"path"`
				Readonly bool   `json:"readonly"`
			} `json:"root"`
			Linux struct {
				UIDMappings []struct {
					ContainerID, HostID, Size int
				} `json:"uidMappings"`
			} `json:"linux"`
		}
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &spec); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(spec.OCIVersion, "1.") || spec.Root.Path != r.rootfs || !spec.Root.Readonly ||
			!spec.Process.Terminal || !slices.Equal(spec.Process.Args, []string{"sh"}) {
			t.Errorf("config.json isn't the starting spec:\n%s", data)
		}
		var rootStat syscall.Stat_t
		if err := syscall.Stat(r.rootfs, &rootStat); err != nil {
			t.Fatal(err)
		}
		if len(spec.Linux.UIDMappings) != 1 || spec.Linux.UIDMappings[0].ContainerID != 0 || spec.Linux.UIDMappings[0].HostID != int(rootStat.Uid) {
			t.Errorf("uidMappings %+v don't map container root onto the rootfs's owner, host uid %d (if you made the rootfs with USERNS_USER, this is expected)",
				spec.Linux.UIDMappings, rootStat.Uid)
		}
		observe("root.path %s (readonly), args %v, container uid 0 -> host uid %d", spec.Root.Path, spec.Process.Args, rootStat.Uid)
		output, err = r.runt("spec", "--bundle", dir, "--rootfs", r.rootfs)
		mustFail(t, "a second runt spec in the same bundle", output, err, "already exists")
	})

	t.Run("BundleRunsConfig", func(t *testing.T) {
		explain(t, "--bundle takes the command, env (not runt's), cwd, user, hostname, and read-only root from config.json")
		r := newRuntTester(t)
		dir, spec := writeFeatureBundle(t, r)
		process := spec["process"].(map[string]any)
		process["args"] = []string{"/bin/sh", "-c", "hostname; pwd; id; echo FOO=$FOO; echo LEAK=$RUNT_FEATURE_HOST_ONLY; touch /runt-feature-bundle"}
		process["env"] = []string{"PATH=/usr/bin:/bin", "FOO=from-config-json"}
		process["cwd"] = "/tmp"
		process["user"] = map[string]any{"uid": 1, "gid": 1}
		spec["hostname"] = "runt-feature-bundle"
		saveBundle(t, dir, spec)
		// runt's own environment, which a bundle's container shouldn't see.
		t.Setenv("RUNT_FEATURE_HOST_ONLY", "leaked")

		output, _ := r.runOnce("runt-feature-bundle", "--bundle", dir)
		for _, want := range []string{"runt-feature-bundle\n", "/tmp\n", "uid=1(daemon) gid=1(daemon)", "FOO=from-config-json\n", "LEAK=\n", "Read-only file system"} {
			if !strings.Contains(output, want) {
				t.Errorf("output is missing %q:\n%s", want, output)
			}
		}
		if _, err := os.Stat(filepath.Join(r.rootfs, "runt-feature-bundle")); err == nil {
			os.Remove(filepath.Join(r.rootfs, "runt-feature-bundle"))
			t.Error("the container wrote to its read-only root filesystem")
		}

		// For contrast, a --rootfs container does get runt's environment.
		output, err := r.runOnce("runt-feature-bundle-env", "--rootfs", r.rootfs, "--", "/bin/sh", "-c", "echo LEAK=$RUNT_FEATURE_HOST_ONLY")
		mustSucceed(t, "runt run --rootfs", output, err, "LEAK=leaked")
	})

	t.Run("BundleRejectsWhatRuntCantDo", func(t *testing.T) {
		explain(t, "a config.json asking for a bind mount of / or the host's network namespace is refused")
		r := newRuntTester(t)
		dir, spec := writeFeatureBundle(t, r)
		mounts := spec["mounts"].([]any)
		spec["mounts"] = append(mounts, map[string]any{"destination": "/mnt/host", "type": "bind", "source": "/", "options": []string{"rbind"}})
		saveBundle(t, dir, spec)
		output, err := r.runOnce("runt-feature-bundle-bad", "--bundle", dir)
		mustFail(t, "a bind mount of the host's /", output, err, "mount at /mnt/host is not supported")

		dir, spec = writeFeatureBundle(t, r)
		linux := spec["linux"].(map[string]any)
		linux["namespaces"] = append(linux["namespaces"].([]any), map[string]any{"type": "network", "path": "/proc/1/ns/net"})
		saveBundle(t, dir, spec)
		output, err = r.runOnce("runt-feature-bundle-bad", "--bundle", dir)
		mustFail(t, "joining the host's network namespace", output, err, "joining an existing network namespace is not supported")
	})
}

// writeFeatureBundle makes a bundle with runt spec, set to run without a
// terminal, and returns its directory and config.json.
func writeFeatureBundle(t *testing.T, r *runtTester) (string, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	if output, err := r.runt("spec", "--bundle", dir, "--rootfs", r.rootfs); err != nil {
		t.Fatalf("runt spec: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	process := spec["process"].(map[string]any)
	process["terminal"] = false
	process["args"] = []string{"/bin/true"}
	return dir, spec
}

// saveBundle writes spec back as the bundle's config.json, after a check
// has changed it.
func saveBundle(t *testing.T, dir string, spec map[string]any) {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// testRegistry serves an OCI image layout as the registry repository
// runt-feature/app, the way Docker Hub and other registries do, with a
// choice of authentication. It records the requests it gets.
type testRegistry struct {
	server *httptest.Server
	host   string
	layout string
	// auth is "" for none, "basic", "bearer" for a token anyone can have
	// (Docker Hub's public images), or "bearer-login" for a token only
	// for the right user name and password.
	auth string
	// tags maps tags to the digest of a manifest or index in the layout.
	tags map[string]string
	// corrupt names blobs to serve with a byte changed.
	corrupt map[string]bool

	mu  sync.Mutex
	log []string
}

const (
	registryRepository = "runt-feature/app"
	registryUser       = "runt"
	registryPassword   = "secret"
	registryToken      = "runt-feature-token"
	registryLoginToken = "runt-feature-login-token"
)

// newTestRegistry starts a registry serving layout, on a free local port,
// with httptest.NewServer, which calls reg.serve for each request. It stops
// when the test ends.
func newTestRegistry(t *testing.T, layout, auth string) *testRegistry {
	reg := &testRegistry{layout: layout, auth: auth, tags: map[string]string{}, corrupt: map[string]bool{}}
	reg.server = httptest.NewServer(http.HandlerFunc(reg.serve))
	reg.host = strings.TrimPrefix(reg.server.URL, "http://")
	t.Cleanup(reg.server.Close)
	return reg
}

// tag makes tag name the manifest or index desc describes.
func (reg *testRegistry) tag(tag string, desc map[string]any) {
	reg.tags[tag] = desc["digest"].(string)
}

// name is the full name of the image tagged tag, as runt pull takes it.
func (reg *testRegistry) name(tag string) string {
	return reg.host + "/" + registryRepository + ":" + tag
}

// requests returns the requests made since the last call.
func (reg *testRegistry) requests() []string {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	log := reg.log
	reg.log = nil
	return log
}

// authorized reports whether req carries the credentials the registry's
// kind of authentication wants.
func (reg *testRegistry) authorized(req *http.Request) bool {
	switch reg.auth {
	case "basic":
		user, password, _ := req.BasicAuth()
		return user == registryUser && password == registryPassword
	case "bearer", "bearer-login":
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		// A token without a scope, as runt login gets, is only good for the
		// API version check.
		return token == registryToken || (token == registryLoginToken && req.URL.Path == "/v2/")
	}
	return true
}

// serve answers one request: it logs it, then plays the token server, or
// challenges a request without the right credentials (401 and a
// WWW-Authenticate header), or serves a manifest or blob from the layout.
// What is written to w is the response.
func (reg *testRegistry) serve(w http.ResponseWriter, req *http.Request) {
	scheme, _, _ := strings.Cut(req.Header.Get("Authorization"), " ")
	reg.mu.Lock()
	reg.log = append(reg.log, fmt.Sprintf("%s %s auth=%s", req.Method, req.URL.RequestURI(), scheme))
	reg.mu.Unlock()

	if req.URL.Path == "/token" {
		// The token server: the scope says what the token is for. Logging
		// in asks for one without a scope, as Docker Hub allows, which
		// only checks the user name and password.
		token := registryToken
		switch req.URL.Query().Get("scope") {
		case "repository:" + registryRepository + ":pull":
		case "":
			token = registryLoginToken
		default:
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		user, password, _ := req.BasicAuth()
		if reg.auth == "bearer-login" && (user != registryUser || password != registryPassword) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": token})
		return
	}
	if !reg.authorized(req) {
		if reg.auth == "basic" {
			w.Header().Set("WWW-Authenticate", `Basic realm="runt-feature"`)
		} else {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="runt-feature"`, reg.host))
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	prefix := "/v2/" + registryRepository + "/"
	var digest string
	switch {
	case req.URL.Path == "/v2/":
		return
	case strings.HasPrefix(req.URL.Path, prefix+"manifests/"):
		ref := strings.TrimPrefix(req.URL.Path, prefix+"manifests/")
		digest = ref
		if tagged, ok := reg.tags[ref]; ok {
			digest = tagged
		}
	case strings.HasPrefix(req.URL.Path, prefix+"blobs/"):
		digest = strings.TrimPrefix(req.URL.Path, prefix+"blobs/")
	default:
		http.NotFound(w, req)
		return
	}
	data, err := os.ReadFile(filepath.Join(reg.layout, "blobs/sha256", filepath.Base(strings.TrimPrefix(digest, "sha256:"))))
	if err != nil {
		http.NotFound(w, req)
		return
	}
	if strings.Contains(req.URL.Path, "/manifests/") {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		json.Unmarshal(data, &probe)
		w.Header().Set("Content-Type", probe.MediaType)
	}
	// Corrupt a copy: ^= 1 flips the lowest bit of the middle byte.
	if reg.corrupt[digest] {
		data = append([]byte{}, data...)
		data[len(data)/2] ^= 1
	}
	w.Write(data)
}

// removeImageAtEnd removes an image from runt's store when the test ends.
func (r *runtTester) removeImageAtEnd(name string) {
	r.t.Cleanup(func() { r.runt("rmi", "-f", name) })
}

// storeEntry is an entry of the image store's index.json.
type storeEntry struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform"`
}

// readStoreIndex reads the image store's index.json. Before the first pull
// there is none.
func readStoreIndex(t *testing.T) []storeEntry {
	t.Helper()
	var index struct {
		Manifests []storeEntry `json:"manifests"`
	}
	data, err := os.ReadFile(filepath.Join(imageStoreDir, "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	return index.Manifests
}

// findStoreEntry returns the image store's entry for the full image name.
func findStoreEntry(t *testing.T, name string) (storeEntry, bool) {
	t.Helper()
	for _, entry := range readStoreIndex(t) {
		if entry.Annotations["io.containerd.image.name"] == name {
			return entry, true
		}
	}
	return storeEntry{}, false
}

// storeHasBlob reports whether the image store holds the blob desc describes.
func storeHasBlob(desc map[string]any) bool {
	_, err := os.Stat(filepath.Join(imageStoreDir, "blobs/sha256", hexDigest(desc)))
	return err == nil
}

// requested reports whether any request in log contains text.
// slices.ContainsFunc asks the function about each element in turn.
func requested(log []string, text string) bool {
	return slices.ContainsFunc(log, func(request string) bool { return strings.Contains(request, text) })
}

// TestFeatureRegistry checks pulling images from a registry: manifests,
// multi-platform indexes, layers, and authentication.
func TestFeatureRegistry(t *testing.T) {
	explain(t, "Registry pulls: the OCI distribution API, with Basic and Bearer auth (test registries on localhost)")
	r := newRuntTester(t)
	b := newLayoutBuilder(t)
	path := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	base, baseDiff := b.addBaseLayer(r)
	top, topDiff := b.gzipLayer(makeTar(t, "feature-pulled", "pulled-"+nonce+"\n"))
	app, appConfig := b.image(runtime.GOARCH, map[string]any{"Cmd": []string{"/bin/cat", "/feature-pulled"}, "Env": []string{path}},
		[]map[string]any{base, top}, []string{baseDiff, topDiff})
	other := otherArchitecture()
	foreign, _ := b.image(other, map[string]any{"Cmd": []string{"/bin/true"}}, []map[string]any{base, top}, []string{baseDiff, topDiff})
	index := b.jsonBlob("application/vnd.oci.image.index.v1+json", map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []any{withPlatform(foreign, other), withPlatform(app, runtime.GOARCH)},
	})
	broken, brokenDiff := b.gzipLayer(makeTar(t, "feature-broken", "broken-"+nonce))
	brokenImage, _ := b.image(runtime.GOARCH, map[string]any{"Cmd": []string{"/bin/true"}}, []map[string]any{base, broken}, []string{baseDiff, brokenDiff})
	// Credentials go in a config.json of the test's own, not root's.
	dockerConfig := func(t *testing.T) string {
		dir := t.TempDir()
		t.Setenv("DOCKER_CONFIG", dir)
		return dir
	}

	t.Run("PullsManifestConfigAndLayers", func(t *testing.T) {
		explain(t, "pull fetches the manifest, config, and layers; pulling again fetches only the manifest")
		r := newRuntTester(t)
		dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "")
		reg.tag("v1", app)
		r.removeImageAtEnd(reg.name("v1"))
		output, err := r.runt("pull", reg.name("v1"))
		mustSucceed(t, "runt pull", output, err, "Pull complete", "Status: Downloaded newer image")
		log := reg.requests()
		for _, want := range []string{"/manifests/v1", "/blobs/" + appConfig["digest"].(string), "/blobs/" + top["digest"].(string)} {
			if !requested(log, want) {
				t.Errorf("runt pull never asked for %s:\n%s", want, strings.Join(log, "\n"))
			}
		}

		output, err = r.runt("pull", reg.name("v1"))
		mustSucceed(t, "runt pull, again", output, err, "Already exists", "Status: Image is up to date")
		if log := reg.requests(); requested(log, "/blobs/") {
			t.Errorf("the second pull downloaded blobs again:\n%s", strings.Join(log, "\n"))
		}
	})

	t.Run("BearerTokenWithoutCredentials", func(t *testing.T) {
		explain(t, "a Bearer challenge, as Docker Hub sends, gets a pull-scoped token that goes with every request after")
		r := newRuntTester(t)
		dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "bearer")
		reg.tag("v1", app)
		r.removeImageAtEnd(reg.name("v1"))
		output, err := r.runt("pull", reg.name("v1"))
		mustSucceed(t, "runt pull", output, err, "Status: Downloaded newer image")
		log := reg.requests()
		if !requested(log, "/token?") || !requested(log, "scope=repository%3A"+strings.ReplaceAll(registryRepository, "/", "%2F")+"%3Apull") {
			t.Errorf("runt didn't ask the token server for a pull token:\n%s", strings.Join(log, "\n"))
		}
		if !requested(log, "/blobs/"+top["digest"].(string)+" auth=Bearer") {
			t.Errorf("the layer wasn't fetched with the token:\n%s", strings.Join(log, "\n"))
		}
		for _, request := range log {
			if strings.Contains(request, "/token?") {
				observe("%s", request)
			}
		}
	})

	t.Run("BearerTokenWithLogin", func(t *testing.T) {
		explain(t, "a private repository's token needs a password: the pull fails until runt login, then works")
		r := newRuntTester(t)
		dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "bearer-login")
		reg.tag("v1", app)
		r.removeImageAtEnd(reg.name("v1"))
		output, err := r.runt("pull", reg.name("v1"))
		mustFail(t, "runt pull before runt login", output, err, "log in again with runt login")
		runtLogin(t, r, reg.host, registryPassword)
		output, err = r.runt("pull", reg.name("v1"))
		mustSucceed(t, "runt pull after runt login", output, err, "Status: Downloaded newer image")
		if log := reg.requests(); !requested(log, "/token?") {
			t.Errorf("no token was asked for:\n%s", strings.Join(log, "\n"))
		}
	})

	t.Run("BasicAuthWithLoginAndLogout", func(t *testing.T) {
		explain(t, "Basic auth: runt login checks the password and saves it in config.json; runt logout removes it")
		r := newRuntTester(t)
		dir := dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "basic")
		reg.tag("v1", app)
		r.removeImageAtEnd(reg.name("v1"))
		output, err := r.runt("pull", reg.name("v1"))
		mustFail(t, "runt pull before runt login", output, err, "log in with runt login")

		login := exec.Command(r.binary, "login", "-u", registryUser, "--password-stdin", reg.host)
		login.Stdin = strings.NewReader("wrong\n")
		output2, err := login.CombinedOutput()
		mustFail(t, "runt login with the wrong password", string(output2), err, "401")

		runtLogin(t, r, reg.host, registryPassword)
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}
		json.Unmarshal(data, &config)
		if want := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPassword)); config.Auths[reg.host].Auth != want {
			t.Errorf("config.json doesn't hold the credentials under %s:\n%s", reg.host, data)
		}
		output, err = r.runt("pull", reg.name("v1"))
		mustSucceed(t, "runt pull after runt login", output, err, "Status: Downloaded newer image")
		if log := reg.requests(); !requested(log, "/blobs/"+top["digest"].(string)+" auth=Basic") {
			t.Errorf("the layer wasn't fetched with Basic authentication:\n%s", strings.Join(log, "\n"))
		}

		output, err = r.runt("logout", reg.host)
		mustSucceed(t, "runt logout", output, err, "Removed login credentials")
		if data, _ := os.ReadFile(filepath.Join(dir, "config.json")); strings.Contains(string(data), reg.host) {
			t.Errorf("runt logout left the credentials in config.json:\n%s", data)
		}
	})

	t.Run("CredentialsFromDocker", func(t *testing.T) {
		explain(t, "credentials in Docker's config.json, or from a docker-credential helper, work without runt login")
		r := newRuntTester(t)
		dir := dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "basic")
		reg.tag("v1", app)
		r.removeImageAtEnd(reg.name("v1"))
		auth := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPassword))
		writeFile(t, filepath.Join(dir, "config.json"), fmt.Sprintf(`{"auths": {%q: {"auth": %q}}}`, reg.host, auth), 0o600)
		output, err := r.runt("pull", reg.name("v1"))
		mustSucceed(t, "runt pull with an auths entry written by hand", output, err, "Status: Downloaded newer image")
		r.runt("rmi", "-f", reg.name("v1"))

		helpers := t.TempDir()
		writeFile(t, filepath.Join(helpers, "docker-credential-runtfeature"), fmt.Sprintf(`#!/bin/sh
[ "$1" = get ] || exit 1
[ "$(cat)" = %q ] || { echo "credentials not found in native keychain"; exit 1; }
echo '{"ServerURL": %q, "Username": %q, "Secret": %q}'
`, reg.host, reg.host, registryUser, registryPassword), 0o755)
		t.Setenv("PATH", helpers+":"+os.Getenv("PATH"))
		writeFile(t, filepath.Join(dir, "config.json"), `{"credsStore": "runtfeature"}`, 0o600)
		output, err = r.runt("pull", reg.name("v1"))
		mustSucceed(t, "runt pull with a credential helper", output, err, "Status: Downloaded newer image")
	})

	t.Run("FollowsMultiPlatformIndex", func(t *testing.T) {
		explain(t, "from a multi-platform index, only the linux/%s manifest is fetched and stored", runtime.GOARCH)
		r := newRuntTester(t)
		dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "")
		reg.tag("multi", index)
		r.removeImageAtEnd(reg.name("multi"))
		output, err := r.runt("pull", reg.name("multi"))
		mustSucceed(t, "runt pull of a multi-platform image", output, err, "Status: Downloaded newer image")
		log := reg.requests()
		if !requested(log, "/manifests/"+app["digest"].(string)) || requested(log, "/manifests/"+foreign["digest"].(string)) {
			t.Errorf("runt pull didn't fetch only this platform's manifest:\n%s", strings.Join(log, "\n"))
		}
		entry, ok := findStoreEntry(t, reg.name("multi"))
		if !ok || entry.Digest != app["digest"].(string) || entry.Platform == nil || entry.Platform.Architecture != runtime.GOARCH {
			t.Errorf("the store's entry is %+v; want the linux/%s manifest %s", entry, runtime.GOARCH, app["digest"])
		}
	})

	t.Run("RunPullsMissingImage", func(t *testing.T) {
		explain(t, "run --image pulls an image that isn't in the store")
		r := newRuntTester(t)
		dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "")
		reg.tag("auto", app)
		r.removeImageAtEnd(reg.name("auto"))
		output, err := r.runOnce("runt-feature-autopull", "--image", reg.name("auto"))
		mustSucceed(t, "runt run of an image not in the store", output, err, "Unable to find image", "Pull complete", "pulled-"+nonce)
	})

	t.Run("RejectsCorruptBlob", func(t *testing.T) {
		explain(t, "a layer damaged in transit fails its digest check, and nothing is stored")
		r := newRuntTester(t)
		dockerConfig(t)
		reg := newTestRegistry(t, b.dir, "")
		reg.tag("broken", brokenImage)
		reg.corrupt[broken["digest"].(string)] = true
		r.removeImageAtEnd(reg.name("broken"))
		output, err := r.runt("pull", reg.name("broken"))
		mustFail(t, "runt pull of a damaged layer", output, err, "does not match its digest")
		if _, ok := findStoreEntry(t, reg.name("broken")); ok || storeHasBlob(broken) {
			t.Error("the damaged image, or its layer, is in the store")
		}
	})
}

// runtLogin logs in to registry host as registryUser with password.
func runtLogin(t *testing.T, r *runtTester, host, password string) {
	t.Helper()
	login := exec.Command(r.binary, "login", "-u", registryUser, "--password-stdin", host)
	login.Stdin = strings.NewReader(password + "\n")
	output, err := login.CombinedOutput()
	mustSucceed(t, "runt login", string(output), err, "Login Succeeded")
}

// writeFile writes content to path with permissions mode, and stops the
// test if it can't.
func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// shortHex is the first 12 hex digits of a blob's digest, as runt images
// shows an image ID.
func shortHex(desc map[string]any) string {
	return hexDigest(desc)[:12]
}

// imageRow returns the fields of the row of runt images output for
// repository and tag, or nil.
func imageRow(output, repository, tag string) []string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == repository && fields[1] == tag {
			return fields
		}
	}
	return nil
}

// TestFeatureImageStore checks the local image store and runt images and
// runt rmi.
func TestFeatureImageStore(t *testing.T) {
	explain(t, "Image store: an OCI image layout at /var/lib/runt/images, with runt images and runt rmi")
	r := newRuntTester(t)
	// Images left by an earlier run that stopped part way would keep the
	// shared base layer alive.
	for _, entry := range readStoreIndex(t) {
		if name := entry.Annotations["io.containerd.image.name"]; strings.Contains(name, "/"+registryRepository) {
			r.runt("rmi", "-f", name)
		}
	}
	b := newLayoutBuilder(t)
	path := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	base, baseDiff := b.addBaseLayer(r)
	topOne, topOneDiff := b.gzipLayer(makeTar(t, "feature-store", "image-one-"+nonce+"\n"))
	topTwo, topTwoDiff := b.gzipLayer(makeTar(t, "feature-store", "image-two-"+nonce+"\n"))
	defaults := map[string]any{"Cmd": []string{"/bin/cat", "/feature-store"}, "Env": []string{path}}
	one, oneConfig := b.image(runtime.GOARCH, defaults, []map[string]any{base, topOne}, []string{baseDiff, topOneDiff})
	two, twoConfig := b.image(runtime.GOARCH, defaults, []map[string]any{base, topTwo}, []string{baseDiff, topTwoDiff})
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	reg := newTestRegistry(t, b.dir, "")
	tagOne, tagAlias, tagTwo := "one-"+nonce, "alias-"+nonce, "two-"+nonce
	reg.tag(tagOne, one)
	reg.tag(tagAlias, one)
	reg.tag(tagTwo, two)
	repository := reg.host + "/" + registryRepository
	idOne, idTwo := shortHex(oneConfig), shortHex(twoConfig)
	for _, tag := range []string{tagOne, tagAlias, tagTwo} {
		r.removeImageAtEnd(reg.name(tag))
	}
	store := r.layerStore()
	unpacked := func(layer map[string]any) bool {
		_, err := os.Stat(filepath.Join(store, hexDigest(layer)))
		return err == nil
	}

	if !t.Run("StoreIsAnOCIImageLayout", func(t *testing.T) {
		explain(t, "pulled images are kept as an OCI image layout, which --image can run as one")
		r := newRuntTester(t)
		for _, tag := range []string{tagOne, tagAlias, tagTwo} {
			output, err := r.runt("pull", reg.name(tag))
			mustSucceed(t, "runt pull "+tag, output, err, "Status: Downloaded newer image")
		}
		data, err := os.ReadFile(filepath.Join(imageStoreDir, "oci-layout"))
		if err != nil || !strings.Contains(string(data), `"imageLayoutVersion"`) {
			t.Errorf("the store's oci-layout file is missing or wrong: %v, %s", err, data)
		}
		for tag, manifest := range map[string]map[string]any{tagOne: one, tagAlias: one, tagTwo: two} {
			entry, ok := findStoreEntry(t, reg.name(tag))
			if !ok || entry.Digest != manifest["digest"] || entry.Annotations["org.opencontainers.image.ref.name"] != tag {
				t.Errorf("index.json's entry for %s is %+v", reg.name(tag), entry)
			}
		}
		for _, blob := range []map[string]any{one, oneConfig, two, twoConfig, base, topOne, topTwo} {
			if !storeHasBlob(blob) {
				t.Errorf("the store is missing blob %s", blob["digest"])
			}
		}

		output, err := r.runOnce("runt-feature-store-layout", "--image", imageStoreDir+":"+tagOne)
		mustSucceed(t, "runt run --image "+imageStoreDir+":"+tagOne, output, err, "image-one-"+nonce)
	}) {
		t.FailNow()
	}

	t.Run("ImagesListsNamesAndIDs", func(t *testing.T) {
		explain(t, "runt images lists each name with its image ID; images -q lists each ID once")
		r := newRuntTester(t)
		output, err := r.runt("images")
		mustSucceed(t, "runt images", output, err)
		for tag, id := range map[string]string{tagOne: idOne, tagAlias: idOne, tagTwo: idTwo} {
			if row := imageRow(output, repository, tag); row == nil || row[2] != id {
				t.Errorf("runt images has no row %s %s %s:\n%s", repository, tag, id, output)
			}
		}
		output, err = r.runt("images", "-q")
		mustSucceed(t, "runt images -q", output, err)
		ids := strings.Fields(output)
		if !slices.Contains(ids, idTwo) || strings.Count(output, idOne) != 1 {
			t.Errorf("runt images -q should list %s once and %s:\n%s", idOne, idTwo, output)
		}
	})

	t.Run("PullByDigest", func(t *testing.T) {
		explain(t, "a pull by digest shows in images --digests; rmi of that name only untags it")
		r := newRuntTester(t)
		byDigest := repository + "@" + two["digest"].(string)
		r.removeImageAtEnd(byDigest)
		output, err := r.runt("pull", byDigest)
		mustSucceed(t, "runt pull by digest", output, err, "Digest: "+two["digest"].(string))
		output, err = r.runt("images", "--digests")
		mustSucceed(t, "runt images --digests", output, err)
		if row := imageRow(output, repository, "<none>"); row == nil || row[2] != two["digest"] || row[3] != idTwo {
			t.Errorf("runt images --digests has no row for %s:\n%s", byDigest, output)
		}
		output, err = r.runt("rmi", byDigest)
		mustSucceed(t, "runt rmi by digest", output, err, "Untagged")
		if strings.Contains(output, "Deleted") {
			t.Errorf("runt rmi of one of two names deleted the image:\n%s", output)
		}
	})

	t.Run("RunByImageID", func(t *testing.T) {
		explain(t, "run --image takes an image ID as well as a name")
		r := newRuntTester(t)
		output, err := r.runOnce("runt-feature-store-id", "--image", idOne)
		mustSucceed(t, "runt run --image "+idOne, output, err, "image-one-"+nonce)
	})

	t.Run("RmiUntagsThenDeletes", func(t *testing.T) {
		explain(t, "rmi untags a name, deletes the image with its last one, and keeps layers another image needs")
		r := newRuntTester(t)
		output, err := r.runt("rmi", idOne)
		mustFail(t, "runt rmi of an ID with two names", output, err, "has several names")
		output, err = r.runt("rmi", reg.name(tagAlias))
		mustSucceed(t, "runt rmi of one name", output, err, "Untagged")
		if strings.Contains(output, "Deleted") || !storeHasBlob(topOne) {
			t.Errorf("removing one of two names deleted the image:\n%s", output)
		}
		if !unpacked(topOne) || !unpacked(base) {
			t.Fatal("image one's layers weren't unpacked by running it")
		}
		output, err = r.runt("rmi", reg.name(tagOne))
		mustSucceed(t, "runt rmi of the last name", output, err, "Untagged", "Deleted: "+oneConfig["digest"].(string))
		topGone := !storeHasBlob(topOne) && !storeHasBlob(one) && !storeHasBlob(oneConfig) && !unpacked(topOne)
		if !topGone {
			t.Error("image one's manifest, configuration, or top layer is still in the store")
		}
		if !storeHasBlob(base) || !unpacked(base) {
			t.Error("the base layer went too, though image two still needs it")
		} else if topGone {
			observe("image one's top layer %s… is gone; the base layer %s… stays for image two", shortHex(topOne), shortHex(base))
		}
	})

	t.Run("RmiRefusesImageInUse", func(t *testing.T) {
		explain(t, "rmi refuses an image a container uses; once both are gone, so is every blob and layer")
		r := newRuntTester(t)
		name := "runt-feature-store-user"
		r.removeAtEnd(name)
		output, err := r.runt("run", "--name", name, "--image", reg.name(tagTwo))
		mustSucceed(t, "runt run", output, err, "image-two-"+nonce)
		output, err = r.runt("rmi", reg.name(tagTwo))
		mustFail(t, "runt rmi of an image a container uses", output, err, "is used by container")
		if output, err := r.runt("rm", name); err != nil {
			t.Fatalf("runt rm: %v\n%s", err, output)
		}
		output, err = r.runt("rmi", reg.name(tagTwo))
		mustSucceed(t, "runt rmi once the container is gone", output, err, "Deleted: "+twoConfig["digest"].(string))
		for what, gone := range map[string]bool{
			"image two's top layer blob":      !storeHasBlob(topTwo),
			"image two's unpacked top layer":  !unpacked(topTwo),
			"the base layer blob":             !storeHasBlob(base),
			"the unpacked base layer":         !unpacked(base),
			"image two's manifest and config": !storeHasBlob(two) && !storeHasBlob(twoConfig),
		} {
			if !gone {
				t.Errorf("%s is still there, though nothing needs it", what)
			}
		}
	})
}

// TestFeatureImageConfig checks that an image's configuration is applied, and
// that the command line overrides it.
func TestFeatureImageConfig(t *testing.T) {
	explain(t, "Image config: ENTRYPOINT, CMD, ENV, WORKDIR, and USER, and the options that override them")
	r := newRuntTester(t)
	b := newLayoutBuilder(t)
	base, baseDiff := b.addBaseLayer(r)
	passwd, err := os.ReadFile(filepath.Join(r.rootfs, "etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	group, err := os.ReadFile(filepath.Join(r.rootfs, "etc/group"))
	if err != nil {
		t.Fatal(err)
	}
	// A user and groups only the image has.
	accounts, accountsDiff := b.gzipLayer(makeTar(t,
		"etc/passwd", string(passwd)+"runtfeature:x:4321:4322:runt feature test:/home/runtfeature:/bin/sh\n",
		"etc/group", string(group)+"runtfeature:x:4322:\nruntextra:x:4323:runtfeature\n",
		"feature-config-"+nonce, ""))
	r.removeUnpackedAtEnd(accounts)
	layers, diffIDs := []map[string]any{base, accounts}, []string{baseDiff, accountsDiff}
	configured, _ := b.image(runtime.GOARCH, map[string]any{
		"Entrypoint": []string{"/bin/sh", "-c"},
		"Cmd":        []string{"echo default-cmd; id; pwd; echo FOO=$FOO BAR=$BAR HOME=$HOME"},
		"Env":        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "FOO=from-image", "BAR=also-from-image"},
		"WorkingDir": "/srv/feature",
		"User":       "runtfeature",
	}, layers, diffIDs)
	bare, _ := b.image(runtime.GOARCH, map[string]any{"Env": []string{"PATH=/usr/bin:/bin"}}, layers, diffIDs)
	b.writeIndex(tagged(configured, "configured"), tagged(bare, "bare"))
	image := b.dir + ":configured"
	run := func(t *testing.T, args ...string) (string, error) {
		return newRuntTester(t).runOnce("runt-feature-config", args...)
	}

	t.Run("ImageDefaults", func(t *testing.T) {
		explain(t, "the image's settings apply, with its USER looked up in the image's own /etc/passwd and /etc/group")
		output, err := run(t, "--image", image)
		mustSucceed(t, "runt run with the image's configuration", output, err,
			"default-cmd\n", "uid=4321(runtfeature) gid=4322(runtfeature)", "4323(runtextra)", "/srv/feature\n",
			"FOO=from-image BAR=also-from-image HOME=/home/runtfeature\n")
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "uid=") {
				observe("USER runtfeature (only in the image) -> %s", line)
			}
		}
	})

	t.Run("CommandReplacesCmdKeepsEntrypoint", func(t *testing.T) {
		explain(t, "a command replaces CMD but keeps ENTRYPOINT")
		output, err := run(t, "--image", image, "--", "echo replaced-cmd; echo $FOO")
		mustSucceed(t, "runt run with a command", output, err, "replaced-cmd\nfrom-image\n")
		if strings.Contains(output, "default-cmd") {
			t.Errorf("the image's CMD still ran:\n%s", output)
		}
	})

	t.Run("EntrypointOverride", func(t *testing.T) {
		explain(t, `--entrypoint replaces ENTRYPOINT and drops CMD; --entrypoint "" removes it`)
		output, err := run(t, "--image", image, "--entrypoint", "/bin/echo", "--", "from", "echo")
		mustSucceed(t, "runt run --entrypoint /bin/echo -- from echo", output, err)
		if output != "from echo\n" {
			t.Errorf("got %q, want %q", output, "from echo\n")
		}
		output, err = run(t, "--image", image, "--entrypoint", "/bin/echo")
		mustSucceed(t, "runt run --entrypoint /bin/echo", output, err)
		if output != "\n" {
			t.Errorf("with --entrypoint and no command, CMD should be dropped and echo print an empty line; got %q", output)
		}
		output, err = run(t, "--image", image, "--entrypoint", "", "--", "/bin/echo", "no-entrypoint")
		mustSucceed(t, `runt run --entrypoint ""`, output, err)
		if output != "no-entrypoint\n" {
			t.Errorf(`with --entrypoint "", got %q`, output)
		}
	})

	t.Run("EnvOverride", func(t *testing.T) {
		explain(t, "-e overrides and adds variables, and -e NAME passes one through from runt")
		t.Setenv("RUNT_FEATURE_PASSED", "from-runt")
		output, err := run(t, "--image", image, "--entrypoint", "/usr/bin/env",
			"-e", "FOO=overridden", "-e", "NEW=added", "-e", "RUNT_FEATURE_PASSED", "-e", "RUNT_FEATURE_NOT_SET")
		mustSucceed(t, "runt run -e", output, err, "FOO=overridden\n", "NEW=added\n", "BAR=also-from-image\n", "RUNT_FEATURE_PASSED=from-runt\n")
		if strings.Contains(output, "FOO=from-image") || strings.Contains(output, "RUNT_FEATURE_NOT_SET") {
			t.Errorf("unexpected environment:\n%s", output)
		}
	})

	t.Run("WorkdirOverride", func(t *testing.T) {
		explain(t, "-w replaces WORKDIR, and must be absolute")
		output, err := run(t, "--image", image, "-w", "/tmp", "--entrypoint", "/bin/pwd")
		mustSucceed(t, "runt run -w /tmp", output, err, "/tmp\n")
		output, err = run(t, "--image", image, "-w", "relative", "--entrypoint", "/bin/pwd")
		mustFail(t, "-w relative", output, err, "must be an absolute path")
	})

	t.Run("UserOverride", func(t *testing.T) {
		explain(t, "-u takes names from the container's /etc/passwd and /etc/group, or bare IDs; unknown names are refused")
		cases := []struct{ spec, want string }{
			{"runtfeature:runtextra", "uid=4321(runtfeature) gid=4323(runtextra)"},
			{"root", "uid=0(root) gid=0(root)"},
			{"daemon", "uid=1(daemon) gid=1(daemon)"},
		}
		for _, c := range cases {
			output, err := run(t, "--image", image, "-u", c.spec, "--entrypoint", "/usr/bin/id")
			mustSucceed(t, "runt run -u "+c.spec, output, err, c.want)
		}
		// id fails when an ID has no name, so ask for the numbers alone.
		output, err := run(t, "--image", image, "-u", "1234:5678", "--entrypoint", "/bin/sh", "--", "-c", "echo uid=$(id -u) gid=$(id -g)")
		mustSucceed(t, "runt run -u 1234:5678", output, err, "uid=1234 gid=5678\n")
		output, err = run(t, "--image", image, "-u", "root", "--entrypoint", "/bin/sh", "--", "-c", "echo HOME=$HOME")
		mustSucceed(t, "runt run -u root", output, err, "HOME=/root\n")
		output, err = run(t, "--image", image, "-u", "nosuchuser", "--entrypoint", "/usr/bin/id")
		mustFail(t, "-u nosuchuser", output, err, `no user "nosuchuser" in the container's /etc/passwd`)
		output, err = run(t, "--image", image, "-u", "runtfeature:nosuchgroup", "--entrypoint", "/usr/bin/id")
		mustFail(t, "-u runtfeature:nosuchgroup", output, err, `no group "nosuchgroup" in the container's /etc/group`)
	})

	t.Run("NoDefaultCommand", func(t *testing.T) {
		explain(t, "an image with no ENTRYPOINT or CMD needs a command")
		output, err := run(t, "--image", b.dir+":bare")
		mustFail(t, "runt run of an image with no command, without one", output, err, "the image has no default command")
		output, err = run(t, "--image", b.dir+":bare", "--", "/bin/echo", "given")
		mustSucceed(t, "runt run of an image with no command, with one", output, err, "given\n")
	})

	t.Run("OverridesWorkWithoutAnImage", func(t *testing.T) {
		explain(t, "-e, -w, and -u work with --rootfs too; --entrypoint doesn't")
		r := newRuntTester(t)
		output, err := run(t, "--rootfs", r.rootfs, "-u", "daemon", "-w", "/tmp", "-e", "RUNT_FEATURE_X=1", "--",
			"/bin/sh", "-c", "id -un; pwd; echo X=$RUNT_FEATURE_X")
		mustSucceed(t, "runt run --rootfs -u -w -e", output, err, "daemon\n/tmp\nX=1\n")
		output, err = run(t, "--rootfs", r.rootfs, "--entrypoint", "/bin/echo", "--", "hi")
		mustFail(t, "--entrypoint with --rootfs", output, err, "--entrypoint only works with --image")
	})
}
