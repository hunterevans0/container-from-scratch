package runtime

import (
	"fmt"
	"regexp"
	"strings"
)

// Image references, as Docker writes them: [REGISTRY/]REPOSITORY[:TAG][@DIGEST].
//
// A reference with no registry is on Docker Hub, and a Docker Hub repository
// with a single path component is one of its official images, under
// library/. So alpine is docker.io/library/alpine:latest, and
// ghcr.io/foo/bar:1 is left as it is. The first component names a registry
// only if it has a dot or a colon (a port), or is localhost; otherwise it is
// part of the repository, as in someuser/someimage.
//
// This file has no build constraint, so its tests run anywhere. For how to
// read the Go here, see doc.go.

const (
	dockerHubDomain = "docker.io"
	// dockerHubRegistry is where Docker Hub's registry API is served.
	dockerHubRegistry = "registry-1.docker.io"
	// dockerHubAuthKey is the key Docker's config.json keeps Docker Hub
	// credentials under, for historical reasons.
	dockerHubAuthKey = "https://index.docker.io/v1/"
)

// The patterns, as regular expressions, that a reference's parts must
// match. In them, ^ and $ anchor the match to the whole string, [a-z0-9]
// is any one of those characters, + is one or more, * zero or more, ? at
// most one, (?:...) groups without capturing, and \w is a letter, digit, or
// underscore. They are compiled once, when runt starts.
var (
	// referencePathComponent is one component of a repository path: lower
	// case letters and digits, separated by '.', '_', '__', or runs of '-'.
	referencePathComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	referenceDomain        = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*(?::[0-9]+)?$`)
	referenceTag           = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
)

// imageReference is a parsed, normalized image reference. Exactly one of
// Tag and Digest is set.
type imageReference struct {
	// Domain is the registry, such as docker.io or localhost:5000.
	Domain string
	// Path is the repository within it, such as library/alpine.
	Path   string
	Tag    string
	Digest string
}

// parseReference parses and normalizes an image reference. With neither a
// tag nor a digest, the tag is latest. With both, the digest wins, as with
// docker pull.
//
// It peels the parts off from the end: the digest after an @, then a tag
// after the last colon, then the registry from the front, and checks what
// is left is a valid repository path. invalid is a small helper, kept in a
// variable, that makes each error message the same way.
func parseReference(text string) (imageReference, error) {
	var ref imageReference
	invalid := func(why string) (imageReference, error) {
		return imageReference{}, fmt.Errorf("invalid image reference %q: %s", text, why)
	}
	name := text
	if before, digest, ok := strings.Cut(name, "@"); ok {
		algorithm, encoded, _ := strings.Cut(digest, ":")
		if algorithm != "sha256" || !sha256Hex.MatchString(encoded) {
			return invalid("the digest must be sha256: and 64 lower-case hex digits")
		}
		name, ref.Digest = before, digest
	}
	// A tag follows the last colon, unless that colon is part of the
	// registry's port.
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		tag := name[colon+1:]
		if !referenceTag.MatchString(tag) {
			return invalid("bad tag")
		}
		name = name[:colon]
		if ref.Digest == "" {
			ref.Tag = tag
		}
	}
	if ref.Digest == "" && ref.Tag == "" {
		ref.Tag = "latest"
	}

	ref.Domain, ref.Path = dockerHubDomain, name
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref.Domain, ref.Path = first, rest
		if !referenceDomain.MatchString(ref.Domain) {
			return invalid("bad registry name")
		}
	}
	if ref.Domain == "index.docker.io" {
		ref.Domain = dockerHubDomain
	}
	if ref.Domain == dockerHubDomain && !strings.Contains(ref.Path, "/") {
		ref.Path = "library/" + ref.Path
	}
	if ref.Path == "" || len(ref.Domain)+1+len(ref.Path) > 255 {
		return invalid("bad repository name")
	}
	// SplitSeq yields the parts one at a time to the loop, rather than
	// making a slice of them all first.
	for component := range strings.SplitSeq(ref.Path, "/") {
		if !referencePathComponent.MatchString(component) {
			return invalid("repository names are lower-case letters, digits, and separators (. _ __ -)")
		}
	}
	return ref, nil
}

// Repository is the full repository name, such as docker.io/library/alpine.
func (ref imageReference) Repository() string {
	return ref.Domain + "/" + ref.Path
}

// String is the full reference, such as docker.io/library/alpine:3.20.
func (ref imageReference) String() string {
	if ref.Digest != "" {
		return ref.Repository() + "@" + ref.Digest
	}
	return ref.Repository() + ":" + ref.Tag
}

// familiarRepository is the repository as Docker shows it, without
// docker.io/ and library/: alpine, someuser/someimage, or
// localhost:5000/someimage.
func (ref imageReference) familiarRepository() string {
	if ref.Domain != dockerHubDomain {
		return ref.Repository()
	}
	return strings.TrimPrefix(ref.Path, "library/")
}

// familiar is the reference as Docker shows it, such as alpine:3.20.
func (ref imageReference) familiar() string {
	if ref.Digest != "" {
		return ref.familiarRepository() + "@" + ref.Digest
	}
	return ref.familiarRepository() + ":" + ref.Tag
}

// registryHost is where the registry's API is served.
func (ref imageReference) registryHost() string {
	if ref.Domain == dockerHubDomain {
		return dockerHubRegistry
	}
	return ref.Domain
}

// manifestRef is what to ask the registry for: the digest if there is one,
// or else the tag.
func (ref imageReference) manifestRef() string {
	if ref.Digest != "" {
		return ref.Digest
	}
	return ref.Tag
}
