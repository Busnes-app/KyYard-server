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
// No credential is sent. Registry credentials are M7a, and until they exist a private registry
// answers with an authorization failure, which is reported as a refusal naming the reason
// rather than as a generic failure an operator would have to guess at.
func (c *Client) pullImage(ctx context.Context, reference string) (outcome, detail string) {
	ctx, cancel := context.WithTimeout(ctx, pullBudget)
	defer cancel()
	q := url.Values{"fromImage": {reference}}
	status, body, err := c.stream(ctx, "/images/create?"+q.Encode())
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
// operator should not have to work out that two different messages mean the same missing
// feature.
const credentialsMissing = "this registry needs a credential, and registry credentials are not implemented yet"

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
