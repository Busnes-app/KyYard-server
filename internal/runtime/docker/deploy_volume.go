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

const notPresent = "volume mount not present on the container"

func mountsVolume(mounts []inspectedMount, name string) bool {
	return slices.ContainsFunc(mounts, func(o inspectedMount) bool { return o.Type == "volume" && o.Name == name })
}

// keeps reports that old already has m exactly: the same volume at the same target, with the
// same write access. A volume the project does not own may only be kept where it is.
func keeps(old []inspectedMount, m protocol.Mount) bool {
	return slices.ContainsFunc(old, func(o inspectedMount) bool {
		return o.Type == "volume" && o.Name == m.Source && o.Destination == m.Target && o.RW == !m.ReadOnly
	})
}

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

// ensureVolumes records a volume step for each volume s mounts that no earlier service did
// (Validate guarantees every mounted volume is in Volumes). An absent volume is created with
// Compose's labels. An existing one the project owns may be mounted anywhere. Any other is
// keep-only, like a bind: s's old container must already mount it at each frame target with the
// same write access, and every later service's precondition checks its own mounts of it (a
// `local` volume with device options is a host path by another name). Anything else would let a
// revision reach another application's data or a host path, or gain write access to it.
func (r *deployRun) ensureVolumes(ctx context.Context, s protocol.DeploymentService, old []inspectedMount) {
	for _, m := range s.Mounts {
		if m.Kind != protocol.MountVolume || r.ensured[m.Source] {
			continue
		}
		r.ensured[m.Source] = true
		r.step(s.Name, protocol.StepVolume, func() (string, string) {
			cctx, cancel := context.WithTimeout(ctx, callBudget)
			defer cancel()
			var v dockerVolume
			err := r.c.get(cctx, "/volumes/"+url.PathEscape(m.Source), &v)
			switch {
			case err == nil && v.owned(r.req.Project):
				return protocol.OutcomeSucceeded, ""
			case err == nil && !mountsVolume(old, m.Source):
				return protocol.OutcomeDenied, notOwned
			case err == nil:
				for _, sm := range s.Mounts {
					if sm.Kind == protocol.MountVolume && sm.Source == m.Source && !keeps(old, sm) {
						return protocol.OutcomeDenied, notPresent
					}
				}
				r.keepOnly[m.Source] = true
				return protocol.OutcomeSucceeded, ""
			case statusOf(err) != http.StatusNotFound:
				return r.outcomeFor(cctx, err, statusOf(err))
			}
			// Only the project's own volumes are created; an external one must already exist.
			short, ok := strings.CutPrefix(m.Source, r.req.Project+"_")
			if !ok || short == "" {
				return protocol.OutcomeDenied, "volume does not exist"
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
