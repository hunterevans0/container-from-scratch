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
	b := &layoutBuilder{t: t, dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(b.dir, "blobs/sha256"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The base layer, uncompressed: the rootfs, plus /srv/old.
	baseFile, err := os.Create(filepath.Join(b.dir, "base.tar"))
	if err != nil {
		t.Fatal(err)
	}
	baseHash := sha256.New()
	tarRootfs(t, r.rootfs, int(rootStat.Uid), int(rootStat.Gid), io.MultiWriter(baseFile, baseHash), func(archive *tar.Writer) {
		addFile(t, archive, "srv/old", "old")
	})
	baseFile.Close()
	baseDigest := hex.EncodeToString(baseHash.Sum(nil))
	if err := os.Rename(filepath.Join(b.dir, "base.tar"), filepath.Join(b.dir, "blobs/sha256", baseDigest)); err != nil {
		t.Fatal(err)
	}
	baseInfo, _ := os.Stat(filepath.Join(b.dir, "blobs/sha256", baseDigest))
	base := map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": "sha256:" + baseDigest, "size": baseInfo.Size()}

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

	config := b.jsonBlob("application/vnd.oci.image.config.v1+json", map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + baseDigest, "sha256:" + hex.EncodeToString(topDiff[:])}},
	})
	manifest := b.jsonBlob("application/vnd.oci.image.manifest.v1+json", map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        config,
		"layers":        []any{base, top},
	})
	manifest["annotations"] = map[string]string{"org.opencontainers.image.ref.name": "test"}
	b.writeJSON("oci-layout", map[string]string{"imageLayoutVersion": "1.0.0"})
	b.writeJSON("index.json", map[string]any{"schemaVersion": 2, "manifests": []any{manifest}})

	// The unpacked layers are shared by every container from the image;
	// remove them once this test is done with them.
	store := fmt.Sprintf("/var/lib/runt/layers/%d-%d", rootStat.Uid, rootStat.Gid)
	t.Cleanup(func() {
		for _, layer := range []map[string]any{base, top} {
			os.RemoveAll(filepath.Join(store, strings.TrimPrefix(layer["digest"].(string), "sha256:")))
		}
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
