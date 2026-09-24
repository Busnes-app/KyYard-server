package api

import (
	"crypto/rand"
	"errors"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const (
	adminBodyLimit   = 4 << 10
	adminUserListMax = 500
	passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9._-]{3,64}$`)

// cleanName reports whether s is 1..max characters and unchanged by protocol.CleanText.
func cleanName(s string, max int) bool {
	return s != "" && utf8.RuneCountInString(s) <= max && protocol.CleanText(s, len(s)) == s
}

// randomPassword draws n characters uniformly from [A-Za-z0-9].
func randomPassword(n int) string {
	b := make([]byte, n)
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, limit)
		if err != nil {
			panic("crypto/rand: " + err.Error())
		}
		b[i] = passwordAlphabet[v.Int64()]
	}
	return string(b)
}

func (s *Server) auditPlatform(r *http.Request, actor, action, resource, details, result string) {
	if err := s.store.Audit().LogAudit(r.Context(), &store.AuditRecord{
		Scope: "platform", UserID: actor, Action: action, Resource: resource,
		Details: details, Result: result, IPAddress: s.requestIP(r),
	}); err != nil {
		log.Printf("[ADMIN] audit %s for %s not recorded: %v", action, resource, err)
	}
}

func (s *Server) handleAdminListOrganizations(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.store.Tenancy().ListOrganizations(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to list organizations")
		return
	}
	if orgs == nil {
		orgs = []store.OrganizationSummary{}
	}
	s.writeJSON(w, http.StatusOK, orgs)
}

func (s *Server) handleAdminCreateOrganization(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, adminBodyLimit)
	var req struct {
		Name        string `json:"name"`
		AdminUserID string `json:"admin_user_id"`
	}
	if err := strictJSON(r, &req); err != nil || !cleanName(req.Name, 64) || req.AdminUserID == "" {
		s.writeError(w, http.StatusBadRequest, "Organization name must be 1 to 64 printable characters and an administrator is required")
		return
	}
	actor := s.actorID(r)
	org := &store.Organization{ID: "org_" + crypto.RandomHex(12), Name: req.Name}
	err := s.store.Tenancy().CreateOrganizationWithAdmin(r.Context(), org, req.AdminUserID)
	details := "admin=" + protocol.CleanText(req.AdminUserID, 128)
	refuse := func(status int, msg, code string) {
		s.auditPlatform(r, actor, "organization.create", org.Name, details+" refused="+code, "failure")
		s.writeJSON(w, status, map[string]string{"error": msg, "code": code})
	}
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		refuse(http.StatusConflict, "An organization with that name already exists", "organization_exists")
	case errors.Is(err, store.ErrNotFound):
		refuse(http.StatusNotFound, "Administrator user not found", "user_not_found")
	case errors.Is(err, store.ErrInvalid):
		refuse(http.StatusConflict, "Administrator user is not active", "user_inactive")
	case err != nil:
		s.writeError(w, http.StatusInternalServerError, "Failed to create organization")
	default:
		s.auditPlatform(r, actor, "organization.create", org.ID, details, "success")
		s.writeJSON(w, http.StatusCreated, map[string]any{"id": org.ID, "name": org.Name, "created_at": org.CreatedAt})
	}
}

type adminUser struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	SSOProvider string    `json:"sso_provider"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, _, err := s.store.Users().ListUsers(r.Context(), 0, adminUserListMax, "")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to list users")
		return
	}
	out := make([]adminUser, 0, len(users))
	for _, u := range users {
		out = append(out, adminUser{u.ID, u.Username, u.DisplayName, u.Role, u.Status, u.SSOProvider, u.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, adminBodyLimit)
	var req struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
	if err := strictJSON(r, &req); err != nil ||
		!usernameRE.MatchString(req.Username) || !cleanName(req.DisplayName, 128) || (req.Role != "user" && req.Role != "admin") {
		s.writeError(w, http.StatusBadRequest, "Username must be 3 to 64 of [a-z0-9._-], display name 1 to 128 printable characters, role user or admin")
		return
	}
	actor := s.actorID(r)
	details := "role=" + req.Role
	exists := func() bool {
		_, err := s.store.Users().GetUserByUsername(r.Context(), req.Username)
		return err == nil
	}
	refuseExisting := func() {
		s.auditPlatform(r, actor, "user.create", req.Username, details+" refused=username_exists", "failure")
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "That username is taken", "code": "username_exists"})
	}
	if exists() {
		refuseExisting()
		return
	}
	temp := randomPassword(24)
	hash, err := password.Hash(temp)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to create user")
		return
	}
	user := &store.User{
		ID: "usr_" + crypto.RandomHex(12), Username: req.Username, DisplayName: req.DisplayName,
		PasswordHash: hash, Role: req.Role, Status: "active", SSOProvider: "local", MustChangePassword: true,
	}
	if err := s.store.Users().CreateUser(r.Context(), user); err != nil {
		if exists() { // lost a race to a concurrent create
			refuseExisting()
			return
		}
		s.writeError(w, http.StatusInternalServerError, "Failed to create user")
		return
	}
	s.auditPlatform(r, actor, "user.create", user.ID, details, "success")
	s.writeJSON(w, http.StatusCreated, map[string]string{
		"id": user.ID, "username": user.Username, "display_name": user.DisplayName, "role": user.Role, "temporary_password": temp,
	})
}
