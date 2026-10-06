package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

type containerCheck struct {
	Name         string `json:"name"`
	Reference    string `json:"reference"`
	LocalDigest  string `json:"local_digest"`
	RemoteDigest string `json:"remote_digest"`
	Verdict      string `json:"verdict"`
	Detail       string `json:"detail"`
}

// podImage is one container as the workload's pods agree on it; agreed is false when they don't.
type podImage struct {
	name, image, imageID string
	agreed               bool
}

// workloadPodImages finds ref in the snapshot and reads its containers from its pods, in first
// seen order. Pods that disagree on a container's image or image ID mark it not agreed.
func workloadPodImages(snap protocol.Snapshot, ref protocol.WorkloadRef) (found, managed bool, images []podImage) {
	if snap.Kubernetes == nil {
		return false, false, nil
	}
	i := slices.IndexFunc(snap.Kubernetes.Workloads, func(w protocol.Workload) bool {
		return w.Namespace == ref.Namespace && w.Name == ref.Name && strings.EqualFold(w.Kind, ref.Kind)
	})
	if i < 0 {
		return false, false, nil
	}
	w := snap.Kubernetes.Workloads[i]
	if w.Application != "" || w.Instance != "" {
		return true, true, nil
	}
	index := map[string]int{}
	for _, p := range snap.Kubernetes.Pods {
		if p.Namespace != w.Namespace || p.OwnerKind != w.Kind || p.OwnerName != w.Name {
			continue
		}
		for _, c := range p.Containers {
			j, seen := index[c.Name]
			if !seen {
				index[c.Name] = len(images)
				images = append(images, podImage{c.Name, c.Image, c.ImageID, true})
				continue
			}
			if images[j].image != c.Image || images[j].imageID != c.ImageID {
				images[j].agreed = false
			}
		}
	}
	return true, false, images
}

// handleWorkloadUpdateCheck is the workload twin of handleContainerUpdateCheck: an explicit
// registry observation under the same gates, never an update approval. Containers are read from
// the stored inventory's pods; each tracked tag is Headed once.
func (s *Server) handleWorkloadUpdateCheck(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if a.ServiceTokenID != "" {
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerConfigure, "bearer")
		s.tenantError(w, store.ErrForbidden)
		return
	}
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	ref, err := workloadRef(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.allowAttempt("image-check:"+a.Principal(), 12, time.Minute) {
		s.writeError(w, 429, "Too many update checks")
		return
	}
	if err := s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerConfigure, endpoint); err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, endpoint, kubernetesRoute) {
		return
	}
	read := func() (bool, bool, []podImage, error) {
		inv, err := s.store.Tenancy().ReadInventory(r.Context(), a, endpoint)
		if err != nil {
			return false, false, nil, err
		}
		var snap protocol.Snapshot
		if err := json.Unmarshal(inv.Snapshot, &snap); err != nil {
			return false, false, nil, err
		}
		found, managed, images := workloadPodImages(snap, ref)
		return found, managed, images, nil
	}
	found, managed, images, err := read()
	if err == nil && !found {
		err = store.ErrNotFound
	}
	if err != nil {
		s.tenantError(w, err)
		return
	}
	out := struct {
		Workload   string           `json:"workload"`
		Verdict    string           `json:"verdict"`
		Detail     string           `json:"detail"`
		CheckedAt  string           `json:"checked_at"`
		Containers []containerCheck `json:"containers"`
	}{Workload: ref.String(), CheckedAt: time.Now().UTC().Format(time.RFC3339), Containers: []containerCheck{}}
	if managed {
		out.Verdict = "managed"
		s.writeJSON(w, 200, out)
		return
	}
	heads := map[string]error{}
	remotes := map[string]string{}
	var ctx context.Context
	acquired := false
	for _, im := range images {
		c := containerCheck{Name: im.name, Reference: im.image, Verdict: "unknown"}
		name, tracked, pinned, ok := trackedReference(im.image)
		switch {
		case !im.agreed:
		case !ok && pinned != "":
			c.Verdict = "pinned"
		case !ok:
		default:
			c.LocalDigest = runningDigest(im.imageID, tracked)
			if _, done := heads[name]; !done {
				if !acquired {
					// Policy before budget, as the container check does.
					if _, err := s.store.Tenancy().ResolveRegistryAccess(r.Context(), a, permissions.ImagePull, name, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate); err != nil {
						s.tenantError(w, err)
						return
					}
					release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
					if !ok {
						return
					}
					defer release()
					extendRegistryDeadline(w)
					// One deadline for every Head, as the pin has.
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(r.Context(), store.ImageCheckDeadline)
					defer cancel()
					acquired = true
				}
				remote, err := s.headDigest(ctx, a, name, tracked)
				if err != nil && !errors.Is(err, errRegistryHead) && ctx.Err() == nil {
					s.tenantError(w, err)
					return
				}
				heads[name], remotes[name] = err, remote
			}
			if err := heads[name]; err != nil {
				c.Verdict, c.Detail = "registry_error", registryDetail(err)
				break
			}
			c.RemoteDigest = remotes[name]
			switch {
			case c.LocalDigest == "":
			case c.LocalDigest == c.RemoteDigest:
				c.Verdict = "up_to_date"
			default:
				c.Verdict = "update_available"
			}
		}
		out.Containers = append(out.Containers, c)
	}
	out.Verdict, out.Detail = workloadVerdict(out.Containers)
	// The pods may have rolled while the registry answered.
	_, _, again, err := read()
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !slices.Equal(again, images) || !s.configurationAllowed(r, a, endpoint) {
		s.tenantError(w, store.ErrAdoptionChanged)
		return
	}
	s.writeJSON(w, 200, out)
}

// workloadVerdict folds container verdicts: any update wins, then any registry failure (with
// its detail), then all current, then all pinned or current; anything else is unknown.
func workloadVerdict(cs []containerCheck) (string, string) {
	if len(cs) == 0 {
		return "unknown", ""
	}
	for _, c := range cs {
		if c.Verdict == "update_available" {
			return c.Verdict, ""
		}
	}
	for _, c := range cs {
		if c.Verdict == "registry_error" {
			return c.Verdict, c.Detail
		}
	}
	if !slices.ContainsFunc(cs, func(c containerCheck) bool { return c.Verdict != "up_to_date" }) {
		return "up_to_date", ""
	}
	if !slices.ContainsFunc(cs, func(c containerCheck) bool { return c.Verdict != "up_to_date" && c.Verdict != "pinned" }) {
		return "pinned", ""
	}
	return "unknown", ""
}
