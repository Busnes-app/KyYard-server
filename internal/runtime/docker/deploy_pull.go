package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// pullWindow is the time one pull (and its inspect) may take: what remains before the deadline
// once every service's replacement is reserved, since all pulls run before any replacement.
// ok is false when less than callBudget would remain, and nothing is pulled for a replacement
// that could not fit.
func pullWindow(remaining time.Duration, services int) (window time.Duration, ok bool) {
	window = remaining - time.Duration(services)*replaceBudget
	return window, window >= callBudget
}

// pull fetches s.Pull by digest with the frame's credential for its host, then proves the
// image the daemon now holds is that repository at that digest. It returns the image ID the
// replacement is created from. Details are fixed: daemon text can echo the registry's answer.
func (r *deployRun) pull(ctx context.Context, s protocol.DeploymentService) (outcome, detail, imageID string) {
	window, ok := pullWindow(time.Until(r.req.Deadline), len(r.req.Services))
	if !ok {
		return protocol.OutcomeTimedOut, "not enough time left before the deadline to pull and replace safely", ""
	}
	pctx, cancel := context.WithTimeout(ctx, window)
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
		for _, rd := range im.RepoDigests {
			if n, d, ok := strings.Cut(rd, "@"); ok && d == digest && canonicalRepository(n) == canonicalRepository(name) {
				return protocol.OutcomeSucceeded, "", im.ID
			}
		}
	}
	return protocol.OutcomeFailed, "pulled image does not match", ""
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
