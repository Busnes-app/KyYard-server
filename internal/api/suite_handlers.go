package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/suite"
	"github.com/google/uuid"
)

// suiteName is an app or target name as KyYard forwards it; the runner decides whether it exists.
var suiteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type suiteStartRequest struct {
	Kind                string   `json:"kind"`
	Apps                []string `json:"apps"`
	Target              string   `json:"target"`
	AcknowledgeNoBackup bool     `json:"acknowledge_no_backup"`
	Confirm             string   `json:"confirm"`
	Password            string   `json:"password"`
	Code                string   `json:"code"`
}

// suiteConfigured answers 404 not_configured when no runner socket is set.
func (s *Server) suiteConfigured(w http.ResponseWriter) bool {
	if s.suite == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "The suite runner is not configured", "code": "not_configured"})
	}
	return s.suite != nil
}

// writeSuiteError passes a runner refusal through with its code and redacted message, and
// answers 502 for a runner that could not be reached or broke the protocol.
func (s *Server) writeSuiteError(w http.ResponseWriter, err error) {
	var refusal *suite.Error
	switch {
	case errors.As(err, &refusal):
		status := refusal.Status
		if status < 400 || status > 499 || status == http.StatusUnauthorized || status == http.StatusForbidden {
			status = http.StatusBadGateway
		}
		s.writeJSON(w, status, map[string]string{"error": protocol.CleanText(refusal.Message, 512), "code": protocol.CleanText(refusal.Code, 64)})
	case errors.Is(err, suite.ErrUnreachable):
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The suite runner did not answer", "code": "runner_unreachable"})
	case errors.Is(err, suite.ErrUnsafeSocket):
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The suite runner socket directory is not private", "code": "runner_socket_unsafe"})
	default:
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The suite runner answered outside protocol v1", "code": "runner_invalid"})
	}
}

func (s *Server) handleSuiteStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.suite == nil {
		s.writeJSON(w, http.StatusOK, map[string]bool{"configured": false})
		return
	}
	status, err := s.suite.Status(r.Context())
	if err != nil {
		s.writeSuiteError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, struct {
		Configured bool `json:"configured"`
		*suite.Status
	}{true, status})
}

func (s *Server) handleSuiteCatalog(w http.ResponseWriter, r *http.Request) {
	s.suiteRead(w, r, func(ctx context.Context) (any, error) { return s.suite.Catalog(ctx) })
}

func (s *Server) handleSuitePlan(w http.ResponseWriter, r *http.Request) {
	s.suiteRead(w, r, func(ctx context.Context) (any, error) { return s.suite.Plan(ctx) })
}

func (s *Server) suiteRead(w http.ResponseWriter, r *http.Request, read func(context.Context) (any, error)) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.suiteConfigured(w) {
		return
	}
	out, err := read(r.Context())
	if err != nil {
		s.writeSuiteError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleSuiteStart requires typed confirmation, the current password and, when enrolled, a TOTP
// code, on the budget the password change spends. It records the row before the runner is
// asked, so an answer lost in transit still leaves an operation to reconcile.
func (s *Server) handleSuiteStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.suiteConfigured(w) {
		return
	}
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		s.writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}
	var req suiteStartRequest
	decodeErr := strictJSON(r, &req)
	id := uuid.NewString()
	details := func() string {
		d := "kind=" + req.Kind + " apps=" + strings.Join(req.Apps, ",")
		if req.Target != "" {
			d += " target=" + req.Target
		}
		return protocol.CleanText(d, 512)
	}
	refuse := func(status int, result, code, msg string) {
		s.auditPlatform(r, user.ID, "suite.operation", id, details()+" refused="+code, result)
		s.writeJSON(w, status, map[string]string{"error": msg, "code": code})
	}
	if !s.allowAccountAttempt("password-change-user:"+user.ID, 5, time.Minute) {
		refuse(http.StatusTooManyRequests, "denied", "too_many_attempts", "Too many attempts; wait a few minutes")
		return
	}
	if decodeErr != nil || !validSuiteRequest(req) {
		refuse(http.StatusBadRequest, "failure", "invalid_request", "Invalid suite operation")
		return
	}
	if user.SSOProvider != "local" || user.PasswordHash == "" {
		refuse(http.StatusForbidden, "denied", "local_password_required", "Suite operations require an account with a local password")
		return
	}
	confirm := "suite"
	if len(req.Apps) == 1 {
		confirm = req.Apps[0]
	}
	if req.Confirm != confirm {
		refuse(http.StatusBadRequest, "failure", "confirm_mismatch", "Type "+confirm+" to confirm")
		return
	}
	if status, code, msg := s.stepUp(r.Context(), user, req.Password, req.Code); status != 0 {
		refuse(status, "denied", code, msg)
		return
	}

	// The runner's work outlives this request, so its record must too.
	ctx := context.WithoutCancel(r.Context())
	apps := append([]string{}, req.Apps...)
	op := &store.SuiteOperation{ID: id, ActorID: user.ID, Kind: req.Kind, Apps: apps, Target: req.Target, State: "running", StartedAt: time.Now().UTC()}
	if err := s.store.Suite().CreateSuiteOperation(ctx, op); err != nil {
		refuse(http.StatusInternalServerError, "failure", "record_failed", "Failed to record the suite operation")
		return
	}
	err = s.suite.Start(ctx, suite.Request{ID: id, Kind: req.Kind, Apps: apps, Target: req.Target, AcknowledgeNoBackup: req.AcknowledgeNoBackup, Actor: user.ID})
	var refusal *suite.Error
	switch {
	case errors.As(err, &refusal):
		now := time.Now().UTC()
		op.State, op.FinishedAt, op.ErrorCode = "failed", &now, protocol.CleanText(refusal.Code, 64)
		s.updateSuiteOperation(ctx, op)
		s.auditPlatform(r, user.ID, "suite.operation", id, details()+" runner="+op.ErrorCode, "failure")
		s.writeSuiteError(w, err)
	case err != nil:
		// The runner may have started it; reading the operation settles which.
		op.ErrorCode = "runner_unreachable"
		s.updateSuiteOperation(ctx, op)
		s.auditPlatform(r, user.ID, "suite.operation", id, details()+" runner=unreachable", "unknown")
		s.writeSuiteError(w, err)
	default:
		s.auditPlatform(r, user.ID, "suite.operation", id, details(), "success")
		s.writeJSON(w, http.StatusAccepted, op)
	}
}

