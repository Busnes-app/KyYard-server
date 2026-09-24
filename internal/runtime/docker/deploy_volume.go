package docker

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const notOwned = "volume is not owned by this project"

// dockerVolume is what the Engine reports for a volume, inspected or created.
type dockerVolume struct {
	Driver  string
	Labels  map[string]string
	Options map[string]string
}

// owned is this project's plain local volume: Compose's project label, no driver options (a
// local volume with device options is a host path by another name).
func (v dockerVolume) owned(project string) bool {
	return v.Labels["com.docker.compose.project"] == project && v.Driver == "local" && len(v.Options) == 0
}

// ensureVolumes records a volume step for each frame volume s mounts that no earlier service
// did. An absent volume is created with Compose's labels. An existing one is mounted only when
// s's old container already mounts it or it is owned by the project; anything else would let a
// revision reach another application's data or a host path.
func (r *deployRun) ensureVolumes(ctx context.Context, s protocol.DeploymentService, old []inspectedMount) {
	for _, m := range s.Mounts {
		if m.Kind != protocol.MountVolume || r.ensured[m.Source] || !slices.Contains(r.req.Volumes, m.Source) {
			continue
		}
		r.ensured[m.Source] = true
		r.step(s.Name, protocol.StepVolume, func() (string, string) {
			cctx, cancel := context.WithTimeout(ctx, callBudget)
			defer cancel()
			var v dockerVolume
			err := r.c.get(cctx, "/volumes/"+url.PathEscape(m.Source), &v)
			switch {
			case err == nil && (v.owned(r.req.Project) || slices.ContainsFunc(old, func(o inspectedMount) bool { return o.Type == "volume" && o.Name == m.Source })):
				return protocol.OutcomeSucceeded, ""
			case err == nil:
				return protocol.OutcomeDenied, notOwned
			case statusOf(err) != http.StatusNotFound:
				return r.outcomeFor(cctx, err, statusOf(err))
			}
			short := m.Source
			if rest, ok := strings.CutPrefix(m.Source, r.req.Project+"_"); ok && rest != "" {
				short = rest
			}
			body := struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}{m.Source, map[string]string{"com.docker.compose.project": r.req.Project, "com.docker.compose.volume": short}}
			status, err := r.c.postJSON(cctx, "/volumes/create", body, &v)
			switch {
			case err != nil:
				return r.outcomeFor(cctx, err, status)
			case status != http.StatusCreated:
				return protocol.OutcomeFailed, "volume create failed"
			case !v.owned(r.req.Project):
				// Docker answers 201 with the existing volume when the name was taken meanwhile.
				return protocol.OutcomeDenied, notOwned
			}
			return protocol.OutcomeSucceeded, ""
		})
	}
}
