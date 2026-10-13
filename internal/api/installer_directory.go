package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kyyard-server/internal/auth"
)

func (s *Server) handleInstallerDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "Method not allowed")
		return
	}
	b, managed, err := s.store.InstallerConfiguration(r.Context())
	if err != nil {
		s.writeError(w, 503, "Directory unavailable")
		return
	}
	if !managed {
		s.writeError(w, 404, "Managed directory unavailable")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		s.writeError(w, 400, "Invalid directory body")
		return
	}
	secret, err := applysetup.Secret(b.SSO.DirectorySecretFile)
	if err != nil {
		s.writeError(w, 503, "Directory credential unavailable")
		return
	}
	event, err := syncauth.Verify([]byte(secret), syncauth.FromRequest(r), raw, syncauth.Options{})
	if err != nil {
		s.writeError(w, 401, "Invalid directory signature")
		return
	}
	var resource struct {
		ID     string `json:"id"`
		Active *bool  `json:"active"`
		Meta   struct {
			Version string `json:"version"`
		} `json:"meta"`
		Roles []struct {
			Value string `json:"value"`
		} `json:"roles"`
	}
	if json.Unmarshal(raw, &resource) != nil || !applysetup.Identifier(resource.ID) || len(resource.ID) > 255 {
		s.writeError(w, 400, "Invalid directory identity")
		return
	}
	revision := strings.TrimSuffix(strings.TrimPrefix(resource.Meta.Version, `W/"`), `"`)
	if resource.Meta.Version != `W/"`+revision+`"` || ((event.Type == "user.created" || event.Type == "user.updated") && resource.Active == nil) {
		s.writeError(w, 400, "Invalid directory revision or assignment")
		return
	}
	admin := false
	for _, role := range resource.Roles {
		if role.Value == "kyyard.admin" {
			admin = true
		}
	}
	active := resource.Active != nil && *resource.Active
	sum := sha256.Sum256(append([]byte(event.Type+"\x00"), raw...))
	if err = s.store.ApplyInstallerDirectory(r.Context(), b.SSO.IssuerURL, resource.ID, event.Type, revision, hex.EncodeToString(sum[:]), active, admin); err != nil {
		s.writeError(w, 422, "Directory revision or assigned identity refused")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInstallerNetwork(w http.ResponseWriter, r *http.Request) {
	trusted := auth.TrustedPeer(r, s.config.Security.TrustedProxies)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if trusted {
		switch r.Header.Get("X-Forwarded-Proto") {
		case "https", "http":
			scheme = r.Header.Get("X-Forwarded-Proto")
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, 200, map[string]any{"clientIp": auth.ClientIP(r, s.config.Security.TrustedProxies), "proxyHeadersTrusted": trusted, "scheme": scheme})
}
