package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"
)

// OCI image layout (github.com/opencontainers/image-spec, image-layout.md):
// a directory with an oci-layout file, an index.json listing the images in
// it, and every manifest, configuration, and layer stored under
// blobs/<algorithm>/<hex> by the digest of its content. skopeo copy
// docker://IMAGE oci:DIR:TAG, buildah push IMAGE oci:DIR, and docker save
// (Docker 25 and later, once untarred) all produce one.
//
// An index lists manifests, or further indexes, for example one per CPU
// architecture. A manifest names the image's configuration and its layers,
// bottom first. Each layer is a tar archive, usually gzipped, of the files
// it adds or changes.

const (
	mediaTypeImageIndex    = "application/vnd.oci.image.index.v1+json"
	mediaTypeImageManifest = "application/vnd.oci.image.manifest.v1+json"
	// Docker's own formats, which an OCI layout can also hold.
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"

	// annotationRefName holds an image's tag within a layout.
	annotationRefName = "org.opencontainers.image.ref.name"
	// annotationImageName holds the full image name in layouts made by
	// containerd and docker save, such as docker.io/library/alpine:3.20.
	annotationImageName = "io.containerd.image.name"

	// maxMetadataSize caps the size of an index, manifest, or
	// configuration, which are small JSON documents.
	maxMetadataSize = 4 << 20
)

// ociDescriptor points to a blob: its type, digest, and size.
type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *ociPlatform      `json:"platform,omitempty"`
}

type ociPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// ociIndex is index.json, or an index blob.
type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType,omitempty"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType,omitempty"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

