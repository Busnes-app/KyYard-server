package backup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/backup"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// sqliteInstance is a fresh SQLite store in a temp data dir, the way every backup adapter
// test needs one.
func sqliteInstance(t *testing.T) (*config.Config, store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.AppName = "busnes_app"
	cfg.Server.Port = 8080
	cfg.Database.Driver = "sqlite"
	cfg.Database.DataDir = dir
	cfg.Database.DSN = filepath.Join(dir, "ky_server.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	cfg.Security.EncryptionKey = bytes.Repeat([]byte{1}, 32)
	cfg.Security.SessionSecret = strings.Repeat("02", 32)
	cfg.Security.InstanceKey = bytes.Repeat([]byte{3}, 32)
	st, err := store.Open(context.Background(), cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return cfg, st
}

func TestSettingsAdapterMapsNotFound(t *testing.T) {
	_, st := sqliteInstance(t)
	s := backup.Settings(context.Background(), st.Settings())
	if _, err := s.Get("nope"); !errors.Is(err, recoveryclient.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	_ = s.Set("a", "1")
	if v, _ := s.Get("a"); v != "1" {
		t.Fatal("set/get")
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("a"); !errors.Is(err, recoveryclient.ErrNotFound) {
		t.Fatal("delete")
	}
}

func TestSealerRoundTripUnderDeploymentKey(t *testing.T) {
	cfg := &config.Config{}
	cfg.Security.EncryptionKey = bytes.Repeat([]byte{1}, 32)
	s, err := backup.NewSealer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Seal([]byte("tok"))
	p, err := s.Open(c)
	if err != nil || string(p) != "tok" {
		t.Fatal(err)
	}
}

// Fixture generated with v0.5.0 StoreRecoveryKey/StorePairing, a synthetic token,
// and a deployment key of 32 bytes of 0x01. No recovery private key is retained.
func TestBaseV050PairingRequiresBaseSealer(t *testing.T) {
	cfg, st := sqliteInstance(t)
	raw, err := os.ReadFile("testdata/pairing-v050.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version   string
		Settings  map[string]string
		PublicKey []byte
		KeyID     string
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	settings := backup.Settings(context.Background(), st.Settings())
	for key, value := range fixture.Settings {
		if err := settings.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	path := recoveryclient.RecoveryKeyPath(cfg.Database.DataDir)
	if err := os.WriteFile(path, fixture.PublicKey, 0600); err != nil {
		t.Fatal(err)
	}
	sealer, err := backup.NewSealer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryclient.LoadPairing(cfg.Database.DataDir, settings, sealer); err == nil {
		t.Fatal("KyYard accepted a base-project pairing token")
	}
	// Historical fixture: prove its original identity is intact, without making
	// the product accept another application's encrypted settings.
	sealer, err = recoveryclient.NewAESGCMSealer(cfg.Security.EncryptionKey, "ky_server_base:setting:kyrecovery_token")
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := recoveryclient.LoadPairing(cfg.Database.DataDir, settings, sealer)
	if err != nil {
		t.Fatal(err)
	}
	if pairing.URL != "https://recovery.example.test" || pairing.Token != "synthetic-v050-token" || pairing.Key.Public.ID() != fixture.KeyID || pairing.Key.Threshold != 2 || pairing.Key.TotalShares != 3 {
		t.Fatal("pairing identity changed")
	}
	after := map[string]string{}
	for key := range fixture.Settings {
		after[key], err = settings.Get(key)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !maps.Equal(after, fixture.Settings) {
		t.Fatal("loading rewrote pairing settings")
	}
	pub, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(pub, fixture.PublicKey) {
		t.Fatal("loading rewrote public key")
	}
}
