//go:build linux

package runtime

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// Tests for imagestore_linux.go: pulling into the store, finding, listing,
// and removing images. They pull from the pretend registry in
// registry_test.go, into a store in a temporary directory. For how Go tests
// work, see "Tests" in doc.go.

// newTestStore makes an image store in a temporary directory, rather than
// /var/lib/runt.
func newTestStore(t *testing.T) *imageStore {
	dir := t.TempDir()
	return &imageStore{
		dir:      filepath.Join(dir, "images"),
		tmp:      filepath.Join(dir, "tmp"),
		lockPath: filepath.Join(dir, "images.lock"),
		layers:   filepath.Join(dir, "layers"),
		cache:    filepath.Join(dir, "build-cache.json"),
		builds:   filepath.Join(dir, "builds"),
	}
}

// storeImage writes a one-layer image straight into the store, as runt
// build does, and returns its manifest's descriptor. marker makes the
// layer, and so the image, unique.
func storeImage(t *testing.T, store *imageStore, marker string) ociDescriptor {
	t.Helper()
	layer, err := store.writeBlob(mediaTypeLayer, []byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	config := ociImageConfig{Architecture: goruntime.GOARCH, OS: "linux"}
	config.RootFS.Type, config.RootFS.DiffIDs = "layers", []string{layer.Digest}
	configData, _ := json.Marshal(config)
	configDesc, err := store.writeBlob(mediaTypeImageConfig, configData)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(ociManifest{SchemaVersion: 2, MediaType: mediaTypeImageManifest, Config: configDesc, Layers: []ociDescriptor{layer}})
	desc, err := store.writeBlob(mediaTypeImageManifest, manifest)
	if err != nil {
		t.Fatal(err)
	}
	desc.Platform = &ociPlatform{OS: "linux", Architecture: goruntime.GOARCH}
	return desc
}

// TestBuiltImagesAndCache names images as runt build and runt tag do, keeps
// images in the build cache, and prunes it.
func TestBuiltImagesAndCache(t *testing.T) {
	store := newTestStore(t)
	stateRoot := t.TempDir()
	// Each image is named as soon as it is stored, since naming collects
	// garbage, and an image no entry names is garbage.
	var unnamed1 ociDescriptor
	for _, marker := range []string{"one", "two", "one"} {
		desc := storeImage(t, store, marker)
		if err := store.nameBuilt(desc, nil); err != nil {
			t.Fatal(err)
		}
		if marker == "one" {
			unnamed1 = desc
		}
	}
	summaries, err := store.summaries()
	if err != nil || len(summaries) != 2 || summaries[0].Repository != "<none>" || summaries[0].Tag != "<none>" {
		t.Fatalf("summaries of two images without names = %+v, %v", summaries, err)
	}
	id1 := summaries[0].ID
	if found, err := store.find(id1[7:15]); err != nil || found.id() != id1 {
		t.Errorf("finding an image without a name by ID: %v", err)
	}

	// Naming it replaces its entry without a name.
	ref, _ := parseReference("app:1.0")
	if err := store.nameBuilt(unnamed1, []imageReference{ref}); err != nil {
		t.Fatal(err)
	}
	if err := store.tag("app:1.0", "localhost:5000/app:copy"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := store.tag("app:1.0", "app@sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Error("tagging with a digest succeeded")
	}
	if err := store.tag("nothing", "app:2"); err == nil {
		t.Error("tagging an image that isn't there succeeded")
	}
	summaries, _ = store.summaries()
	names := map[string]string{}
	for _, summary := range summaries {
		names[summary.Repository+":"+summary.Tag] = summary.ID
	}
	if len(summaries) != 3 || names["app:1.0"] != id1 || names["localhost:5000/app:copy"] != id1 || names["<none>:<none>"] == "" {
		t.Errorf("after naming, the store has %v", names)
	}

	// Removing the image without a name leaves the other images alone.
	if err := store.remove(names["<none>:<none>"], false, stateRoot, io.Discard); err != nil {
		t.Fatalf("remove the image without a name: %v", err)
	}
	if summaries, _ := store.summaries(); len(summaries) != 2 {
		t.Errorf("after removing it, the store has %+v", summaries)
	}

	// An image only the cache has keeps its blobs, until the cache is
	// pruned.
	cachedDesc := storeImage(t, store, "cached")
	if err := store.addToCache("key", cachedDesc); err != nil {
		t.Fatal(err)
	}
	if err := store.collectGarbage(true, nil); err != nil {
		t.Fatal(err)
	}
	if _, image, ok := store.cached("key"); !ok || image.manifest.Layers[0].Size != int64(len("cached")) {
		t.Errorf("the cached image is gone after garbage collection")
	}
	if _, _, ok := store.cached("other key"); ok {
		t.Error("found a cache entry that was never added")
	}
	blobs := store.blobCount(t)
	entries, reclaimed, err := store.pruneCache(stateRoot)
	if err != nil || entries != 1 || reclaimed == 0 || store.blobCount(t) != blobs-3 {
		t.Errorf("pruneCache = %d, %d, %v; %d of %d blobs left", entries, reclaimed, err, store.blobCount(t), blobs)
	}
	if _, _, ok := store.cached("key"); ok {
		t.Error("the cache entry is still there after pruning")
	}
}

// TestTagAndPush pushes images from the store to the pretend registry: one
// pulled from it under another tag, whose blobs it has already, and a new
// one, whose blobs it doesn't.
func TestTagAndPush(t *testing.T) {
	r := newTestRegistry(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	r.tag("v1", r.multiPlatform("pulled"))
	store := newTestStore(t)
	ref, _ := parseReference(r.host() + "/test/app:v1")
	if err := store.pull(ref, io.Discard); err != nil {
		t.Fatal(err)
	}
	push := func(name string) string {
		t.Helper()
		ref, err := parseReference(name)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := store.push(ref, &out); err != nil {
			t.Fatalf("push %s: %v\n%s", name, err, out.String())
		}
		return out.String()
	}

	if err := store.tag(r.host()+"/test/app:v1", r.host()+"/test/app:copy"); err != nil {
		t.Fatal(err)
	}
	out := push(r.host() + "/test/app:copy")
	pulled, _ := store.find(r.host() + "/test/app:v1")
	if !strings.Contains(out, "Layer already exists") || len(r.uploads) != 0 || r.tags["copy"] != pulled.entry.Digest {
		t.Errorf("pushing a copy uploaded %q, and tagged %s:\n%s", r.uploads, r.tags["copy"], out)
	}

	built := storeImage(t, store, "built")
	builtRef, _ := parseReference(r.host() + "/test/app:built")
	if err := store.nameBuilt(built, []imageReference{builtRef}); err != nil {
		t.Fatal(err)
	}
	out = push(r.host() + "/test/app:built")
	if !strings.Contains(out, "Pushed") || len(r.uploads) != 2 || r.tags["built"] != built.Digest {
		t.Errorf("pushing a new image uploaded %q, and tagged %s:\n%s", r.uploads, r.tags["built"], out)
	}

	// Pulled back into another store, it is the same image.
	other := newTestStore(t)
	if err := other.pull(builtRef, io.Discard); err != nil {
		t.Fatalf("pull the pushed image: %v", err)
	}
	if found, err := other.find(builtRef.String()); err != nil || found.entry.Digest != built.Digest {
		t.Errorf("the pulled image is %+v, %v", found.entry, err)
	}

	for _, name := range []string{r.host() + "/test/app:nothing", r.host() + "/test/app@sha256:" + strings.Repeat("0", 64)} {
		ref, _ := parseReference(name)
		if err := store.push(ref, io.Discard); err == nil {
			t.Errorf("pushing %s succeeded", name)
		}
	}
}

// blobCount is the number of blobs in the store.
func (s *imageStore) blobCount(t *testing.T) int {
	entries, err := os.ReadDir(filepath.Join(s.dir, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// TestPullAndRemove pulls images into a store, looks them up by name and ID,
// and removes them, checking what is left behind at each step.
func TestPullAndRemove(t *testing.T) {
	// DOCKER_CONFIG points credentials at an empty directory, so the
	// test never reads the real ones. pull is a helper for this test only,
	// pulling a tag and returning what it printed.
	r := newTestRegistry(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	r.tag("v1", r.multiPlatform("one"))
	store := newTestStore(t)
	pull := func(tag string) string {
		t.Helper()
		ref, err := parseReference(r.host() + "/test/app:" + tag)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := store.pull(ref, &out); err != nil {
			t.Fatalf("pull %s: %v\n%s", tag, err, out.String())
		}
		return out.String()
	}

	out := pull("v1")
	if !strings.Contains(out, "Pull complete") || !strings.Contains(out, "Downloaded newer image") {
		t.Errorf("pull output:\n%s", out)
	}
	// The manifest, configuration, and layer of this platform's image only.
	if n := store.blobCount(t); n != 3 {
		t.Errorf("the store has %d blobs; want 3", n)
	}
	image, err := store.find(r.host() + "/test/app:v1")
	if err != nil {
		t.Fatalf("find by name: %v", err)
	}
	if got := image.image.config.Config.Cmd; len(got) != 1 || got[0] != "native" {
		t.Errorf("stored image has command %q; want this platform's", got)
	}
	// The first 8 hex digits of the ID, after its sha256: prefix.
	id := image.id()
	if byID, err := store.find(id[len("sha256:") : len("sha256:")+8]); err != nil || byID.id() != id {
		t.Errorf("find by ID prefix: %v", err)
	}
	if _, err := store.find(r.host() + "/test/app:v2"); err == nil {
		t.Error("found an image that was never pulled")
	}

	pulls := r.blobPulls
	if out := pull("v1"); !strings.Contains(out, "up to date") || r.blobPulls != pulls {
		t.Errorf("pulling again downloaded %d blobs:\n%s", r.blobPulls-pulls, out)
	}

	// The same image under a second name.
	r.tag("v2", r.multiPlatform("one"))
	pull("v2")
	summaries, err := store.summaries()
	if err != nil || len(summaries) != 2 || summaries[0].ID != summaries[1].ID || summaries[1].Tag != "v2" ||
		summaries[0].Repository != r.host()+"/test/app" || summaries[0].Size == 0 {
		t.Errorf("summaries = %+v, %v", summaries, err)
	}
	// io.Discard is a writer that throws away whatever is written to it.
	stateRoot := t.TempDir()
	if err := store.remove(id, false, stateRoot, io.Discard); err == nil || !strings.Contains(err.Error(), "several names") {
		t.Errorf("removing an image with two names by ID: %v", err)
	}
	var removed strings.Builder
	if err := store.remove(r.host()+"/test/app:v2", false, stateRoot, &removed); err != nil {
		t.Fatalf("remove v2: %v", err)
	}
	if !strings.Contains(removed.String(), "Untagged") || strings.Contains(removed.String(), "Deleted") || store.blobCount(t) != 3 {
		t.Errorf("removing one of two names deleted the image:\n%s", removed.String())
	}

	// v1 now names another image; the first one's blobs go.
	r.tag("v1", r.multiPlatform("two"))
	pull("v1")
	if n := store.blobCount(t); n != 3 {
		t.Errorf("after replacing v1 the store has %d blobs; want 3", n)
	}
	image, err = store.find(r.host() + "/test/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	id = image.id()

	// A container made from it, and unpacked layers: the image's own, one
	// the container uses, one nothing uses, and a half-unpacked one.
	state := &State{ID: "c0ffee000001", Status: StatusStopped, ImageID: id}
	unpacked := filepath.Join(store.layers, "100000-100000")
	imageLayer := filepath.Join(unpacked, strings.TrimPrefix(image.image.manifest.Layers[0].Digest, "sha256:"))
	containerLayer := filepath.Join(unpacked, strings.Repeat("1", 64))
	unused := filepath.Join(unpacked, strings.Repeat("2", 64))
	unfinished := filepath.Join(unpacked, "unpacking-123")
	for _, dir := range []string{imageLayer, containerLayer, unused, unfinished} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state.Layers = []string{imageLayer, containerLayer}
	if err := createStateDir(stateRoot, state.ID); err != nil {
		t.Fatal(err)
	}
	if err := saveState(stateRoot, state); err != nil {
		t.Fatal(err)
	}
	if err := store.remove(r.host()+"/test/app:v1", false, stateRoot, io.Discard); err == nil || !strings.Contains(err.Error(), state.ID) {
		t.Errorf("removing an image a container uses: %v", err)
	}
	removed.Reset()
	if err := store.remove(r.host()+"/test/app:v1", true, stateRoot, &removed); err != nil {
		t.Fatalf("rmi -f: %v", err)
	}
	if !strings.Contains(removed.String(), "Deleted: "+id) {
		t.Errorf("rmi -f output:\n%s", removed.String())
	}
	if n := store.blobCount(t); n != 0 {
		t.Errorf("the store has %d blobs left; want none", n)
	}
	// Each layer directory, and whether it should still be there: (err ==
	// nil) is whether it is.
	for dir, want := range map[string]bool{imageLayer: true, containerLayer: true, unused: false, unfinished: false} {
		if _, err := os.Stat(dir); (err == nil) != want {
			t.Errorf("%s exists: %v; want %v", filepath.Base(dir), err == nil, want)
		}
	}
	if summaries, err := store.summaries(); err != nil || len(summaries) != 0 {
		t.Errorf("summaries after removing everything = %+v, %v", summaries, err)
	}
}

// TestIsLayoutRef checks telling layout directories from store references.
func TestIsLayoutRef(t *testing.T) {
	layout := newTestLayout(t)
	for ref, want := range map[string]bool{
		"alpine":            false,
		"alpine:3.20":       false,
		"ghcr.io/a/b:1":     false,
		"./alpine":          true,
		"/images/alpine":    true,
		layout.dir:          true,
		layout.dir + ":tag": true,
	} {
		if got := isLayoutRef(ref); got != want {
			t.Errorf("isLayoutRef(%q) = %v; want %v", ref, got, want)
		}
	}
}
