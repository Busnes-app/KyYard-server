package store_test

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/backup"
)

// restoreThroughCapsule seals the payload to a throwaway key, opens it with the product's
// drill checks, and returns a copy of the restored database and the key it carried.
func restoreThroughCapsule(t *testing.T, payload recoveryclient.Payload) (string, []byte) {
	t.Helper()
	// The drill checks the restoring host names the variables the recipe expects.
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	path := filepath.Join(t.TempDir(), "ky_server.db")
	var key []byte
	result, err := recoveryclient.Drill(context.Background(), t.TempDir(), payload, func(dir string, opened capsule.Manifest) []recoveryclient.Check {
		checks := backup.Checks(dir, opened)
		copied := recoveryclient.Check{Name: "Copy out", Passed: true}
		db, err := os.ReadFile(filepath.Join(dir, "data", "ky_server.db"))
		if err == nil {
			err = os.WriteFile(path, db, 0600)
		}
		var raw []byte
		if err == nil {
			raw, err = os.ReadFile(filepath.Join(dir, "data", "encryption.key"))
		}
		if err == nil {
			key, err = hex.DecodeString(strings.TrimSpace(string(raw)))
		}
		if err != nil {
			copied.Passed, copied.Message = false, err.Error()
		}
		return append(checks, copied)
	})
	mustTenant(t, err)
	if !result.Passed || len(key) != 32 {
		t.Fatalf("capsule round trip failed: %+v", result)
	}
	return path, key
}
