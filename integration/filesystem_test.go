//go:build integration && linux

package integration

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestWritableLayerPerContainer checks that two containers started from the
// same rootfs each write to a layer of their own: neither sees the other's
// files, the rootfs itself is left alone, and runt rm deletes the layer.
func TestWritableLayerPerContainer(t *testing.T) {
	r := newRuntTester(t)
	idA := r.start("runt-layer-a", "/bin/sleep", "3001")
	r.start("runt-layer-b", "/bin/sleep", "3002")

	if output, err := r.exec("runt-layer-a", "echo from-a > /root/runt-layer-test"); err != nil {
		t.Fatalf("A writing: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(r.rootfs, "root/runt-layer-test")); err == nil {
		os.Remove(filepath.Join(r.rootfs, "root/runt-layer-test"))
		// Deleting a file would then delete it from the rootfs too.
		t.Fatal("A's file was written into the shared rootfs")
	}
	if output, err := r.exec("runt-layer-a", "cat /root/runt-layer-test"); err != nil || output != "from-a\n" {
		t.Errorf("A reading back its file: %v, %q", err, output)
	}
	if output, err := r.exec("runt-layer-b", "cat /root/runt-layer-test"); err == nil {
		t.Errorf("B can read A's file: %q", output)
	}

	if output, err := r.exec("runt-layer-a", "rm /etc/issue && ! test -e /etc/issue"); err != nil {
		t.Fatalf("A deleting /etc/issue: %v\n%s", err, output)
	}
	if output, err := r.exec("runt-layer-b", "test -e /etc/issue"); err != nil {
		t.Errorf("A's deleting /etc/issue reached B: %v, %q", err, output)
	}
	if _, err := os.Stat(filepath.Join(r.rootfs, "etc/issue")); err != nil {
		t.Errorf("A's deleting /etc/issue reached the shared rootfs: %v", err)
	}

	layer := filepath.Join("/var/lib/runt/containers", idA)
	if _, err := os.Stat(filepath.Join(layer, "upper/root/runt-layer-test")); err != nil {
		t.Errorf("A's file is not in its writable layer: %v", err)
	}
	if output, err := r.runt("rm", "-f", "runt-layer-a"); err != nil {
		t.Fatalf("runt rm -f: %v\n%s", err, output)
	}
	if _, err := os.Stat(layer); err == nil {
		t.Errorf("runt rm left %s behind", layer)
	}
}

// layoutBuilder writes an OCI image layout for the tests.
type layoutBuilder struct {
	t   *testing.T
	dir string
}

func newLayoutBuilder(t *testing.T) *layoutBuilder {
	t.Helper()
	b := &layoutBuilder{t: t, dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(b.dir, "blobs/sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.writeJSON("oci-layout", map[string]string{"imageLayoutVersion": "1.0.0"})
	return b
}

// baseLayer is the rootfs as an uncompressed layer, plus /srv/old, made once
// for every test that needs an image, since tarring the rootfs takes a
// while. TestMain removes it.
var baseLayer struct {
	once   sync.Once
	dir    string
	path   string
	digest string // hex
	size   int64
}

// addBaseLayer adds the base layer to the layout, returning its descriptor
// and diff ID.
func (b *layoutBuilder) addBaseLayer(r *runtTester) (map[string]any, string) {
	b.t.Helper()
	baseLayer.once.Do(func() {
		var rootStat syscall.Stat_t
		if err := syscall.Stat(r.rootfs, &rootStat); err != nil {
			b.t.Fatal(err)
		}
		dir, err := os.MkdirTemp("", "runt-base-layer-")
		if err != nil {
			b.t.Fatal(err)
		}
		baseLayer.dir = dir
		file, err := os.Create(filepath.Join(dir, "base.tar"))
		if err != nil {
			b.t.Fatal(err)
		}
		defer file.Close()
		hash := sha256.New()
		tarRootfs(b.t, r.rootfs, int(rootStat.Uid), int(rootStat.Gid), io.MultiWriter(file, hash), func(archive *tar.Writer) {
			addFile(b.t, archive, "srv/old", "old")
		})
		info, err := file.Stat()
		if err != nil {
			b.t.Fatal(err)
		}
		baseLayer.path, baseLayer.digest, baseLayer.size = file.Name(), hex.EncodeToString(hash.Sum(nil)), info.Size()
	})
	if baseLayer.path == "" {
		b.t.Fatal("making the base layer failed in an earlier test")
	}
	if err := os.Link(baseLayer.path, filepath.Join(b.dir, "blobs/sha256", baseLayer.digest)); err != nil {
		b.t.Fatal(err)
	}
	desc := map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": "sha256:" + baseLayer.digest, "size": baseLayer.size}
	return desc, "sha256:" + baseLayer.digest
}

// writeImage writes an image's configuration and manifest, and an index.json
// listing it under tag. defaults is the configuration's "config": the
// command, environment, and so on.
func (b *layoutBuilder) writeImage(tag string, defaults map[string]any, layers []map[string]any, diffIDs []string) {
	b.t.Helper()
	manifest, _ := b.image(runtime.GOARCH, defaults, layers, diffIDs)
	b.writeIndex(tagged(manifest, tag))
}

// image writes an image's configuration and manifest for architecture,
// without listing it in index.json, and returns their descriptors.
func (b *layoutBuilder) image(architecture string, defaults map[string]any, layers []map[string]any, diffIDs []string) (manifest, config map[string]any) {
	b.t.Helper()
	config = b.jsonBlob("application/vnd.oci.image.config.v1+json", map[string]any{
		"architecture": architecture,
		"os":           "linux",
		"config":       defaults,
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	manifest = b.jsonBlob("application/vnd.oci.image.manifest.v1+json", map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        config,
		"layers":        layers,
	})
	return manifest, config
}

// writeIndex writes index.json, listing entries.
func (b *layoutBuilder) writeIndex(entries ...map[string]any) {
	b.t.Helper()
	b.writeJSON("index.json", map[string]any{"schemaVersion": 2, "manifests": entries})
}

// tagged returns a copy of desc with the annotation that tags an index.json
// entry.
func tagged(desc map[string]any, tag string) map[string]any {
	entry := map[string]any{}
	for key, value := range desc {
		entry[key] = value
	}
	entry["annotations"] = map[string]string{"org.opencontainers.image.ref.name": tag}
	return entry
}

// TestMain removes the base layer, and what runt unpacked of it, once every
// test is done with them.
func TestMain(m *testing.M) {
	code := m.Run()
	if baseLayer.dir != "" {
		os.RemoveAll(baseLayer.dir)
		unpacked, _ := filepath.Glob(filepath.Join("/var/lib/runt/layers/*", baseLayer.digest))
		for _, dir := range unpacked {
			os.RemoveAll(dir)
		}
	}
	os.Exit(code)
}

// blob stores data under its digest and returns its descriptor.
func (b *layoutBuilder) blob(mediaType string, data []byte) map[string]any {
	b.t.Helper()
	sum := sha256.Sum256(data)
	encoded := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(b.dir, "blobs/sha256", encoded), data, 0o644); err != nil {
		b.t.Fatal(err)
	}
	return map[string]any{"mediaType": mediaType, "digest": "sha256:" + encoded, "size": len(data)}
}

func (b *layoutBuilder) jsonBlob(mediaType string, v any) map[string]any {
	b.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.blob(mediaType, data)
}

func (b *layoutBuilder) writeJSON(name string, v any) {
	b.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, name), data, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// tarRootfs writes the rootfs as a layer, with its ownership shifted back
// down from the user-namespace range, as an image built elsewhere would have
// it, plus extra entries.
func tarRootfs(t *testing.T, rootfs string, uidBase, gidBase int, out io.Writer, extra func(*tar.Writer)) {
	t.Helper()
	archive := tar.NewWriter(out)
	err := filepath.WalkDir(rootfs, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil // sockets can't go in a tar archive
		}
		name, _ := filepath.Rel(rootfs, path)
		header.Name = name
		if info.IsDir() {
			header.Name += "/"
		}
		stat := info.Sys().(*syscall.Stat_t)
		header.Uid, header.Gid = int(stat.Uid)-uidBase, int(stat.Gid)-gidBase
		header.Uname, header.Gname = "", ""
		header.Format = tar.FormatPAX
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = io.Copy(archive, file)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	extra(archive)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
}

func addFile(t *testing.T, archive *tar.Writer, name, content string) {
	t.Helper()
	header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}
	if err := archive.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

// TestImage builds a two-layer OCI image from the rootfs, the second layer
// adding a file, deleting one, and hiding a directory's old contents, and
// runs it.
func TestImage(t *testing.T) {
	r := newRuntTester(t)
	var rootStat syscall.Stat_t
	if err := syscall.Stat(r.rootfs, &rootStat); err != nil {
		t.Fatal(err)
	}
	b := newLayoutBuilder(t)

	// The base layer, uncompressed: the rootfs, plus /srv/old.
	base, baseDiff := b.addBaseLayer(r)

	// The top layer, gzipped.
	var topTar strings.Builder
	archive := tar.NewWriter(&topTar)
	addFile(t, archive, "runt-image-test", "from-layer-2\n")
	addFile(t, archive, "etc/.wh.issue", "")
	addFile(t, archive, "srv/.wh..wh..opq", "")
	addFile(t, archive, "srv/new", "new")
	archive.Close()
	topDiff := sha256.Sum256([]byte(topTar.String()))
	var topGzip strings.Builder
	gz := gzip.NewWriter(&topGzip)
	io.WriteString(gz, topTar.String())
	gz.Close()
	top := b.blob("application/vnd.oci.image.layer.v1.tar+gzip", []byte(topGzip.String()))

	b.writeImage("test", nil, []map[string]any{base, top}, []string{baseDiff, "sha256:" + hex.EncodeToString(topDiff[:])})

	// The unpacked layers are shared by every container from the image;
	// remove the top one once this test is done with it (TestMain removes
	// the base).
	store := fmt.Sprintf("/var/lib/runt/layers/%d-%d", rootStat.Uid, rootStat.Gid)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Join(store, strings.TrimPrefix(top["digest"].(string), "sha256:")))
	})

	r.removeAtEnd("runt-image")
	output, err := r.runt("run", "--name", "runt-image", "--image", b.dir+":test", "--", "/bin/sh", "-c",
		"cat /runt-image-test; test -e /etc/issue || echo issue-gone; ls /srv")
	if err != nil {
		t.Fatalf("runt run --image: %v\n%s", err, output)
	}
	for _, want := range []string{"from-layer-2", "issue-gone", "new"} {
		if !strings.Contains(output, want) {
			t.Errorf("output is missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "old") {
		t.Errorf("/srv/old shows through the opaque /srv of the top layer:\n%s", output)
	}
	for _, layer := range []map[string]any{base, top} {
		if _, err := os.Stat(filepath.Join(store, strings.TrimPrefix(layer["digest"].(string), "sha256:"))); err != nil {
			t.Errorf("layer %s was not unpacked into the store: %v", layer["digest"], err)
		}
	}
}

// TestBundle runs a container from an OCI runtime bundle made with runt spec,
// with config.json changed to set the command, environment, working
// directory, and hostname.
func TestBundle(t *testing.T) {
	r := newRuntTester(t)
	dir := t.TempDir()
	if output, err := r.runt("spec", "--bundle", dir, "--rootfs", r.rootfs); err != nil {
		t.Fatalf("runt spec: %v\n%s", err, output)
	}
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	process := spec["process"].(map[string]any)
	process["terminal"] = false
	process["args"] = []string{"/bin/sh", "-c", "hostname; pwd; echo FOO=$FOO; touch /runt-bundle-test"}
	process["env"] = []string{"PATH=/usr/bin:/bin", "FOO=bar"}
	process["cwd"] = "/tmp"
	spec["hostname"] = "bundled"
	if data, err = json.Marshal(spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	r.removeAtEnd("runt-bundle")
	output, _ := r.runt("run", "--name", "runt-bundle", "--bundle", dir)
	for _, want := range []string{"bundled", "/tmp", "FOO=bar", "Read-only file system"} {
		if !strings.Contains(output, want) {
			t.Errorf("output is missing %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(filepath.Join(r.rootfs, "runt-bundle-test")); err == nil {
		os.Remove(filepath.Join(r.rootfs, "runt-bundle-test"))
		t.Error("the container wrote to its read-only root filesystem")
	}
}
