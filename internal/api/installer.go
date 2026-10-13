package api

import (
	"context"
	"errors"
	"strings"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/kyyard-server/internal/sso"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func (s *Server) validateInstallerRuntime(b applysetup.Common) error {
	if err := store.ValidateInstallerBundle(b); err != nil {
		return err
	}
	if s.config.SSO.Enabled || s.config.SCIM.Enabled {
		return errors.New("installer refuses independent SSO or retired SCIM configuration")
	}
	if strings.TrimRight(s.config.Server.AppURL, "/")+"/api/sso/kyidentity/callback" != b.SSO.RedirectURI {
		return errors.New("installer callback conflicts with configured KyYard origin")
	}
	if _, err := applysetup.Secret(b.SSO.ClientSecretFile); err != nil {
		return err
	}
	if _, err := applysetup.Secret(b.SSO.DirectorySecretFile); err != nil {
		return err
	}
	return nil
}

// Native provider readers consume this immutable record immediately; no restart
// or second database writer is needed to activate the configuration.
func (s *Server) ApplyInstaller(ctx context.Context, raw []byte, check bool) (applysetup.Report, error) {
	var b applysetup.Common
	if err := applysetup.DecodeRaw(raw, &b); err != nil {
		return applysetup.Report{}, err
	}
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	if err := s.validateInstallerRuntime(b); err != nil {
		return applysetup.Report{}, err
	}
	return s.store.ApplyInstaller(ctx, b, check)
}

func (s *Server) installerProvider(ctx context.Context) (sso.Provider, bool, error) {
	b, managed, err := s.store.InstallerConfiguration(ctx)
	if err != nil || !managed {
		return sso.Provider{}, managed, err
	}
	if err = s.validateInstallerRuntime(b); err != nil {
		return sso.Provider{}, true, err
	}
	secret, err := applysetup.Secret(b.SSO.ClientSecretFile)
	if err != nil {
		return sso.Provider{}, true, err
	}
	return sso.Provider{ID: store.InstallerProvider, Name: "KyIdentity", Kind: "oidc", Issuer: b.SSO.IssuerURL, ClientID: b.SSO.ClientID, ClientSecret: secret, Enabled: true, AutoProvision: false, InstallerManaged: true}, true, nil
}
