//go:build linux

package runtime

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *imageStore {
	dir := t.TempDir()
	return &imageStore{
		dir:      filepath.Join(dir, "images"),
		tmp:      filepath.Join(dir, "tmp"),
		lockPath: filepath.Join(dir, "images.lock"),
		layers:   filepath.Join(dir, "layers"),
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
