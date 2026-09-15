package backup

import (
	"context"
	"errors"

	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky_server_base/internal/config"
	"github.com/Busness-app/ky_server_base/internal/store"
)

const recoveryTokenLabel = "ky_server_base:setting:kyrecovery_token"

type settingsAdapter struct {
	ctx context.Context
	s   store.SettingsStore
}

// Settings binds the request context to the settings store for one call into the lib.
func Settings(ctx context.Context, s store.SettingsStore) recoveryclient.Settings {
	return settingsAdapter{ctx: ctx, s: s}
}

func (a settingsAdapter) Get(key string) (string, error) {
	v, err := a.s.GetSetting(a.ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", recoveryclient.ErrNotFound
	}
	return v, err
}
func (a settingsAdapter) Set(key, val string) error { return a.s.SetSetting(a.ctx, key, val) }
func (a settingsAdapter) Delete(key string) error   { return a.s.DeleteSetting(a.ctx, key) }

// NewSealer seals the KyRecovery token under the deployment key, domain-separated so a row
// copied from another setting will not open.
func NewSealer(cfg *config.Config) (recoveryclient.Sealer, error) {
	return recoveryclient.NewAESGCMSealer(cfg.Security.EncryptionKey, recoveryTokenLabel)
}

// RunConfig is what Run needs from the scaffold's configuration.
func RunConfig(cfg *config.Config, appVersion string) (recoveryclient.RunConfig, error) {
	sealer, err := NewSealer(cfg)
	if err != nil {
		return recoveryclient.RunConfig{}, err
	}
	return recoveryclient.RunConfig{
		DataDir: cfg.Database.DataDir, AppName: cfg.Server.AppName, AppVersion: appVersion,
		BackupDir: cfg.Backup.Dir, Keep: cfg.Backup.Keep, Sealer: sealer,
	}, nil
}
