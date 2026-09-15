package backup

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky_server_base/internal/config"
	_ "modernc.org/sqlite"
)

// encryptionKeyPath is where a restore drops the key that decrypts users.totp_secret_enc,
// relative to the restore target: the same <DataDir>/encryption.key config.LoadFromEnv reads.
const encryptionKeyPath = "data/encryption.key"

// recoveryPubPath is where a restore drops the suite recovery public key, matching the
// <DataDir>/recovery.pub that recoveryclient.RecoveryKeyPath reads.
const recoveryPubPath = "data/recovery.pub"

// ErrNoDatabaseSnapshot is returned when the payload cannot carry a consistent copy of the
// database, so a capsule without one is never sealed as if it were a backup.
var ErrNoDatabaseSnapshot = errors.New("backup: no consistent database snapshot for this driver")

// Collect assembles the payload every sealing caller uses: the local application files
// (SQLite database, configuration) plus the members that may only ever travel inside a
// sealed capsule (the encryption key, the pinned recovery public key). Nothing that returns
// from here may leave the process except through a Sealer.
func Collect(ctx context.Context, cfg *config.Config, appVersion string) (recoveryclient.Payload, error) {
	if strings.ToLower(cfg.Database.Driver) != "sqlite" {
		return recoveryclient.Payload{}, fmt.Errorf("%w: %s", ErrNoDatabaseSnapshot, cfg.Database.Driver)
	}
	dbBytes, err := snapshotSQLite(ctx, cfg.Database.DSN, cfg.Database.DataDir)
	if err != nil {
		return recoveryclient.Payload{}, err
	}
	const dbPath = "data/ky_server.db"
	files := []recoveryclient.File{{Path: dbPath, Data: dbBytes, Mode: 0600}}
	sqlitePaths := []string{dbPath}

	cfgJSON, _ := json.MarshalIndent(map[string]any{
		"server":   cfg.Server,
		"database": map[string]any{"driver": cfg.Database.Driver},
	}, "", "  ")
	files = append(files, recoveryclient.File{Path: "config/settings.json", Data: cfgJSON, Mode: 0600})

	if len(cfg.Security.EncryptionKey) != 32 {
		return recoveryclient.Payload{}, fmt.Errorf("backup: encryption key is %d bytes, want 32; refusing to seal a capsule that cannot decrypt what it restores", len(cfg.Security.EncryptionKey))
	}
	files = append(files, recoveryclient.File{
		Path: encryptionKeyPath,
		Data: []byte(hex.EncodeToString(cfg.Security.EncryptionKey) + "\n"),
		Mode: 0600,
	})

	if pub, err := os.ReadFile(recoveryclient.RecoveryKeyPath(cfg.Database.DataDir)); err == nil {
		files = append(files, recoveryclient.File{Path: recoveryPubPath, Data: pub, Mode: 0600})
	}

	payload := recoveryclient.Payload{
		ServiceName: cfg.Server.AppName,
		AppVersion:  appVersion,
		Files:       files,
		Dependencies: map[string]any{
			"ports": []int{cfg.Server.Port},
			"env":   []string{"KY_PORT", "KY_DB_DRIVER"},
		},
		VerificationRecipe: map[string]any{
			"check_sqlite_integrity": true,
			"sqlite_paths":           sqlitePaths,
			"required_files":         requiredFiles(files),
			"expected_env":           []string{"KY_PORT", "KY_DB_DRIVER"},
			"expected_ports":         []int{cfg.Server.Port},
		},
	}
	return payload, nil
}

// snapshotSQLite returns a consistent single-file copy of the live database. The store runs
// in WAL mode, so reading the main file misses every commit still in the -wal and can tear
// under a concurrent checkpoint; the lib's SQLiteSnapshot runs VACUUM INTO through a live
// connection. The scaffold opens its own handle from the DSN because store.Store exposes no
// *sql.DB.
func snapshotSQLite(ctx context.Context, dsn, dataDir string) ([]byte, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	dir, err := os.MkdirTemp(dataDir, "snapshot-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "ky_server.db")
	if err := recoveryclient.SQLiteSnapshot(ctx, db, path); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDatabaseSnapshot, err)
	}
	return os.ReadFile(path)
}

// Members names what a capsule carries, for the screen; it is what Collect would seal now.
func Members(cfg *config.Config) []string {
	m := []string{"data/ky_server.db", "config/settings.json", encryptionKeyPath}
	if _, err := os.Stat(recoveryclient.RecoveryKeyPath(cfg.Database.DataDir)); err == nil {
		m = append(m, recoveryPubPath)
	}
	return m
}

func requiredFiles(files []recoveryclient.File) []string {
	req := make([]string, 0, len(files))
	for _, f := range files {
		req = append(req, f.Path)
	}
	return req
}
