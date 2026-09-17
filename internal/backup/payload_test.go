package backup_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/backup"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// payloadConfig is a real SQLite store in a temp data dir: the collectors snapshot the live
// database, so there has to be one.
func payloadConfig(t *testing.T) (*config.Config, []byte) {
	t.Helper()
	cfg, _ := sqliteInstance(t)
	return cfg, cfg.Security.EncryptionKey
}

// The encryption key must ride in the capsule: users.totp_secret_enc is AES-GCM under it, so
// a restore without it hands the operator a database whose MFA secrets are gone for good.
func TestCollectCarriesTheEncryptionKey(t *testing.T) {
	cfg, key := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range payload.Files {
		if f.Path != "data/encryption.key" {
			continue
		}
		found = true
		if want := hex.EncodeToString(key) + "\n"; string(f.Data) != want {
			t.Errorf("content: got %q, want the lowercase hex keyfile reads", f.Data)
		}
		if f.Mode != 0600 {
			t.Errorf("mode: got %o, want 600", f.Mode)
		}
	}
	if !found {
		t.Fatal("payload has no data/encryption.key")
	}
	req, _ := payload.VerificationRecipe["required_files"].([]string)
	if !slices.Contains(req, "data/encryption.key") {
		t.Errorf("required_files: got %v, want data/encryption.key among them", req)
	}
}

// A restore must come back paired, not half-paired: recovery.pub is public and the capsule is
// sealed to that very key, so it rides along whenever the instance has one.
func TestCollectCarriesTheRecoveryPublicKeyWhenPaired(t *testing.T) {
	cfg, _ := payloadConfig(t)
	ctx := context.Background()

	unpaired, err := backup.Collect(ctx, cfg, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if f := findFile(unpaired.Files, "data/recovery.pub"); f != nil {
		t.Error("unpaired instance shipped a recovery.pub")
	}
	if req, _ := unpaired.VerificationRecipe["required_files"].([]string); slices.Contains(req, "data/recovery.pub") {
		t.Errorf("unpaired required_files: got %v", req)
	}

	pubPath := recoveryclient.RecoveryKeyPath(cfg.Database.DataDir)
	if err := os.MkdirAll(filepath.Dir(pubPath), 0700); err != nil {
		t.Fatal(err)
	}
	pub := []byte("a-recovery-public-key")
	if err := os.WriteFile(pubPath, pub, 0600); err != nil {
		t.Fatal(err)
	}
	paired, err := backup.Collect(ctx, cfg, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	f := findFile(paired.Files, "data/recovery.pub")
	if f == nil {
		t.Fatal("paired instance has no data/recovery.pub in the payload")
	}
	if string(f.Data) != string(pub) {
		t.Error("data/recovery.pub is not the pinned public key byte for byte")
	}
	if f.Mode != 0600 {
		t.Errorf("mode: got %o, want 600", f.Mode)
	}
	if req, _ := paired.VerificationRecipe["required_files"].([]string); !slices.Contains(req, "data/recovery.pub") {
		t.Errorf("required_files: got %v, want data/recovery.pub among them", req)
	}
}

func findFile(files []recoveryclient.File, path string) *recoveryclient.File {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
	}
	return nil
}

func TestCollectRefusesAShortKey(t *testing.T) {
	cfg, _ := payloadConfig(t)
	cfg.Security.EncryptionKey = cfg.Security.EncryptionKey[:16]
	if _, err := backup.Collect(context.Background(), cfg, "1.0.0"); err == nil {
		t.Fatal("a 16-byte encryption key was accepted")
	}
}

// The store runs in WAL mode, so a plain read of the main file misses every commit still in
// the -wal. The snapshot must carry a row committed moments ago and never checkpointed.
func TestSnapshotSeesUncheckpointedCommit(t *testing.T) {
	cfg, st := sqliteInstance(t)
	ctx := context.Background()
	if err := st.Settings().SetSetting(ctx, "canary", "still-in-the-wal"); err != nil {
		t.Fatal(err)
	}
	payload, err := backup.Collect(ctx, cfg, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	f := findFile(payload.Files, "data/ky_server.db")
	if f == nil {
		t.Fatal("no data/ky_server.db in the payload")
	}
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restored, f.Data, 0600); err != nil {
		t.Fatal(err)
	}
	copyStore, err := store.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: restored})
	if err != nil {
		t.Fatalf("snapshot does not open: %v", err)
	}
	defer copyStore.Close()
	if got, err := copyStore.Settings().GetSetting(ctx, "canary"); err != nil || got != "still-in-the-wal" {
		t.Fatalf("snapshot lacks the uncheckpointed row: %q, %v", got, err)
	}
	if check, _ := payload.VerificationRecipe["check_sqlite_integrity"].(bool); !check {
		t.Error("recipe does not ask the drill to check the database")
	}
}

