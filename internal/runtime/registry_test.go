package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
)

// Tests for registry.go: talking to a registry, and stored credentials. The
// registry is a pretend one, served by the test itself on a local port with
// httptest, so the tests need no internet. For how Go tests work, see
// "Tests" in doc.go.

// testRegistry is a registry for tests. Like Docker Hub, it hands out tokens
// for pulling, checking a user name and password if it has one, and sends
// blob requests on to another path, as a registry would to a CDN.
type testRegistry struct {
	t      *testing.T
	server *httptest.Server
	// user and password, if set, are what the token server wants.
	user, password string

	mu        sync.Mutex
	blobs     map[string][]byte
	types     map[string]string // manifest digest to media type
	tags      map[string]string // tag to manifest digest
	blobPulls int
	// scopes are the scopes tokens were asked for, and uploads the
	// digests of the blobs pushed, in order.
	scopes  []string
	uploads []string
}

const testToken = "test-token"

// newTestRegistry starts the registry. httptest.NewServer serves HTTP on a
// free local port, calling the handler it is given for each request:
// http.HandlerFunc turns the method r.serve into one. The server is shut
// down when the test ends.
func newTestRegistry(t *testing.T) *testRegistry {
	r := &testRegistry{t: t, blobs: map[string][]byte{}, types: map[string]string{}, tags: map[string]string{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

// host is the registry's host:port, to put in image references.
func (r *testRegistry) host() string {
	return strings.TrimPrefix(r.server.URL, "http://")
}

// serve answers one request, as a registry would, by its path: the token
// server, the "CDN" blobs are redirected to, and, once the request carries
// a token, the API's manifests and blobs. What is written to w is the
// response; req is the request.
func (r *testRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case req.URL.Path == "/token":
		user, password, _ := req.BasicAuth()
		if r.user != "" && (user != r.user || password != r.password) {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		r.scopes = append(r.scopes, req.URL.Query().Get("scope"))
		json.NewEncoder(w).Encode(map[string]string{"token": testToken})
		return
	case strings.HasPrefix(req.URL.Path, "/cdn/"):
		data, ok := r.blobs[strings.TrimPrefix(req.URL.Path, "/cdn/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		r.blobPulls++
		w.Write(data)
		return
	}
	if req.Header.Get("Authorization") != "Bearer "+testToken {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:test/app:pull"`, r.server.URL))
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": "UNAUTHORIZED", "message": "authentication required"}}})
		return
	}
	if req.URL.Path == "/v2/" {
		return
	}
	if r.servePush(w, req) {
		return
	}
	repo, rest, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v2/"), "/manifests/")
	if ok && repo == "test/app" {
		digest := rest
		if !strings.HasPrefix(rest, "sha256:") {
			digest = r.tags[rest]
		}
		data, ok := r.blobs[digest]
		if !ok || r.types[digest] == "" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": "MANIFEST_UNKNOWN", "message": "manifest unknown"}}})
			return
		}
		w.Header().Set("Content-Type", r.types[digest])
		w.Write(data)
		return
	}
	if repo, digest, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v2/"), "/blobs/"); ok && repo == "test/app" {
		http.Redirect(w, req, "/cdn/"+digest, http.StatusTemporaryRedirect)
		return
	}
	http.NotFound(w, req)
}

// servePush answers the requests of a push, and reports whether req was one:
// HEAD of a blob, POST to start an upload, which is sent on to /upload/
// with a query of its own that must come back, PUT of the blob there, and
// PUT of a manifest. The caller holds r.mu.
func (r *testRegistry) servePush(w http.ResponseWriter, req *http.Request) bool {
	const prefix = "/v2/test/app/"
	switch {
	case req.Method == http.MethodHead && strings.HasPrefix(req.URL.Path, prefix+"blobs/"):
		if _, ok := r.blobs[strings.TrimPrefix(req.URL.Path, prefix+"blobs/")]; !ok {
			w.WriteHeader(http.StatusNotFound)
		}
	case req.Method == http.MethodPost && req.URL.Path == prefix+"blobs/uploads/":
		w.Header().Set("Location", fmt.Sprintf("/upload/%d?state=kept", len(r.uploads)))
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPut && strings.HasPrefix(req.URL.Path, "/upload/"):
		data, _ := io.ReadAll(req.Body)
		sum := sha256.Sum256(data)
		digest := req.URL.Query().Get("digest")
		if digest != "sha256:"+hex.EncodeToString(sum[:]) || req.URL.Query().Get("state") != "kept" || req.ContentLength != int64(len(data)) {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return true
		}
		r.blobs[digest] = data
		r.uploads = append(r.uploads, digest)
		w.WriteHeader(http.StatusCreated)
	case req.Method == http.MethodPut && strings.HasPrefix(req.URL.Path, prefix+"manifests/"):
		data, _ := io.ReadAll(req.Body)
		var manifest ociManifest
		json.Unmarshal(data, &manifest)
		for _, blob := range append(manifest.Layers, manifest.Config) {
			if _, ok := r.blobs[blob.Digest]; !ok {
				http.Error(w, "manifest blob unknown", http.StatusBadRequest)
				return true
			}
		}
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		r.blobs[digest] = data
		r.types[digest] = req.Header.Get("Content-Type")
		r.tags[strings.TrimPrefix(req.URL.Path, prefix+"manifests/")] = digest
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
	default:
		return false
	}
	return true
}

// blob stores data and returns its descriptor.
func (r *testRegistry) blob(mediaType string, data []byte) ociDescriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	r.blobs[digest] = data
	return ociDescriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}
}

// jsonBlob stores v, encoded as JSON, and remembers the media type of
// manifests and indexes, to send as their Content-Type.
func (r *testRegistry) jsonBlob(mediaType string, v any) ociDescriptor {
	data, err := json.Marshal(v)
	if err != nil {
		r.t.Fatal(err)
	}
	desc := r.blob(mediaType, data)
	if strings.Contains(mediaType, "manifest") || strings.Contains(mediaType, "index") {
		r.mu.Lock()
		r.types[desc.Digest] = mediaType
		r.mu.Unlock()
	}
	return desc
}

// image stores a one-layer image for arch whose layer content is marker,
// with cmd as its default command, and returns its manifest's descriptor.
func (r *testRegistry) image(arch, marker string, cmd ...string) ociDescriptor {
	layer := r.blob("application/vnd.oci.image.layer.v1.tar", []byte(marker))
	config := ociImageConfig{Architecture: arch, OS: "linux"}
	config.RootFS.Type = "layers"
	config.RootFS.DiffIDs = []string{layer.Digest}
	config.Config.Cmd = cmd
	desc := r.jsonBlob(mediaTypeImageManifest, ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeImageManifest,
		Config:        r.jsonBlob("application/vnd.oci.image.config.v1+json", config),
		Layers:        []ociDescriptor{layer},
	})
	desc.Platform = &ociPlatform{OS: "linux", Architecture: arch}
	return desc
}