// validSuiteRequest is the protocol's shape: one app, except an upgrade of every installed app
// (none); a target only for install; the backup acknowledgement only for upgrade.
func validSuiteRequest(req suiteStartRequest) bool {
	switch req.Kind {
	case "install":
		if !suiteName.MatchString(req.Target) {
			return false
		}
	case "uninstall", "unmanage", "upgrade":
		if req.Target != "" {
			return false
		}
	default:
		return false
	}
	if len(req.Apps) > 1 || (len(req.Apps) == 0 && req.Kind != "upgrade") || (req.AcknowledgeNoBackup && req.Kind != "upgrade") {
		return false
	}
	for _, app := range req.Apps {
		if !suiteName.MatchString(app) {
			return false
		}
	}
	return len(req.Confirm) <= 64 && len(req.Password) <= 1024 && len(req.Code) <= 64
}

// handleSuiteOperation reads a running operation from the runner and records the answer; a
// settled row is returned as recorded. An operation the runner no longer knows becomes unknown.
func (s *Server) handleSuiteOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.suiteConfigured(w) {
		return
	}
	id := r.PathValue("id")
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		s.writeError(w, http.StatusBadRequest, "Invalid operation ID")
		return
	}
	op, err := s.store.Suite().ReadSuiteOperation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, "No such suite operation")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to read the suite operation")
		return
	}
	if op.State != "running" {
		s.writeJSON(w, http.StatusOK, op)
		return
	}
	remote, err := s.suite.Operation(r.Context(), id)
	switch {
	case errors.Is(err, suite.ErrNotFound):
		op.State, op.ErrorCode = "unknown", "lost"
	case err != nil:
		s.writeSuiteError(w, err)
		return
	default:
		snapshot, err := json.Marshal(remote)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to record the suite operation")
			return
		}
		op.State, op.FinishedAt, op.Snapshot, op.ErrorCode = remote.State, remote.Finished, snapshot, ""
		if remote.Error != nil {
			op.ErrorCode = protocol.CleanText(remote.Error.Code, 64)
		}
	}
	if err := s.store.Suite().UpdateSuiteOperation(r.Context(), op); err != nil {
		// Another read settled it first; return what it recorded.
		if op, err = s.store.Suite().ReadSuiteOperation(r.Context(), id); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to read the suite operation")
			return
		}
	}
	s.writeJSON(w, http.StatusOK, op)
}

func (s *Server) updateSuiteOperation(ctx context.Context, op *store.SuiteOperation) {
	if err := s.store.Suite().UpdateSuiteOperation(ctx, op); err != nil {
		log.Printf("[SUITE] operation %s not updated: %v", op.ID, err)
	}
}
