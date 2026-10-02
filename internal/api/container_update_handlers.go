package api

import (
	"context"
	"encoding/json"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// handleContainerUpdateCheck is an explicit registry observation, never an update approval.
// It uses the existing credential/egress policy and global registry admission pool.
func (s *Server) handleContainerUpdateCheck(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if a.ServiceTokenID != "" {
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerConfigure, "bearer")
		s.tenantError(w, store.ErrForbidden)
		return
	}
	if !s.allowAttempt("image-check:"+a.Principal(), 12, time.Minute) {
		s.writeError(w, 429, "Too many update checks")
		return
	}
	if err = s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerConfigure, endpoint); err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
	target, err := s.store.Tenancy().ReadInspectionTarget(r.Context(), a, endpoint, r.PathValue("container"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	inv, err := s.store.Tenancy().ReadInventory(r.Context(), a, endpoint)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	// Inventory is encoded by the store; the normal bounded decoder preserves its caps.
	var snapshot protocol.Snapshot
	err = json.Unmarshal(inv.Snapshot, &snapshot)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	reference := ""
	for _, c := range snapshot.Containers {
		if c.ID == target.ContainerID {
			reference = c.Image
			break
		}
	}
	if reference == target.ImageID {
		for _, image := range snapshot.Images {
			if image.ID == target.ImageID && len(image.Tags) == 1 {
				reference = image.Tags[0]
			}
		}
	}
	ref, err := registry.ParseReference(reference)
	if err != nil || reference == target.ImageID {
		s.writeJSON(w, 200, map[string]string{"verdict": "unknown", "image_id": target.ImageID})
		return
	}
	out := map[string]string{"reference": reference, "image_id": target.ImageID, "verdict": "unknown", "local_digest": "", "remote_digest": "", "checked_at": time.Now().UTC().Format(time.RFC3339)}
	for _, image := range snapshot.Images {
		if image.ID != target.ImageID {
			continue
		}
		for _, digest := range image.Digests {
			local, e := registry.ParseReference(digest)
			if e == nil && local.Host == ref.Host && local.Repository == ref.Repository {
				out["local_digest"] = local.Digest
				break
			}
		}
	}
	if ref.Digest != "" {
		out["verdict"] = "pinned"
		s.writeJSON(w, 200, out)
		return
	}
	access, err := s.store.Tenancy().ResolveRegistryAccess(r.Context(), a, permissions.ImagePull, reference, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
	if !ok {
		return
	}
	defer release()
	extendRegistryDeadline(w)
	ctx, cancel := context.WithTimeout(r.Context(), store.ImageCheckDeadline)
	defer cancel()
	private := access.Registry != nil && access.Registry.AllowPrivate
	remote, err := s.resolver().Head(ctx, ref, access.Credential, private)
	if err != nil {
		out["verdict"] = "registry_error"
	} else {
		out["remote_digest"] = remote
		if out["local_digest"] != "" {
			if out["local_digest"] == remote {
				out["verdict"] = "up_to_date"
			} else {
				out["verdict"] = "update_available"
			}
		}
	}
	current, err := s.store.Tenancy().ReadInspectionTarget(r.Context(), a, endpoint, target.ContainerID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if current != target || !s.configurationAllowed(r, a, endpoint) {
		s.tenantError(w, store.ErrAdoptionChanged)
		return
	}
	s.writeJSON(w, 200, out)
}
