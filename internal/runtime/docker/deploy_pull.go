package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// pullPhase is how long all of a frame's pulls may take together, counted from when the first
// could start: at most 80% of what remains, and always one replaceBudget short of the deadline.
// Pulls run in order against one shared deadline; each replacement keeps its own time check.
func pullPhase(remaining time.Duration) time.Duration {
	return min(remaining-replaceBudget, remaining*4/5)
}

// pull fetches s.Pull by digest with the frame's credential for its host, proves the image the
// daemon now holds is that repository at that digest, and tags it with s.Pull.Tag so the
// host's tag follows the update. It returns the image ID the replacement is created from.
// Details are fixed: daemon text can echo the registry's answer.
func (r *deployRun) pull(ctx context.Context, s protocol.DeploymentService) (outcome, detail, imageID string) {
	if time.Until(r.pullDeadline) < callBudget {
		return protocol.OutcomeTimedOut, "not enough time left before the deadline to pull and replace safely", ""
	}
	pctx, cancel := context.WithDeadline(ctx, r.pullDeadline)
	defer cancel()
	name, digest, _ := strings.Cut(s.Pull.Reference, "@")
	q := url.Values{"fromImage": {name}, "tag": {digest}}
	req, err := http.NewRequestWithContext(pctx, http.MethodPost, r.c.base+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return protocol.OutcomeFailed, "pull failed", ""
	}
	if auth, ok := r.req.Registries[s.Pull.Host()]; ok {
		raw, _ := json.Marshal(struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Server   string `json:"serveraddress"`
		}{auth.Username, auth.Secret, s.Pull.Host()})
		// URL-safe: the daemon decodes with base64.URLEncoding and pulls anonymously on failure.
		req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(raw))
	}
	resp, err := r.c.http.Do(req)
	if err != nil {
		o, d := r.outcomeFor(pctx, err, 0)
		return o, d, ""
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return protocol.OutcomeFailed, "unauthorized", ""
	case resp.StatusCode == http.StatusNotFound:
		return protocol.OutcomeFailed, "not found", ""
	case resp.StatusCode != http.StatusOK:
		return protocol.OutcomeFailed, "pull failed", ""
	}
	// A pull reports late failures as an error line inside the 200.
	failure, err := scanPullStream(resp.Body)
	if err != nil {
		o, d := r.outcomeFor(pctx, err, 0)
		return o, d, ""
	}
	if failure != "" {
		return protocol.OutcomeFailed, "pull failed", ""
	}
	var im struct {
		ID          string `json:"Id"`
		RepoDigests []string
	}
	if err := r.c.get(pctx, "/images/"+url.PathEscape(s.Pull.Reference)+"/json", &im); err != nil {
		o, d := r.outcomeFor(pctx, err, statusOf(err))
		return o, d, ""
	}
	if len(im.ID) == 71 && strings.HasPrefix(im.ID, "sha256:") && protocol.ValidImageReference(im.ID) {
		if slices.ContainsFunc(im.RepoDigests, func(rd string) bool {
			n, d, ok := strings.Cut(rd, "@")
			return ok && d == digest && canonicalRepository(n) == canonicalRepository(name)
		}) {
			return r.tag(pctx, s.Pull.Tag, im.ID)
		}
	}
	return protocol.OutcomeFailed, "pulled image does not match", ""
}

// tag points tagRef at the verified pulled image; the Engine answers 201.
func (r *deployRun) tag(ctx context.Context, tagRef, id string) (outcome, detail, imageID string) {
	if tagRef == "" {
		return protocol.OutcomeSucceeded, "", id
	}
	repo, tag := protocol.SplitImageReference(tagRef)
	status, err := r.c.post(ctx, "/images/"+url.PathEscape(id)+"/tag?repo="+url.QueryEscape(repo)+"&tag="+url.QueryEscape(tag))
	if err != nil {
		o, d := r.outcomeFor(ctx, err, 0)
		return o, d, ""
	}
	if status != http.StatusCreated {
		return protocol.OutcomeFailed, "tag failed", ""
	}
	return protocol.OutcomeSucceeded, "", id
}

// canonicalRepository spells a Docker Hub repository the way the daemon reports it.
func canonicalRepository(name string) string {
	for _, host := range []string{"docker.io/", "index.docker.io/"} {
		if rest, ok := strings.CutPrefix(name, host); ok {
			return strings.TrimPrefix(rest, "library/")
		}
	}
	return strings.TrimPrefix(name, "library/")
}
