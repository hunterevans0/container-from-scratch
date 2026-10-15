//go:build linux

package runtime

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Whiteouts in OCI layers: an empty file .wh.NAME means NAME was deleted, and
// .wh..wh..opq in a directory means everything below it in earlier layers is
// hidden. overlayfs marks the same things its own way, so they are converted
// as layers are unpacked: a deleted file becomes a 0:0 character device, and
// a hidden directory gets the trusted.overlay.opaque extended attribute.
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// unpackImage makes sure every layer of an image is unpacked, with
// its file ownership shifted into the container's ID ranges, and returns the
// layer directories top first, the order overlayfs takes them in.
//
// Layers are stored by digest under layerRoot/<uid base>-<gid base>, since
// their ownership depends on the ranges, and are shared by every container
// that uses them. A layer is unpacked into a temporary directory and renamed
// into place when complete, so a half-unpacked layer is never used, and two
// runt processes unpacking the same layer at once don't get in each other's
// way.
func unpackImage(image *ociImage, uids, gids IDRange) ([]string, error) {
	store := filepath.Join(layerRoot, fmt.Sprintf("%d-%d", uids.Base, gids.Base))
	if err := os.MkdirAll(store, 0o700); err != nil {
		return nil, fmt.Errorf("create layer store: %w", err)
	}
	var dirs []string
	for i, layer := range image.manifest.Layers {
		blob, err := image.blobPath(layer.Digest)
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(store, filepath.Base(blob))
		dirs = append(dirs, dir)
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		temp, err := os.MkdirTemp(store, "unpacking-")
		if err != nil {
			return nil, fmt.Errorf("create layer directory: %w", err)
		}
		err = unpackLayer(blob, layer, image.config.RootFS.DiffIDs[i], temp, uids, gids)
		if err == nil {
			err = os.Rename(temp, dir)
		}
		if err != nil {
			os.RemoveAll(temp)
			if _, statErr := os.Stat(dir); statErr == nil {
				// Another runt finished unpacking it first.
				continue
			}
			return nil, fmt.Errorf("unpack layer %s: %w", layer.Digest, err)
		}
	}
	slices.Reverse(dirs)
	return dirs, nil
}

// unpackLayer extracts the layer blob into dest, checking it against both its
// digest and diffID, the digest of its uncompressed content.
func unpackLayer(blob string, desc ociDescriptor, diffID, dest string, uids, gids IDRange) error {
	compression, err := layerCompression(desc.MediaType)
	if err != nil {
		return err
	}
	file, err := os.Open(blob)
	if err != nil {
		return err
	}
	defer file.Close()
	blobHash := sha256.New()
	raw := io.TeeReader(file, blobHash)
	var content io.Reader = raw
	if compression == "gzip" {
		gz, err := gzip.NewReader(raw)
		if err != nil {
			return err
		}
		defer gz.Close()
		content = gz
	}
	contentHash := sha256.New()
	content = io.TeeReader(content, contentHash)

	if err := extractLayer(content, dest, uids, gids); err != nil {
		return err
	}
	// The tar reader stops at the end-of-archive marker; the padding after
	// it still counts toward the digests.
	if _, err := io.Copy(io.Discard, content); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(blobHash.Sum(nil)); got != desc.Digest {
		return fmt.Errorf("layer content does not match its digest (got %s)", got)
	}
	if got := "sha256:" + hex.EncodeToString(contentHash.Sum(nil)); got != diffID {
		return fmt.Errorf("uncompressed layer does not match its diff ID %s (got %s)", diffID, got)
	}
	return nil
}

