package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// tenantRoute authenticates identity only. The store selects the named action and
// checks live authorization in the transaction that reads or mutates tenant data.
func (s *Server) tenantRoute(h func(http.ResponseWriter, *http.Request, store.TenantAccess)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		correlation := crypto.RandomHex(16)
		w.Header().Set("X-Request-ID", correlation)
		w.Header().Set("Cache-Control", "no-store")
		user, _, err := s.sessions.AuthenticateRequest(r)
		if err != nil {
			if errors.Is(err, auth.ErrPasswordChangeRequired) {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Change your password before continuing", "code": "password_change_required"})
			} else {
				s.writeError(w, http.StatusUnauthorized, "Authentication required")
			}
			return
		}
		org, env := r.PathValue("organization"), r.PathValue("environment")
		if org == "" || len(org) > 64 || len(env) > 64 {
			s.writeError(w, http.StatusBadRequest, "Invalid tenant scope")
			return
		}
		h(w, r, store.TenantAccess{ActorID: user.ID, OrganizationID: org, EnvironmentID: env, CorrelationID: correlation, IPAddress: s.requestIP(r)})
	}
}
func (s *Server) tenantError(w http.ResponseWriter, err error) {
	var blocked *store.PreflightBlockedError
	if errors.As(err, &blocked) {
		body := map[string]any{"error": "Deployment preflight reported blockers", "code": "preflight_blocked", "blockers": blocked.Blockers}
		if len(blocked.Services) > 0 {
			body["services"] = blocked.Services
		}
		s.writeJSON(w, http.StatusConflict, body)
		return
	}
	switch {
	case errors.Is(err, store.ErrForbidden):
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Tenant access denied", "code": "tenant_access_denied"})
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "Resource not found in this organization")
	case errors.Is(err, store.ErrAlreadyExists):
		s.writeError(w, http.StatusConflict, "Resource already exists")
	case errors.Is(err, store.ErrAdoptionChanged):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Inventory or ownership changed; refresh and preview again", "code": "adoption_changed"})
	case errors.Is(err, store.ErrApplicationAdopted):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Release adoption before discarding this application", "code": "application_adopted"})
	case errors.Is(err, store.ErrApplicationLimit):
		s.writeError(w, http.StatusConflict, "Application storage limit reached")
	case errors.Is(err, store.ErrRevisionConflict):
		s.writeError(w, http.StatusConflict, "Application changed; refresh before continuing")
	case errors.Is(err, store.ErrInUse):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Discard the environment's draft applications and revoke its endpoints first", "code": "environment_in_use"})
	case errors.Is(err, store.ErrEndpointOffline):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The endpoint is not connected, so nothing was sent", "code": "endpoint_offline"})
	case errors.Is(err, store.ErrDeploymentInProgress):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "A deployment is being applied; wait for its result", "code": "deployment_in_progress"})
	case errors.Is(err, store.ErrLastAdmin):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The organization needs at least one active administrator", "code": "last_administrator"})
	case errors.Is(err, store.ErrRemovalTooLarge):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This instance has more containers than KyYard removes in one operation; release it and remove the containers by hand", "code": "removal_too_large"})
	case errors.Is(err, store.ErrDeploymentPlanned):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "A live deployment plan exists; let it expire before releasing", "code": "deployment_planned"})
	case errors.Is(err, store.ErrRuntimeUnsupported):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This endpoint's runtime does not support this action", "code": "runtime_unsupported"})
	case errors.Is(err, store.ErrNamespaceUnknown):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The cluster's manifest does not grant that namespace", "code": "namespace_unknown"})
	case errors.Is(err, store.ErrMappingRequired):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Map the application's services to adopted containers first", "code": "mapping_required"})
	case errors.Is(err, errCheckInProgress):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "An image update check or update plan for this application is already running", "code": "check_in_progress"})
	case errors.Is(err, errTooManyChecks):
		s.writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many registry checks are running; try again in a moment", "code": "too_many_checks"})
	case errors.Is(err, store.ErrRegistryNotConfigured):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "No registry is configured for this image's host and anonymous pulls are off", "code": "registry_not_configured"})
	case errors.Is(err, store.ErrPrivateRegistriesDisabled):
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Private-address registries are disabled by the operator (KY_REGISTRY_ALLOW_PRIVATE)", "code": "private_registries_disabled"})
	case errors.Is(err, store.ErrInvalidTimezone):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This server does not recognise that time zone", "code": "invalid_timezone"})
	case errors.Is(err, store.ErrInvalidWindow):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A window lasts at least 15 minutes and ends by midnight", "code": "invalid_window"})
	case errors.Is(err, store.ErrInvalidWeekdays):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Choose one to seven distinct days, 0 (Sunday) to 6", "code": "invalid_weekdays"})
	case errors.Is(err, store.ErrInvalidMode):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Mode is apply or plan_only", "code": "invalid_mode"})
	case errors.Is(err, store.ErrPolicyNotPaused):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The update policy is not paused", "code": "policy_not_paused"})
	case errors.Is(err, store.ErrMigrationOpen):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The application has an open migration; confirm its cutover or abandon it first", "code": "migration_open"})
	case errors.Is(err, store.ErrMigrationNotReady):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The migration report still has blocked findings or choices to make", "code": "migration_not_ready"})
	case errors.Is(err, store.ErrMigrationState):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The migration's status does not allow this step", "code": "migration_state"})
	case errors.Is(err, store.ErrMigrationStale):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The source definition changed since the analysis; analyze again", "code": "migration_stale"})
	case errors.Is(err, store.ErrApplicationNameTaken):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Other applications hold the destination's name and each suffix up to (9)", "code": "application_name_taken"})
	case errors.Is(err, store.ErrDestinationNameTooLong):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The destination's name would be longer than 255 bytes; rename the cluster", "code": "destination_name_too_long"})
	case errors.Is(err, store.ErrInventoryStale):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The cluster's inventory is stale; wait for its next report and try again", "code": "inventory_stale"})
	case errors.Is(err, store.ErrStorageClassUnknown):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The cluster does not report that StorageClass", "code": "storage_class_unknown"})
	case errors.Is(err, store.ErrSizeInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A size is a whole number of Mi, Gi or Ti from 1Mi to 16Ti", "code": "size_invalid"})
	case errors.Is(err, store.ErrVolumeUnknown):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The source mounts no named volume by that name", "code": "volume_unknown"})
	case errors.Is(err, store.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, "Invalid tenant input")
	default:
		s.writeError(w, http.StatusInternalServerError, "Tenant operation failed")
	}
}
func tenantPage(r *http.Request) (int, int, error) {
	offset, limit := 0, 50
	var err error
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, store.ErrInvalid
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			return 0, 0, store.ErrInvalid
		}
	}
	if offset < 0 || limit < 1 || limit > 200 {
		return 0, 0, store.ErrInvalid
	}
	return offset, limit, nil
}

