package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky_server_base/internal/backup"
	"github.com/Busness-app/ky_server_base/internal/store"
)

// appVersion is what the capsule manifest records for this build.
const appVersion = "1.0.0"

// errRecoveryKeyMismatch answers a swapped recovery.pub: the pin in the database and the key
// on disk disagree, so refuse rather than seal a capsule nobody's custodians can open.
const errRecoveryKeyMismatch = "Recovery key file does not match the pinned key ID; refusing to seal"

// privateRecoveryHint names the opt-in, so a refused LAN destination is not a dead end. Both
// the pairing and the run refusals end with it.
const privateRecoveryHint = " (set KY_BACKUP_ALLOW_PRIVATE_RECOVERY=true for a KyRecovery on your own network)"

// depositWriteBudget is how long the admin's connection may stay open for the receipt: the
// upload budget plus room for sealing. The listener's WriteTimeout is sized for JSON replies.
const depositWriteBudget = 16 * time.Minute

// AuditDetails flattens the lib's details map into the bounded audit column. Locally
// derived fields go first so a long remote error cannot displace the useful facts.
func AuditDetails(m map[string]any) string {
	seen := map[string]bool{}
	keys := []string{"outcome", "capsule_id", "local_path", "local_error", "deposited", "digest", "size_bytes"}
	var extra, remote []string
	for k := range m {
		switch k {
		case "outcome", "capsule_id", "local_path", "local_error", "deposited", "digest", "size_bytes":
		case "error", "receipt_unrecorded":
			remote = append(remote, k)
		default:
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(remote)
	keys = append(keys, extra...)
	keys = append(keys, remote...)
	var b strings.Builder
	for _, k := range keys {
		v, ok := m[k]
		if !ok || seen[k] {
			continue
		}
		seen[k] = true
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s", k, auditValue(v))
	}
	return recoveryclient.AuditSafe(b.String())
}

func auditValue(v any) string {
	return strings.ReplaceAll(fmt.Sprintf("%q", fmt.Sprint(v)), "=", `\x3d`)
}

// auditBackup records a backup event against the acting admin. Details never carry the
// token or capsule bytes, and both text fields are bounded and printable before they are
// stored: a resource may be an operator-typed URL and details may quote a remote body.
func (s *Server) auditBackup(ctx context.Context, userID string, r *http.Request, action, resource, details string) {
	resource, details = recoveryclient.AuditSafe(resource), recoveryclient.AuditSafe(details)
	if err := s.store.Audit().LogAudit(ctx, &store.AuditRecord{
		UserID:    userID,
		Action:    action,
		Resource:  resource,
		Details:   details,
		IPAddress: s.requestIP(r),
	}); err != nil {
		log.Printf("[BACKUP] audit %s for %s not recorded: %v", action, resource, err)
	}
}

// actorID is the session user behind an admin request, resolved while the request is live.
func (s *Server) actorID(r *http.Request) string {
	user, _, err := s.sessions.AuthenticateRequest(r)
	if err != nil {
		return ""
	}
	return user.ID
}

// handleBackupDrill seals the live payload to a throwaway key, opens it in a sandbox and
// runs the verification recipe. It reports, not proves, whether the suite key is pinned.
func (s *Server) handleBackupDrill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	payload, err := backup.Collect(ctx, s.config, appVersion)
	if errors.Is(err, backup.ErrNoDatabaseSnapshot) {
		// An honest failed drill, not a 500: the operator needs to read why no backup exists.
		s.writeJSON(w, http.StatusOK, &recoveryclient.DrillResult{Passed: false, ErrorMessage: err.Error(),
			Checks: []recoveryclient.Check{{Name: "Database", Passed: false, Message: err.Error()}}})
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to collect backup files")
		return
	}
	result, err := backup.RunDrill(ctx, s.config, payload)
	if errors.Is(err, backup.ErrDrillBusy) {
		s.writeError(w, http.StatusConflict, "A restore drill is already running")
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to execute restore drill")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// handleExportCapsule hands the operator the sealed capsule itself. Only the custodians'
// shares open it, so the download is safe to store anywhere; kyrecovery is where it belongs.
// It is a POST so the CSRF check applies: a cross-site GET must not be able to pull it.
func (s *Server) handleExportCapsule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	actor := s.actorID(r)
	settings := backup.Settings(ctx, s.store.Settings())
	key, err := recoveryclient.LoadRecoveryKey(s.config.Database.DataDir, settings)
	if errors.Is(err, recoveryclient.ErrNotPaired) {
		s.writeError(w, http.StatusPreconditionFailed, "No recovery key; pair or pin one")
		return
	}
	if errors.Is(err, recoveryclient.ErrKeyMismatch) {
		s.writeError(w, http.StatusConflict, errRecoveryKeyMismatch)
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to load recovery key")
		return
	}
	payload, err := backup.Collect(ctx, s.config, appVersion)
	if errors.Is(err, backup.ErrNoDatabaseSnapshot) {
		s.writeError(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to collect backup files")
		return
	}
	raw, m, err := recoveryclient.Seal(payload, key)
	if err != nil {
		// Logged because writeError only reaches the browser: without this an export that has
		// outgrown the capsule limits is a bare 500 with nothing anywhere naming the cause.
		log.Printf("[BACKUP] export capsule: seal failed: %s", recoveryclient.AuditSafe(err.Error()))
		if errors.Is(err, capsule.ErrCapsuleTooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, recoveryclient.TooLargeMessage)
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Failed to seal capsule")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.kycap"`, recoveryclient.FilenameSafe(m.CapsuleID)))
	w.Header().Set("X-Recovery-Key-ID", m.RecoveryKeyID)
	// A capsule leaving the server is a copy of everything it holds; the trail says who took one.
	s.auditBackup(ctx, actor, r, "admin.backup_export", m.CapsuleID, AuditDetails(map[string]any{"size_bytes": len(raw)}))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

type RemotePairRequest struct {
	RecoveryURL string `json:"recovery_url"`
	PairingCode string `json:"pairing_code"`
}

// handlePairRemoteRecovery claims a 6-digit PIN with KyRecovery, pins the suite recovery
// public key it hands back, and stores the URL and the sealed bearer token.
// The route wraps it in `tracked`, so it is counted as detached from the moment ServeHTTP
// dispatches, before requireAdmin authenticates.
func (s *Server) handlePairRemoteRecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req RemotePairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.RecoveryURL == "" || req.PairingCode == "" {
		s.writeError(w, http.StatusBadRequest, "Both recovery_url and pairing_code are required")
		return
	}
	// Validated here as well as in the client so a bad URL is rejected before it is stored
	// or audited as a pairing attempt.
	if err := recoveryclient.ValidateURL(req.RecoveryURL, s.config.Backup.AllowPrivateRecovery); err != nil {
		msg := err.Error()
		if errors.Is(err, recoveryclient.ErrPrivateDestination) {
			msg += privateRecoveryHint
		}
		s.writeError(w, http.StatusBadRequest, msg)
		return
	}

	// Pairing is write-once: the pin, the stored pairing and the audit row must all land even
	// if the admin's connection drops mid-claim. The actor is resolved while it is still live.
	actor := s.actorID(r)
	ctx := context.WithoutCancel(r.Context())
	target := recoveryclient.AuditSafe(req.RecoveryURL)

	// The service name sent here is what kyrecovery pins for the token and what every
	// capsule's manifest is checked against, so it is the same AppName the collector seals under.
	result, err := s.recovery.ClaimPairing(ctx, req.RecoveryURL, req.PairingCode, s.config.Server.AppName, s.config.Server.AppName)
	if err != nil {
		s.auditBackup(ctx, actor, r, "backup.pair_failed", target, "error="+err.Error())
		s.writeError(w, http.StatusBadRequest, "Recovery pairing failed")
		return
	}

	settings := backup.Settings(ctx, s.store.Settings())
	if err := recoveryclient.StoreRecoveryKey(s.config.Database.DataDir, settings, result.Key); err != nil {
		if errors.Is(err, fs.ErrExist) {
			s.auditBackup(ctx, actor, r, "backup.pair_failed", target, "error=already paired to a different recovery key")
			s.writeError(w, http.StatusConflict, "Already paired to a different recovery key")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Failed to save recovery key")
		return
	}
	sealer, err := backup.NewSealer(s.config)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to prepare the pairing seal")
		return
	}
	// The key is now pinned on disk whatever happens next, so it is recorded whatever happens
	// next: a write-once pin that exists nowhere in the audit trail is the gap this closes.
	if err := recoveryclient.StorePairing(settings, sealer, req.RecoveryURL, result.APIToken); err != nil {
		s.auditBackup(ctx, actor, r, "backup.pair_failed", target,
			"error=key pinned but the pairing was not stored: "+err.Error())
		s.writeError(w, http.StatusInternalServerError, "Failed to save recovery pairing")
		return
	}
	s.auditBackup(ctx, actor, r, "backup.paired", target,
		fmt.Sprintf("recovery_key_id=%s allow_private=%v", result.Key.Public.ID(), s.config.Backup.AllowPrivateRecovery))

	s.writeJSON(w, http.StatusOK, map[string]any{
		"recovery_key_id": result.Key.Public.ID(),
		"threshold":       result.Key.Threshold,
		"total_shares":    result.Key.TotalShares,
	})
}

// handleRunBackup seals a capsule and deposits it now, outside the schedule: one capsule to
// the local directory and, when paired, to KyRecovery. The receipt it returns is what a
// restore is later checked against.
//
// The run uses a context that outlives the request: once bytes are on their way, a closed
// tab must not leave KyRecovery holding a capsule this instance has no receipt for.
// The route wraps it in `tracked`, so it is counted as detached from the moment ServeHTTP
// dispatches, before requireAdmin authenticates.
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(depositWriteBudget))
	// The acting admin is resolved while the request is still live; the audit row is written
	// on the same detached context as the run, so a dropped connection cannot decide whether
	// the row exists.
	actor := s.actorID(r)
	ctx := context.WithoutCancel(r.Context())

	rc, err := backup.RunConfig(s.config, appVersion)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to prepare backup configuration")
		return
	}
	settings := backup.Settings(ctx, s.store.Settings())
	res, err := recoveryclient.Run(ctx, rc, settings, func() (recoveryclient.Payload, error) {
		return backup.Collect(ctx, s.config, appVersion)
	}, s.recovery)

	action, outcome, details := recoveryclient.Outcome(res, err)
	details["outcome"] = outcome
	s.auditBackup(ctx, actor, r, action, res.Manifest.CapsuleID, AuditDetails(details))

	if errors.Is(err, recoveryclient.ErrReceiptUnrecorded) {
		// The store has the capsule; only this side's record is missing. This is the one path
		// where the two sides disagree about what exists, so the cause goes on record here too.
		log.Printf("[BACKUP] run %s: receipt not recorded: %s", res.Manifest.CapsuleID, recoveryclient.AuditSafe(err.Error()))
		s.writeJSON(w, http.StatusOK, struct {
			recoveryclient.Result
			ReceiptUnrecorded bool `json:"receipt_unrecorded"`
		}{res, true})
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, recoveryclient.ErrKeyPinMissing):
			s.writeError(w, http.StatusPreconditionFailed, "Paired, but recovery.pub is missing or does not match the pin")
		case errors.Is(err, recoveryclient.ErrNotPaired):
			s.writeError(w, http.StatusPreconditionFailed, "No recovery key")
		case errors.Is(err, backup.ErrNoDatabaseSnapshot):
			// Only SQLite can be snapshotted, so on any other driver no capsule can be made at
			// all. A configuration fact the operator must read, not a server fault.
			s.writeError(w, http.StatusPreconditionFailed, err.Error())
		case errors.Is(err, recoveryclient.ErrNoDestination):
			s.writeError(w, http.StatusPreconditionFailed, "Nowhere to put a capsule: pair with KyRecovery or set KY_BACKUP_DIR")
		case errors.Is(err, recoveryclient.ErrInProgress):
			s.writeError(w, http.StatusConflict, "A backup is already in progress")
		case errors.Is(err, recoveryclient.ErrKeyMismatch):
			s.writeError(w, http.StatusConflict, errRecoveryKeyMismatch)
		case errors.Is(err, recoveryclient.ErrPrivateDestination):
			// The pairing was stored before the opt-in was turned off, or the host now resolves
			// private. Nothing left; the operator needs the switch named, not a 500.
			s.writeError(w, http.StatusPreconditionFailed, "The recovery host resolves to a private address"+privateRecoveryHint)
		case errors.Is(err, capsule.ErrCapsuleTooLarge):
			s.writeError(w, http.StatusRequestEntityTooLarge, recoveryclient.TooLargeMessage)
		case errors.Is(err, recoveryclient.ErrRemote):
			// The cause is audited; the browser gets only that the store did not take it.
			log.Printf("[BACKUP] run failed: %s", recoveryclient.AuditSafe(err.Error()))
			msg := "KyRecovery did not accept the deposit"
			if res.LocalPath != "" {
				msg += "; the local copy was written"
			}
			if res.LocalError != "" {
				msg += "; " + res.LocalError
			}
			s.writeError(w, http.StatusBadGateway, msg)
		default:
			// Anything raised before a byte left: a stored URL the client refuses, a store
			// read, a collector error. The cause is audited; the message does not guess.
			log.Printf("[BACKUP] run failed (local): %s", recoveryclient.AuditSafe(err.Error()))
			s.writeError(w, http.StatusInternalServerError, "Backup failed before reaching KyRecovery")
		}
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// handleUnpair forgets the KyRecovery pairing. Deposits stop; the key pin and any local
// backup directory are untouched. The token on KyRecovery's side is revoked there, by its
// admin.
func (s *Server) handleUnpair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	actor := s.actorID(r)
	settings := backup.Settings(ctx, s.store.Settings())
	target, _ := s.store.Settings().GetSetting(ctx, "kyrecovery_url")
	target = recoveryclient.AuditSafe(target)
	if err := recoveryclient.ClearPairing(settings); err != nil {
		if errors.Is(err, recoveryclient.ErrNotPaired) {
			s.writeError(w, http.StatusPreconditionFailed, "Not paired with KyRecovery")
			return
		}
		s.auditBackup(ctx, actor, r, "admin.backup_unpair", target, "error="+err.Error())
		s.writeError(w, http.StatusInternalServerError, "Failed to remove the pairing")
		return
	}
	s.auditBackup(ctx, actor, r, "admin.backup_unpair", target, "success")
	s.writeJSON(w, http.StatusOK, map[string]any{"paired": false})
}

type PinKeyRequest struct {
	PublicKey   string `json:"public_key"`
	Threshold   int    `json:"threshold"`
	TotalShares int    `json:"total_shares"`
}

// handlePinKey pins the suite recovery public key by hand, for an instance with no
// KyRecovery to pair with. The key is the one the ceremony page shows; the topology is the
// k-of-n it was split with. Write-once, like pairing.
// The route wraps it in `tracked`, so it is counted as detached from the moment ServeHTTP
// dispatches, before requireAdmin authenticates.
func (s *Server) handlePinKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req PinKeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}
	key, err := recoveryclient.ParsePinRequest(req.PublicKey, req.Threshold, req.TotalShares)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Write-once like pairing, so the pin and its audit row outlive the request too.
	actor := s.actorID(r)
	ctx := context.WithoutCancel(r.Context())
	settings := backup.Settings(ctx, s.store.Settings())
	if err := recoveryclient.StoreRecoveryKey(s.config.Database.DataDir, settings, key); err != nil {
		if errors.Is(err, fs.ErrExist) {
			s.auditBackup(ctx, actor, r, "admin.backup_key_pin", key.Public.ID(), "error=already pinned to a different recovery key")
			s.writeError(w, http.StatusConflict, "Already pinned to a different recovery key")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Failed to save recovery key")
		return
	}
	s.auditBackup(ctx, actor, r, "admin.backup_key_pin", key.Public.ID(),
		fmt.Sprintf("threshold=%d total_shares=%d", key.Threshold, key.TotalShares))
	s.writeJSON(w, http.StatusOK, map[string]any{
		"recovery_key_id": key.Public.ID(), "threshold": key.Threshold, "total_shares": key.TotalShares,
	})
}

type ScheduleRequest struct {
	IntervalSec int64 `json:"interval_sec"`
}

// handleSetSchedule stores how often the scheduler backs up. Zero turns it off, so the
// admin-only, CSRF-protected and audited boundary matters.
func (s *Server) handleSetSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req ScheduleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON request body")
		return
	}
	ctx := r.Context()
	actor := s.actorID(r)
	settings := backup.Settings(ctx, s.store.Settings())
	if err := recoveryclient.SetInterval(settings, req.IntervalSec); err != nil {
		if errors.Is(err, recoveryclient.ErrBadInterval) {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Failed to save the schedule")
		return
	}
	// Read back what the store holds, so the audit row and the reply never describe a
	// schedule the scheduler will not run.
	stored, err := recoveryclient.Interval(s.config.Backup.DepositInterval, settings)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to read the schedule back")
		return
	}
	sec := int64(stored / time.Second)
	s.auditBackup(ctx, actor, r, "admin.backup_schedule", "", fmt.Sprintf("interval_sec=%d", sec))
	s.writeJSON(w, http.StatusOK, map[string]any{"interval_sec": sec})
}

// handleBackupStatus reports pairing and the last receipt. It never decrypts or echoes the
// credential.
func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	ctx := r.Context()
	settings := backup.Settings(ctx, s.store.Settings())
	out := map[string]any{
		"paired":                 false,
		"key_pinned":             false,
		"app_name":               s.config.Server.AppName,
		"app_version":            appVersion,
		"allow_private_recovery": s.config.Backup.AllowPrivateRecovery,
		// Only the SQLite path can snapshot a database into a capsule; the screen says so.
		"database_driver": s.config.Database.Driver,
		"members":         backup.Members(s.config),
	}
	if u, err := s.store.Settings().GetSetting(ctx, "kyrecovery_url"); err == nil {
		out["recovery_url"] = u
	}
	key, err := recoveryclient.LoadRecoveryKey(s.config.Database.DataDir, settings)
	switch {
	case err == nil:
		out["key_pinned"] = true
		out["paired"] = recoveryclient.HasPairing(settings)
		out["recovery_key_id"] = key.Public.ID()
		out["threshold"] = key.Threshold
		out["total_shares"] = key.TotalShares
	case errors.Is(err, recoveryclient.ErrKeyMismatch):
		out["recovery_key_error"] = "recovery.pub does not match the pinned key ID"
	case recoveryclient.HasPairing(settings):
		out["recovery_key_error"] = "paired, but recovery.pub is missing; restore it or re-pair"
	}
	if last, ok, err := recoveryclient.LastDeposit(settings); err == nil && ok {
		out["last_deposit"] = last
	}
	if s.config.Backup.Dir != "" {
		out["local_dir"] = s.config.Backup.Dir
		out["local_keep"] = s.config.Backup.Keep
		if copies, err := recoveryclient.ListLocalCopies(s.config.Backup.Dir, s.config.Server.AppName); err == nil {
			out["local_copies"] = copies
		} else {
			out["local_error"] = recoveryclient.AuditSafe(err.Error())
		}
	}
	if interval, err := recoveryclient.Interval(s.config.Backup.DepositInterval, settings); err == nil {
		out["interval_sec"] = int64(interval / time.Second)
		out["min_interval_sec"] = int64(recoveryclient.MinInterval / time.Second)
		if next, ok, err := recoveryclient.NextRun(s.config.Backup.DepositInterval, settings); err == nil && ok {
			out["next_run_at"] = next.Format(time.RFC3339)
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}
