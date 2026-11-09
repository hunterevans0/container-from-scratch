//go:build linux

package runtime

// Each container's root filesystem, as an overlay mount (see below). The
// image layers it stacks are unpacked by layer_linux.go. For how to read the
// Go here, see doc.go.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// DataRoot holds what outlives a reboot, unlike StateRoot: unpacked
	// image layers and each container's writable layer.
	DataRoot = "/var/lib/runt"
	// containerFSRoot has a directory per container, named by its ID, with
	// its writable layer.
	containerFSRoot = DataRoot + "/containers"
	// layerRoot holds unpacked image layers, shared by every container
	// made from them.
	layerRoot = DataRoot + "/layers"
)

// Each container's root filesystem is an overlay mount: read-only layers
// underneath, shared with every other container that uses them, and an
// empty writable layer on top, the container's own. Reading a file finds it
// in the topmost layer that has it. Changing one copies it up into the
// writable layer first, and deleting one leaves a "whiteout" there that
// hides it. The layers underneath never change.
//
// A container's directory, containerFSRoot/<id>, has:
//   - upper: the writable layer.
//   - work: scratch space overlayfs needs on the same filesystem.
//   - rootfs: where the overlay is mounted while the container runs.
//
// runt mounts the overlay itself, as host root, before cloning init. Init
// then sees it at rootfs like any other directory. The writable layer stays
// until runt rm, as Docker keeps a stopped container's changes.

// containerFSDir is the directory holding container id's writable layer.
func containerFSDir(id string) string {
	return filepath.Join(containerFSRoot, id)
}

// unmountContainerFS unmounts container id's root filesystem, if it is
// mounted. The writable layer stays.
func unmountContainerFS(id string) error {
	merged := filepath.Join(containerFSDir(id), "rootfs")
	// MNT_DETACH unmounts at once even if something is still using the
	// mount, which then goes away when it is no longer used. EINVAL means
	// nothing was mounted there, and ENOENT that there is no such directory:
	// either way, it is already unmounted.
	err := syscall.Unmount(merged, syscall.MNT_DETACH)
	if err == nil || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return fmt.Errorf("unmount %s: %w", merged, err)
}

// removeContainerFS unmounts container id's root filesystem and deletes its
// writable layer.
func removeContainerFS(id string) error {
	if err := unmountContainerFS(id); err != nil {
		return err
	}
	dir := containerFSDir(id)
	// Deleting files through a mounted overlay would only add whiteouts to
	// the writable layer; make sure it is really gone first.
	merged := filepath.Join(dir, "rootfs")
	// Something is mounted at merged if it has a different device from its
	// parent directory; a missing path isn't a mount point, and a path that
	// can't be checked counts as one. Lstat describes a file (without
	// following a symlink), including Dev, the number of the filesystem it
	// is on.
	var stat, parent syscall.Stat_t
	if err := syscall.Lstat(merged, &stat); err != nil {
		if !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("%s is still mounted", merged)
		}
	} else if err := syscall.Lstat(filepath.Dir(merged), &parent); err != nil || stat.Dev != parent.Dev {
		return fmt.Errorf("%s is still mounted", merged)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove container filesystem: %w", err)
	}
	return nil
}
