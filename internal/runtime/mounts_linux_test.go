//go:build linux

package runtime

// Tests for mounts_linux.go: following symbolic links inside a container's
// root filesystem, and filling a volume from an image. Making the mounts
// themselves needs root and a container; the integration tests do that (see
// integration/storage_test.go). For how Go tests work, see "Tests" in
// doc.go.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveInRoot checks that symbolic links in a root filesystem are
// followed as the container would follow them, never out of it.
func TestResolveInRoot(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"etc", "var/lib", "srv/real"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Each link, and where it leads.
	for link, target := range map[string]string{
		"abs-etc":    "/etc",              // absolute: from the container's root
		"var/lib/up": "../../srv/real",    // relative to var/lib
		"escape":     "../../../../../..", // can't go above the root
		"escape-abs": "/../../tmp",
		"srv/chain":  "/var/lib/up",
		"dangling":   "/nowhere/at/all",
		"loop-a":     "loop-b",
		"loop-b":     "loop-a",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for p, want := range map[string]string{
		"/etc":                 "/etc",
		"/abs-etc/passwd":      "/etc/passwd",
		"/var/lib/up/x":        "/srv/real/x",
		"/srv/chain/x":         "/srv/real/x",
		"/escape/etc":          "/etc",
		"/escape-abs":          "/tmp",
		"/dangling/sub":        "/nowhere/at/all/sub",
		"/missing/../abs-etc":  "/etc",
		"/file/under-a-file":   "/file/under-a-file",
		"/./var//lib/../lib/.": "/var/lib",
		"/../../..":            "/",
	} {
		got, err := resolveInRoot(root, p)
		if err != nil {
			t.Errorf("resolveInRoot(%q): %v", p, err)
		} else if got != filepath.Join(root, want) {
			t.Errorf("resolveInRoot(%q) = %q; want %q in the root", p, got, want)
		}
	}
	if got, err := resolveInRoot(root, "/loop-a/x"); err == nil {
		t.Errorf("resolveInRoot through a loop of links = %q; want an error", got)
	}
}

// TestPopulateVolume checks that an empty volume is filled with what the
// image has at its destination, links copied as links, and that a volume
// with files in it already is left alone.
func TestPopulateVolume(t *testing.T) {
	rootfs := t.TempDir()
	image := filepath.Join(rootfs, "var/lib/app")
	for _, dir := range []string{"var/lib/app/sub", "outside"} {
		if err := os.MkdirAll(filepath.Join(rootfs, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(image, "sub/config"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	// os.FileMode keeps the sticky bit apart from the permissions.
	if err := os.Chmod(image, 0o770|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	// A link that would lead out of the volume: copied as it is, not
	// followed.
	if err := os.Symlink("/outside", filepath.Join(image, "link")); err != nil {
		t.Fatal(err)
	}
	// /app, in the container, leads to /var/lib/app.
	if err := os.Symlink("var/lib/app", filepath.Join(rootfs, "app")); err != nil {
		t.Fatal(err)
	}

	data := t.TempDir()
	if err := populateVolume(data, rootfs, "/app"); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(data, "sub/config")); err != nil || string(content) != "hello" {
		t.Errorf("sub/config in the volume: %q, %v", content, err)
	}
	if info, err := os.Stat(filepath.Join(data, "sub/config")); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("sub/config's permissions weren't copied: %v, %v", info.Mode(), err)
	}
	if info, err := os.Stat(data); err != nil || info.Mode()&(os.ModePerm|os.ModeSticky) != 0o770|os.ModeSticky {
		t.Errorf("the volume didn't take the permissions of the image's directory: %v, %v", info.Mode(), err)
	}
	if link, err := os.Readlink(filepath.Join(data, "link")); err != nil || link != "/outside" {
		t.Errorf("link in the volume: %q, %v; want it to lead to /outside", link, err)
	}

	// Filled once, it isn't filled again.
	if err := os.WriteFile(filepath.Join(image, "later"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := populateVolume(data, rootfs, "/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "later")); err == nil {
		t.Error("a volume with files in it was filled again")
	}
	// Nothing in the image at the destination leaves a volume empty.
	empty := t.TempDir()
	if err := populateVolume(empty, rootfs, "/no/such/dir"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("the volume got %d files from nowhere", len(entries))
	}
}
