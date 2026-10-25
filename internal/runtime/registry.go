package runtime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A registry client for the OCI distribution API
// (github.com/opencontainers/distribution-spec), which Docker Hub, GitHub's
// ghcr.io, and every other registry serve. Pulling an image takes three kinds
// of request, all GETs:
//
//   - /v2/REPOSITORY/manifests/TAG-OR-DIGEST, for the image's manifest, or
//     an index of manifests for several platforms.
//   - /v2/REPOSITORY/blobs/DIGEST, for its configuration and each layer.
//     Registries often answer with a redirect to a CDN.
//
// Authentication works by challenge. A request without credentials gets 401
// and a WWW-Authenticate header. "Basic" means send a user name and password
// with every request. "Bearer realm=URL,service=...,scope=..." means ask the
// token server at URL for a token, with the user name and password if there
// are any, and send that instead. Docker Hub asks for a token even for
// public images, and hands one out without credentials.
//
// Also here: reading and writing stored credentials (runt login and
// logout). This file has no build constraint, so its tests, which serve a
// pretend registry from the test itself, run anywhere. For how to read the
// Go here, see doc.go.

// manifestAccept lists the manifest types runt understands, for the Accept
// header. A registry answers with the first one it has.
var manifestAccept = strings.Join([]string{
	mediaTypeImageIndex, mediaTypeImageManifest,
	mediaTypeDockerManifestList, mediaTypeDockerManifest,
}, ", ")

// registryClient talks to one repository in a registry.
type registryClient struct {
	http *http.Client
	// base is the registry's address, such as https://registry-1.docker.io.
	base       string
	repository string
	// credentials returns the user name and password to log in with, if
	// there are any. It is only called when the registry asks. It is a
	// field holding a function, so a public image never touches the stored
	// credentials, and runt login and the tests can supply their own.
	credentials func() (user, password string)
	// authorization is the Authorization header to send, once the
	// registry has asked for one.
	authorization string
}

// newRegistryClient returns a client for ref's repository. It uses the
// credentials stored for the registry (see storedCredentials), and reports
// problems reading them to warnings.
func newRegistryClient(ref imageReference, warnings io.Writer) *registryClient {
	return &registryClient{
		http:       newHTTPClient(),
		base:       registryURL(ref.registryHost()),
		repository: ref.Path,
		credentials: func() (string, string) {
			user, password, err := storedCredentials(ref.Domain)
			if err != nil {
				fmt.Fprintf(warnings, "runt: warning: reading credentials for %s: %v\n", ref.Domain, err)
			}
			return user, password
		},
	}
}

// newHTTPClient makes the HTTP client registry requests go through. It
// starts from a copy of Go's default transport (the part that opens
// connections, and handles proxies and TLS), which DefaultTransport holds
// as an interface; the type assertion takes out the *http.Transport so it
// can be copied and changed.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Layers can take a long time to download, so there is no limit on a
	// whole request, only on waiting for the server to start answering.
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: transport}
}

// registryURL is the base URL of a registry. As Docker does, it uses plain
// HTTP for a registry on this machine, such as a test registry on
// localhost:5000, and HTTPS for every other.
func registryURL(host string) string {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	if name == "localhost" {
		return "http://" + host
	}
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return "http://" + host
	}
	return "https://" + host
}

// get makes a GET request to the registry, logging in and trying again if
// the registry asks for credentials.
//
// The loop runs at most twice: the first attempt, and if that gets 401
// Unauthorized, one more with the credentials the challenge asked for.
// After that, any answer, 401 included, is returned to the caller, whose
// job it is to close the response's Body.
func (c *registryClient) get(path string, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequest(http.MethodGet, c.base+path, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "runt/0.1.0")
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		if c.authorization != "" {
			request.Header.Set("Authorization", c.authorization)
		}
		response, err := c.http.Do(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return response, nil
		}
		challenge := response.Header.Get("WWW-Authenticate")
		response.Body.Close()
		if c.authorization, err = c.authenticate(challenge); err != nil {
			return nil, err
		}
	}
}

