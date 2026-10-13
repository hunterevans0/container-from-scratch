//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// tarEntry is one entry of a test layer.
type tarEntry struct {
	name     string
	typeflag byte
	content  string
	linkname string
	uid      int
	mode     int64
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	archive := tar.NewWriter(&buf)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Linkname: entry.linkname, Uid: entry.uid, Gid: entry.uid, Mode: mode, Size: int64(len(entry.content))}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// TestExtractLayer checks that a layer is unpacked with its ownership
// shifted, its whiteouts turned into overlayfs's, and no way out of the
// layer directory. It needs root, for chown, mknod, and trusted.* extended
// attributes.
func TestExtractLayer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("unpacking a layer needs root")
	}
	uids, gids := IDRange{Base: 100000, Size: 65536}, IDRange{Base: 200000, Size: 65536}
	root := t.TempDir()
	layer := buildTar(t, []tarEntry{
		{name: "./", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/passwd", typeflag: tar.TypeReg, content: "root:x:0:0::/root:/bin/sh\n"},
		{name: "usr/bin/su", typeflag: tar.TypeReg, content: "binary", mode: 0o4755},
		{name: "usr/bin/sudo", typeflag: tar.TypeLink, linkname: "usr/bin/su"},
		{name: "home/user/notes", typeflag: tar.TypeReg, content: "mine", uid: 1000},
		{name: "etc/.wh.shadow", typeflag: tar.TypeReg},
		{name: "var/cache/.wh..wh..opq", typeflag: tar.TypeReg},
		{name: "dev/sda", typeflag: tar.TypeBlock},
		{name: "../../outside", typeflag: tar.TypeReg, content: "escaped?"},
	})
	if err := extractLayer(layer, root, uids, gids); err != nil {
		t.Fatal(err)
	}

	owner := func(path string) (int, int) {
		var stat syscall.Stat_t
		if err := syscall.Lstat(filepath.Join(root, path), &stat); err != nil {
			t.Fatal(err)
		}
		return int(stat.Uid), int(stat.Gid)
	}
	if uid, gid := owner("etc/passwd"); uid != 100000 || gid != 200000 {
		t.Errorf("etc/passwd is owned by %d:%d; want 100000:200000", uid, gid)
	}
	if uid, gid := owner("home/user/notes"); uid != 101000 || gid != 201000 {
		t.Errorf("home/user/notes is owned by %d:%d; want 101000:201000", uid, gid)
	}
	// usr/bin is not in the archive, so it was made for su, and belongs to
	// container root.
	if uid, _ := owner("usr/bin"); uid != 100000 {
		t.Errorf("usr/bin is owned by %d; want 100000", uid)
	}
	if info, err := os.Stat(filepath.Join(root, "usr/bin/su")); err != nil || info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("usr/bin/su lost its setuid bit: %v, %v", info.Mode(), err)
	}
	var su, sudo syscall.Stat_t
	syscall.Stat(filepath.Join(root, "usr/bin/su"), &su)
	syscall.Stat(filepath.Join(root, "usr/bin/sudo"), &sudo)
	if su.Ino != sudo.Ino {
		t.Error("usr/bin/sudo is not a hard link to usr/bin/su")
	}

	var whiteout syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(root, "etc/shadow"), &whiteout); err != nil || whiteout.Mode&syscall.S_IFMT != syscall.S_IFCHR || whiteout.Rdev != 0 {
		t.Errorf("etc/shadow is not a 0:0 character device whiteout: mode %o, rdev %d, %v", whiteout.Mode, whiteout.Rdev, err)
	}
	opaque := make([]byte, 1)
	if n, err := syscall.Getxattr(filepath.Join(root, "var/cache"), "trusted.overlay.opaque", opaque); err != nil || string(opaque[:n]) != "y" {
		t.Errorf("var/cache is not marked opaque: %q, %v", opaque[:n], err)
	}
	if _, err := os.Lstat(filepath.Join(root, "dev/sda")); err == nil {
		t.Error("the block device was created")
	}
	// ../../outside is cleaned to a name inside the layer.
	if _, err := os.Stat(filepath.Join(root, "outside")); err != nil {
		t.Errorf("../../outside did not land inside the layer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(root)), "outside")); err == nil {
		t.Error("../../outside escaped the layer directory")
	}
}

// TestExtractLayerSymlinkEscape checks that a layer can't write through a
// symbolic link it made earlier to reach outside the layer directory.
func TestExtractLayerSymlinkEscape(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("unpacking a layer needs root")
	}
	uids := IDRange{Base: 100000, Size: 65536}
	target := t.TempDir()
	for _, entries := range [][]tarEntry{
		{
			{name: "escape", typeflag: tar.TypeSymlink, linkname: target},
			{name: "escape/planted", typeflag: tar.TypeReg, content: "gotcha"},
		},
		{
			{name: "escape", typeflag: tar.TypeSymlink, linkname: target},
			{name: "escape/.wh.planted", typeflag: tar.TypeReg},
		},
		{
			{name: "link", typeflag: tar.TypeLink, linkname: "../../../etc/passwd"},
		},
	} {
		root := t.TempDir()
		if err := extractLayer(buildTar(t, entries), root, uids, uids); err == nil {
			t.Errorf("extracting %v succeeded; want an error", entries)
		}
		if _, err := os.Lstat(filepath.Join(target, "planted")); err == nil {
			t.Fatalf("extracting %v wrote outside the layer", entries)
		}
	}
}