// A driver the collectors cannot snapshot must refuse, not seal a keys-and-config capsule
// that a receipt would then call a backup.
func TestCollectRefusesADriverItCannotSnapshot(t *testing.T) {
	cfg, _ := payloadConfig(t)
	cfg.Database.Driver = "postgres"
	if _, err := backup.Collect(context.Background(), cfg, "1.0.0"); !errors.Is(err, backup.ErrNoDatabaseSnapshot) {
		t.Fatalf("got %v, want ErrNoDatabaseSnapshot", err)
	}
}

func TestRestorePreservesKeysAndRevokesOnlySnapshotGrants(t *testing.T) {
	cfg, st := sqliteInstance(t)
	ctx := context.Background()
	user := &store.User{ID: "restore-user", Username: "restore-user", Status: "active", Role: "admin", SSOProvider: "local", PasswordHash: "hash"}
	if err := st.Users().CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.Sessions().CreateSession(ctx, &store.Session{TokenHash: "session", UserID: user.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, user.PasswordHash); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions().CreateMFAChallenge(ctx, &store.MFAChallenge{TokenHash: "challenge", UserID: user.ID, ExpiresAt: now.Add(time.Hour)}, user.PasswordHash); err != nil {
		t.Fatal(err)
	}
	if err := st.Devices().CreatePairing(ctx, &store.DevicePairing{Secret: "pair", Code: "123456", UserID: user.ID, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	payload, err := backup.Collect(ctx, cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, file := range payload.Files {
		path := filepath.Join(root, file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.Data, os.FileMode(file.Mode)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KY_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("KY_ENCRYPTION_KEY", "")
	t.Setenv("KY_SESSION_SECRET", "")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	restored, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Security.SessionSecret != cfg.Security.SessionSecret || !bytes.Equal(restored.Security.InstanceKey, cfg.Security.InstanceKey) || !bytes.Equal(restored.Security.EncryptionKey, cfg.Security.EncryptionKey) {
		t.Fatal("restored keys differ from active keys")
	}
	copyStore, err := store.Open(ctx, restored.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer copyStore.Close()
	if _, err := copyStore.Sessions().GetSession(ctx, "session"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("restored session survived", err)
	}
	if _, _, err := copyStore.Sessions().ConsumeMFAChallenge(ctx, "challenge"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("restored challenge survived", err)
	}
	if _, err := copyStore.Devices().GetPairingBySecret(ctx, "pair"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("restored pairing survived", err)
	}
	if _, err := st.Sessions().GetSession(ctx, "session"); err != nil {
		t.Fatal("live session was revoked", err)
	}
	if _, _, err := st.Sessions().ConsumeMFAChallenge(ctx, "challenge"); err != nil {
		t.Fatal("live challenge was revoked", err)
	}
	if _, err := st.Devices().GetPairingBySecret(ctx, "pair"); err != nil {
		t.Fatal("live pairing was revoked", err)
	}
}
