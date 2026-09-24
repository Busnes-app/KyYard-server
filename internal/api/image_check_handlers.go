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

var (
	errCheckInProgress = errors.New("image update check in progress")
	errTooManyChecks   = errors.New("too many registry checks in flight")
)

// acquireRegistrySlot takes one of the server's registry slots, or answers 429.
func (s *Server) acquireRegistrySlot(w http.ResponseWriter) (release func(), ok bool) {
	select {
	case s.registrySlots <- struct{}{}:
		return func() { <-s.registrySlots }, true
	default:
		s.tenantError(w, errTooManyChecks)
		return nil, false
	}
}

// extendRegistryDeadline outlasts the server's WriteTimeout, which is shorter than registry
// work may run.
func extendRegistryDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(store.ImageCheckDeadline + 5*time.Second))
}

func (s *Server) resolver() store.DigestResolver {
	if s.digestResolver == nil {
		return registryResolver{}
	}
	return s.digestResolver
}

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
	release, ok := s.acquireRegistrySlot(w)
	if !ok {
		return
	}
	defer release()
	extendRegistryDeadline(w)
	out, err := s.store.Tenancy().CheckImageUpdates(r.Context(), a, app, s.resolver(), s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}
