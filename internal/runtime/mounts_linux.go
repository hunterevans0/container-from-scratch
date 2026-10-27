//go:build linux

package runtime

// Making a container's bind mounts, volumes, and tmpfs mounts (see
// mounts.go). The supervisor and init each do part of it:
//
//  1. The supervisor, as host root, makes any volumes that don't exist yet,
//     filling new ones from the image, and bind-mounts each bind mount's and
//     volume's directory onto a staging directory of the container's own,
//     /run/runt-mounts/<id>/<n> (stageMounts). Init couldn't do this itself:
//     it runs as container root, an unprivileged host user, who can't
//     usually get to a directory such as /home/you/project.
//  2. Init, cloned with a copy of the host's mounts, staging ones included,
//     bind-mounts each staging directory to where it belongs in the
//     container's root filesystem, and mounts each tmpfs (makeMounts). This
//     comes before pivot_root, while the staging directories are still in
//     sight, and after /dev is made, so a mount can go inside it.
//  3. Once init is ready, the supervisor unmounts the staging directories
//     (unstageMounts). Init's copies of the mounts are its own, and stay.
//
// For how to read the Go here, see doc.go.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// mountStagingRoot has a directory per container being set up, named by its
// ID, with a mount point for each of its bind mounts and volumes. /run is a
// tmpfs, so nothing is left here after a reboot.
const mountStagingRoot = "/run/runt-mounts"

func mountStagingDir(id string) string {
	return filepath.Join(mountStagingRoot, id)
}

// stageMounts runs in the supervisor, before init is cloned. It returns
// mounts as init is to make them: each volume made if need be, and each bind
// mount and volume turned into a bind mount from its staging directory.
// rootfs is the container's root filesystem, as mounted on the host, which
// new volumes are filled from if populate is set. The caller holds the
// volumes' lock, and calls unstageMounts afterwards, even if this fails.
func stageMounts(id string, mounts []Mount, rootfs string, populate bool, uids, gids IDRange) ([]Mount, error) {
	// Container root has to find the mount points, and nobody else need
	// see what they are: 0711 lets anyone pass through the top directory,
	// and the container's own lets in only its group, container root's.
	if err := os.MkdirAll(mountStagingRoot, 0o711); err != nil {
		return nil, fmt.Errorf("create %s: %w", mountStagingRoot, err)
	}
	dir := mountStagingDir(id)
	if err := os.Mkdir(dir, 0o710); err != nil {
		return nil, fmt.Errorf("create mount staging directory: %w", err)
	}
	if err := os.Chown(dir, 0, gids.Base); err != nil {
		return nil, fmt.Errorf("set owner of %s: %w", dir, err)
	}
	staged := make([]Mount, len(mounts))
	for i, mount := range mounts {
		if mount.Type == MountTmpfs {
			staged[i] = mount
			continue
		}
		source := mount.Source
		if mount.Type == MountVolume {
			volume, _, err := ensureVolume(mount.Source, uids, gids)
			if err != nil {
				return nil, err
			}
			source = volume.Mountpoint
			if populate && !mount.hasOption("nocopy") {
				if err := populateVolume(source, rootfs, mount.Destination); err != nil {
					return nil, fmt.Errorf("fill volume %s from the image: %w", mount.Source, err)
				}
			}
		}
		// Stat follows a symbolic link, as the bind mount will.
		info, err := os.Stat(source)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("bind mount source %s does not exist", source)
		}
		if err != nil {
			return nil, err
		}
		// A bind mount goes onto a directory if its source is one, and
		// onto a file otherwise.
		target := filepath.Join(dir, strconv.Itoa(i))
		if info.IsDir() {
			err = os.Mkdir(target, 0o700)
		} else {
			err = os.WriteFile(target, nil, 0o600)
		}
		if err != nil {
			return nil, fmt.Errorf("create mount point %s: %w", target, err)
		}
		// MS_REC takes along whatever is mounted inside the source, as
		// Docker's bind mounts do.
		if err := syscall.Mount(source, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return nil, fmt.Errorf("bind mount %s: %w", source, err)
		}
		// A bind mount ignores MS_RDONLY and the like the first time, so
		// read-only takes a second step. Init's copy keeps the flags, and,
		// as it came from the host, can't be made writable again.
		if flags, _ := mountFlags(mount.Options); flags != 0 {
			if err := remountBind(target, flags); err != nil {
				return nil, fmt.Errorf("make the mount of %s %s: %w", source, strings.Join(mount.Options, ","), err)
			}
		}
		staged[i] = Mount{Type: MountBind, Source: target, Destination: mount.Destination}
	}
	return staged, nil
}

