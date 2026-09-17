package protocol

import "regexp"

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

// ValidImageReference reports whether s can name an image. Both the control plane and the
// agent check it: the agent is the last thing between a request body and the socket, and the
// repository's rule is that neither check may be the only one.
func ValidImageReference(s string) bool {
	if len(s) == 0 || len(s) > MaxImageReferenceBytes {
		return false
	}
	return imageReference.MatchString(s) || imageID.MatchString(s)
}
