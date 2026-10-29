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
	"strings"
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

// mountContainerFS mounts container id's root filesystem: an overlay of
// layers, given top first, under a new writable layer. It returns the mount
// point.
func mountContainerFS(id string, layers []string, uids, gids IDRange) (string, error) {
	if len(layers) == 0 {
		return "", errors.New("no filesystem layers")
	}
	for _, layer := range layers {
		// The mount options use these to separate layers and options.
		if strings.ContainsAny(layer, ":,\\") {
			return "", fmt.Errorf("layer path %q contains ':', ',', or '\\', which overlayfs can't take", layer)
		}
	}
	// Init runs as container root, an unprivileged host user, and has to
	// reach the mount point. 0711 lets anyone pass through the top
	// directory, and the container's own directory lets in only its group,
	// container root's.
	if err := os.MkdirAll(containerFSRoot, 0o711); err != nil {
		return "", fmt.Errorf("create %s: %w", containerFSRoot, err)
	}
	dir := containerFSDir(id)
	if err := os.Mkdir(dir, 0o710); err != nil {
		return "", fmt.Errorf("create container filesystem directory: %w", err)
	}
	// cleanup removes what has been made so far, on any failure from here
	// on.
	cleanup := func() { os.RemoveAll(dir) }
	if err := os.Chown(dir, 0, gids.Base); err != nil {
		cleanup()
		return "", fmt.Errorf("set owner of %s: %w", dir, err)
	}

	upper := filepath.Join(dir, "upper")
	work := filepath.Join(dir, "work")
	merged := filepath.Join(dir, "rootfs")
	// The root directory of the overlay takes its owner and mode from the
	// writable layer: container root, with the mode of the top layer's
	// root.
	mode := os.FileMode(0o755)
	if info, err := os.Stat(layers[0]); err == nil {
		mode = info.Mode().Perm()
	}
	// The steps are a slice of anonymous functions, run in order, so that
	// one error check covers them all.
	for _, step := range []func() error{
		func() error { return os.Mkdir(upper, mode) },
		func() error { return os.Chmod(upper, mode) }, // undo the umask
		func() error { return os.Chown(upper, uids.Base, gids.Base) },
		func() error { return os.Mkdir(work, 0o700) },
		func() error { return os.Mkdir(merged, 0o755) },
	} {
		if err := step(); err != nil {
			cleanup()
			return "", fmt.Errorf("prepare container filesystem: %w", err)
		}
	}

	// The overlay's options name its layers: lowerdir lists the read-only
	// ones, top first and separated by colons, upperdir is the writable one,
	// and workdir the scratch space.
	//
	// redirect_dir=off and metacopy=off keep the writable layer complete in
	// itself, so runt build can turn it into an image layer (see
	// buildlayer_linux.go). Otherwise, renaming a directory from a lower
	// layer could leave only a pointer to it in the writable layer, and
	// changing a file's owner or mode only its new metadata, without its
	// content. Either feature may be on by default, depending on how the
	// kernel was built.
	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,redirect_dir=off,metacopy=off", strings.Join(layers, ":"), upper, work)
	// Mount options must fit in one page.
	if len(options) >= os.Getpagesize() {
		cleanup()
		return "", fmt.Errorf("too many layers (%d) to fit overlayfs's mount options", len(layers))
	}
	// nodev: an image could contain device files, and a mount made by host
	// root would let the container open them.
	if err := syscall.Mount("overlay", merged, "overlay", syscall.MS_NODEV, options); err != nil {
		cleanup()
		return "", fmt.Errorf("mount overlay filesystem: %w", err)
	}
	return merged, nil
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
	if mounted, err := isMountPoint(merged); err != nil || mounted {
		return fmt.Errorf("%s is still mounted", merged)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove container filesystem: %w", err)
	}
	return nil
}

// isMountPoint reports whether something is mounted at path, which then has
// a different device from its parent directory. A missing path isn't one.
// Lstat describes a file (without following a symlink), including Dev, the
// number of the filesystem it is on.
func isMountPoint(path string) (bool, error) {
	var stat, parent syscall.Stat_t
	if err := syscall.Lstat(path, &stat); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return false, nil
		}
		return false, err
	}
	if err := syscall.Lstat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	return stat.Dev != parent.Dev, nil
}

// pruneContainerFS removes writable layers that no container owns any more.
// /run is a tmpfs, so after a reboot the containers' state is gone but their
// writable layers in /var/lib remain, with nothing left to remove them.
// Every container's state directory is made before its writable layer, and
// removed after it, so a layer without one is an orphan.
func pruneContainerFS() {
	entries, err := os.ReadDir(containerFSRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if _, err := os.Stat(stateDir(StateRoot, entry.Name())); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := removeContainerFS(entry.Name()); err != nil {
			warn(err)
		}
	}
}
