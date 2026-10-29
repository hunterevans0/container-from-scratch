//go:build linux

package runtime

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// COPY and ADD, for runt build (build_linux.go): finding the files to copy,
// in the build context or another stage's filesystem, working out a digest
// of them for the build cache, and writing them into the image's root
// filesystem, mounted on the host as a container's would be.
//
// Both the files and the image come from someone else, so neither may lead
// outside where it belongs. Paths in the context or a stage are looked up
// with resolveInRoot, with that directory as /, so that .. and symbolic
// links can't climb out of it; and so are paths in the image, where a
// symbolic link to /etc means the image's /etc, never the host's.
//
// For how to read the Go here, see doc.go.

// copyRoot is a directory COPY or ADD takes files from: the build context,
// with what .dockerignore leaves out, or another stage's (or image's) root
// filesystem.
type copyRoot struct {
	dir string
	// ignore holds .dockerignore's patterns, for the context.
	ignore []ignorePattern
}

// openContext opens the build context, reading its .dockerignore if it has
// one.
func openContext(dir string) (*copyRoot, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("build context %s is not a directory", dir)
	}
	root := &copyRoot{dir: dir}
	file, err := os.Open(filepath.Join(dir, ".dockerignore"))
	if errors.Is(err, os.ErrNotExist) {
		return root, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if root.ignore, err = parseDockerignore(file); err != nil {
		return nil, fmt.Errorf(".dockerignore: %w", err)
	}
	return root, nil
}

// excluded reports whether .dockerignore leaves out rel.
func (r *copyRoot) excluded(rel string) bool {
	return len(r.ignore) > 0 && ignored(r.ignore, rel)
}

// copySource is one file or directory a COPY or ADD names, after any
// wildcards in its name are matched.
type copySource struct {
	root *copyRoot
	// rel is its path in root, slash-separated, and "" for root itself.
	rel string
	// host is where it is on the host, with every symbolic link on the way
	// followed, within root.
	host string
	info fs.FileInfo
	// url, for ADD, is where it was downloaded from; it is then in a file
	// at host, with no root.
	url string
}

