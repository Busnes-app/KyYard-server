package docker

import (
	"context"
	"fmt"
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
	status, body, err := c.postBody(ctx, "/images/create?"+q.Encode())
	switch {
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the pull did not finish in time"
	case err != nil:
		return protocol.OutcomeFailed, bound(err.Error(), protocol.MaxResultDetailBytes)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return protocol.OutcomeDenied, "this registry needs a credential, and registry credentials are not implemented yet"
	case status == http.StatusNotFound:
		return protocol.OutcomeDenied, "the registry has no such image or tag"
	case status >= 400:
		return protocol.OutcomeFailed, fmt.Sprintf("the registry or runtime refused with status %d", status)
	}
	// The Engine streams progress as JSON lines and reports a late failure in the body with a
	// 200 already sent, so the body is what says whether the pull actually worked.
	if line := lastErrorLine(body); line != "" {
		if strings.Contains(strings.ToLower(line), "unauthorized") || strings.Contains(strings.ToLower(line), "denied") {
			return protocol.OutcomeDenied, "this registry needs a credential, and registry credentials are not implemented yet"
		}
		return protocol.OutcomeFailed, bound(line, protocol.MaxResultDetailBytes)
	}
	return protocol.OutcomeSucceeded, ""
}

// removeImage deletes a reference from this host. An image a container still uses is refused
// rather than forced: Docker would oblige with force and leave containers pointing at nothing.
func (c *Client) removeImage(ctx context.Context, reference string) (outcome, detail string) {
	ctx, cancel := context.WithTimeout(ctx, operationBudget)
	defer cancel()
	status, err := c.del(ctx, "/images/"+url.PathEscape(reference))
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

// lastErrorLine finds the failure the Engine reported inside a 200 response. The stream is one
// JSON object per line; anything carrying an "error" field is the reason the pull did not work.
func lastErrorLine(body string) string {
	var found string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `"error"`) {
			found = strings.TrimSpace(line)
		}
	}
	return found
}
