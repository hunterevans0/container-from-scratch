//go:build linux

package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runt build: building an image from a Dockerfile (dockerfile.go), one
// instruction at a time, as docker build's classic builder does.
//
// The image being built starts as the one FROM names, and each instruction
// makes a new one from it, which goes into the image store:
//
//   - RUN runs its command in a temporary container made from the image,
//     with runt's own supervisor, as runt run would, and the container's
//     writable layer, everything the command changed, becomes a new layer
//     on top (see buildlayer_linux.go).
//   - COPY and ADD mount the image's filesystem on the host, with a
//     writable layer on top as a container's would have, copy files into
//     it, and make that layer the new one in the same way (see
//     buildcopy_linux.go).
//   - The others (ENV, CMD, WORKDIR, and so on) only change the image's
//     configuration, and add no layer.
//
// Every step's image is recorded in the build cache under a key: a digest
// of the key of the step before, the instruction, the ARG values in scope,
// and, for COPY and ADD, the files copied. The first step's key comes from
// the base image's digest. So a key stands for everything that went into
// making the image, and when a later build reaches a step with a key the
// cache has, it takes the image from there instead of running the step
// again ("Using cache"). Once one step misses, so does every step after it,
// since its key goes into theirs.
//
// A Dockerfile can have several stages, each starting with a FROM; the
// image is the last one's (or --target's), and the others are there for
// COPY --from to take files from, or for FROM to start from.
//
// For how to read the Go here, see doc.go.

// buildComment marks the history entries runt build adds to an image.
const buildComment = "runt build"

// Build builds an image from a Dockerfile, as docker build does, writing
// its progress to out and RUN's output to runt's own, and returns the
// image's ID.
func Build(options BuildOptions, out io.Writer) (string, error) {
	b, err := newBuilder(options, out)
	if err != nil {
		return "", err
	}
	defer b.close()
	return b.build()
}

// builder is one build in progress.
type builder struct {
	options BuildOptions
	out     io.Writer
	store   *imageStore
	file    *dockerfile
	context *copyRoot
	tags    []imageReference
	// target is the number of the stage to stop at.
	target     int
	uids, gids IDRange
	// dir is this build's own directory, for the layers it makes before
	// they go into the store, and mount points for COPY --from. unlockDir
	// lets go of its lock, which tells other builds it is in use.
	dir       string
	unlockDir func()
	// globals are the ARG values FROM lines can use: those declared before
	// the first FROM, and the platform ones BuildKit predefines.
	globals map[string]string
	// usedArgs are the names of every ARG, to warn about --build-arg
	// values nothing uses.
	usedArgs map[string]bool
	// stages are the stages built so far, by number.
	stages []*buildStage
	// step is the number of the step being run, and steps how many there
	// are.
	step, steps int
}

// buildStage is a stage being built: its image so far, and what the
// instructions after it need to know that the image doesn't record.
type buildStage struct {
	name string
	// image is the image so far, and desc its manifest's descriptor. Its
	// blobs are in the store, unless desc.Digest is empty: then it is FROM
	// scratch, with no step done yet.
	image *ociImage
	desc  ociDescriptor
	// key is the cache key of the last step (see stepKey).
	key string
	// args are the ARG values in scope.
	args map[string]string
	// cmdSet is set once the stage has a CMD of its own, which a later
	// ENTRYPOINT doesn't clear, as it does an inherited one.
	cmdSet bool
}

// newBuilder reads the Dockerfile and the context, and makes the build's
// directory.
func newBuilder(options BuildOptions, out io.Writer) (*builder, error) {
	b := &builder{options: options, out: out, store: defaultImageStore(), usedArgs: map[string]bool{}}
	for _, tag := range options.Tags {
		ref, err := parseReference(tag)
		if err != nil {
			return nil, err
		}
		if ref.Digest != "" {
			return nil, fmt.Errorf("-t %s: a name to tag with can't have a digest", tag)
		}
		b.tags = append(b.tags, ref)
	}
	var err error
	if options.Context, err = filepath.Abs(options.Context); err != nil {
		return nil, err
	}
	if b.context, err = openContext(options.Context); err != nil {
		return nil, err
	}
	var dockerfile io.Reader = os.Stdin
	if options.Dockerfile != "-" {
		name := options.Dockerfile
		if name == "" {
			name = filepath.Join(options.Context, "Dockerfile")
		}
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		dockerfile = file
	}
	if b.file, err = parseDockerfile(dockerfile); err != nil {
		return nil, err
	}

	b.target = len(b.file.stages) - 1
	if options.Target != "" {
		b.target = slices.IndexFunc(b.file.stages, func(stage *dockerStage) bool { return stage.name == strings.ToLower(options.Target) })
		if b.target < 0 {
			return nil, fmt.Errorf("--target %s: the Dockerfile has no stage called that", options.Target)
		}
	}
	for _, stage := range b.file.stages[:b.target+1] {
		b.steps += 1 + len(stage.instructions)
	}
	if err := b.readGlobalArgs(); err != nil {
		return nil, err
	}
	if b.uids, b.gids, err = idRanges(Config{UsernsUser: options.UsernsUser}); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(b.store.builds, 0o700); err != nil {
		return nil, err
	}
	removeStaleBuilds(b.store.builds)
	if b.dir, err = os.MkdirTemp(b.store.builds, "build-"); err != nil {
		return nil, err
	}
	if b.unlockDir, err = lockFile(filepath.Join(b.dir, "lock"), true); err != nil {
		os.RemoveAll(b.dir)
		return nil, err
	}
	return b, nil
}

