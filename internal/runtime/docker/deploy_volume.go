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

func mountsVolume(mounts []inspectedMount, name string) bool {
	return slices.ContainsFunc(mounts, func(o inspectedMount) bool { return o.Type == "volume" && o.Name == name })
}

// mountedBefore reports whether the old container of any frame service mounting name already
// mounts it: first's from its precondition, the others read here. A read that fails is false.
// The others' identity is checked by their own precondition before any container is touched.
func (r *deployRun) mountedBefore(ctx context.Context, name, first string, old []inspectedMount) bool {
	if mountsVolume(old, name) {
		return true
	}
	for _, s := range r.req.Services {
		if s.Name == first || !slices.ContainsFunc(s.Mounts, func(m protocol.Mount) bool { return m.Kind == protocol.MountVolume && m.Source == name }) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, callBudget)
		var in struct{ Mounts []inspectedMount }
		err := r.c.get(cctx, "/containers/"+url.PathEscape(s.Replaces.ContainerID)+"/json", &in)
		cancel()
		if err == nil && mountsVolume(in.Mounts, name) {
			return true
		}
	}
	return false
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
// Compose's labels. An existing one is mounted only when it is owned by the project or the old
// container of some service mounting it already does; anything else would let a revision reach
// another application's data or a host path.
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
			case err == nil && (v.owned(r.req.Project) || r.mountedBefore(ctx, m.Source, s.Name, old)):
				return protocol.OutcomeSucceeded, ""
			case err == nil:
				return protocol.OutcomeDenied, notOwned
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