// authenticate answers a WWW-Authenticate challenge, returning the
// Authorization header to send from now on.
func (c *registryClient) authenticate(challenge string) (string, error) {
	scheme, params := parseChallenge(challenge)
	user, password := c.credentials()
	switch strings.ToLower(scheme) {
	case "basic":
		if user == "" {
			return "", errors.New("the registry needs a user name and password; log in with runt login")
		}
		// HTTP Basic authentication: "Basic " and USER:PASSWORD in base64,
		// an encoding of bytes as letters, digits, + and /.
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password)), nil
	case "bearer":
		token, err := c.fetchToken(params, user, password)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	}
	return "", fmt.Errorf("the registry asks for unsupported authentication %q", challenge)
}

// fetchToken asks the token server named in a Bearer challenge for a token
// to pull from the repository.
func (c *registryClient) fetchToken(params map[string]string, user, password string) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("the registry's token server %q is not a URL", params["realm"])
	}
	// Credentials only go over HTTPS, unless the registry itself is local
	// and plain HTTP.
	if realm.Scheme != "https" && !(realm.Scheme == "http" && strings.HasPrefix(c.base, "http://")) {
		return "", fmt.Errorf("the registry's token server %s doesn't use HTTPS", realm)
	}
	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" && c.repository != "" {
		scope = "repository:" + c.repository + ":pull"
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	realm.RawQuery = query.Encode()
	request, err := http.NewRequest(http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "runt/0.1.0")
	if user != "" {
		request.SetBasicAuth(user, password)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("get a registry token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return "", errors.New("the registry refused the user name and password; log in again with runt login")
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get a registry token: %s", response.Status)
	}
	// The token server answers with JSON holding the token, under either
	// name. A json.Decoder reads JSON straight from the response, and
	// LimitReader stops a server from sending without end.
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxMetadataSize)).Decode(&body); err != nil {
		return "", fmt.Errorf("read registry token: %w", err)
	}
	if body.Token == "" {
		body.Token = body.AccessToken
	}
	if body.Token == "" {
		return "", errors.New("the registry's token server sent no token")
	}
	return body.Token, nil
}

// parseChallenge splits a WWW-Authenticate header, such as
// Bearer realm="https://auth.docker.io/token",service="registry.docker.io",
// into its scheme and parameters. Values may be quoted, and quoted values
// may contain commas.
//
// It reads one key=value pair per round of the loop, from the front of
// rest, and then cuts it off. A quoted value is read a character at a time
// up to its closing quote, with a backslash meaning "take the next
// character as it is". The loop has a starting statement and a condition
// but no step: each round moves rest along itself.
func parseChallenge(header string) (scheme string, params map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	params = map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, value, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimLeft(value, " ")
		if strings.HasPrefix(value, `"`) {
			var unquoted strings.Builder
			i := 1
			for ; i < len(value) && value[i] != '"'; i++ {
				if value[i] == '\\' && i+1 < len(value) {
					i++
				}
				unquoted.WriteByte(value[i])
			}
			params[key] = unquoted.String()
			rest = value[min(i+1, len(value)):]
		} else {
			end := strings.IndexByte(value, ',')
			if end < 0 {
				end = len(value)
			}
			params[key] = strings.TrimSpace(value[:end])
			rest = value[end:]
		}
		rest = strings.TrimLeft(rest, ", ")
	}
	return scheme, params
}

// registryError turns an unsuccessful response into an error, with the
// message from the registry's JSON error body if it sent one.
func registryError(response *http.Response, what string) error {
	var body struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&body)
	detail := response.Status
	if len(body.Errors) > 0 {
		detail = strings.ToLower(body.Errors[0].Code) + ": " + body.Errors[0].Message
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s: %s (does the image exist, and do you need runt login?)", what, detail)
	}
	return fmt.Errorf("%s: %s", what, detail)
}

