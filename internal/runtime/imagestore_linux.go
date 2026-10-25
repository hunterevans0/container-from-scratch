//go:build linux

package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// runt's image store is an OCI image layout itself, at imageStoreDir: pulled
// images' manifests, configurations, and layers go in its blobs/sha256/, and
// its index.json lists each image under its full name, such as
// docker.io/library/alpine:3.20, in the io.containerd.image.name annotation,
// as docker save writes it. Only the manifest for this machine's platform is
// kept. An image's ID is the digest of its configuration, as in Docker.
//
// Several runt processes use the store at once, so a lock file guards it:
//   - pulls download into a directory of their own under tmp/, holding the
//     lock shared, then take it exclusively to move the blobs into the
//     store and update index.json;
//   - starting a container from an image holds it shared from finding the
//     image until the container's state, which lists the layers it uses, is
//     saved;
//   - runt rmi holds it exclusively while it untags images and removes
//     blobs and unpacked layers that nothing uses any more.
//
// For how to read the Go here, see doc.go.

// errNoSuchImage means the store has no image by that name or ID. It is a
// single error value, made once, so callers can recognize it with
// errors.Is, even after it has been wrapped with more detail.
var errNoSuchImage = errors.New("no such image")

// imageStore is where the store's files are. It is a struct, rather than
// constants, so the tests can make a store in a temporary directory.
type imageStore struct {
	// dir is the OCI image layout.
	dir string
	// tmp holds pulls in progress. It is on the same filesystem as dir, so
	// blobs can be moved in by renaming.
	tmp string
	// lockPath is the lock file.
	lockPath string
	// layers holds unpacked layers (layerRoot).
	layers string
}

// defaultImageStore is the store under /var/lib/runt that runt uses.
func defaultImageStore() *imageStore {
	return &imageStore{
		dir:      DataRoot + "/images",
		tmp:      DataRoot + "/tmp",
		lockPath: DataRoot + "/images.lock",
		layers:   layerRoot,
	}
}

// lock takes the store's lock, shared or exclusive, waiting for it if need
// be. It returns a function that releases it, which may be called more than
// once.
func (s *imageStore) lock(exclusive bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o711); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(s.lockPath), err)
	}
	unlock, err := lockFile(s.lockPath, exclusive)
	if err != nil {
		return nil, fmt.Errorf("lock image store: %w", err)
	}
	return unlock, nil
}

// blobPath is where the blob with digest is, or would be, in the store. It
// borrows ociImage's method by making an ociImage for the store's layout on
// the spot.
func (s *imageStore) blobPath(digest string) (string, error) {
	return (&ociImage{layout: s.dir}).blobPath(digest)
}

// hasBlob reports whether the store has the blob with digest.
func (s *imageStore) hasBlob(digest string) bool {
	path, err := s.blobPath(digest)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// readIndex reads the store's index.json. A store that doesn't exist yet is
// empty.
func (s *imageStore) readIndex() (*ociIndex, error) {
	if _, err := os.Stat(filepath.Join(s.dir, "index.json")); errors.Is(err, os.ErrNotExist) {
		return &ociIndex{SchemaVersion: 2, MediaType: mediaTypeImageIndex}, nil
	}
	return readLayoutIndex(s.dir)
}

// writeIndex replaces index.json, creating the layout if it doesn't exist.
// The caller holds the lock exclusively.
func (s *imageStore) writeIndex(index *ociIndex) error {
	if err := os.MkdirAll(filepath.Join(s.dir, "blobs", "sha256"), 0o700); err != nil {
		return fmt.Errorf("create image store: %w", err)
	}
	marker := filepath.Join(s.dir, "oci-layout")
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(marker, []byte(`{"imageLayoutVersion":"1.0.0"}`+"\n"), 0o600); err != nil {
			return fmt.Errorf("create image store: %w", err)
		}
	}
	index.SchemaVersion = 2
	index.MediaType = mediaTypeImageIndex
	if index.Manifests == nil {
		index.Manifests = []ociDescriptor{}
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	temp := filepath.Join(s.dir, "index.json.tmp")
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write image store index: %w", err)
	}
	if err := os.Rename(temp, filepath.Join(s.dir, "index.json")); err != nil {
		return fmt.Errorf("write image store index: %w", err)
	}
	return nil
}

// entryName is the full name an index.json entry is stored under.
func entryName(desc ociDescriptor) string {
	if name := desc.Annotations[annotationImageName]; name != "" {
		return name
	}
	return desc.Annotations[annotationRefName]
}