// multiPlatform stores an index of an image for this machine and one for
// another architecture.
func (r *testRegistry) multiPlatform(marker string) ociDescriptor {
	other := "arm64"
	if goruntime.GOARCH == "arm64" {
		other = "amd64"
	}
	return r.jsonBlob(mediaTypeImageIndex, ociIndex{SchemaVersion: 2, MediaType: mediaTypeImageIndex, Manifests: []ociDescriptor{
		r.image(other, "foreign "+marker, "foreign"),
		r.image(goruntime.GOARCH, marker, "native"),
	}})
}

// tag points tag at the manifest or index desc describes.
func (r *testRegistry) tag(tag string, desc ociDescriptor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags[tag] = desc.Digest
}

// TestParseChallenge checks reading WWW-Authenticate headers.
func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:a/b:pull,push"`)
	if scheme != "Bearer" || params["realm"] != "https://auth.docker.io/token" || params["service"] != "registry.docker.io" || params["scope"] != "repository:a/b:pull,push" {
		t.Errorf("parseChallenge = %q, %q", scheme, params)
	}
	scheme, params = parseChallenge(`Basic realm=registry, charset="UTF-8\"x"`)
	if scheme != "Basic" || params["realm"] != "registry" || params["charset"] != `UTF-8"x` {
		t.Errorf("parseChallenge = %q, %q", scheme, params)
	}
}

