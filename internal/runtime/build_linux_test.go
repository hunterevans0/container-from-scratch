//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests for build_linux.go, buildcopy_linux.go, and buildlayer_linux.go:
// the parts of runt build that don't need a container. Most run as any
// user, by mapping container root onto the user running the test (a range
// of one ID), so that giving files to "container root" is giving them to
// oneself. Those that need overlayfs's whiteouts need root, and skip
// themselves without it. Building whole images is tested in
// integration/build_test.go. For how Go tests work, see "Tests" in doc.go.

// ownRange maps container root onto the user running the test.
func ownRange() (uids, gids IDRange) {
	return IDRange{Base: os.Getuid(), Size: 1}, IDRange{Base: os.Getgid(), Size: 1}
}

// writeFiles makes files under root, given as path and content pairs. A
// path ending in / is a directory.
func writeFiles(t *testing.T, root string, files ...string) {
	t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		path := filepath.Join(root, files[i])
		if strings.HasSuffix(files[i], "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(files[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readLayer reads a gzipped layer back: each entry's header, and the
// content of regular files, by name.
func readLayer(t *testing.T, layer *layerBlob) (map[string]*tar.Header, map[string]string) {
	t.Helper()
	data, err := os.ReadFile(layer.path)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); "sha256:"+hex.EncodeToString(sum[:]) != layer.desc.Digest || int64(len(data)) != layer.desc.Size {
		t.Errorf("the layer's descriptor %+v doesn't match its file", layer.desc)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	uncompressed, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(uncompressed); "sha256:"+hex.EncodeToString(sum[:]) != layer.diffID {
		t.Errorf("the layer's diff ID %s doesn't match its content", layer.diffID)
	}
	headers, contents := map[string]*tar.Header{}, map[string]string{}
	archive := tar.NewReader(bytes.NewReader(uncompressed))
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return headers, contents
		}
		if err != nil {
			t.Fatal(err)
		}
		headers[header.Name] = header
		content, _ := io.ReadAll(archive)
		contents[header.Name] = string(content)
	}
}