// storedImage is an image in the store.
type storedImage struct {
	// entry is its index.json entry, and name the full name it is
	// stored under.
	entry ociDescriptor
	name  string
	image *ociImage
}

// id is the image's ID: the digest of its configuration. The method's
// receiver is a storedImage, not a pointer to one, so it gets a copy, which
// is fine for reading.
func (image storedImage) id() string {
	return image.image.manifest.Config.Digest
}

// list returns every image in the store. Entries that can't be read, such as
// one whose blobs are missing, are left out.
func (s *imageStore) list() ([]storedImage, error) {
	index, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	var images []storedImage
	for _, entry := range index.Manifests {
		image, err := loadImage(s.dir, entry)
		if err != nil {
			continue
		}
		images = append(images, storedImage{entry: entry, name: entryName(entry), image: image})
	}
	return images, nil
}

// imageIDPrefix matches an image ID or the start of one: an optional
// sha256:, then 1 to 64 hex digits.
var imageIDPrefix = regexp.MustCompile(`^(?:sha256:)?[0-9a-f]{1,64}$`)

// find looks an image up by name, such as alpine:3.20, or by its ID or the
// start of it, as Docker does.
func (s *imageStore) find(text string) (storedImage, error) {
	images, err := s.list()
	if err != nil {
		return storedImage{}, err
	}
	matches := s.match(images, text)
	if len(matches) == 0 {
		return storedImage{}, fmt.Errorf("%w: %s", errNoSuchImage, text)
	}
	// Several matches are fine if they are all the same image, under
	// different names. matches[1:] is every match but the first.
	for _, match := range matches[1:] {
		if match.id() != matches[0].id() {
			return storedImage{}, fmt.Errorf("%q matches more than one image; use more of the ID", text)
		}
	}
	return matches[0], nil
}

// match returns the images text names: the one with that name, or else every
// image whose ID starts with it.
func (s *imageStore) match(images []storedImage, text string) []storedImage {
	if ref, err := parseReference(text); err == nil {
		for _, image := range images {
			if image.name == ref.String() || image.name == text {
				return []storedImage{image}
			}
		}
	}
	if !imageIDPrefix.MatchString(text) {
		return nil
	}
	prefix := "sha256:" + strings.TrimPrefix(text, "sha256:")
	var matches []storedImage
	for _, image := range images {
		if strings.HasPrefix(image.id(), prefix) {
			matches = append(matches, image)
		}
	}
	return matches
}

// openImageRef opens the image ref names: an OCI image layout directory, or
// else an image in the store. With an image from the store, the caller
// should hold its lock.
func openImageRef(ref string) (*ociImage, error) {
	if isLayoutRef(ref) {
		return openImage(ref)
	}
	found, err := defaultImageStore().find(ref)
	if err != nil {
		return nil, err
	}
	return found.image, nil
}

// EnsureImage makes sure the image ref names is available, pulling it into
// the store if it's not a layout directory and not in the store, as docker
// run does. Progress goes to out.
func EnsureImage(ref string, out io.Writer) error {
	if isLayoutRef(ref) {
		return nil
	}
	store := defaultImageStore()
	unlock, err := store.lock(false)
	if err != nil {
		return err
	}
	_, err = store.find(ref)
	unlock()
	if !errors.Is(err, errNoSuchImage) {
		return err
	}
	parsed, parseErr := parseReference(ref)
	if parseErr != nil {
		return parseErr
	}
	fmt.Fprintf(out, "Unable to find image '%s' locally\n", parsed.familiar())
	return store.pull(parsed, out)
}

// Pull downloads an image from a registry into the store, writing progress
// to out.
func Pull(ref string, out io.Writer) error {
	parsed, err := parseReference(ref)
	if err != nil {
		return err
	}
	return defaultImageStore().pull(parsed, out)
}

