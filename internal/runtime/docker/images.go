package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// pullBudget is generous because a pull is bounded by the network and the image, not by the
// daemon. It is still bounded: a command an operator is waiting on must end.
const pullBudget = 10 * time.Minute

// pullImage fetches a reference onto this host. The reference travels as a query parameter, so
// it is escaped rather than concatenated, exactly as a container identifier is.
//
// No credential is sent: credentials travel only with a deployment, which pulls by digest. A
// private registry's authorization failure is reported as a refusal naming that route rather
// than as a generic failure an operator would have to guess at.
func (c *Client) pullImage(ctx context.Context, reference string) (outcome, detail string) {
	if !protocol.ValidImageReference(reference) {
		return protocol.OutcomeDenied, "that is not an image reference"
	}
	ctx, cancel := context.WithTimeout(ctx, pullBudget)
	defer cancel()
	// The tag is always sent. The Engine reads an empty tag as every tag in the repository, so
	// omitting it would turn "pull nginx" into fetching the whole repository onto the host --
	// the obvious thing to type doing the worst thing. A reference that names none gets the
	// same default `docker pull` uses.
	name, tag := protocol.SplitImageReference(reference)
	if tag == "" {
		tag = "latest"
	}
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	status, body, err := c.stream(ctx, http.MethodPost, "/images/create?"+q.Encode())
	if body != nil {
		defer body.Close()
	}
	switch {
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the pull did not finish in time"
	case err != nil:
		return protocol.OutcomeFailed, bound(err.Error(), protocol.MaxResultDetailBytes)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return protocol.OutcomeDenied, credentialsMissing
	case status == http.StatusNotFound:
		return protocol.OutcomeDenied, "the registry has no such image or tag"
	case status >= 400:
		return protocol.OutcomeFailed, fmt.Sprintf("the registry or runtime refused with status %d", status)
	}
	return readPullStream(body)
}

// credentialsMissing is the same sentence wherever the registry asks for one, because an
// operator should not have to work out that two different messages mean the same thing.
const credentialsMissing = "this registry needs a credential; pull it through a deployment, which carries the organization's registry credential"

// readPullStream decides what a pull did from the progress the Engine streams under a 200. It
// reads to the end rather than buffering a slice of it: a stream cut short at a byte ceiling
// would hide the trailing error line, and reporting success because the failure did not fit is
// the worst direction to fail for the operation people use to apply a patched image.
func readPullStream(body io.Reader) (outcome, detail string) {
	if body == nil {
		return protocol.OutcomeUnknown, "the runtime returned no progress to read"
	}
	scanner := bufio.NewScanner(body)
	// One line per layer per update; a single line far past this is not progress we can read.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var failure string
	for scanner.Scan() {
		var event struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		// Decoded rather than searched for a substring: a progress line that merely mentions
		// the word would otherwise read as a failure.
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Error != "" {
			failure = event.Error
			if event.ErrorDetail.Message != "" {
				failure = event.ErrorDetail.Message
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// The stream ended badly, so what the pull did is genuinely unknown. Saying so is the
		// honest answer; saying "succeeded" would be a guess in the dangerous direction.
		return protocol.OutcomeUnknown, bound("the progress stream ended early: "+err.Error(), protocol.MaxResultDetailBytes)
	}
	if failure != "" {
		if lower := strings.ToLower(failure); strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication required") || strings.Contains(lower, "denied") {
			return protocol.OutcomeDenied, credentialsMissing
		}
		return protocol.OutcomeFailed, bound(failure, protocol.MaxResultDetailBytes)
	}
	return protocol.OutcomeSucceeded, ""
}

// removeImage deletes a reference from this host. An image a container still uses is refused
// rather than forced: Docker would oblige with force and leave containers pointing at nothing.
// The expected image ID is checked immediately before the delete, for the same reason a
// container action re-reads its state: a tag is a label the host reassigns, and a pull between
// the decision and the act would otherwise destroy something nobody looked at.
func (c *Client) removeImage(ctx context.Context, reference, wantID string) (outcome, detail string) {
	// Checked here as well as at the control plane: this reference becomes a path segment on a
	// root-equivalent socket, and PathEscape must not be the only thing standing in its way.
	if !protocol.ValidImageReference(reference) {
		return protocol.OutcomeDenied, "that is not an image reference"
	}
	ctx, cancel := context.WithTimeout(ctx, operationBudget)
	defer cancel()
	escaped := url.PathEscape(reference)
	if wantID == "" {
		// The control plane always pins the image a removal was decided about. A removal that
		// arrives without one was not built by that path, and the agent is the last checkpoint
		// before a root-equivalent socket.
		return protocol.OutcomeDenied, "this removal did not say which image it was decided about"
	}
	var inspected struct {
		ID string `json:"Id"`
	}
	if err := c.get(ctx, "/images/"+escaped+"/json", &inspected); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return protocol.OutcomeDenied, "this host does not have that image"
		}
		return protocol.OutcomeFailed, bound("inspecting the image: "+err.Error(), protocol.MaxResultDetailBytes)
	}
	if inspected.ID != wantID {
		return protocol.OutcomeDenied, "this reference now points at a different image than the one this was decided about"
	}
	status, err := c.del(ctx, "/images/"+escaped)
	switch {
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the runtime did not answer in time"
	case err != nil:
		return protocol.OutcomeFailed, bound(err.Error(), protocol.MaxResultDetailBytes)
	case status == http.StatusConflict:
		return protocol.OutcomeDenied, "a container is still using this image"
	case status == http.StatusNotFound:
		return protocol.OutcomeDenied, "this host does not have that image"
	case status >= 400:
		return protocol.OutcomeFailed, fmt.Sprintf("the runtime refused with status %d", status)
	}
	return protocol.OutcomeSucceeded, ""
}

// ContainerImageDigests reads the immutable image ID of a container, then that
// image's pullable repository digests. Tags are never resolved during discovery.
func (c *Client) ContainerImageDigests(ctx context.Context, container string) ([]string, error) {
	// Discovery uses a short-lived client; release its pooled daemon connection.
	defer c.http.CloseIdleConnections()
	if !protocol.ValidContainerID(container) {
		return nil, fmt.Errorf("invalid container identifier")
	}
	var inspected struct{ Image string }
	if err := c.get(ctx, "/containers/"+url.PathEscape(container)+"/json", &inspected); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(inspected.Image, "sha256:") || len(inspected.Image) != 71 || !protocol.ValidImageReference(inspected.Image) {
		return nil, fmt.Errorf("container has no immutable image ID")
	}
	var image struct{ RepoDigests []string }
	if err := c.get(ctx, "/images/"+url.PathEscape(inspected.Image)+"/json", &image); err != nil {
		return nil, err
	}
	return image.RepoDigests, nil
}