// close removes the build's directory.
func (b *builder) close() {
	b.unlockDir()
	if err := os.RemoveAll(b.dir); err != nil {
		warn(err)
	}
}

// removeStaleBuilds removes what builds that never finished left in dir:
// their directories, whose lock no build holds any more. One made in the
// last minute may not have taken its lock yet, so it is left alone.
func removeStaleBuilds(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		build := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < time.Minute {
			continue
		}
		lock, err := os.Open(filepath.Join(build, "lock"))
		if err == nil {
			// LOCK_NB: fail at once, rather than wait, if a build holds it.
			err = flock(lock, syscall.LOCK_EX|syscall.LOCK_NB)
			lock.Close()
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		// COPY --from's mounts, if it died with them mounted.
		mounts, _ := filepath.Glob(filepath.Join(build, "from-*"))
		for _, mount := range mounts {
			syscall.Unmount(mount, syscall.MNT_DETACH)
		}
		os.RemoveAll(build)
	}
}

// readGlobalArgs works out the values of the ARGs before the first FROM,
// starting from the platform ones that BuildKit predefines, such as
// TARGETARCH, for Dockerfiles that build for several platforms.
func (b *builder) readGlobalArgs() error {
	arch := goruntime.GOARCH
	b.globals = map[string]string{
		"TARGETPLATFORM": "linux/" + arch, "TARGETOS": "linux", "TARGETARCH": arch, "TARGETVARIANT": "",
		"BUILDPLATFORM": "linux/" + arch, "BUILDOS": "linux", "BUILDARCH": arch, "BUILDVARIANT": "",
	}
	lexer := wordLexer{escape: b.file.escape, lookup: func(name string) (string, bool) {
		value, ok := b.globals[name]
		return value, ok
	}}
	for _, inst := range b.file.args {
		pairs, err := lexer.keyValues(inst.args, false, true)
		if err != nil {
			return fmt.Errorf("line %d: %w", inst.line, err)
		}
		for _, pair := range pairs {
			b.usedArgs[pair.key] = true
			if value, given := b.options.BuildArgs[pair.key]; given {
				b.globals[pair.key] = value
			} else if pair.hasValue {
				b.globals[pair.key] = pair.value
			}
		}
	}
	return nil
}

