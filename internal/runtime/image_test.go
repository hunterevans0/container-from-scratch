package runtime

// Tests for image.go: reading OCI image layouts. The tests build small
// layouts of their own, in temporary directories, with testLayout. For how
// Go tests work, see "Tests" in doc.go.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// testLayout builds an OCI image layout in a temporary directory. It keeps
// the test's t, so its methods can fail the test themselves.
type testLayout struct {
	t   *testing.T
	dir string
}

// newTestLayout makes an empty layout: the blobs directory and the
// oci-layout file.
func newTestLayout(t *testing.T) *testLayout {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return &testLayout{t: t, dir: dir}
}

// blob stores data and returns a descriptor for it.
func (l *testLayout) blob(mediaType string, data []byte) ociDescriptor {
	l.t.Helper()
	sum := sha256.Sum256(data)
	encoded := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(l.dir, "blobs", "sha256", encoded), data, 0o644); err != nil {
		l.t.Fatal(err)
	}
	return ociDescriptor{MediaType: mediaType, Digest: "sha256:" + encoded, Size: int64(len(data))}
}

// jsonBlob stores v, encoded as JSON, as a blob.
func (l *testLayout) jsonBlob(mediaType string, v any) ociDescriptor {
	l.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		l.t.Fatal(err)
	}
	return l.blob(mediaType, data)
}

// image stores a one-layer image whose layer content is marker, and returns
// its manifest's descriptor.
func (l *testLayout) image(marker string) ociDescriptor {
	l.t.Helper()
	layer := l.blob("application/vnd.oci.image.layer.v1.tar", []byte(marker))
	config := ociImageConfig{Architecture: goruntime.GOARCH, OS: "linux"}
	config.RootFS.Type = "layers"
	config.RootFS.DiffIDs = []string{layer.Digest}
	return l.jsonBlob(mediaTypeImageManifest, ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeImageManifest,
		Config:        l.jsonBlob("application/vnd.oci.image.config.v1+json", config),
		Layers:        []ociDescriptor{layer},
	})
}

// index writes index.json, listing entries. The ... makes index variadic:
// it takes any number of descriptors, which arrive as a slice.
func (l *testLayout) index(entries ...ociDescriptor) {
	l.t.Helper()
	data, err := json.Marshal(ociIndex{SchemaVersion: 2, MediaType: mediaTypeImageIndex, Manifests: entries})
	if err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.dir, "index.json"), data, 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// tagged returns desc with a tag. desc is passed by value, a copy, so
// changing it doesn't change the caller's.
func tagged(desc ociDescriptor, tag string) ociDescriptor {
	desc.Annotations = map[string]string{annotationRefName: tag}
	return desc
}

// layerMarker returns the content of an image's only layer.
func layerMarker(t *testing.T, image *ociImage) string {
	t.Helper()
	path, err := image.blobPath(image.manifest.Layers[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestOpenImage checks that openImage picks an image by tag, falls back to
// the only image or the one tagged latest, follows a multi-platform index to
// this machine's architecture, and refuses blobs that don't match their
// digest.
//
// Each image's single layer holds a word (its "marker"), so which image was
// opened can be told by reading the layer.
func TestOpenImage(t *testing.T) {
	l := newTestLayout(t)
	other := "arm64"
	if goruntime.GOARCH == "arm64" {
		other = "amd64"
	}
	native, foreign := l.image("native"), l.image("foreign")
	native.Platform = &ociPlatform{OS: "linux", Architecture: goruntime.GOARCH}
	foreign.Platform = &ociPlatform{OS: "linux", Architecture: other}
	multi := l.jsonBlob(mediaTypeImageIndex, ociIndex{SchemaVersion: 2, Manifests: []ociDescriptor{foreign, native}})
	l.index(tagged(l.image("one"), "1.0"), tagged(multi, "latest"))

	for ref, want := range map[string]string{l.dir + ":1.0": "one", l.dir + ":latest": "native", l.dir: "native"} {
		image, err := openImage(ref)
		if err != nil {
			t.Errorf("openImage(%q): %v", ref, err)
			continue
		}
		if got := layerMarker(t, image); got != want {
			t.Errorf("openImage(%q) found layer %q, want %q", ref, got, want)
		}
	}
	if _, err := openImage(l.dir + ":2.0"); err == nil || !strings.Contains(err.Error(), "1.0") {
		t.Errorf("openImage with a missing tag = %v; want an error listing the tags", err)
	}

	// A manifest whose content has changed no longer matches its digest.
	l2 := newTestLayout(t)
	manifest := l2.image("one")
	l2.index(manifest)
	if _, err := openImage(l2.dir); err != nil {
		t.Fatalf("openImage of the only image: %v", err)
	}
	path, _ := (&ociImage{layout: l2.dir}).blobPath(manifest.Digest)
	if err := os.WriteFile(path, []byte(`{"schemaVersion":2,"layers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openImage(l2.dir); err == nil {
		t.Error("openImage accepted a manifest that doesn't match its digest")
	}

	if _, err := openImage(t.TempDir()); err == nil {
		t.Error("openImage accepted a directory without an oci-layout file")
	}
}

// TestBlobPath checks that only well-formed sha256 digests name a blob, so a
// digest from an untrusted index can't point outside the layout.
func TestBlobPath(t *testing.T) {
	image := &ociImage{layout: "/layout"}
	good := "sha256:" + strings.Repeat("ab", 32)
	if path, err := image.blobPath(good); err != nil || path != filepath.Join("/layout", "blobs", "sha256", strings.Repeat("ab", 32)) {
		t.Errorf("blobPath(%q) = %q, %v", good, path, err)
	}
	for _, digest := range []string{"sha256:../../etc/passwd", "sha512:" + strings.Repeat("ab", 64), "sha256:ABC", strings.Repeat("ab", 32)} {
		if _, err := image.blobPath(digest); err == nil {
			t.Errorf("blobPath(%q) succeeded; want an error", digest)
		}
	}
}

// TestSplitImageRef checks that a tag is only split off after the last
// slash.
func TestSplitImageRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"/images/alpine":      {"/images/alpine", ""},
		"/images/alpine:3.20": {"/images/alpine", "3.20"},
		"./a:b/alpine":        {"./a:b/alpine", ""},
		"./a:b/alpine:edge":   {"./a:b/alpine", "edge"},
		"alpine:latest":       {"alpine", "latest"},
	} {
		if layout, tag := splitImageRef(ref); layout != want[0] || tag != want[1] {
			t.Errorf("splitImageRef(%q) = %q, %q; want %q, %q", ref, layout, tag, want[0], want[1])
		}
	}
}

// TestLayerCompression checks the layer media types runt accepts.
func TestLayerCompression(t *testing.T) {
	for mediaType, want := range map[string]string{
		"application/vnd.oci.image.layer.v1.tar":                       "",
		"application/vnd.oci.image.layer.v1.tar+gzip":                  "gzip",
		"application/vnd.docker.image.rootfs.diff.tar.gzip":            "gzip",
		"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip": "gzip",
	} {
		if got, err := layerCompression(mediaType); err != nil || got != want {
			t.Errorf("layerCompression(%q) = %q, %v; want %q", mediaType, got, err, want)
		}
	}
	for _, mediaType := range []string{"application/vnd.oci.image.layer.v1.tar+zstd", "text/plain"} {
		if _, err := layerCompression(mediaType); err == nil {
			t.Errorf("layerCompression(%q) succeeded; want an error", mediaType)
		}
	}
}
