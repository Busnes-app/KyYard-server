package docker

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// ensureVolumes records a volume step for each frame volume s mounts that no earlier service
// did. Docker answers 201 for a name that already exists, so the step is idempotent and an
// existing volume keeps its own labels and data.
func (r *deployRun) ensureVolumes(ctx context.Context, s protocol.DeploymentService) {
	for _, m := range s.Mounts {
		if m.Kind != protocol.MountVolume || r.ensured[m.Source] || !slices.Contains(r.req.Volumes, m.Source) {
			continue
		}
		r.ensured[m.Source] = true
		r.step(s.Name, protocol.StepVolume, func() (string, string) {
			cctx, cancel := context.WithTimeout(ctx, callBudget)
			defer cancel()
			// Compose's labels, so `docker compose` recognises the volume as the project's own.
			short := m.Source
			if rest, ok := strings.CutPrefix(m.Source, r.req.Project+"_"); ok && rest != "" {
				short = rest
			}
			body := struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}{m.Source, map[string]string{"com.docker.compose.project": r.req.Project, "com.docker.compose.volume": short}}
			var out struct{}
			status, err := r.c.postJSON(cctx, "/volumes/create", body, &out)
			switch {
			case err == nil && status == http.StatusCreated:
				return protocol.OutcomeSucceeded, ""
			case err == nil:
				return protocol.OutcomeFailed, "volume create failed"
			}
			return r.outcomeFor(cctx, err, status)
		})
	}
}
