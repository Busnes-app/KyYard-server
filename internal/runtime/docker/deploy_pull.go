package docker

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// maxPullStreamBytes bounds one progress line; the stream itself is read to its end.
const maxPullStreamBytes = 1 << 20

var errPullStream = errors.New("the pull progress stream ended early")

// pull fetches s.Pull by digest with the frame's credential for its host, then proves the
// image the daemon now holds is that repository at that digest. It returns the image ID the
// replacement is created from. Details are fixed: daemon text can echo the registry's answer.
func (r *deployRun) pull(ctx context.Context, s protocol.DeploymentService) (outcome, detail, imageID string) {
	name, digest, _ := strings.Cut(s.Pull.Reference, "@")
	// The replacement's own budget is kept back; a slow registry never eats it.
	budget := max(time.Until(r.req.Deadline)-replaceBudget, callBudget)
	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
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
		req.Header.Set("X-Registry-Auth", base64.StdEncoding.EncodeToString(raw))
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
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxPullStreamBytes)
	for scanner.Scan() {
		var line struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Error != nil {
			return protocol.OutcomeFailed, "pull failed", ""
		}
	}
	if err := scanner.Err(); err != nil {
		o, d := r.outcomeFor(pctx, errPullStream, 0)
		return o, d, ""
	}
	ictx, icancel := context.WithTimeout(ctx, callBudget)
	defer icancel()
	var im struct {
		ID          string `json:"Id"`
		RepoDigests []string
	}
	if err := r.c.get(ictx, "/images/"+url.PathEscape(s.Pull.Reference)+"/json", &im); err != nil {
		o, d := r.outcomeFor(ictx, err, statusOf(err))
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