// extractLayer unpacks a layer's tar archive into root, which starts empty.
//
// The archive comes from an image and can't be trusted, so no entry may
// reach outside root: names are cleaned of .. and leading slashes, and no
// path may lead through a symbolic link, which could point anywhere on the
// host. Layers made from a real filesystem never need to.
//
// Ownership is shifted into the container's ID ranges, as make rootfs does
// for the plain rootfs. Device files are skipped: container root couldn't
// make them either, and the overlay is mounted nodev anyway. Extended
// attributes are skipped too; the one images use, security.capability,
// would be ignored under no_new_privs.
func extractLayer(r io.Reader, root string, uids, gids IDRange) error {
	type dirTime struct {
		path  string
		mtime time.Time
	}
	var dirs []dirTime
	devices := 0
	sawRoot := false

	archive := tar.NewReader(r)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean("/"+header.Name), "/")
		dir, base := path.Split(name)

		if base == whiteoutOpaque {
			parent, err := layerDir(root, dir, uids, gids, true)
			if err != nil {
				return err
			}
			if err := syscall.Setxattr(parent, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
				return fmt.Errorf("mark %s opaque: %w", dir, err)
			}
			continue
		}
		if strings.HasPrefix(base, whiteoutPrefix) {
			if strings.HasPrefix(base, whiteoutPrefix+whiteoutPrefix) {
				// Other .wh..wh. names are AUFS bookkeeping.
				continue
			}
			parent, err := layerDir(root, dir, uids, gids, true)
			if err != nil {
				return err
			}
			target := filepath.Join(parent, strings.TrimPrefix(base, whiteoutPrefix))
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := syscall.Mknod(target, syscall.S_IFCHR, 0); err != nil {
				return fmt.Errorf("create whiteout for %s: %w", name, err)
			}
			continue
		}

		var target string
		if name == "" {
			// The archive's entry for "./" describes root itself.
			if header.Typeflag != tar.TypeDir {
				continue
			}
			target, sawRoot = root, true
		} else {
			parent, err := layerDir(root, dir, uids, gids, true)
			if err != nil {
				return err
			}
			target = filepath.Join(parent, base)
			// A later entry for the same name replaces an earlier one,
			// except that a directory keeps its contents.
			if existing, err := os.Lstat(target); err == nil && !(existing.IsDir() && header.Typeflag == tar.TypeDir) {
				if err := os.RemoveAll(target); err != nil {
					return err
				}
			}
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
		case tar.TypeReg:
			if err := writeLayerFile(target, archive); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkName := strings.TrimPrefix(path.Clean("/"+header.Linkname), "/")
			linkDir, linkBase := path.Split(linkName)
			parent, err := layerDir(root, linkDir, uids, gids, false)
			if err != nil || linkBase == "" {
				return fmt.Errorf("hard link %s to %s: target is not in this layer", name, header.Linkname)
			}
			if err := os.Link(filepath.Join(parent, linkBase), target); err != nil {
				return fmt.Errorf("hard link %s: %w", name, err)
			}
			// A hard link shares its target's owner, mode, and times.
			continue
		case tar.TypeFifo:
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				return fmt.Errorf("create fifo %s: %w", name, err)
			}
		case tar.TypeChar, tar.TypeBlock:
			devices++
			continue
		default:
			continue
		}

		if header.Uid < 0 || header.Uid >= uids.Size || header.Gid < 0 || header.Gid >= gids.Size {
			return fmt.Errorf("%s is owned by %d:%d, outside the container's ID range", name, header.Uid, header.Gid)
		}
		if err := os.Lchown(target, uids.Base+header.Uid, gids.Base+header.Gid); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeSymlink {
			continue
		}
		// After chown, which clears setuid and setgid bits.
		if err := os.Chmod(target, header.FileInfo().Mode()); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			// Adding entries changes a directory's mtime, so set it last.
			dirs = append(dirs, dirTime{target, header.ModTime})
			continue
		}
		if err := os.Chtimes(target, header.ModTime, header.ModTime); err != nil {
			return err
		}
	}

	if !sawRoot {
		if err := os.Chmod(root, 0o755); err != nil {
			return err
		}
		if err := os.Chown(root, uids.Base, gids.Base); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chtimes(dirs[i].path, dirs[i].mtime, dirs[i].mtime); err != nil {
			return err
		}
	}
	if devices > 0 {
		warn(fmt.Errorf("skipped %d device files in an image layer", devices))
	}
	return nil
}

// layerDir returns the directory dir inside root, checking that no part of
// it is a symbolic link. With create set, missing directories are made,
// owned by container root, as the archive may not list every parent.
func layerDir(root, dir string, uids, gids IDRange, create bool) (string, error) {
	current := root
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist) && create:
			if err := os.Mkdir(current, 0o755); err != nil {
				return "", err
			}
			if err := os.Chown(current, uids.Base, gids.Base); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case !info.IsDir():
			return "", fmt.Errorf("layer path %s leads through %s, which is not a directory", dir, strings.TrimPrefix(current, root))
		}
	}
	return current, nil
}

func writeLayerFile(target string, content io.Reader) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, content); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