// unstageMounts unmounts container id's staging directories and removes
// them. Each is removed with os.Remove, never os.RemoveAll: if one were
// still mounted somehow, RemoveAll would delete the files of the directory
// mounted there, on the host.
func unstageMounts(id string) error {
	dir := mountStagingDir(id)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		target := filepath.Join(dir, entry.Name())
		// EINVAL: nothing is mounted there, as when making it failed.
		if err := syscall.Unmount(target, syscall.MNT_DETACH); err != nil && !errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("unmount %s: %w", target, err)
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove %s: %w", target, err)
		}
	}
	return os.Remove(dir)
}

// mountFlags turns mount options into the flags for the mount system call
// (ro, nosuid, nodev, and noexec), and the options it passes on to the
// filesystem (those with an =, such as tmpfs's size=). Others, such as
// nocopy, are runt's own, and left out.
func mountFlags(options []string) (flags uintptr, data string) {
	var passed []string
	for _, option := range options {
		switch option {
		case "ro":
			flags |= syscall.MS_RDONLY
		case "nosuid":
			flags |= syscall.MS_NOSUID
		case "nodev":
			flags |= syscall.MS_NODEV
		case "noexec":
			flags |= syscall.MS_NOEXEC
		default:
			if strings.Contains(option, "=") {
				passed = append(passed, option)
			}
		}
	}
	return flags, strings.Join(passed, ",")
}

// remountBind adds flags, such as MS_RDONLY, to the bind mount at path. A
// remount sets every flag at once, so it has to repeat the nosuid, nodev,
// and noexec flags the mount already has, or it would take them away. In a
// user namespace, the kernel also locks them on mounts that came from a
// more privileged one, and refuses a remount that would clear them.
func remountBind(path string, flags uintptr) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("read mount flags of %s: %w", path, err)
	}
	// ST_NOSUID, ST_NODEV, and ST_NOEXEC have the same values as the MS_
	// flags.
	kept := uintptr(stat.Flags) & (syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	return syscall.Mount("", path, "", syscall.MS_BIND|syscall.MS_REMOUNT|flags|kept, "")
}

// populateVolume fills an empty volume, whose files are in data, with what
// the container's image has at destination, as Docker does: so a volume at
// a database image's /var/lib/postgresql starts with the image's files
// there, owners and permissions included. rootfs is the container's root
// filesystem, as mounted on the host. A volume with files in it already, or
// an image without a directory at destination, is left as it is.
//
// The volume's files are owned by whoever owns them in the image, shifted
// into the container's ID range as the image's layers are.
func populateVolume(data, rootfs, destination string) error {
	entries, err := os.ReadDir(data)
	if err != nil || len(entries) > 0 {
		return err
	}
	source, err := resolveInRoot(rootfs, destination)
	if err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return nil
	}
	if err != nil {
		return err
	}
	return copyTree(source, data)
}

// copyTree copies the directory source into the existing directory target:
// directories, regular files, and symbolic links, with their owners and
// permissions, and target takes source's own. Anything else, such as a
// device or a socket, is skipped. Symbolic links are copied as links, never
// followed, so nothing outside source is copied.
//
// filepath.WalkDir calls the function for source and everything under it,
// each directory before what is in it.
func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		to := filepath.Join(target, relative)
		mode := info.Mode()
		switch {
		case relative == ".":
		case mode.IsDir():
			err = os.Mkdir(to, 0o700)
		case mode.IsRegular():
			err = copyFile(path, to)
		case mode&fs.ModeSymlink != 0:
			var link string
			if link, err = os.Readlink(path); err == nil {
				err = os.Symlink(link, to)
			}
		default:
			return nil
		}
		if err != nil {
			return err
		}
		// Sys is the kernel's own description of the file, with its owner.
		stat := info.Sys().(*syscall.Stat_t)
		if err := os.Lchown(to, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
		if mode&fs.ModeSymlink != 0 {
			return nil
		}
		// After the owner: changing the owner clears the setuid and setgid
		// bits.
		return os.Chmod(to, mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	})
}

// copyFile copies a regular file's contents to a new file. O_NOFOLLOW makes
// opening the source fail rather than follow it, should it have been
// swapped for a symbolic link in the meantime, and O_EXCL makes creating the
// copy fail rather than write through one already there.
func copyFile(from, to string) error {
	in, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// makeMounts runs in init, before pivot_root: it makes each of mounts, as
// stageMounts returned them, at its destination in rootfs, in order. A
// destination that doesn't exist is made, in the container's writable
// layer, unless keepRoot is set: then the root is a bundle's root
// filesystem, which runt doesn't change.
func makeMounts(rootfs string, mounts []Mount, keepRoot bool) error {
	if len(mounts) == 0 {
		return nil
	}
	// The root as it really is, symbolic links and all followed, for
	// openMountTarget to check paths against.
	root, err := filepath.EvalSymlinks(rootfs)
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		if err := makeMount(root, mount, keepRoot); err != nil {
			return fmt.Errorf("mount %s: %w", mount.Destination, err)
		}
	}
	return nil
}

