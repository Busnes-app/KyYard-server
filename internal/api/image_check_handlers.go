package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/google/uuid"
)

var errCheckInProgress = errors.New("image update check in progress")

// registryResolver adapts internal/registry to store.DigestResolver: one client per call so a
// private-address allowance never leaks between hosts.
type registryResolver struct{}

func (registryResolver) Head(ctx context.Context, ref registry.Reference, cred *registry.Credential, allowPrivate bool) (string, error) {
	r, err := registry.New(registry.Options{AllowPrivate: allowPrivate}).Head(ctx, ref, cred)
	if err != nil {
		return "", err
	}
	return r.Digest, nil
}

func (s *Server) handleImageChecks(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	out, err := s.store.Tenancy().ReadImageChecks(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCheckImageUpdates(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := uuid.Parse(r.PathValue("application"))
	if err != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	app := id.String()
	// Authorize before the slot, so a caller who may not check can neither see nor hold it.
	if err := s.store.Tenancy().CheckImageUpdateAccess(r.Context(), a, app); err != nil {
		s.tenantError(w, err)
		return
	}
	key := a.OrganizationID + "/" + a.EnvironmentID + "/" + app
	if _, busy := s.imageChecks.LoadOrStore(key, struct{}{}); busy {
		s.tenantError(w, errCheckInProgress)
		return
	}
	defer s.imageChecks.Delete(key)
	// The server's WriteTimeout is shorter than a check may run.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(store.ImageCheckDeadline + 5*time.Second))
	resolver := s.digestResolver
	if resolver == nil {
		resolver = registryResolver{}
	}
	out, err := s.store.Tenancy().CheckImageUpdates(r.Context(), a, app, resolver, s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}
