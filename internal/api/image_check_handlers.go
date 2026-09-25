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

// Registry work in flight: an organization's quota comes first, so one tenant cannot hold the
// whole pool; the pool fits registrySlotsTotal/registrySlotsPerOrganization busy organizations.
const (
	registrySlotsPerOrganization = 2
	registrySlotsTotal           = 8
)

// guardApplication admits one registry operation per application at a time, an update check or
// an update plan, keyed by the canonical application ID, or answers 409 check_in_progress.
func (s *Server) guardApplication(w http.ResponseWriter, a store.TenantAccess, app string) (release func(), ok bool) {
	release, ok = s.holdApplication(a, app)
	if !ok {
		s.tenantError(w, errCheckInProgress)
	}
	return release, ok
}

// holdApplication is guardApplication without a response, for the policy scheduler.
func (s *Server) holdApplication(a store.TenantAccess, app string) (release func(), ok bool) {
	key := a.OrganizationID + "/" + a.EnvironmentID + "/" + app
	if _, busy := s.imageChecks.LoadOrStore(key, struct{}{}); busy {
		return nil, false
	}
	return func() { s.imageChecks.Delete(key) }, true
}

// acquireRegistrySlot takes one of org's registry slots and one of the server's, or answers 429.
func (s *Server) acquireRegistrySlot(w http.ResponseWriter, org string) (release func(), ok bool) {
	release, ok = s.takeRegistrySlot(org)
	if !ok {
		w.Header().Set("Retry-After", "5")
		s.tenantError(w, errTooManyChecks)
	}
	return release, ok
}

// takeRegistrySlot is acquireRegistrySlot without a response.
func (s *Server) takeRegistrySlot(org string) (release func(), ok bool) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.registryHeld == nil {
		s.registryHeld = map[string]int{}
	}
	if s.registryHeld[org] >= registrySlotsPerOrganization {
		return nil, false
	}
	select {
	case s.registrySlots <- struct{}{}:
	default:
		return nil, false
	}
	s.registryHeld[org]++
	return func() {
		s.registryMu.Lock()
		defer s.registryMu.Unlock()
		if s.registryHeld[org]--; s.registryHeld[org] == 0 {
			delete(s.registryHeld, org)
		}
		<-s.registrySlots
	}, true
}

// waitRegistrySlot polls takeRegistrySlot about once a second until wait has passed.
func (s *Server) waitRegistrySlot(org string, wait time.Duration) (release func(), ok bool) {
	deadline := time.Now().Add(wait)
	for {
		if release, ok := s.takeRegistrySlot(org); ok {
			return release, true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, false
		}
		time.Sleep(min(time.Second, left))
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
	done, ok := s.guardApplication(w, a, app)
	if !ok {
		return
	}
	defer done()
	release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
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