// TestRegistryClient fetches an image's manifest and blobs, logging in to the
// token server first, and checks that tampered content is refused.
func TestRegistryClient(t *testing.T) {
	r := newTestRegistry(t)
	r.user, r.password = "alice", "secret"
	index := r.multiPlatform("hello")
	r.tag("v1", index)

	ref, err := parseReference(r.host() + "/test/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	client := newRegistryClient(ref, os.Stderr)
	if _, _, err := client.fetchManifest("v1"); err == nil || !strings.Contains(err.Error(), "runt login") {
		t.Errorf("fetching without credentials: %v; want a hint to log in", err)
	}

	if _, err := Login(r.host(), "alice", "wrong"); err == nil {
		t.Error("Login accepted a wrong password")
	}
	path, err := Login(r.host(), "alice", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 && goruntime.GOOS != "windows" {
		t.Errorf("credentials file %s: %v, %v", path, info, err)
	}

	client = newRegistryClient(ref, os.Stderr)
	desc, data, err := client.fetchManifest("v1")
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if desc.MediaType != mediaTypeImageIndex || desc.Digest != index.Digest {
		t.Errorf("fetchManifest = %+v; want the index %s", desc, index.Digest)
	}
	var list ociIndex
	json.Unmarshal(data, &list)
	entry, err := pickPlatform(list.Manifests)
	if err != nil {
		t.Fatal(err)
	}
	_, data, err = client.fetchManifest(entry.Digest)
	if err != nil {
		t.Fatalf("fetchManifest by digest: %v", err)
	}
	var manifest ociManifest
	json.Unmarshal(data, &manifest)
	layerPath := filepath.Join(t.TempDir(), "layer")
	if err := client.fetchBlob(manifest.Layers[0], layerPath); err != nil {
		t.Fatalf("fetchBlob: %v", err)
	}
	if content, _ := os.ReadFile(layerPath); string(content) != "hello" {
		t.Errorf("layer content = %q; want the native image's", content)
	}

	// A registry sending content that doesn't match its digest.
	r.mu.Lock()
	r.blobs[manifest.Layers[0].Digest] = []byte("HELLO")
	r.mu.Unlock()
	if err := client.fetchBlob(manifest.Layers[0], layerPath); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("fetchBlob of tampered content: %v", err)
	}
	if _, _, err := client.fetchManifest("sha256:" + strings.Repeat("0", 64)); err == nil {
		t.Error("fetchManifest of an unknown digest succeeded")
	}

	if found, err := Logout(r.host()); err != nil || !found {
		t.Errorf("Logout = %v, %v", found, err)
	}
	if user, _, err := storedCredentials(r.host()); err != nil || user != "" {
		t.Errorf("credentials after logout: %q, %v", user, err)
	}
}

// TestRegistryPush uploads a blob and a manifest, asking for a token that
// allows pushing, and checks the registry has them after.
func TestRegistryPush(t *testing.T) {
	r := newTestRegistry(t)
	ref, err := parseReference(r.host() + "/test/app:pushed")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	client := newRegistryClient(ref, os.Stderr)
	client.push = true

	layer := []byte("a layer to push")
	sum := sha256.Sum256(layer)
	desc := ociDescriptor{MediaType: mediaTypeLayer, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(layer))}
	path := filepath.Join(t.TempDir(), "layer")
	if err := os.WriteFile(path, layer, 0o600); err != nil {
		t.Fatal(err)
	}
	if exists, err := client.hasBlob(desc); err != nil || exists {
		t.Fatalf("hasBlob before the upload = %v, %v", exists, err)
	}
	if err := client.uploadBlob(desc, path); err != nil {
		t.Fatalf("uploadBlob: %v", err)
	}
	if exists, err := client.hasBlob(desc); err != nil || !exists {
		t.Errorf("hasBlob after the upload = %v, %v", exists, err)
	}
	if len(r.scopes) == 0 || r.scopes[0] != "repository:test/app:pull,push" {
		t.Errorf("token scopes = %q; want pull,push", r.scopes)
	}

	config := r.jsonBlob(mediaTypeImageConfig, map[string]string{"os": "linux"})
	manifest, err := json.Marshal(ociManifest{SchemaVersion: 2, MediaType: mediaTypeImageManifest, Config: config, Layers: []ociDescriptor{desc}})
	if err != nil {
		t.Fatal(err)
	}
	manifestSum := sha256.Sum256(manifest)
	manifestDesc := ociDescriptor{MediaType: mediaTypeImageManifest, Digest: "sha256:" + hex.EncodeToString(manifestSum[:])}
	if err := client.putManifest("pushed", manifestDesc, manifest); err != nil {
		t.Fatalf("putManifest: %v", err)
	}
	if got, _, err := client.fetchManifest("pushed"); err != nil || got.Digest != manifestDesc.Digest {
		t.Errorf("the pushed tag gives %+v, %v", got, err)
	}

	// A manifest whose blobs the registry doesn't have is refused.
	missing := []byte(`{"schemaVersion":2,"layers":[{"digest":"sha256:` + strings.Repeat("0", 64) + `"}]}`)
	if err := client.putManifest("broken", ociDescriptor{MediaType: mediaTypeImageManifest}, missing); err == nil {
		t.Error("putManifest of a manifest with missing blobs succeeded")
	}
}

// TestStoredCredentials checks finding credentials in a config.json written
// by Docker, which keeps other settings runt must leave alone.
func TestStoredCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	// "Ym9iOmh1bnRlcjI=" is base64 for bob:hunter2.
	config := `{"auths":{"https://index.docker.io/v1/":{"auth":"Ym9iOmh1bnRlcjI="},"ghcr.io":{}},"psFormat":"table {{.ID}}"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if user, password, err := storedCredentials("docker.io"); err != nil || user != "bob" || password != "hunter2" {
		t.Errorf("Docker Hub credentials = %q, %q, %v", user, password, err)
	}
	if user, _, err := storedCredentials("ghcr.io"); err != nil || user != "" {
		t.Errorf("ghcr.io credentials = %q, %v; want none", user, err)
	}
	if found, err := Logout(""); err != nil || !found {
		t.Fatalf("Logout from Docker Hub = %v, %v", found, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !strings.Contains(string(data), "psFormat") || strings.Contains(string(data), "Ym9i") {
		t.Errorf("config.json after logout:\n%s", data)
	}
}