// TestExportLayer turns a directory, standing in for a writable layer, into
// a layer: directories, files, a hard link, a symbolic link, and a FIFO,
// owned by container root, with the files skip names left out.
func TestExportLayer(t *testing.T) {
	uids, gids := ownRange()
	upper, out := t.TempDir(), t.TempDir()
	writeFiles(t, upper, "etc/app.conf", "setting=1\n", "etc/hosts", "", "srv/", "")
	if err := os.Link(filepath.Join(upper, "etc/app.conf"), filepath.Join(upper, "etc/same.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/app.conf", filepath.Join(upper, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(upper, "srv/fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	skip := func(rel string, info fs.FileInfo) bool { return rel == "etc/hosts" }
	layer, err := exportLayer(upper, uids, gids, skip, out)
	if err != nil || layer == nil {
		t.Fatalf("exportLayer = %v, %v", layer, err)
	}
	headers, contents := readLayer(t, layer)
	if contents["etc/app.conf"] != "setting=1\n" || headers["etc/app.conf"].Uid != 0 || headers["etc/app.conf"].Uname != "" {
		t.Errorf("etc/app.conf = %+v, %q", headers["etc/app.conf"], contents["etc/app.conf"])
	}
	if header := headers["etc/same.conf"]; header == nil || header.Typeflag != tar.TypeLink || header.Linkname != "etc/app.conf" {
		t.Errorf("the hard link is %+v", header)
	}
	if header := headers["link"]; header == nil || header.Typeflag != tar.TypeSymlink || header.Linkname != "/etc/app.conf" {
		t.Errorf("the symbolic link is %+v", header)
	}
	if header := headers["srv/fifo"]; header == nil || header.Typeflag != tar.TypeFifo {
		t.Errorf("the FIFO is %+v", header)
	}
	if headers["etc/"] == nil || headers["etc/hosts"] != nil {
		t.Errorf("entries: %v", slices.Collect(maps.Keys(headers)))
	}

	if layer, err := exportLayer(t.TempDir(), uids, gids, nil, out); err != nil || layer != nil {
		t.Errorf("exporting an empty directory = %+v, %v; want no layer", layer, err)
	}
	// Only the layer file is left: nothing from the empty export.
	if entries, _ := os.ReadDir(out); len(entries) != 1 {
		t.Errorf("exportLayer left %d files behind", len(entries))
	}
}

// TestExportLayerWhiteouts checks that overlayfs's whiteouts and opaque
// directories become OCI's, and come back as overlayfs's when the layer is
// unpacked again. It needs root, to make them.
func TestExportLayerWhiteouts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("making whiteouts needs root")
	}
	uids, gids := IDRange{Base: 100000, Size: 65536}, IDRange{Base: 100000, Size: 65536}
	upper := t.TempDir()
	writeFiles(t, upper, "var/cache/new", "fresh")
	if err := syscall.Mknod(filepath.Join(upper, "gone"), syscall.S_IFCHR, 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(filepath.Join(upper, "var/cache"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	filepath.WalkDir(upper, func(path string, _ fs.DirEntry, _ error) error {
		return os.Lchown(path, 100000, 100000)
	})
	if err := os.Lchown(filepath.Join(upper, "var/cache/new"), 101000, 101000); err != nil {
		t.Fatal(err)
	}
	layer, err := exportLayer(upper, uids, gids, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	headers, _ := readLayer(t, layer)
	if headers[".wh.gone"] == nil || headers["var/cache/.wh..wh..opq"] == nil || headers["var/cache/new"].Uid != 1000 {
		t.Fatalf("entries: %v", slices.Collect(maps.Keys(headers)))
	}

	unpacked := t.TempDir()
	if err := unpackLayer(layer.path, layer.desc, layer.diffID, unpacked, uids, gids); err != nil {
		t.Fatalf("unpack the layer: %v", err)
	}
	var stat syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(unpacked, "gone"), &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFCHR || stat.Rdev != 0 {
		t.Errorf("the whiteout came back as %+v, %v", stat, err)
	}
	if !isOpaque(filepath.Join(unpacked, "var/cache")) {
		t.Error("the opaque directory came back without its attribute")
	}
}

// TestCopySources finds COPY's sources in a context: by name, by wildcard,
// with .dockerignore leaving some out, and never outside it.
func TestCopySources(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		".dockerignore", "secret.txt\nsub/*.log\n",
		"a.txt", "a", "b.txt", "b", "secret.txt", "s", "c.md", "c",
		"sub/keep.txt", "k", "sub/debug.log", "d", "sub/deeper/x.txt", "x")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	context, err := openContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := func(sources []copySource) []string {
		var names []string
		for _, source := range sources {
			names = append(names, source.rel)
		}
		return names
	}
	if sources, err := context.findSources("*.txt"); err != nil || !slices.Equal(names(sources), []string{"a.txt", "b.txt"}) {
		t.Errorf("*.txt = %q, %v", names(sources), err)
	}
	if sources, err := context.findSources("sub/*/*.txt"); err != nil || !slices.Equal(names(sources), []string{"sub/deeper/x.txt"}) {
		t.Errorf("sub/*/*.txt = %q, %v", names(sources), err)
	}
	for _, missing := range []string{"secret.txt", "nothing", "*.go", "escape", "../../../../etc/passwd"} {
		if sources, err := context.findSources(missing); err == nil {
			t.Errorf("%s found %q", missing, names(sources))
		}
	}

	sources, err := context.findSources("sub")
	if err != nil || len(sources) != 1 || !sources[0].info.IsDir() {
		t.Fatalf("sub = %+v, %v", sources, err)
	}
	var walked []string
	sources[0].walk(func(entry sourceEntry) error {
		walked = append(walked, entry.rel)
		return nil
	})
	if !slices.Equal(walked, []string{"", "deeper", "deeper/x.txt", "keep.txt"}) {
		t.Errorf("walking sub gives %q", walked)
	}

	// The digest changes with the content, but not with the time.
	hash := func() string {
		h := sha256.New()
		if err := sources[0].hash(h); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	before := hash()
	keep := filepath.Join(dir, "sub/keep.txt")
	os.Chtimes(keep, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if hash() != before {
		t.Error("touching a file changed the digest")
	}
	writeFiles(t, dir, "sub/keep.txt", "changed")
	if hash() == before {
		t.Error("changing a file left the digest as it was")
	}
}

// TestRootWriter copies files into a stand-in for an image's root
// filesystem: with their permissions, over what was there, through the
// image's own symbolic links without leaving it, and from an archive.
func TestRootWriter(t *testing.T) {
	uids, gids := ownRange()
	root, context := t.TempDir(), t.TempDir()
	writeFiles(t, root, "etc/", "", "usr/old", "old")
	// The image's /app leads to its /etc, and /up tries to climb out.
	if err := os.Symlink("/etc", filepath.Join(root, "app")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../..", filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, context, "tool", "#!/bin/sh\n", "dir/inner", "inner")
	if err := os.Chmod(filepath.Join(context, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := openContext(context)
	if err != nil {
		t.Fatal(err)
	}
	source := func(rel string) copySource {
		found, err := src.source(rel)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	w := &rootWriter{root: root, uids: uids, gids: gids}
	for _, step := range []struct {
		source copySource
		dest   string
	}{
		{source("tool"), "/app/tool"},
		{source("tool"), "/up/escaped"},
		{source("tool"), "/usr/old"},
		{source("dir"), "/new/dir"},
	} {
		if err := w.copySource(step.source, step.dest); err != nil {
			t.Fatalf("copy to %s: %v", step.dest, err)
		}
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"etc/tool": "#!/bin/sh\n", "escaped": "#!/bin/sh\n", "usr/old": "#!/bin/sh\n", "new/dir/inner": "inner"} {
		if data, err := os.ReadFile(filepath.Join(root, path)); err != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", path, data, err, want)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "etc/tool")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("etc/tool's mode = %v, %v; want 0755", info, err)
	}

	// An archive, for ADD, with an entry trying to climb out of dest.
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, name := range []string{"pkg/readme", "../../outside"} {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: 2})
		tw.Write([]byte("hi"))
	}
	tw.Close()
	archivePath := filepath.Join(t.TempDir(), "files.tar")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if kind, err := archiveKind(archivePath); err != nil || kind != "tar" {
		t.Errorf("archiveKind = %q, %v", kind, err)
	}
	if kind, err := archiveKind(filepath.Join(context, "tool")); err != nil || kind != "" {
		t.Errorf("archiveKind of a script = %q, %v", kind, err)
	}
	if err := w.extractArchive(archivePath, "/opt"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"opt/pkg/readme", "opt/outside"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}

	// --chmod and --chown, with names from the image's own files.
	writeFiles(t, root, "etc/passwd", "root:x:0:0::/root:/bin/sh\n", "etc/group", "root:x:0:\n")
	if owner, err := parseChown("root:root", root); err != nil || owner.UID != 0 || owner.GID != 0 {
		t.Errorf("parseChown(root:root) = %+v, %v", owner, err)
	}
	if owner, err := parseChown("1000", root); err != nil || owner.UID != 1000 || owner.GID != 1000 {
		t.Errorf("parseChown(1000) = %+v, %v; want the group with the same ID", owner, err)
	}
	if _, err := parseChown("nobody", root); err == nil {
		t.Error("parseChown found a user the image doesn't have")
	}
	if mode, err := parseChmod("4755"); err != nil || mode != fs.ModeSetuid|0o755 {
		t.Errorf("parseChmod(4755) = %v, %v", mode, err)
	}
	if _, err := parseChmod("rwx"); err == nil {
		t.Error("parseChmod accepted rwx")
	}
}

// testStage is a stage whose image has env, for the configuration
// instructions.
func testStage(env ...string) *buildStage {
	st := &buildStage{image: &ociImage{}, args: map[string]string{}, desc: ociDescriptor{Digest: "sha256:0"}}
	st.image.config.Config.Env = env
	return st
}

// apply runs one instruction's change to the stage's configuration, and
// returns the result.
func apply(t *testing.T, b *builder, st *buildStage, text string) ociImageConfig {
	t.Helper()
	inst, err := parseInstruction(text, 1)
	if err != nil {
		t.Fatal(err)
	}
	change, err := b.configChange(st, inst)
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	if err := change(&st.image.config); err != nil {
		t.Fatal(err)
	}
	return st.image.config
}

// TestConfigChanges checks the instructions that only change the image's
// configuration.
func TestConfigChanges(t *testing.T) {
	b := &builder{file: &dockerfile{escape: '\\'}}
	st := testStage("A=old", "PATH=/bin")
	st.args["VERSION"] = "1.2"

	config := apply(t, b, st, `ENV A=new B=$A C="$VERSION and ${PATH}"`)
	if !slices.Equal(config.Config.Env, []string{"A=new", "PATH=/bin", "B=old", "C=1.2 and /bin"}) {
		t.Errorf("ENV gives %q", config.Config.Env)
	}
	apply(t, b, st, "WORKDIR /srv")
	if config := apply(t, b, st, "WORKDIR app/$VERSION"); config.Config.WorkingDir != "/srv/app/1.2" {
		t.Errorf("a relative WORKDIR gives %q", config.Config.WorkingDir)
	}
	if config := apply(t, b, st, `LABEL "org.example.name"="my app" version=$VERSION`); config.Config.Labels["org.example.name"] != "my app" || config.Config.Labels["version"] != "1.2" {
		t.Errorf("LABEL gives %v", config.Config.Labels)
	}
	if config := apply(t, b, st, "EXPOSE 80 53/udp 8000-8002/TCP"); len(config.Config.ExposedPorts) != 5 {
		t.Errorf("EXPOSE gives %v", config.Config.ExposedPorts)
	}
	apply(t, b, st, `SHELL ["/bin/bash", "-c"]`)
	if config := apply(t, b, st, "CMD echo $HOME"); !slices.Equal(config.Config.Cmd, []string{"/bin/bash", "-c", "echo $HOME"}) {
		t.Errorf("a shell-form CMD gives %q", config.Config.Cmd)
	}
	// The stage set its own CMD, so ENTRYPOINT keeps it.
	if config := apply(t, b, st, `ENTRYPOINT ["/entry"]`); !slices.Equal(config.Config.Entrypoint, []string{"/entry"}) || len(config.Config.Cmd) != 3 {
		t.Errorf("ENTRYPOINT gives %q and CMD %q", config.Config.Entrypoint, config.Config.Cmd)
	}
	// A CMD from the base image is cleared by ENTRYPOINT.
	inherited := testStage()
	inherited.image.config.Config.Cmd = []string{"base"}
	if config := apply(t, b, inherited, `ENTRYPOINT ["/entry"]`); config.Config.Cmd != nil {
		t.Errorf("ENTRYPOINT kept the base image's CMD %q", config.Config.Cmd)
	}
	if config := apply(t, b, st, "HEALTHCHECK --interval=5s --retries=3 CMD curl -f localhost"); config.Config.Healthcheck == nil ||
		config.Config.Healthcheck.Interval != 5*time.Second || config.Config.Healthcheck.Retries != 3 ||
		!slices.Equal(config.Config.Healthcheck.Test, []string{"CMD-SHELL", "curl -f localhost"}) {
		t.Errorf("HEALTHCHECK gives %+v", config.Config.Healthcheck)
	}
	if config := apply(t, b, st, "STOPSIGNAL SIGQUIT"); config.Config.StopSignal != "SIGQUIT" {
		t.Errorf("STOPSIGNAL gives %q", config.Config.StopSignal)
	}

	for _, text := range []string{"EXPOSE 0", "EXPOSE 80/icmp", "EXPOSE 90-80", "STOPSIGNAL SIGNOPE", "SHELL /bin/bash", "HEALTHCHECK curl", "HEALTHCHECK --interval=fast CMD true", "ENV A"} {
		inst, err := parseInstruction(text, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.configChange(st, inst); err == nil {
			t.Errorf("%s was accepted", text)
		}
	}
}

// TestStepKey checks that a step's cache key depends on what it should, and
// nothing else.
func TestStepKey(t *testing.T) {
	b := &builder{file: &dockerfile{escape: '\\'}}
	run, _ := parseInstruction("RUN make", 1)
	other, _ := parseInstruction("RUN make install", 1)
	st := testStage()
	st.key = digestOf("FROM base")
	key := b.stepKey(st, run, "")
	if b.stepKey(st, run, "") != key {
		t.Error("the same step has two keys")
	}
	st.args["B"], st.args["A"] = "2", "1"
	withArgs := b.stepKey(st, run, "")
	for name, changed := range map[string]string{
		"another instruction": b.stepKey(st, other, ""),
		"another file digest": b.stepKey(st, run, "files"),
		"no ARG values":       key,
	} {
		if changed == withArgs {
			t.Errorf("%s gives the same key", name)
		}
	}
	st.args["A"] = "changed"
	if b.stepKey(st, run, "") == withArgs {
		t.Error("another ARG value gives the same key")
	}
	st.key = digestOf("FROM other")
	if b.stepKey(st, run, "") == withArgs {
		t.Error("another base image gives the same key")
	}
}