// ociImageConfig is the part of an image's configuration runt reads.
// DiffIDs are the digests of the layers once uncompressed, bottom first.
// Config holds the defaults for running the image (see applyImageConfig).
type ociImageConfig struct {
	Created      time.Time `json:"created,omitzero"`
	Architecture string    `json:"architecture"`
	OS           string    `json:"os"`
	Config       struct {
		User       string   `json:"User,omitempty"`
		Env        []string `json:"Env,omitempty"`
		Entrypoint []string `json:"Entrypoint,omitempty"`
		Cmd        []string `json:"Cmd,omitempty"`
		WorkingDir string   `json:"WorkingDir,omitempty"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// ociImage is an image found in a layout.
type ociImage struct {
	layout   string
	manifest ociManifest
	config   ociImageConfig
}

// splitImageRef splits LAYOUT[:TAG] into the layout directory and the tag.
// A colon only starts a tag after the last path separator, so a path like
// ./a:b/c keeps its colon.
func splitImageRef(ref string) (layout, tag string) {
	colon := strings.LastIndex(ref, ":")
	if colon > strings.LastIndexAny(ref, `/`+string(filepath.Separator)) {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// isLayoutRef reports whether ref names an OCI image layout directory,
// rather than an image in runt's image store: it is a path, starting with
// / or ., or the directory it names has an oci-layout file.
func isLayoutRef(ref string) bool {
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, ".") {
		return true
	}
	layout, _ := splitImageRef(ref)
	_, err := os.Stat(filepath.Join(layout, "oci-layout"))
	return err == nil
}

// openImage finds an image in an OCI image layout, by tag if ref has one,
// and reads its manifest and configuration. If the image comes in several
// architectures, it picks the one runt is running on.
func openImage(ref string) (*ociImage, error) {
	layout, tag := splitImageRef(ref)
	index, err := readLayoutIndex(layout)
	if err != nil {
		return nil, err
	}
	desc, err := pickTagged(index.Manifests, tag)
	if err != nil {
		return nil, fmt.Errorf("image layout %s: %w", layout, err)
	}
	return loadImage(layout, desc)
}

// readLayoutIndex checks that layout is an OCI image layout and reads its
// index.json.
func readLayoutIndex(layout string) (*ociIndex, error) {
	var marker struct {
		Version string `json:"imageLayoutVersion"`
	}
	if err := readJSONFile(filepath.Join(layout, "oci-layout"), &marker); err != nil || marker.Version == "" {
		return nil, fmt.Errorf("%s is not an OCI image layout (no valid oci-layout file)", layout)
	}
	var index ociIndex
	if err := readJSONFile(filepath.Join(layout, "index.json"), &index); err != nil {
		return nil, err
	}
	return &index, nil
}

// loadImage reads the manifest desc points to in layout, following an index
// to this platform's manifest, and the image configuration it names.
func loadImage(layout string, desc ociDescriptor) (*ociImage, error) {
	image := &ociImage{layout: layout}
	var err error
	if image.manifest, err = image.resolveManifest(desc, 0); err != nil {
		return nil, err
	}
	data, err := image.readBlob(image.manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("image configuration: %w", err)
	}
	if err := json.Unmarshal(data, &image.config); err != nil {
		return nil, fmt.Errorf("image configuration: %w", err)
	}
	if image.config.OS != "" && image.config.OS != "linux" {
		return nil, fmt.Errorf("image is for %s, not linux", image.config.OS)
	}
	if len(image.manifest.Layers) == 0 {
		return nil, errors.New("image has no layers")
	}
	if len(image.config.RootFS.DiffIDs) != len(image.manifest.Layers) {
		return nil, fmt.Errorf("image manifest has %d layers, but its configuration lists %d", len(image.manifest.Layers), len(image.config.RootFS.DiffIDs))
	}
	return image, nil
}

// pickTagged chooses the entry of index.json with tag, or the only entry
// (or the one tagged latest) when there is no tag.
func pickTagged(entries []ociDescriptor, tag string) (ociDescriptor, error) {
	var tags []string
	for _, entry := range entries {
		ref := entry.Annotations[annotationRefName]
		name := entry.Annotations[annotationImageName]
		if tag != "" && (ref == tag || name == tag || strings.HasSuffix(name, ":"+tag)) {
			return entry, nil
		}
		if tag == "" && ref == "latest" {
			return entry, nil
		}
		if ref != "" {
			tags = append(tags, ref)
		}
	}
	if tag == "" && len(entries) == 1 {
		return entries[0], nil
	}
	if len(entries) == 0 {
		return ociDescriptor{}, errors.New("index.json lists no images")
	}
	if tag == "" {
		return ociDescriptor{}, fmt.Errorf("it holds several images; add :TAG (tags: %s)", strings.Join(tags, ", "))
	}
	return ociDescriptor{}, fmt.Errorf("no image tagged %q (tags: %s)", tag, strings.Join(tags, ", "))
}

// resolveManifest reads the manifest desc points to. If it is an index, it
// follows the entry for this platform.
func (image *ociImage) resolveManifest(desc ociDescriptor, depth int) (ociManifest, error) {
	if depth > 4 {
		return ociManifest{}, errors.New("image indexes are nested too deeply")
	}
	data, err := image.readBlob(desc)
	if err != nil {
		return ociManifest{}, fmt.Errorf("image manifest: %w", err)
	}
	// Some layouts leave out the media type, so look at the content too.
	var probe struct {
		MediaType string          `json:"mediaType"`
		Manifests json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ociManifest{}, fmt.Errorf("image manifest %s: %w", desc.Digest, err)
	}
	mediaType := desc.MediaType
	if mediaType == "" {
		mediaType = probe.MediaType
	}
	if mediaType == mediaTypeImageIndex || mediaType == mediaTypeDockerManifestList || (mediaType == "" && probe.Manifests != nil) {
		var index ociIndex
		if err := json.Unmarshal(data, &index); err != nil {
			return ociManifest{}, fmt.Errorf("image index %s: %w", desc.Digest, err)
		}
		entry, err := pickPlatform(index.Manifests)
		if err != nil {
			return ociManifest{}, err
		}
		return image.resolveManifest(entry, depth+1)
	}
	if mediaType != "" && mediaType != mediaTypeImageManifest && mediaType != mediaTypeDockerManifest {
		return ociManifest{}, fmt.Errorf("image manifest %s has unsupported media type %s", desc.Digest, mediaType)
	}
	var manifest ociManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ociManifest{}, fmt.Errorf("image manifest %s: %w", desc.Digest, err)
	}
	return manifest, nil
}

// pickPlatform chooses the linux entry for the CPU architecture runt is
// running on. Entries without a platform, such as the attestations docker
// buildx adds, are skipped.
func pickPlatform(entries []ociDescriptor) (ociDescriptor, error) {
	var seen []string
	for _, entry := range entries {
		if entry.Platform == nil {
			continue
		}
		if entry.Platform.OS == "linux" && entry.Platform.Architecture == goruntime.GOARCH {
			return entry, nil
		}
		seen = append(seen, entry.Platform.OS+"/"+entry.Platform.Architecture)
	}
	return ociDescriptor{}, fmt.Errorf("image has no linux/%s variant (it has: %s)", goruntime.GOARCH, strings.Join(seen, ", "))
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// blobPath returns where the blob with digest is stored. Only sha256 digests
// are accepted, which also stops a digest from naming a path outside the
// layout.
func (image *ociImage) blobPath(digest string) (string, error) {
	algorithm, encoded, _ := strings.Cut(digest, ":")
	if algorithm != "sha256" || !sha256Hex.MatchString(encoded) {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	return filepath.Join(image.layout, "blobs", algorithm, encoded), nil
}

// readBlob reads a small blob and checks it against its descriptor.
func (image *ociImage) readBlob(desc ociDescriptor) ([]byte, error) {
	path, err := image.blobPath(desc.Digest)
	if err != nil {
		return nil, err
	}
	if desc.Size > maxMetadataSize {
		return nil, fmt.Errorf("blob %s is too large (%d bytes)", desc.Digest, desc.Size)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if desc.Size != 0 && int64(len(data)) != desc.Size {
		return nil, fmt.Errorf("blob %s is %d bytes, but should be %d", desc.Digest, len(data), desc.Size)
	}
	if sum := sha256.Sum256(data); "sha256:"+hex.EncodeToString(sum[:]) != desc.Digest {
		return nil, fmt.Errorf("blob %s does not match its digest", desc.Digest)
	}
	return data, nil
}

// layerCompression returns how a layer with mediaType is compressed: "gzip"
// or "" for none.
func layerCompression(mediaType string) (string, error) {
	switch {
	case strings.HasSuffix(mediaType, ".tar"):
		// application/vnd.oci.image.layer.v1.tar, and Docker's
		// application/vnd.docker.image.rootfs.diff.tar.
		return "", nil
	case strings.HasSuffix(mediaType, ".tar+gzip"), strings.HasSuffix(mediaType, ".tar.gzip"):
		return "gzip", nil
	case strings.HasSuffix(mediaType, "+zstd"):
		return "", fmt.Errorf("zstd-compressed layers are not supported; convert the image to gzip (skopeo copy --dest-compress-format gzip)")
	}
	return "", fmt.Errorf("unsupported layer media type %q", mediaType)
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