// build runs every step, then names the image.
func (b *builder) build() (string, error) {
	for i, stage := range b.file.stages[:b.target+1] {
		st, err := b.from(stage)
		if err != nil {
			return "", fmt.Errorf("line %d: %w", stage.from.line, err)
		}
		for _, inst := range stage.instructions {
			if err := b.runStep(st, inst); err != nil {
				return "", fmt.Errorf("line %d: %s: %w", inst.line, inst.command, err)
			}
		}
		if i == b.target && st.desc.Digest == "" {
			return "", errors.New("the image has nothing in it: FROM scratch needs at least one instruction after it")
		}
		b.stages = append(b.stages, st)
	}

	var unused []string
	for name := range b.options.BuildArgs {
		if !b.usedArgs[name] {
			unused = append(unused, name)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		fmt.Fprintf(b.out, "[Warning] One or more build-args %v were not consumed\n", unused)
	}

	final := b.stages[b.target]
	unlock, err := b.store.lock(true)
	if err != nil {
		return "", err
	}
	err = b.store.nameBuilt(final.desc, b.tags)
	unlock()
	if err != nil {
		return "", err
	}
	id := final.image.manifest.Config.Digest
	fmt.Fprintf(b.out, "Successfully built %s\n", shortDigest(id))
	for _, tag := range b.tags {
		fmt.Fprintf(b.out, "Successfully tagged %s\n", tag.familiar())
	}
	return id, nil
}

// printStep shows the step about to run, as docker build does.
func (b *builder) printStep(inst instruction) {
	b.step++
	fmt.Fprintf(b.out, "Step %d/%d : %s\n", b.step, b.steps, inst.original)
}

// printImage shows the image a step left the stage with.
func (b *builder) printImage(st *buildStage) {
	if st.desc.Digest != "" {
		fmt.Fprintf(b.out, " ---> %s\n", shortDigest(st.image.manifest.Config.Digest))
	}
}

// from starts a stage, from the image its FROM names: scratch, an earlier
// stage by name, or an image, pulled first if the store doesn't have it (or
// with --pull, in any case).
func (b *builder) from(stage *dockerStage) (*buildStage, error) {
	b.printStep(stage.from)
	lexer := wordLexer{escape: b.file.escape, lookup: func(name string) (string, bool) {
		value, ok := b.globals[name]
		return value, ok
	}}
	name, err := lexer.word(stage.image)
	if err != nil {
		return nil, err
	}
	if platform := stage.from.flags["platform"]; platform != "" {
		if platform, err = lexer.word(platform); err != nil {
			return nil, err
		}
		if want := "linux/" + goruntime.GOARCH; platform != want && !strings.HasPrefix(platform, want+"/") {
			return nil, fmt.Errorf("--platform=%s: runt build only builds for this machine, %s", platform, want)
		}
	}
	st := &buildStage{name: stage.name, args: map[string]string{}}

	if strings.EqualFold(name, "scratch") {
		st.image = &ociImage{layout: b.store.dir}
		st.image.config.Architecture, st.image.config.OS = goruntime.GOARCH, "linux"
		st.image.config.RootFS.Type, st.image.config.RootFS.DiffIDs = "layers", []string{}
		st.key = digestOf("FROM scratch")
		return st, nil
	}
	if i := slices.IndexFunc(b.stages, func(earlier *buildStage) bool { return earlier.name != "" && earlier.name == strings.ToLower(name) }); i >= 0 {
		earlier := b.stages[i]
		st.image, st.desc, st.key = earlier.image, earlier.desc, earlier.key
		b.printImage(st)
		return st, nil
	}
	found, err := b.baseImage(name)
	if err != nil {
		return nil, err
	}
	st.image, st.desc = found.image, found.entry
	st.desc.Annotations = nil
	st.key = digestOf("FROM " + found.entry.Digest)
	b.printImage(st)
	return st, nil
}

// baseImage finds the image name names in the store, pulling it if need be.
func (b *builder) baseImage(name string) (storedImage, error) {
	if isLayoutRef(name) {
		return storedImage{}, fmt.Errorf("%s: runt build takes images from a registry or runt's store, not image layout directories", name)
	}
	var err error
	if b.options.Pull {
		err = Pull(name, b.out)
	} else {
		err = EnsureImage(name, b.out)
	}
	if err != nil {
		return storedImage{}, err
	}
	unlock, err := b.store.lock(false)
	if err != nil {
		return storedImage{}, err
	}
	defer unlock()
	return b.store.find(name)
}

// runStep runs one instruction of a stage.
func (b *builder) runStep(st *buildStage, inst instruction) error {
	b.printStep(inst)
	switch inst.command {
	case "ARG":
		return b.arg(st, inst)
	case "RUN":
		return b.run(st, inst)
	case "COPY", "ADD":
		return b.copy(st, inst)
	}
	return b.configure(st, inst)
}

// stepKey is the cache key of inst, run on the stage as it is (see the top
// of the file). extra is anything else the step's result depends on.
func (b *builder) stepKey(st *buildStage, inst instruction, extra string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%c\n%s\n", st.key, b.file.escape, inst.original)
	names := make([]string, 0, len(st.args))
	for name := range st.args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(h, "arg %s=%s\n", name, st.args[name])
	}
	fmt.Fprintf(h, "%s\n", extra)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// digestOf is the digest of text, as a cache key.
func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// useCache takes the image the cache has for key, if it has one, as the
// stage's image, and reports whether it did.
func (b *builder) useCache(st *buildStage, key string) bool {
	if b.options.NoCache {
		return false
	}
	unlock, err := b.store.lock(false)
	if err != nil {
		return false
	}
	defer unlock()
	desc, image, ok := b.store.cached(key)
	if !ok {
		return false
	}
	st.image, st.desc, st.key = image, desc, key
	fmt.Fprintln(b.out, " ---> Using cache")
	b.printImage(st)
	return true
}

// commit adds what a step made to the store: the stage's image with layer
// on top, if the step made one, and with change made to its configuration,
// if it changes it. The new image is recorded in the cache under key, and
// becomes the stage's image.
func (b *builder) commit(st *buildStage, key string, inst instruction, layer *layerBlob, change func(config *ociImageConfig) error) error {
	config, err := cloneConfig(st.image.config)
	if err != nil {
		return err
	}
	if change != nil {
		if err := change(&config); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	config.Created = now
	config.RootFS.Type = "layers"
	manifest := ociManifest{SchemaVersion: 2, MediaType: mediaTypeImageManifest}
	// Docker's media types for the base image's layers become OCI's, which
	// mean the same, so the manifest is all OCI.
	for _, desc := range st.image.manifest.Layers {
		switch desc.MediaType {
		case "application/vnd.docker.image.rootfs.diff.tar.gzip":
			desc.MediaType = mediaTypeLayerGzip
		case "application/vnd.docker.image.rootfs.diff.tar":
			desc.MediaType = mediaTypeLayer
		}
		manifest.Layers = append(manifest.Layers, desc)
	}
	if layer != nil {
		manifest.Layers = append(manifest.Layers, layer.desc)
		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, layer.diffID)
	}
	if manifest.Layers == nil {
		manifest.Layers = []ociDescriptor{}
	}
	if config.RootFS.DiffIDs == nil {
		config.RootFS.DiffIDs = []string{}
	}
	config.History = append(config.History, ociHistory{Created: now, CreatedBy: inst.original, Comment: buildComment, EmptyLayer: layer == nil})
	configData, err := json.Marshal(config)
	if err != nil {
		return err
	}

	unlock, err := b.store.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	if layer != nil {
		if err := b.store.moveBlob(layer.path, layer.desc.Digest); err != nil {
			return err
		}
	}
	if manifest.Config, err = b.store.writeBlob(mediaTypeImageConfig, configData); err != nil {
		return err
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	desc, err := b.store.writeBlob(mediaTypeImageManifest, manifestData)
	if err != nil {
		return err
	}
	desc.Platform = &ociPlatform{OS: "linux", Architecture: config.Architecture}
	if err := b.store.addToCache(key, desc); err != nil {
		return err
	}
	st.image = &ociImage{layout: b.store.dir, manifest: manifest, config: config}
	st.desc, st.key = desc, key
	b.printImage(st)
	return nil
}

// cloneConfig returns a copy of config that shares nothing with it, by way
// of JSON, so that changing the copy's lists and maps leaves the original
// alone.
func cloneConfig(config ociImageConfig) (ociImageConfig, error) {
	var clone ociImageConfig
	data, err := json.Marshal(config)
	if err == nil {
		err = json.Unmarshal(data, &clone)
	}
	return clone, err
}

// lexer processes a stage's arguments, with ENV's variables and then ARG's,
// as the instruction about to run sees them.
func (b *builder) lexer(st *buildStage) wordLexer {
	return wordLexer{escape: b.file.escape, lookup: func(name string) (string, bool) {
		for _, variable := range st.image.config.Config.Env {
			if key, value, _ := strings.Cut(variable, "="); key == name {
				return value, true
			}
		}
		value, ok := st.args[name]
		return value, ok
	}}
}

// arg declares ARGs: their values, from --build-arg, or else the default
// given, or else the global ARG of the same name, are in scope for the rest
// of the stage.
func (b *builder) arg(st *buildStage, inst instruction) error {
	pairs, err := b.lexer(st).keyValues(inst.args, false, true)
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		b.usedArgs[pair.key] = true
		value, set := b.options.BuildArgs[pair.key]
		if !set && pair.hasValue {
			value, set = pair.value, true
		}
		if !set {
			value, set = b.globals[pair.key]
		}
		if set {
			st.args[pair.key] = value
		} else {
			delete(st.args, pair.key)
		}
	}
	st.key = b.stepKey(st, inst, "")
	b.printImage(st)
	return nil
}

// shell is the stage's shell, which SHELL sets, for the shell form of RUN,
// CMD, and ENTRYPOINT.
func (st *buildStage) shell() []string {
	if shell := st.image.config.Config.Shell; len(shell) > 0 {
		return shell
	}
	return []string{"/bin/sh", "-c"}
}

// command is an instruction's command: its exec form, or else the shell
// running the arguments.
func (st *buildStage) command(inst instruction) []string {
	if inst.isExec {
		return inst.exec
	}
	return append(slices.Clone(st.shell()), inst.args)
}

// run runs a RUN's command in a container made from the stage's image, and
// adds what it changed as a layer.
func (b *builder) run(st *buildStage, inst instruction) error {
	command := st.command(inst)
	if len(command) == 0 {
		return errors.New("no command to run")
	}
	key := b.stepKey(st, inst, "")
	if b.useCache(st, key) {
		return nil
	}
	if len(st.image.manifest.Layers) == 0 {
		return errors.New("the image has no files yet, so nothing to run; FROM scratch only has what COPY puts there")
	}
	// ARG values are in the command's environment, though not the
	// image's, unless ENV has set the same name.
	var env []string
	for name, value := range st.args {
		if !hasEnv(st.image.config.Config.Env, name) {
			env = append(env, name+"="+value)
		}
	}
	sort.Strings(env)
	// The command runs as it is, with no entrypoint, in the image's
	// working directory, as its user, and with its environment, as
	// applyImageConfig sets them up.
	noEntrypoint := ""
	config := Config{
		Image:      shortDigest(st.image.manifest.Config.Digest),
		image:      st.image,
		Entrypoint: &noEntrypoint,
		Command:    command,
		SetEnv:     env,
		Network:    b.options.Network,
		UIDs:       b.uids,
		GIDs:       b.gids,
	}
	// supervise reports the container's ID once the command is running, or
	// it has failed to run; it is kept to remove the container afterwards.
	id := ""
	err := supervise(config, true, true, func(created string, _ error) {
		if created != "" {
			id = created
			fmt.Fprintf(b.out, " ---> Running in %s\n", created)
		}
	})
	// The container goes once its writable layer has been made into a
	// layer, or straight away if the command failed. sync.OnceFunc makes a
	// function that does its work only the first time it is called.
	removeContainer := sync.OnceFunc(func() {
		if id == "" {
			return
		}
		if err := Remove(id, true); err != nil {
			warn(err)
		}
		fmt.Fprintf(b.out, "Removing intermediate container %s\n", id)
	})
	defer removeContainer()
	if err != nil {
		// %v, not %w: the command's exit code is runt build's failure, not
		// runt's own exit code to pass on.
		if code, ok := ExitCode(err); ok {
			return fmt.Errorf("the command %q returned a non-zero code: %d", strings.Join(command, " "), code)
		}
		return err
	}
	// init mounts /etc/hosts and /etc/resolv.conf over the container's own,
	// and makes an empty file to mount over if the image has none. The
	// command can't change them, so any in the writable layer are those
	// empty files, which don't belong in the image.
	skip := func(rel string, info fs.FileInfo) bool {
		return (rel == "etc/hosts" || rel == "etc/resolv.conf") && info.Mode().IsRegular()
	}
	layer, err := exportLayer(filepath.Join(containerFSDir(id), "upper"), b.uids, b.gids, skip, b.dir)
	if err != nil {
		return err
	}
	removeContainer()
	return b.commit(st, key, inst, layer, nil)
}

// copy runs a COPY or ADD: it finds the files, looks the step up in the
// cache, and if it isn't there, copies them into the stage's image and
// adds them as a layer.
//
// It holds the image store's lock shared from start to end of the copying,
// since nothing records that its layers are in use, as a container's state
// does; and so it resolves --from's image first, since pulling one takes
// the lock exclusively.
func (b *builder) copy(st *buildStage, inst instruction) error {
	add := inst.command == "ADD"
	lexer := b.lexer(st)
	var args []string
	if inst.isExec {
		for _, arg := range inst.exec {
			word, err := lexer.word(arg)
			if err != nil {
				return err
			}
			args = append(args, word)
		}
	} else {
		var err error
		if args, err = lexer.words(inst.args); err != nil {
			return err
		}
	}
	if len(args) < 2 {
		return errors.New("needs one or more sources and a destination")
	}
	flags := map[string]string{}
	for name, value := range inst.flags {
		word, err := lexer.word(value)
		if err != nil {
			return err
		}
		flags[name] = word
	}
	sources, dest := args[:len(args)-1], args[len(args)-1]
	// A destination ending in / is a directory, which the sources go
	// into; a relative one is relative to WORKDIR.
	destDir := strings.HasSuffix(dest, "/") || dest == "." || strings.HasSuffix(dest, "/.")
	if !path.IsAbs(dest) {
		dest = path.Join("/", st.image.config.Config.WorkingDir, dest)
	}
	dest = path.Clean(dest)
	if flags["checksum"] != "" && (len(sources) != 1 || !isURL(sources[0])) {
		return errors.New("--checksum only works with a single URL")
	}

	var fromImage *ociImage
	if from := flags["from"]; from != "" {
		var err error
		if fromImage, err = b.fromImage(from); err != nil {
			return err
		}
	}
	unlock, err := b.store.lock(false)
	if err != nil {
		return err
	}
	defer unlock()
	root := b.context
	if fromImage != nil {
		layers, err := b.unpack(fromImage)
		if err != nil {
			return err
		}
		dir, unmount, err := mountReadOnly(layers, filepath.Join(b.dir, fmt.Sprintf("from-%d", b.step)))
		if err != nil {
			return err
		}
		defer unmount()
		root = &copyRoot{dir: dir}
	}
	var found []copySource
	for _, source := range sources {
		if isURL(source) {
			if !add {
				return fmt.Errorf("%s: COPY can't download; use ADD, or RUN curl", source)
			}
			downloaded, err := download(source, flags["checksum"], b.dir)
			if err != nil {
				return err
			}
			defer os.Remove(downloaded.host)
			found = append(found, downloaded)
			continue
		}
		matches, err := root.findSources(source)
		if err != nil {
			return err
		}
		found = append(found, matches...)
	}
	if len(found) > 1 && !destDir {
		return fmt.Errorf("with more than one source, the destination must be a directory, ending in / (%s/)", dest)
	}
	contents := sha256.New()
	for _, source := range found {
		if err := source.hash(contents); err != nil {
			return err
		}
	}
	key := b.stepKey(st, inst, hex.EncodeToString(contents.Sum(nil)))
	if b.useCache(st, key) {
		return nil
	}

	// The image's filesystem, with a writable layer for the copies, as a
	// container's would be. Its directory goes with a state directory, as
	// a container's does, so that runt run doesn't take it for one left
	// behind (see pruneContainerFS).
	layers, err := b.unpack(st.image)
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	if err := createStateDir(StateRoot, id); err != nil {
		return err
	}
	defer os.RemoveAll(stateDir(StateRoot, id))
	merged, err := mountContainerFS(id, layers, b.uids, b.gids)
	if err != nil {
		return err
	}
	defer func() {
		if err := removeContainerFS(id); err != nil {
			warn(err)
		}
	}()
	if err := b.copyFiles(merged, found, dest, destDir, add, flags); err != nil {
		return err
	}
	if err := unmountContainerFS(id); err != nil {
		return err
	}
	layer, err := exportLayer(filepath.Join(containerFSDir(id), "upper"), b.uids, b.gids, nil, b.dir)
	if err != nil {
		return err
	}
	unlock()
	return b.commit(st, key, inst, layer, nil)
}

// copyFiles writes the sources into the image's filesystem at merged, at
// dest, as COPY or ADD does:
//
//   - a directory's contents go into dest;
//   - a file goes into dest if dest is a directory (or ends in /), or else
//     becomes dest;
//   - for ADD, a local archive is unpacked into dest, and a download is a
//     file named for its URL.
func (b *builder) copyFiles(merged string, sources []copySource, dest string, destDir, add bool, flags map[string]string) error {
	writer := &rootWriter{root: merged, uids: b.uids, gids: b.gids}
	if spec := flags["chown"]; spec != "" {
		owner, err := parseChown(spec, merged)
		if err != nil {
			return err
		}
		writer.owner, writer.dirOwner = &owner, owner
	}
	if spec := flags["chmod"]; spec != "" {
		mode, err := parseChmod(spec)
		if err != nil {
			return err
		}
		writer.mode = &mode
	}
	// intoDest reports whether a file goes into dest, rather than being
	// written as dest.
	intoDest := func() bool {
		if destDir {
			return true
		}
		host, err := resolveInRoot(merged, dest)
		if err != nil {
			return false
		}
		info, err := os.Stat(host)
		return err == nil && info.IsDir()
	}
	for _, source := range sources {
		switch {
		case source.url != "":
			target := dest
			if intoDest() {
				target = path.Join(dest, urlBase(source.url))
			}
			if err := writer.copySource(source, target); err != nil {
				return err
			}
		case source.info.IsDir():
			if err := writer.copySource(source, dest); err != nil {
				return err
			}
		default:
			if add {
				kind, err := archiveKind(source.host)
				if err != nil {
					return err
				}
				if kind != "" {
					if err := writer.extractArchive(source.host, dest); err != nil {
						return err
					}
					continue
				}
			}
			target := dest
			if intoDest() {
				target = path.Join(dest, path.Base(source.rel))
			}
			if err := writer.copySource(source, target); err != nil {
				return err
			}
		}
	}
	return writer.finish()
}

// fromImage finds the image COPY --from names: an earlier stage, by name or
// number, or else an image, pulled first if the store doesn't have it.
func (b *builder) fromImage(from string) (*ociImage, error) {
	if n, err := strconv.Atoi(from); err == nil {
		if n < 0 || n >= len(b.stages) {
			return nil, fmt.Errorf("--from=%d: there is no stage %d before this one", n, n)
		}
		return b.stages[n].image, nil
	}
	name := strings.ToLower(from)
	for _, stage := range b.stages {
		if stage.name == name {
			return stage.image, nil
		}
	}
	if slices.ContainsFunc(b.file.stages, func(stage *dockerStage) bool { return stage.name == name }) {
		return nil, fmt.Errorf("--from=%s: a stage can only copy from stages before it", from)
	}
	found, err := b.baseImage(from)
	if err != nil {
		return nil, err
	}
	return found.image, nil
}

// unpack unpacks an image's layers for a mount, as a container's would be.
// An image with none yet, FROM scratch, gets an empty directory, since
// overlayfs needs at least one.
func (b *builder) unpack(image *ociImage) ([]string, error) {
	if len(image.manifest.Layers) > 0 {
		return unpackImage(image, b.uids, b.gids)
	}
	empty := filepath.Join(b.dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		return nil, err
	}
	return []string{empty}, nil
}

// configure runs an instruction that only changes the image's
// configuration.
func (b *builder) configure(st *buildStage, inst instruction) error {
	change, err := b.configChange(st, inst)
	if err != nil {
		return err
	}
	key := b.stepKey(st, inst, "")
	if b.useCache(st, key) {
		return nil
	}
	return b.commit(st, key, inst, nil, change)
}

// configChange works out what an instruction changes in the image's
// configuration, as a function that makes the change. Its arguments are
// processed now, with the variables as they are before it.
func (b *builder) configChange(st *buildStage, inst instruction) (func(config *ociImageConfig) error, error) {
	lexer := b.lexer(st)
	switch inst.command {
	case "ENV", "LABEL":
		pairs, err := lexer.keyValues(inst.args, true, false)
		if err != nil {
			return nil, err
		}
		return func(config *ociImageConfig) error {
			for _, pair := range pairs {
				if inst.command == "ENV" {
					config.Config.Env = mergeEnv(config.Config.Env, []string{pair.key + "=" + pair.value})
					continue
				}
				if config.Config.Labels == nil {
					config.Config.Labels = map[string]string{}
				}
				config.Config.Labels[pair.key] = pair.value
			}
			return nil
		}, nil
	case "MAINTAINER":
		return func(config *ociImageConfig) error {
			config.Author = inst.args
			return nil
		}, nil
	case "CMD":
		cmd := st.command(inst)
		st.cmdSet = true
		return func(config *ociImageConfig) error {
			config.Config.Cmd = cmd
			return nil
		}, nil
	case "ENTRYPOINT":
		entrypoint := st.command(inst)
		// An ENTRYPOINT clears the CMD the base image had, which was meant
		// for its own entrypoint, but not one this stage set.
		clearCmd := !st.cmdSet
		return func(config *ociImageConfig) error {
			config.Config.Entrypoint = entrypoint
			if clearCmd {
				config.Config.Cmd = nil
			}
			return nil
		}, nil
	case "SHELL":
		if !inst.isExec || len(inst.exec) == 0 {
			return nil, errors.New(`SHELL takes a JSON array, such as ["/bin/bash", "-c"]`)
		}
		return func(config *ociImageConfig) error {
			config.Config.Shell = inst.exec
			return nil
		}, nil
	case "WORKDIR":
		dir, err := lexer.word(inst.args)
		if err != nil {
			return nil, err
		}
		return func(config *ociImageConfig) error {
			// A relative directory is relative to the one before.
			config.Config.WorkingDir = path.Join("/", config.Config.WorkingDir, dir)
			if path.IsAbs(dir) {
				config.Config.WorkingDir = path.Clean(dir)
			}
			return nil
		}, nil
	case "USER":
		user, err := lexer.word(inst.args)
		if err != nil {
			return nil, err
		}
		return func(config *ociImageConfig) error {
			config.Config.User = user
			return nil
		}, nil
	case "STOPSIGNAL":
		signal, err := lexer.word(inst.args)
		if err != nil {
			return nil, err
		}
		if _, err := ParseSignal(signal); err != nil {
			return nil, err
		}
		return func(config *ociImageConfig) error {
			config.Config.StopSignal = signal
			return nil
		}, nil
	case "EXPOSE":
		words, err := lexer.words(inst.args)
		if err != nil {
			return nil, err
		}
		ports, err := exposedPorts(words)
		if err != nil {
			return nil, err
		}
		return func(config *ociImageConfig) error {
			if config.Config.ExposedPorts == nil {
				config.Config.ExposedPorts = map[string]struct{}{}
			}
			for _, port := range ports {
				config.Config.ExposedPorts[port] = struct{}{}
			}
			return nil
		}, nil
	case "VOLUME":
		volumes := inst.exec
		if !inst.isExec {
			volumes = strings.Fields(inst.args)
		}
		var processed []string
		for _, volume := range volumes {
			word, err := lexer.word(volume)
			if err != nil {
				return nil, err
			}
			if word == "" {
				return nil, errors.New("a volume's path can't be empty")
			}
			processed = append(processed, word)
		}
		return func(config *ociImageConfig) error {
			if config.Config.Volumes == nil {
				config.Config.Volumes = map[string]struct{}{}
			}
			for _, volume := range processed {
				config.Config.Volumes[volume] = struct{}{}
			}
			return nil
		}, nil
	case "HEALTHCHECK":
		check, err := parseHealthcheck(inst)
		if err != nil {
			return nil, err
		}
		return func(config *ociImageConfig) error {
			config.Config.Healthcheck = check
			return nil
		}, nil
	}
	return nil, fmt.Errorf("unknown instruction %s", inst.command)
}

// exposedPorts reads EXPOSE's ports: PORT, PORT/PROTOCOL, or a range,
// START-END[/PROTOCOL]. The protocol is tcp unless given.
func exposedPorts(words []string) ([]string, error) {
	var ports []string
	for _, word := range words {
		number, protocol, _ := strings.Cut(word, "/")
		protocol = strings.ToLower(protocol)
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" && protocol != "udp" && protocol != "sctp" {
			return nil, fmt.Errorf("EXPOSE %s: the protocol must be tcp, udp, or sctp", word)
		}
		first, last, isRange := strings.Cut(number, "-")
		if !isRange {
			last = first
		}
		start, err1 := strconv.ParseUint(first, 10, 16)
		end, err2 := strconv.ParseUint(last, 10, 16)
		if err1 != nil || err2 != nil || start == 0 || end < start {
			return nil, fmt.Errorf("EXPOSE %s: not a port or range of ports", word)
		}
		for port := start; port <= end; port++ {
			ports = append(ports, fmt.Sprintf("%d/%s", port, protocol))
		}
	}
	return ports, nil
}

