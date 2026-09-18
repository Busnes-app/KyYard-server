package protocol

import (
	"regexp"
	"strings"
)

// MaxImageReferenceBytes bounds a reference. Docker's own limit is far larger, but a reference
// becomes part of a URL addressed to the host's root-equivalent socket, and nothing legitimate
// comes close to this.
const MaxImageReferenceBytes = 512

// A reference is host and path components separated by slashes, with an optional tag or
// digest. Each component starts and ends alphanumeric with single separators between
// alphanumerics, which is Docker's rule and which rejects "..", "." and empty components by
// construction -- the segments that would otherwise let a reference choose a different Engine
// API route rather than a different image.
const imageComponent = `[a-zA-Z0-9]+(?:[._-][a-zA-Z0-9]+)*`

var (
	imageReference = regexp.MustCompile(`^` + imageComponent + `(?::[0-9]+)?(?:/` + imageComponent + `)*(?::[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}|@sha256:[a-f0-9]{64})?$`)
	// An image ID as the daemon reports it, which is also a thing a removal may name.
	imageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// IsImageAction reports whether an action names an image rather than a container.
func IsImageAction(action string) bool {
	return action == ActionImagePull || action == ActionImageRemove
}

// SplitImageReference separates a reference into the image name and the tag or digest that
// pins it to one image. An empty tag means the reference named neither, which is not a thing
// this product sends to a runtime: the Engine API reads an empty tag as "every tag in the
// repository", so "pull nginx" would fetch the whole repository onto the host.
func SplitImageReference(s string) (name, tag string) {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		return s[:at], s[at+1:]
	}
	// A colon before the last slash is a registry port, not a tag.
	if colon := strings.LastIndex(s, ":"); colon > strings.LastIndex(s, "/") {
		return s[:colon], s[colon+1:]
	}
	return s, ""
}

// ValidImageReference reports whether s can name an image. Both the control plane and the
// agent check it: the agent is the last thing between a request body and the socket, and the
// repository's rule is that neither check may be the only one.
func ValidImageReference(s string) bool {
	if len(s) == 0 || len(s) > MaxImageReferenceBytes {
		return false
	}
	return imageReference.MatchString(s) || imageID.MatchString(s)
}