// findSources returns what pattern names in root: one source, or with
// wildcards, every one that matches, in name order. It fails if there is
// none.
func (r *copyRoot) findSources(pattern string) ([]copySource, error) {
	clean := strings.TrimPrefix(path.Clean("/"+pattern), "/")
	if !strings.ContainsAny(clean, "*?[") {
		source, err := r.source(clean)
		if err != nil {
			return nil, err
		}
		return []copySource{source}, nil
	}
	// Walk from the deepest directory with no wildcard in its name, and
	// match everything under it as deep as the pattern goes.
	// path.Match's * doesn't match a /, so each part of the pattern
	// matches one part of a path.
	var fixed []string
	for _, part := range strings.Split(clean, "/") {
		if strings.ContainsAny(part, "*?[") {
			break
		}
		fixed = append(fixed, part)
	}
	start := strings.Join(fixed, "/")
	depth := strings.Count(clean, "/") + 1
	base, err := resolveInRoot(r.dir, start)
	if err != nil {
		return nil, err
	}
	var sources []copySource
	err = filepath.WalkDir(base, func(hostPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		under, _ := filepath.Rel(base, hostPath)
		rel := path.Join(start, filepath.ToSlash(under))
		if under == "." {
			return nil
		}
		matched, _ := path.Match(clean, rel)
		if matched && !r.excluded(rel) {
			source, err := r.source(rel)
			if err != nil {
				return err
			}
			sources = append(sources, source)
		}
		if entry.IsDir() && strings.Count(rel, "/")+1 >= depth {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no files match %s", pattern)
	}
	return sources, nil
}

// source looks up the file or directory at rel in root, following
// symbolic links within it.
func (r *copyRoot) source(rel string) (copySource, error) {
	if rel != "" && r.excluded(rel) {
		return copySource{}, fmt.Errorf("%s: %w (or .dockerignore leaves it out)", rel, os.ErrNotExist)
	}
	host, err := resolveInRoot(r.dir, rel)
	if err != nil {
		return copySource{}, err
	}
	info, err := os.Lstat(host)
	if err != nil {
		return copySource{}, fmt.Errorf("%s: %w", rel, os.ErrNotExist)
	}
	return copySource{root: r, rel: rel, host: host, info: info}, nil
}

// sourceEntry is a file or directory in a source: its path relative to the
// source, "" for the source itself, and its details.
type sourceEntry struct {
	rel  string
	host string
	info fs.FileInfo
}

// walk calls fn for the source and, if it is a directory, everything in it,
// each directory before what is in it, leaving out what .dockerignore does.
// A directory .dockerignore leaves out is still looked through if an
// exception might bring back something in it.
func (s copySource) walk(fn func(entry sourceEntry) error) error {
	if !s.info.IsDir() {
		return fn(sourceEntry{host: s.host, info: s.info})
	}
	exceptions := s.root != nil && hasExceptions(s.root.ignore)
	return filepath.WalkDir(s.host, func(hostPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		under, _ := filepath.Rel(s.host, hostPath)
		under = filepath.ToSlash(under)
		if under == "." {
			under = ""
		}
		if under != "" && s.root != nil && s.root.excluded(path.Join(s.rel, under)) {
			if entry.IsDir() && !exceptions {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return fn(sourceEntry{rel: under, host: hostPath, info: info})
	})
}

// hash feeds what the source holds into h, for the build cache: each
// entry's path, type, permissions, and link target or content. Owners and
// times are left out, as Docker leaves them out: COPY sets the owner, and
// a file that is touched but not changed shouldn't cost the cache.
func (s copySource) hash(h io.Writer) error {
	fmt.Fprintf(h, "source %s %s\n", s.rel, s.url)
	return s.walk(func(entry sourceEntry) error {
		mode := entry.info.Mode()
		fmt.Fprintf(h, "%q %o %s\n", entry.rel, uint32(mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)), mode.Type())
		switch {
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(entry.host)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "-> %q\n", link)
		case mode.IsRegular():
			file, err := os.OpenFile(entry.host, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer file.Close()
			sum := sha256.New()
			if _, err := io.Copy(sum, file); err != nil {
				return err
			}
			fmt.Fprintf(h, "%x\n", sum.Sum(nil))
		}
		return nil
	})
}

// rootWriter writes files into an image's root filesystem, mounted on the
// host at root, as COPY and ADD do. Container IDs are written as host IDs in
// the container's ranges.
type rootWriter struct {
	root       string
	uids, gids IDRange
	// owner, if not nil, owns everything written, as --chown gives it;
	// otherwise each entry has its own. dirOwner owns directories made
	// along the way.
	owner    *User
	dirOwner User
	// mode, if not nil, replaces each file's and directory's permissions,
	// as --chmod gives them.
	mode *fs.FileMode
	// dirs are the directories written, whose times are set at the end
	// (see finish), since writing into a directory changes its time.
	dirs []sourceEntryTime
}

// sourceEntryTime is a directory and the time it should have.
type sourceEntryTime struct {
	path  string
	mtime time.Time
}

// dir returns where the directory at p in the image is on the host, making
// it and any directories above it that don't exist. Symbolic links on the
// way are followed within the image.
func (w *rootWriter) dir(p string) (string, error) {
	host, err := resolveInRoot(w.root, p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(w.root, host)
	if err != nil {
		return "", err
	}
	current := w.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(current, 0o755); err != nil {
				return "", err
			}
			if err := os.Lchown(current, w.uids.Base+int(w.dirOwner.UID), w.gids.Base+int(w.dirOwner.GID)); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case !info.IsDir():
			return "", fmt.Errorf("%s: %s is not a directory", p, strings.TrimPrefix(current, w.root))
		}
	}
	return host, nil
}

// put writes one entry at p in the image, as header describes it, with
// content for a regular file. What is there already is replaced, unless both
// are directories, whose contents then merge. header.Uid and Gid are
// container IDs. For a hard link, header.Linkname is the path of the file in
// the image it links to.
func (w *rootWriter) put(p string, header *tar.Header, content io.Reader) error {
	parent, err := w.dir(path.Dir(p))
	if err != nil {
		return err
	}
	base := path.Base(p)
	if base == "/" || base == "." {
		return nil
	}
	target := filepath.Join(parent, base)
	isDir := header.Typeflag == tar.TypeDir
	if existing, err := os.Lstat(target); err == nil && !(existing.IsDir() && isDir) {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	switch header.Typeflag {
	case tar.TypeDir:
		err = os.Mkdir(target, 0o700)
		if errors.Is(err, os.ErrExist) {
			err = nil
		}
	case tar.TypeReg:
		err = writeLayerFile(target, content)
	case tar.TypeSymlink:
		err = os.Symlink(header.Linkname, target)
	case tar.TypeLink:
		var linkDir string
		if linkDir, err = w.dir(path.Dir(header.Linkname)); err == nil {
			err = os.Link(filepath.Join(linkDir, path.Base(header.Linkname)), target)
		}
		// A hard link is its target, owner and all.
		if err != nil {
			return fmt.Errorf("hard link %s to %s: %w", p, header.Linkname, err)
		}
		return nil
	case tar.TypeFifo:
		err = syscall.Mkfifo(target, 0o600)
	default:
		// Devices and the like can't be made in a user namespace's image.
		return nil
	}
	if err != nil {
		return err
	}

	uid, gid := header.Uid, header.Gid
	if w.owner != nil {
		uid, gid = int(w.owner.UID), int(w.owner.GID)
	}
	if uid < 0 || uid >= w.uids.Size || gid < 0 || gid >= w.gids.Size {
		return fmt.Errorf("%s would be owned by %d:%d, outside the container's ID range", p, uid, gid)
	}
	if err := os.Lchown(target, w.uids.Base+uid, w.gids.Base+gid); err != nil {
		return err
	}
	if header.Typeflag == tar.TypeSymlink {
		return nil
	}
	// After the owner, since changing the owner clears setuid and setgid.
	mode := header.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	if w.mode != nil {
		mode = *w.mode
	}
	if err := os.Chmod(target, mode); err != nil {
		return err
	}
	if isDir {
		w.dirs = append(w.dirs, sourceEntryTime{target, header.ModTime})
		return nil
	}
	return os.Chtimes(target, header.ModTime, header.ModTime)
}

// finish sets the times of the directories written, deepest last written
// first.
func (w *rootWriter) finish() error {
	for i := len(w.dirs) - 1; i >= 0; i-- {
		if err := os.Chtimes(w.dirs[i].path, w.dirs[i].mtime, w.dirs[i].mtime); err != nil {
			return err
		}
	}
	return nil
}

// copySource writes source to dest in the image: a file at dest, or a
// directory's contents into dest, as COPY does. COPY's files are owned by
// container root unless --chown says otherwise.
func (w *rootWriter) copySource(source copySource, dest string) error {
	return source.walk(func(entry sourceEntry) error {
		target := dest
		if entry.rel != "" {
			target = path.Join(dest, entry.rel)
		}
		if entry.rel == "" && entry.info.IsDir() {
			// A directory's contents go into dest; dest itself stays as
			// it is, or is made.
			_, err := w.dir(dest)
			return err
		}
		link := ""
		if entry.info.Mode()&fs.ModeSymlink != 0 {
			var err error
			if link, err = os.Readlink(entry.host); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(entry.info, link)
		if err != nil {
			// A socket, which can't be copied.
			return nil
		}
		header.Uid, header.Gid = 0, 0
		if header.Typeflag != tar.TypeReg {
			return w.put(target, header, nil)
		}
		file, err := os.OpenFile(entry.host, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		return w.put(target, header, file)
	})
}

// extractArchive unpacks the tar archive in the file at host, compressed
// with gzip or bzip2 or not at all, into the directory dest in the image,
// as ADD does with a local archive. Entries keep the archive's owners
// unless --chown says otherwise, and their names are cleaned of .. and
// leading slashes, so they land under dest.
func (w *rootWriter) extractArchive(host, dest string) error {
	file, err := os.Open(host)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := decompress(file)
	if err != nil {
		return err
	}
	archive := tar.NewReader(reader)
	if _, err := w.dir(dest); err != nil {
		return err
	}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("unpack %s: %w", filepath.Base(host), err)
		}
		name := strings.TrimPrefix(path.Clean("/"+header.Name), "/")
		if name == "" {
			continue
		}
		if header.Typeflag == tar.TypeLink {
			header.Linkname = path.Join(dest, strings.TrimPrefix(path.Clean("/"+header.Linkname), "/"))
		}
		if err := w.put(path.Join(dest, name), header, archive); err != nil {
			return err
		}
	}
}

// archiveKind says whether the file at path is an archive ADD unpacks, by
// its first bytes: "gzip", "bzip2", or "tar", or "" if it is none of
// these. An xz archive is an error, since Go's standard library can't
// decompress it.
func archiveKind(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return "gzip", nil
	case len(head) >= 3 && string(head[:3]) == "BZh":
		return "bzip2", nil
	case len(head) >= 6 && string(head[:6]) == "\xfd7zXZ\x00":
		return "", fmt.Errorf("%s is an xz archive, which ADD can't unpack; use COPY and RUN tar", filepath.Base(path))
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		// A tar header has "ustar" at byte 257.
		return "tar", nil
	}
	return "", nil
}

// decompress returns a reader of what file holds, decompressed if it is
// gzipped or bzipped.
func decompress(file *os.File) (io.Reader, error) {
	buffered := bufio.NewReader(file)
	head, _ := buffered.Peek(3)
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return gzip.NewReader(buffered)
	case string(head) == "BZh":
		return bzip2.NewReader(buffered), nil
	}
	return buffered, nil
}

// download fetches a URL for ADD into a new file in dir, checking it
// against checksum (a digest) if one is given, and returns it as a source.
// Its time is the server's Last-Modified, if it sends one, and its
// permissions 0600, as Docker gives it.
func download(rawURL, checksum, dir string) (copySource, error) {
	response, err := newHTTPClient().Get(rawURL)
	if err != nil {
		return copySource{}, fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return copySource{}, fmt.Errorf("download %s: %s", rawURL, response.Status)
	}
	file, err := os.CreateTemp(dir, "download-")
	if err != nil {
		return copySource{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hash), response.Body); err != nil {
		return copySource{}, fmt.Errorf("download %s: %w", rawURL, err)
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); checksum != "" && got != checksum {
		return copySource{}, fmt.Errorf("download %s: its digest is %s, not %s", rawURL, got, checksum)
	}
	if err := file.Chmod(0o600); err != nil {
		return copySource{}, err
	}
	if modified, err := http.ParseTime(response.Header.Get("Last-Modified")); err == nil {
		os.Chtimes(file.Name(), modified, modified)
	}
	info, err := os.Lstat(file.Name())
	if err != nil {
		return copySource{}, err
	}
	return copySource{host: file.Name(), info: info, url: rawURL}, nil
}

// urlBase is the file name ADD gives what it downloads from rawURL: the last
// part of its path.
func urlBase(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

// isURL reports whether an ADD source is a URL to download.
func isURL(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// parseChown works out --chown's user and group, USER[:GROUP], by name or
// ID, from the image's /etc/passwd and /etc/group, as root sees it on the
// host. Unlike runt run -u, a user without a group gets the group with the
// same ID, not their own group, as in Docker.
func parseChown(spec, root string) (User, error) {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	lookup := func(file string) string {
		path, err := resolveInRoot(root, file)
		if err != nil {
			return ""
		}
		return path
	}
	uid, numeric := parseID(userPart)
	if !numeric {
		passwd, err := readPasswd(lookup("/etc/passwd"))
		if err != nil {
			return User{}, err
		}
		index := slices.IndexFunc(passwd, func(entry passwdEntry) bool { return entry.name == userPart })
		if index < 0 {
			return User{}, fmt.Errorf("--chown: no user %q in the image's /etc/passwd", userPart)
		}
		uid = passwd[index].uid
	}
	gid := uid
	if hasGroup {
		var numericGroup bool
		if gid, numericGroup = parseID(groupPart); !numericGroup {
			groups, err := readGroup(lookup("/etc/group"))
			if err != nil {
				return User{}, err
			}
			index := slices.IndexFunc(groups, func(entry groupEntry) bool { return entry.name == groupPart })
			if index < 0 {
				return User{}, fmt.Errorf("--chown: no group %q in the image's /etc/group", groupPart)
			}
			gid = groups[index].gid
		}
	}
	return User{UID: uint32(uid), GID: uint32(gid)}, nil
}

// parseChmod reads --chmod's permissions, in octal, such as 755.
func parseChmod(text string) (fs.FileMode, error) {
	mode, err := strconv.ParseUint(text, 8, 32)
	if err != nil || mode > 0o7777 {
		return 0, fmt.Errorf("--chmod=%s: give permissions in octal, such as 755", text)
	}
	// The setuid, setgid, and sticky bits are numbered differently in
	// Go's FileMode than in octal permissions.
	result := fs.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		result |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		result |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		result |= fs.ModeSticky
	}
	return result, nil
}

// mountReadOnly makes an image's root filesystem readable on the host at
// target, for COPY --from: its layers, given top first, under a read-only
// overlay, or the one layer itself, or an empty directory if it has none.
// It returns where the root filesystem is, and a function that unmounts it.
func mountReadOnly(layers []string, target string) (string, func(), error) {
	switch len(layers) {
	case 0:
		return target, func() {}, os.MkdirAll(target, 0o755)
	case 1:
		return layers[0], func() {}, nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", nil, err
	}
	// An overlay with no writable layer is read-only, and needs at least
	// two layers.
	if err := syscall.Mount("overlay", target, "overlay", syscall.MS_RDONLY|syscall.MS_NODEV, "lowerdir="+strings.Join(layers, ":")); err != nil {
		return "", nil, fmt.Errorf("mount stage filesystem: %w", err)
	}
	return target, func() { syscall.Unmount(target, syscall.MNT_DETACH) }, nil
}
