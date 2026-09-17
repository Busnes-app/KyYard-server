package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// operationBudget bounds one container operation. A stop waits for the container to go, so it
// is the slowest of the three, and Docker's own default grace period is ten seconds.
const operationBudget = 30 * time.Second

// Operate runs one lifecycle action against one container, after checking that the container
// is still what the operator was looking at. Docker has no universal resource version, so the
// precondition is operation-specific identity read immediately before acting: the same image
// digest, the same state. A precondition that no longer holds is a refusal, not a failure,
// because nothing was attempted.
func (c *Client) Operate(ctx context.Context, cmd protocol.Command) (outcome, detail string) {
	// Image actions name a reference, not a container, and have their own budgets.
	switch cmd.Action {
	case protocol.ActionImagePull:
		return c.pullImage(ctx, cmd.Reference)
	case protocol.ActionImageRemove:
		return c.removeImage(ctx, cmd.Reference)
	}
	ctx, cancel := context.WithTimeout(ctx, operationBudget)
	defer cancel()

	// Escaped at every use: the identifier comes from a request body, and a value carrying a
	// slash or a query would otherwise choose which Engine API route the agent calls rather
	// than which container it acts on. The server constrains the grammar as well; neither
	// check is allowed to be the only one.
	container := url.PathEscape(cmd.Container)
	var inspected struct {
		Image string `json:"Image"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := c.get(ctx, "/containers/"+container+"/json", &inspected); err != nil {
		if strings.Contains(err.Error(), "404") {
			return protocol.OutcomeDenied, "the container no longer exists"
		}
		return protocol.OutcomeFailed, bound("inspecting the container: "+err.Error(), protocol.MaxResultDetailBytes)
	}
	if want := cmd.Expects.ImageDigest; want != "" && want != inspected.Image {
		return protocol.OutcomeDenied, "the container is running a different image than the one this was decided about"
	}
	if want := cmd.Expects.State; want != "" && want != inspected.State.Status {
		return protocol.OutcomeDenied, fmt.Sprintf("the container is %s, not %s", inspected.State.Status, want)
	}

	if cmd.Action == protocol.ActionRemove {
		return c.remove(ctx, container, inspected.State.Status)
	}
	var path string
	switch cmd.Action {
	case protocol.ActionStart:
		path = "/containers/" + container + "/start"
	case protocol.ActionStop:
		path = "/containers/" + container + "/stop"
	case protocol.ActionRestart:
		path = "/containers/" + container + "/restart"
	default:
		return protocol.OutcomeDenied, "unsupported action"
	}
	status, err := c.post(ctx, path)
	switch {
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the runtime did not answer in time"
	case err != nil:
		return protocol.OutcomeFailed, bound(err.Error(), protocol.MaxResultDetailBytes)
	case status == http.StatusNotModified:
		// Already in the state asked for. Nothing happened and nothing needed to.
		return protocol.OutcomeSucceeded, "the container was already in that state"
	case status >= 400:
		return protocol.OutcomeFailed, fmt.Sprintf("the runtime refused with status %d", status)
	}
	return protocol.OutcomeSucceeded, ""
}

// post sends an action request and returns the status, which carries meaning of its own: 304
// means the container was already where the caller wanted it.
func (c *Client) post(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// remove destroys a container, and refuses to do it to a running one. Docker would oblige with
// force, which kills the process first; an operator who means that can stop it and say so, and
// the difference is worth a second decision rather than a flag.
//
// Named volumes are left alone: this never passes v=1, so removing a container does not remove
// the data it was using. A volume is destroyed by its own action, with its own confirmation.
func (c *Client) remove(ctx context.Context, container, state string) (outcome, detail string) {
	if state == "running" || state == "restarting" || state == "paused" {
		return protocol.OutcomeDenied, fmt.Sprintf("the container is %s; stop it first", state)
	}
	status, err := c.del(ctx, "/containers/"+container)
	switch {
	case err != nil && ctx.Err() != nil:
		return protocol.OutcomeTimedOut, "the runtime did not answer in time"
	case err != nil:
		return protocol.OutcomeFailed, bound(err.Error(), protocol.MaxResultDetailBytes)
	case status == http.StatusConflict:
		// Docker says conflict when something still depends on it.
		return protocol.OutcomeDenied, "the runtime refused: something still depends on this container"
	case status == http.StatusNotFound:
		return protocol.OutcomeDenied, "the container no longer exists"
	case status >= 400:
		return protocol.OutcomeFailed, fmt.Sprintf("the runtime refused with status %d", status)
	}
	return protocol.OutcomeSucceeded, ""
}

func (c *Client) del(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// postBody sends an action request and returns the status and body. A pull reports late
// failures inside a 200, so the body is part of the answer rather than something to discard.
func (c *Client) postBody(ctx context.Context, path string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), err
}