// makeMount makes one mount at its destination in root.
//
// It mounts onto /proc/self/fd/N, N being a descriptor for the mount point,
// rather than onto the mount point's path. The path would be looked up
// again when mounting, and could lead somewhere else by then; the
// descriptor is the very directory or file openMountTarget checked. This is
// how runc makes its mounts too. /proc is still the host's at this point,
// in which init is a process like any other.
func makeMount(root string, mount Mount, keepRoot bool) error {
	dir := true
	if mount.Type == MountBind {
		info, err := os.Stat(mount.Source)
		if err != nil {
			return err
		}
		dir = info.IsDir()
	}
	target, err := openMountTarget(root, mount.Destination, dir, keepRoot)
	if err != nil {
		return err
	}
	defer target.Close()
	through := fmt.Sprintf("/proc/self/fd/%d", target.Fd())
	if mount.Type == MountBind {
		// The staging mount's flags, read-only for one, come along.
		return syscall.Mount(mount.Source, through, "", syscall.MS_BIND|syscall.MS_REC, "")
	}
	flags, data := mountFlags(mount.Options)
	return syscall.Mount("tmpfs", through, "tmpfs", flags, data)
}

// oPath is O_PATH, which the syscall package doesn't define, from
// <asm-generic/fcntl.h>, as on amd64 and arm64.
const oPath = 0o10000000

// openMountTarget opens the mount point for destination in root, making it
// if need be (see makeMounts): a directory if dir is set, or else an empty
// file. It checks that what it opened is under root, and is of the right
// kind.
//
// O_PATH opens a file only to refer to it, not to read or write it, which
// works for a directory too, and O_NOFOLLOW opens a symbolic link itself
// rather than where it leads.
func openMountTarget(root, destination string, dir, keepRoot bool) (*os.File, error) {
	path, err := resolveInRoot(root, destination)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if keepRoot {
			return nil, errors.New("no such file or directory in the bundle's root filesystem, which runt doesn't change")
		}
		// resolveInRoot followed every symbolic link there was, so these
		// are made where the container will see them.
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if dir {
			err = os.Mkdir(path, 0o755)
		} else {
			var file *os.File
			if file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
				file.Close()
			}
		}
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	fd, err := syscall.Open(path, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	target := os.NewFile(uintptr(fd), path)
	// fail closes the descriptor and returns an error, for the checks
	// below.
	fail := func(format string, args ...any) (*os.File, error) {
		target.Close()
		return nil, fmt.Errorf(format, args...)
	}
	// The kernel's own idea of where the descriptor leads.
	real, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return fail("find mount point: %w", err)
	}
	if real == root || !isUnder(real, root) {
		return fail("the mount point leads out of the container's root filesystem, to %s", real)
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fail("stat mount point: %w", err)
	}
	switch kind := stat.Mode & syscall.S_IFMT; {
	case kind == syscall.S_IFLNK:
		return fail("the mount point is a symbolic link")
	case dir && kind != syscall.S_IFDIR:
		return fail("the mount point isn't a directory, and what is mounted there is")
	case !dir && kind == syscall.S_IFDIR:
		return fail("the mount point is a directory, and what is mounted there isn't")
	}
	return target, nil
}

// resolveInRoot returns the path that p, a path in the container, leads to
// in root, the container's root filesystem as the host sees it: with each
// symbolic link along the way followed as the container would follow it,
// with root as /, and so never out of root. What doesn't exist is taken as
// it is. (Before pivot_root, the kernel would follow a link to /etc to the
// host's /etc.)
//
// It works through p a part at a time, keeping resolved, the path so far
// relative to root, with no links in it. A link puts its target in front
// of what is left of p, and starts again from root if the target is
// absolute. A .. goes up a part, but never above root.
func resolveInRoot(root, p string) (string, error) {
	resolved := ""
	remaining := p
	links := 0
	for remaining != "" {
		var part string
		part, remaining, _ = strings.Cut(strings.TrimLeft(remaining, "/"), "/")
		switch part {
		case "", ".":
			continue
		case "..":
			// filepath.Dir("a") is ".", which is root.
			if resolved = filepath.Dir(resolved); resolved == "." {
				resolved = ""
			}
			continue
		}
		next := filepath.Join(resolved, part)
		info, err := os.Lstat(filepath.Join(root, next))
		// ENOTDIR: something along the way is a file, so this can't exist
		// either.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || (err == nil && info.Mode()&fs.ModeSymlink == 0) {
			resolved = next
			continue
		}
		if err != nil {
			return "", err
		}
		// 40 links is the kernel's own limit.
		if links++; links > 40 {
			return "", fmt.Errorf("%s: %w", p, syscall.ELOOP)
		}
		link, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(link, "/") {
			resolved = ""
		}
		remaining = link + "/" + remaining
	}
	return filepath.Join(root, resolved), nil
}