// fetchManifest gets the manifest or index that ref (a tag or digest)
// names, and checks it against the digest if ref is one. It returns the
// manifest's descriptor and content.
func (c *registryClient) fetchManifest(ref string) (ociDescriptor, []byte, error) {
	response, err := c.get("/v2/"+c.repository+"/manifests/"+ref, manifestAccept)
	if err != nil {
		return ociDescriptor{}, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ociDescriptor{}, nil, registryError(response, "get manifest "+c.repository+":"+ref)
	}
	// Read one byte more than allowed, so a manifest that is too big shows
	// up as too long, rather than being cut short and looking fine.
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataSize+1))
	if err != nil {
		return ociDescriptor{}, nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > maxMetadataSize {
		return ociDescriptor{}, nil, errors.New("the manifest is too large")
	}
	sum := sha256.Sum256(data)
	desc := ociDescriptor{
		MediaType: strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]),
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		Size:      int64(len(data)),
	}
	if strings.HasPrefix(ref, "sha256:") && ref != desc.Digest {
		return ociDescriptor{}, nil, fmt.Errorf("manifest %s from the registry has digest %s", ref, desc.Digest)
	}
	// Some registries say application/json; the content says what it is.
	var probe struct {
		MediaType string          `json:"mediaType"`
		Manifests json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ociDescriptor{}, nil, fmt.Errorf("manifest %s: %w", desc.Digest, err)
	}
	switch {
	case probe.MediaType != "":
		desc.MediaType = probe.MediaType
	case probe.Manifests != nil:
		desc.MediaType = mediaTypeImageIndex
	case desc.MediaType != mediaTypeImageManifest && desc.MediaType != mediaTypeDockerManifest:
		desc.MediaType = mediaTypeImageManifest
	}
	return desc, data, nil
}

// fetchBlob downloads the blob desc names into the file at path, checking
// its size and digest on the way.
func (c *registryClient) fetchBlob(desc ociDescriptor, path string) error {
	response, err := c.get("/v2/"+c.repository+"/blobs/"+desc.Digest, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return registryError(response, "get blob "+desc.Digest)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	// One byte more than expected shows that the blob is too large.
	// MultiWriter writes each piece both to the file and to the hash, so
	// the digest is worked out as the blob downloads.
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, desc.Size+1))
	// Closing a written file can fail too (the data may only reach the disk
	// then), so its error counts, unless there already was one.
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download blob %s: %w", desc.Digest, err)
	}
	if n != desc.Size {
		return fmt.Errorf("blob %s is %d bytes, but should be %d", desc.Digest, n, desc.Size)
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); got != desc.Digest {
		return fmt.Errorf("blob %s does not match its digest (got %s)", desc.Digest, got)
	}
	return nil
}

// Credentials are kept the way Docker keeps them, in its config.json:
// $DOCKER_CONFIG/config.json, or ~/.docker/config.json. Under sudo, ~ is
// root's home. A registry's entry under "auths" holds "auth", the base64 of
// USER:PASSWORD. Docker can keep them in a credential helper instead, a
// program called docker-credential-NAME, named by "credHelpers" for one
// registry or "credsStore" for all. runt reads from those too, but runt
// login always writes to "auths".

// dockerConfigAuth is one registry's entry under "auths".
type dockerConfigAuth struct {
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

// dockerConfigPath is where Docker's config.json is.
func dockerConfigPath() (string, error) {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// credentialKey is the key Docker keeps a registry's credentials under.
func credentialKey(domain string) string {
	if domain == dockerHubDomain {
		return dockerHubAuthKey
	}
	return domain
}

// readDockerConfig reads config.json as raw JSON fields, so that writing it
// back keeps the fields runt doesn't know about. A missing file is empty.
func readDockerConfig(path string) (map[string]json.RawMessage, error) {
	config := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// storedCredentials returns the user name and password stored for a
// registry, or empty strings if there are none.
func storedCredentials(domain string) (user, password string, err error) {
	path, err := dockerConfigPath()
	if err != nil {
		return "", "", err
	}
	config, err := readDockerConfig(path)
	if err != nil {
		return "", "", err
	}
	// Decode the three fields runt reads. Errors are ignored: a field
	// that is missing or malformed simply stays empty.
	var auths map[string]dockerConfigAuth
	var helpers map[string]string
	var store string
	json.Unmarshal(config["auths"], &auths)
	json.Unmarshal(config["credHelpers"], &helpers)
	json.Unmarshal(config["credsStore"], &store)

	key := credentialKey(domain)
	keys := []string{key, domain, "https://" + domain, "http://" + domain}
	if domain == dockerHubDomain {
		keys = append(keys, "index.docker.io", dockerHubRegistry)
	}
	for _, k := range keys {
		entry, ok := auths[k]
		if !ok || entry.Auth == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return "", "", fmt.Errorf("%s: the auth for %s is not base64", path, k)
		}
		user, password, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return "", "", fmt.Errorf("%s: the auth for %s is not USER:PASSWORD", path, k)
		}
		return user, password, nil
	}
	helper := helpers[domain]
	if helper == "" {
		helper = helpers[key]
	}
	if helper == "" {
		helper = store
	}
	if helper == "" {
		return "", "", nil
	}
	return helperCredentials(helper, key)
}

// helperCredentials asks the credential helper docker-credential-NAME for a
// registry's credentials: it reads the registry's key on its input and
// prints them as JSON.
func helperCredentials(name, key string) (user, password string, err error) {
	// strings.NewReader makes an io.Reader of a string, which becomes the
	// helper's input. Output runs the program and returns what it printed.
	command := exec.Command("docker-credential-"+name, "get")
	command.Stdin = strings.NewReader(key)
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(string(output)+string(exitErr.Stderr), "not found") {
			return "", "", nil
		}
		return "", "", fmt.Errorf("credential helper docker-credential-%s: %w", name, err)
	}
	var credentials struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(output, &credentials); err != nil {
		return "", "", fmt.Errorf("credential helper docker-credential-%s: %w", name, err)
	}
	if credentials.Username == "<token>" {
		// An identity token, for OAuth; runt doesn't do that.
		return "", "", nil
	}
	return credentials.Username, credentials.Secret, nil
}