// pull does Pull's work, in two halves. First, with the lock shared, so
// other pulls and containers can carry on, it downloads into a temporary
// directory of its own: the manifest (following an index to this
// platform's), the configuration, and each layer the store doesn't already
// have. Then, with the lock exclusive, it moves what it downloaded into the
// store and adds the image to index.json.
func (s *imageStore) pull(ref imageReference, out io.Writer) error {
	unlock, err := s.lock(false)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.MkdirAll(s.tmp, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", s.tmp, err)
	}
	tmp, err := os.MkdirTemp(s.tmp, "pull-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// Downloaded blobs, by digest, waiting in tmp to be moved into the
	// store. save writes a blob already in memory there, and records it.
	downloaded := map[string]string{}
	save := func(desc ociDescriptor, data []byte) error {
		path := filepath.Join(tmp, strings.TrimPrefix(desc.Digest, "sha256:"))
		downloaded[desc.Digest] = path
		return os.WriteFile(path, data, 0o600)
	}

	fmt.Fprintf(out, "Pulling %s\n", ref.familiar())
	client := newRegistryClient(ref, out)
	desc, data, err := client.fetchManifest(ref.manifestRef())
	if err != nil {
		return err
	}
	// An index lists a manifest per platform; follow it to this one's.
	for depth := 0; desc.MediaType == mediaTypeImageIndex || desc.MediaType == mediaTypeDockerManifestList; depth++ {
		if depth > 4 {
			return errors.New("image indexes are nested too deeply")
		}
		var index ociIndex
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("image index %s: %w", desc.Digest, err)
		}
		entry, err := pickPlatform(index.Manifests)
		if err != nil {
			return err
		}
		if desc, data, err = client.fetchManifest(entry.Digest); err != nil {
			return err
		}
	}
	if desc.MediaType != mediaTypeImageManifest && desc.MediaType != mediaTypeDockerManifest {
		return fmt.Errorf("image manifest %s has unsupported media type %s", desc.Digest, desc.MediaType)
	}
	var manifest ociManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("image manifest %s: %w", desc.Digest, err)
	}
	if err := save(desc, data); err != nil {
		return err
	}

	// The configuration, and a check that runt can run the image before
	// downloading its layers.
	if manifest.Config.Size > maxMetadataSize {
		return fmt.Errorf("image configuration %s is too large", manifest.Config.Digest)
	}
	var config ociImageConfig
	configPath, err := s.blobPath(manifest.Config.Digest)
	if err != nil {
		return err
	}
	if !s.hasBlob(manifest.Config.Digest) {
		configPath = filepath.Join(tmp, strings.TrimPrefix(manifest.Config.Digest, "sha256:"))
		if err := client.fetchBlob(manifest.Config, configPath); err != nil {
			return err
		}
		downloaded[manifest.Config.Digest] = configPath
	}
	if err := readJSONFile(configPath, &config); err != nil {
		return fmt.Errorf("image configuration: %w", err)
	}
	if config.OS != "" && config.OS != "linux" {
		return fmt.Errorf("image is for %s, not linux", config.OS)
	}
	if len(manifest.Layers) == 0 || len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return fmt.Errorf("image manifest has %d layers, but its configuration lists %d", len(manifest.Layers), len(config.RootFS.DiffIDs))
	}
	for _, layer := range manifest.Layers {
		if _, err := layerCompression(layer.MediaType); err != nil {
			return err
		}
	}

	// Looking up a key a map doesn't have gives the zero value, "" for a
	// string, so downloaded[digest] != "" asks whether it was downloaded.
	for _, layer := range manifest.Layers {
		short := shortDigest(layer.Digest)
		if s.hasBlob(layer.Digest) || downloaded[layer.Digest] != "" {
			fmt.Fprintf(out, "%s: Already exists\n", short)
			continue
		}
		fmt.Fprintf(out, "%s: Downloading %s\n", short, HumanSize(layer.Size))
		path := filepath.Join(tmp, strings.TrimPrefix(layer.Digest, "sha256:"))
		if err := client.fetchBlob(layer, path); err != nil {
			return err
		}
		downloaded[layer.Digest] = path
		fmt.Fprintf(out, "%s: Pull complete\n", short)
	}
	unlock()

	// Move it all into the store. Blobs that were already there may have
	// been removed by runt rmi since; the image is then incomplete, and
	// isn't added.
	unlock, err = s.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	index, err := s.readIndex()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "blobs", "sha256"), 0o700); err != nil {
		return fmt.Errorf("create image store: %w", err)
	}
	for digest, path := range downloaded {
		target, err := s.blobPath(digest)
		if err != nil {
			return err
		}
		if err := os.Rename(path, target); err != nil {
			return fmt.Errorf("store blob %s: %w", digest, err)
		}
	}
	for _, blob := range append([]ociDescriptor{desc, manifest.Config}, manifest.Layers...) {
		if !s.hasBlob(blob.Digest) {
			return fmt.Errorf("blob %s was removed from the image store during the pull; pull again", blob.Digest)
		}
	}

	// The new index: every entry but the one under this name, if any, and
	// then a new entry for it.
	name := ref.String()
	upToDate := false
	var kept []ociDescriptor
	for _, entry := range index.Manifests {
		if entryName(entry) == name {
			upToDate = entry.Digest == desc.Digest
			continue
		}
		kept = append(kept, entry)
	}
	entry := ociDescriptor{
		MediaType:   desc.MediaType,
		Digest:      desc.Digest,
		Size:        desc.Size,
		Annotations: map[string]string{annotationImageName: name},
		Platform:    &ociPlatform{OS: "linux", Architecture: config.Architecture},
	}
	if ref.Tag != "" {
		entry.Annotations[annotationRefName] = ref.Tag
	}
	index.Manifests = append(kept, entry)
	if err := s.writeIndex(index); err != nil {
		return err
	}
	// The image this name used to have may no longer be needed.
	if err := s.collectGarbage(false, nil); err != nil {
		warn(err)
	}

	fmt.Fprintf(out, "Digest: %s\n", desc.Digest)
	if upToDate {
		fmt.Fprintf(out, "Status: Image is up to date for %s\n", ref.familiar())
	} else {
		fmt.Fprintf(out, "Status: Downloaded newer image for %s\n", ref.familiar())
	}
	return nil
}