// parseHealthcheck reads HEALTHCHECK [OPTIONS] CMD COMMAND, or HEALTHCHECK
// NONE, which turns off the base image's. runt doesn't run health checks
// itself, but keeps them in the image, as Docker does, for whatever runs
// it.
func parseHealthcheck(inst instruction) (*ociHealthcheck, error) {
	kind, rest := cutSpace(inst.args)
	if strings.EqualFold(kind, "NONE") {
		if rest != "" || len(inst.flags) > 0 {
			return nil, errors.New("HEALTHCHECK NONE takes nothing else")
		}
		return &ociHealthcheck{Test: []string{"NONE"}}, nil
	}
	if !strings.EqualFold(kind, "CMD") || rest == "" {
		return nil, errors.New("HEALTHCHECK takes CMD and a command, or NONE")
	}
	check := &ociHealthcheck{Test: []string{"CMD-SHELL", rest}}
	var list []string
	if strings.HasPrefix(rest, "[") && json.Unmarshal([]byte(rest), &list) == nil {
		check.Test = append([]string{"CMD"}, list...)
	}
	durations := map[string]*time.Duration{
		"interval": &check.Interval, "timeout": &check.Timeout,
		"start-period": &check.StartPeriod, "start-interval": &check.StartInterval,
	}
	for name, field := range durations {
		if text, ok := inst.flags[name]; ok {
			duration, err := time.ParseDuration(text)
			if err != nil || (duration != 0 && duration < time.Millisecond) {
				return nil, fmt.Errorf("--%s=%s: give a duration of at least 1ms, such as 30s", name, text)
			}
			*field = duration
		}
	}
	if text, ok := inst.flags["retries"]; ok {
		retries, err := strconv.Atoi(text)
		if err != nil || retries < 0 {
			return nil, fmt.Errorf("--retries=%s: give a number", text)
		}
		check.Retries = retries
	}
	return check, nil
}