// strictJSON rejects unknown fields and trailing documents so clients cannot smuggle scope.
func strictJSON(r *http.Request, v any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return store.ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return store.ErrInvalid
	}
	return nil
}
func tenantName(r *http.Request) (string, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := strictJSON(r, &input); err != nil {
		return "", err
	}
	return input.Name, nil
}
func (s *Server) handleTenantOrganization(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	o, err := s.store.Tenancy().ReadOrganization(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, o)
}
func (s *Server) handleTenantEnvironment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	e, err := s.store.Tenancy().ReadEnvironment(r.Context(), a)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, e)
}
func (s *Server) handleTenantEnvironments(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	offset, limit, err := tenantPage(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().ListEnvironments(r.Context(), a, offset, limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}
func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	name, err := tenantName(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	e, err := s.store.Tenancy().AddEnvironment(r.Context(), a, name)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, e)
}
func (s *Server) handleUpdateEnvironment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	name, err := tenantName(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if err := s.store.Tenancy().UpdateEnvironment(r.Context(), a, name); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleRemoveEnvironment(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	if err := s.store.Tenancy().RemoveEnvironment(r.Context(), a); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleTenantAudit(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	offset, limit, err := tenantPage(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().ReadAudit(r.Context(), a, offset, limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}
func (s *Server) handleMyOrganizations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordChangeRequired) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "Change your password before continuing", "code": "password_change_required"})
		} else {
			s.writeError(w, http.StatusUnauthorized, "Authentication required")
		}
		return
	}
	orgs, err := s.store.Tenancy().ListMemberOrganizations(r.Context(), user.ID)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, orgs)
}
func memberUser(r *http.Request) (string, error) {
	id := r.PathValue("user")
	if id == "" || len(id) > 64 {
		return "", store.ErrInvalid
	}
	return id, nil
}
func (s *Server) handleTenantMembers(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	offset, limit, err := tenantPage(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().ListMembers(r.Context(), a, offset, limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}
func (s *Server) handlePutMembership(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	userID, err := memberUser(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	var input struct {
		Role   store.TenantRole `json:"role"`
		Status string           `json:"status"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.tenantError(w, err)
		return
	}
	if input.Status == "" {
		input.Status = "active"
	}
	if err := s.store.Tenancy().PutMembership(r.Context(), a, userID, input.Role, input.Status); err != nil {
		s.tenantError(w, err)
		return
	}
	// A role narrowed or a membership suspended takes effect on what is already running, in
	// this request rather than whenever a stream happens to end. The stream re-checks its own
	// authorization as well; this is what makes the change immediate.
	s.logs.closeActorStreams(userID, "your access to this log was withdrawn")
	s.execs.closeActor(userID)
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleRemoveMembership(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	userID, err := memberUser(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if err := s.store.Tenancy().RemoveMembership(r.Context(), a, userID); err != nil {
		s.tenantError(w, err)
		return
	}
	s.logs.closeActorStreams(userID, "your access to this log was withdrawn")
	s.execs.closeActor(userID)
	w.WriteHeader(http.StatusNoContent)
}
