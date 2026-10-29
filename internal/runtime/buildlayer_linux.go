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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"
)

// Turning a container's writable layer into an image layer, for runt build:
// unpacking a layer (layer_linux.go) the other way round. The writable layer
// holds exactly what changed on top of the layers underneath, which is what
// an image layer holds too, only marked overlayfs's way:
//
//   - a deleted file is a character device with device number 0/0, which
//     becomes an empty .wh.NAME;
//   - a directory whose earlier contents are hidden (one deleted and made
//     again) has the trusted.overlay.opaque attribute, which becomes an
//     empty .wh..wh..opq inside it;
//   - files are owned by host IDs in the container's range, which are
//     shifted back down to the container's own.
//
// The archive is gzipped as it is written, and both digests worked out on
// the way, as unpackLayer does when reading one: the diff ID of the tar
// archive, and the digest of the compressed blob. For how to read the Go
// here, see doc.go.

// layerBlob is a layer runt build made: a file holding the gzipped blob,
// its descriptor, and its diff ID.
type layerBlob struct {
	path   string
	desc   ociDescriptor
	diffID string
}

// exportLayer writes the changes in upper, a container's writable layer, as
// a gzipped layer in a new file in dir. It returns nil if there are none.
// skip, if set, leaves out entries by their path relative to upper.
//
// The archive is written through a chain of writers, the opposite of
// unpackLayer's readers: the tar writer writes to a MultiWriter that feeds
// both the diff ID's hash and the gzip compressor, which writes to another
// MultiWriter feeding the blob's hash and the file.
func exportLayer(upper string, uids, gids IDRange, skip func(rel string, info fs.FileInfo) bool, dir string) (*layerBlob, error) {
	file, err := os.CreateTemp(dir, "layer-")
	if err != nil {
		return nil, err
	}
	// keep is set once the file is complete and wanted; otherwise it goes.
	keep := false
	defer func() {
		file.Close()
		if !keep {
			os.Remove(file.Name())
		}
	}()
	blobHash := sha256.New()
	compressed := gzip.NewWriter(io.MultiWriter(file, blobHash))
	diffHash := sha256.New()
	archive := tar.NewWriter(io.MultiWriter(compressed, diffHash))
	written, err := writeChanges(archive, upper, uids, gids, skip)
	if err != nil {
		return nil, err
	}
	if written == 0 {
		return nil, nil
	}
	// Closing each writer writes what it still holds: the end-of-archive
	// marker, then the end of the compressed stream.
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	keep = true
	return &layerBlob{
		path: file.Name(),
		desc: ociDescriptor{
			MediaType: mediaTypeLayerGzip,
			Digest:    "sha256:" + hex.EncodeToString(blobHash.Sum(nil)),
			Size:      info.Size(),
		},
		diffID: "sha256:" + hex.EncodeToString(diffHash.Sum(nil)),
	}, nil
}

// writeChanges writes an entry to archive for everything in upper, and
// returns how many it wrote. WalkDir goes through upper in name order,
// each directory before what is in it, which is the order a layer needs.
func writeChanges(archive *tar.Writer, upper string, uids, gids IDRange, skip func(rel string, info fs.FileInfo) bool) (int, error) {
	written := 0
	// Hard links: the first path seen for each file (by device and inode
	// number) that has more than one name. Later names become links to it.
	type inode struct{ dev, ino uint64 }
	linked := map[inode]string{}
	err := filepath.WalkDir(upper, func(hostPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(upper, hostPath)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if skip != nil && skip(rel, info) {
			return nil
		}
		stat := info.Sys().(*syscall.Stat_t)
		mode := info.Mode()

		// A whiteout: a character device numbered 0/0.
		if mode&fs.ModeCharDevice != 0 && stat.Rdev == 0 {
			dir, base := path.Split(rel)
			written++
			return archive.WriteHeader(&tar.Header{Name: dir + whiteoutPrefix + base, Typeflag: tar.TypeReg, Mode: 0o600, ModTime: info.ModTime().Truncate(time.Second)})
		}
		var link string
		switch {
		case mode&fs.ModeSymlink != 0:
			if link, err = os.Readlink(hostPath); err != nil {
				return err
			}
		case mode.IsDir(), mode.IsRegular(), mode&fs.ModeNamedPipe != 0:
		default:
			// Sockets, and devices other than whiteouts, which the
			// container couldn't have made.
			return nil
		}
		// FileInfoHeader fills in the header from the file's details. The
		// host's user and group names, and its access and change times,
		// mean nothing in an image, so they are cleared.
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = rel
		if mode.IsDir() {
			header.Name += "/"
		}
		header.Uname, header.Gname = "", ""
		header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
		header.ModTime = header.ModTime.Truncate(time.Second)
		header.Uid, header.Gid = int(stat.Uid)-uids.Base, int(stat.Gid)-gids.Base
		if header.Uid < 0 || header.Uid >= uids.Size || header.Gid < 0 || header.Gid >= gids.Size {
			return fmt.Errorf("%s is owned by host %d:%d, outside the container's ID range", rel, stat.Uid, stat.Gid)
		}
		if mode.IsRegular() && stat.Nlink > 1 {
			key := inode{uint64(stat.Dev), stat.Ino}
			if first, ok := linked[key]; ok {
				header.Typeflag, header.Linkname, header.Size = tar.TypeLink, first, 0
			} else {
				linked[key] = rel
			}
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		written++
		if header.Typeflag == tar.TypeReg {
			if err := copyInto(archive, hostPath); err != nil {
				return err
			}
		}
		if mode.IsDir() && isOpaque(hostPath) {
			written++
			return archive.WriteHeader(&tar.Header{Name: rel + "/" + whiteoutOpaque, Typeflag: tar.TypeReg, Mode: 0o600, ModTime: header.ModTime})
		}
		return nil
	})
	return written, err
}

// copyInto writes the content of the file at path to w.
func copyInto(w io.Writer, path string) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}

// isOpaque reports whether overlayfs has marked the directory at path
// opaque, hiding what the layers underneath have in it. Getxattr reads an
// extended attribute's value into the slice it is given, and returns its
// length; ENODATA means the directory doesn't have it, and ENOTSUP that its
// filesystem has no such attributes at all.
func isOpaque(path string) bool {
	value := make([]byte, 8)
	n, err := syscall.Getxattr(path, "trusted.overlay.opaque", value)
	if err != nil && !errors.Is(err, syscall.ENODATA) && !errors.Is(err, syscall.ENOTSUP) {
		warn(fmt.Errorf("read overlay attribute of %s: %w", path, err))
	}
	return err == nil && string(value[:n]) == "y"
}