// shortDigest is the first 12 hex digits of a digest, as Docker shows
// layers and image IDs.
func shortDigest(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return hex
}

// HumanSize formats a size in decimal units, as Docker shows image sizes.
// It divides by 1000 until the number is under 1000, and %.3g prints it with
// three significant digits, as in 12.3MB.
func HumanSize(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	size := float64(n)
	unit := 0
	for size >= 1000 && unit < len(units)-1 {
		size /= 1000
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.3g%s", size, units[unit])
}

// ImageSummary describes an image in the store, for runt images.
type ImageSummary struct {
	// Repository and Tag are as Docker shows them, such as alpine and
	// 3.20. An image pulled by digest has no tag, and its Digest instead.
	Repository string
	Tag        string
	Digest     string
	ID         string
	Created    time.Time
	// Size is the size of the image's blobs, compressed.
	Size int64
}

// ListImages returns the images in the store, in the order they were
// added.
func ListImages() ([]ImageSummary, error) {
	return defaultImageStore().summaries()
}

// summaries does ListImages's work, for this store.
func (s *imageStore) summaries() ([]ImageSummary, error) {
	unlock, err := s.lock(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	images, err := s.list()
	if err != nil {
		return nil, err
	}
	var summaries []ImageSummary
	for _, image := range images {
		summary := ImageSummary{
			Repository: "<none>",
			Tag:        "<none>",
			ID:         image.id(),
			Created:    image.image.config.Created,
			Size:       image.entry.Size + image.image.manifest.Config.Size,
		}
		for _, layer := range image.image.manifest.Layers {
			summary.Size += layer.Size
		}
		if ref, err := parseReference(image.name); err == nil && strings.Contains(image.name, "/") {
			summary.Repository = ref.familiarRepository()
			summary.Tag, summary.Digest = ref.Tag, ref.Digest
			if summary.Tag == "" {
				summary.Tag = "<none>"
			}
		} else if image.name != "" {
			summary.Tag = image.name
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// RemoveImage removes an image from the store, by name or ID, as docker rmi
// does. By name, it untags that name, and the image itself goes once no
// name is left for it. By ID, it removes every name the image has, which
// takes force if there is more than one. An image a container was made
// from also takes force; the container's layers stay until it is removed.
// Blobs and unpacked layers that nothing needs any more are deleted. What
// was untagged and deleted is written to out.
func RemoveImage(ref string, force bool, out io.Writer) error {
	return defaultImageStore().remove(ref, force, StateRoot, out)
}

// remove is RemoveImage, for the containers whose state is in stateRoot.
func (s *imageStore) remove(ref string, force bool, stateRoot string, out io.Writer) error {
	unlock, err := s.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	index, err := s.readIndex()
	if err != nil {
		return err
	}
	images, err := s.list()
	if err != nil {
		return err
	}
	matches := s.match(images, ref)
	if len(matches) == 0 {
		return fmt.Errorf("%w: %s", errNoSuchImage, ref)
	}
	id := matches[0].id()
	for _, match := range matches[1:] {
		if match.id() != id {
			return fmt.Errorf("%q matches more than one image; use more of the ID", ref)
		}
	}
	// untag is a set of the names to remove: a map whose keys are the
	// names, each mapped to true.
	untag := map[string]bool{}
	var names []string
	for _, match := range matches {
		untag[match.name] = true
		names = append(names, match.name)
	}
	if len(matches) > 1 && !force {
		return fmt.Errorf("image %s has several names (%s); remove them one at a time, or use rmi -f", shortDigest(id), strings.Join(names, ", "))
	}

	// The image is deleted if no other name is left for it.
	var kept []ociDescriptor
	deleted := true
	for _, entry := range index.Manifests {
		if untag[entryName(entry)] {
			continue
		}
		kept = append(kept, entry)
		for _, image := range images {
			if image.name == entryName(entry) && image.id() == id {
				deleted = false
			}
		}
	}
	// Read under the lock: a container being made from an image holds it
	// until its state is saved.
	states, err := listStates(stateRoot)
	if err != nil {
		return err
	}
	if deleted && !force {
		for _, state := range states {
			if state.ImageID == id {
				refresh(state)
				return fmt.Errorf("image %s is used by container %s (%s); remove the container first, or use rmi -f", shortDigest(id), state.ID, state.Status)
			}
		}
	}

	index.Manifests = kept
	if err := s.writeIndex(index); err != nil {
		return err
	}
	for _, name := range names {
		display := name
		if parsed, err := parseReference(name); err == nil {
			display = parsed.familiar()
		}
		fmt.Fprintf(out, "Untagged: %s\n", display)
	}
	if deleted {
		fmt.Fprintf(out, "Deleted: %s\n", id)
	}
	return s.collectGarbage(true, states)
}

// collectGarbage deletes the blobs no image in the store refers to. With
// layers set, it also deletes unpacked layers that neither an image in the
// store nor one of the containers in states uses, and leftovers of pulls and
// unpacking that never finished. The caller holds the lock exclusively, so
// nothing is being pulled or unpacked.
//
// It works as a "mark and sweep" garbage collector does: first mark every
// blob reachable from index.json (each manifest, and the configuration and
// layers it names, following indexes down), then sweep away every blob
// that wasn't marked.
func (s *imageStore) collectGarbage(layers bool, states []*State) error {
	index, err := s.readIndex()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	keepLayers := map[string]bool{}
	layout := &ociImage{layout: s.dir}
	// mark calls itself for an index's entries, so it is declared first
	// (var mark ...) and then given its body, which can then refer to it.
	var mark func(desc ociDescriptor, depth int)
	mark = func(desc ociDescriptor, depth int) {
		keep[strings.TrimPrefix(desc.Digest, "sha256:")] = true
		data, err := layout.readBlob(desc)
		if err != nil || depth > 4 {
			return
		}
		var content struct {
			Manifests []ociDescriptor `json:"manifests"`
			Config    *ociDescriptor  `json:"config"`
			Layers    []ociDescriptor `json:"layers"`
		}
		if json.Unmarshal(data, &content) != nil {
			return
		}
		for _, entry := range content.Manifests {
			mark(entry, depth+1)
		}
		if content.Config != nil {
			keep[strings.TrimPrefix(content.Config.Digest, "sha256:")] = true
		}
		for _, layer := range content.Layers {
			hex := strings.TrimPrefix(layer.Digest, "sha256:")
			keep[hex] = true
			keepLayers[hex] = true
		}
	}
	for _, entry := range index.Manifests {
		mark(entry, 0)
	}

	blobs := filepath.Join(s.dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if !keep[entry.Name()] {
			if err := os.Remove(filepath.Join(blobs, entry.Name())); err != nil {
				return fmt.Errorf("remove unused blob: %w", err)
			}
		}
	}
	if !layers {
		return nil
	}

	inUse := map[string]bool{}
	for _, state := range states {
		for _, layer := range state.Layers {
			inUse[layer] = true
		}
	}
	// Unpacked layers are kept per ID range, in layers/<uid base>-<gid
	// base>/<digest>.
	ranges, err := os.ReadDir(s.layers)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, idRange := range ranges {
		dir := filepath.Join(s.layers, idRange.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, layer := range entries {
			path := filepath.Join(dir, layer.Name())
			if keepLayers[layer.Name()] || inUse[path] {
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove unused layer: %w", err)
			}
		}
	}
	if err := os.RemoveAll(s.tmp); err != nil {
		return fmt.Errorf("remove unfinished pulls: %w", err)
	}
	return nil
}