// Login checks a user name and password with a registry, given by domain
// such as docker.io or localhost:5000, and stores them in Docker's
// config.json. It returns the file's path.
func Login(server, user, password string) (string, error) {
	domain, err := loginDomain(server)
	if err != nil {
		return "", err
	}
	if user == "" || password == "" {
		return "", errors.New("a user name and password are required")
	}
	ref := imageReference{Domain: domain}
	client := &registryClient{
		http:        newHTTPClient(),
		base:        registryURL(ref.registryHost()),
		credentials: func() (string, string) { return user, password },
	}
	// /v2/ is the registry's API version check. Once logged in, it answers
	// 200.
	response, err := client.get("/v2/", "")
	if err != nil {
		return "", fmt.Errorf("log in to %s: %w", domain, err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("log in to %s: %s", domain, response.Status)
	}
	path, err := dockerConfigPath()
	if err != nil {
		return "", err
	}
	auth := dockerConfigAuth{Auth: base64.StdEncoding.EncodeToString([]byte(user + ":" + password))}
	// The anonymous function is the change to make: set this registry's
	// entry. updateDockerAuths does the reading and writing around it.
	return path, updateDockerAuths(path, func(auths map[string]json.RawMessage) error {
		data, err := json.Marshal(auth)
		auths[credentialKey(domain)] = data
		return err
	})
}

// Logout removes the credentials stored for a registry. It reports whether
// there were any.
func Logout(server string) (bool, error) {
	domain, err := loginDomain(server)
	if err != nil {
		return false, err
	}
	path, err := dockerConfigPath()
	if err != nil {
		return false, err
	}
	found := false
	err = updateDockerAuths(path, func(auths map[string]json.RawMessage) error {
		_, found = auths[credentialKey(domain)]
		delete(auths, credentialKey(domain))
		return nil
	})
	return found, err
}

// loginDomain turns runt login's argument into a registry domain: empty
// means Docker Hub, and a URL is reduced to its host.
func loginDomain(server string) (string, error) {
	if server == "" || server == dockerHubAuthKey || server == "index.docker.io" || server == dockerHubRegistry {
		return dockerHubDomain, nil
	}
	if strings.Contains(server, "://") {
		u, err := url.Parse(server)
		if err != nil {
			return "", err
		}
		server = u.Host
	}
	if !referenceDomain.MatchString(server) {
		return "", fmt.Errorf("invalid registry %q", server)
	}
	return server, nil
}

// updateDockerAuths changes the "auths" entries of config.json, keeping
// everything else in the file as it was. The file holds passwords, so it
// is only readable by its owner.
//
// json.RawMessage holds a piece of JSON undecoded, so entries and fields
// runt doesn't understand are written back exactly as they were read.
func updateDockerAuths(path string, change func(auths map[string]json.RawMessage) error) error {
	config, err := readDockerConfig(path)
	if err != nil {
		return err
	}
	auths := map[string]json.RawMessage{}
	if raw, ok := config["auths"]; ok {
		if err := json.Unmarshal(raw, &auths); err != nil {
			return fmt.Errorf("%s: auths: %w", path, err)
		}
	}
	if err := change(auths); err != nil {
		return err
	}
	if config["auths"], err = json.Marshal(auths); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
