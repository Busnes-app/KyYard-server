package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/Busnes-app/kyyard-server/internal/store"
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
	app := r.PathValue("application")
	key := a.OrganizationID + "/" + a.EnvironmentID + "/" + app
	if _, busy := s.imageChecks.LoadOrStore(key, struct{}{}); busy {
		s.tenantError(w, errCheckInProgress)
		return
	}
	defer s.imageChecks.Delete(key)
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
