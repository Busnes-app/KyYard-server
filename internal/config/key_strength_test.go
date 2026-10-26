package config_test

import (
	"bytes"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectorySecretFloorPreservesKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KY_DATA_DIR", dir)
	if _, err := config.LoadFromEnv(); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range []string{"session.key", "encryption.key", "instance.key"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
	}
	for _, size := range []int{15, 16, 32} {
		t.Setenv("KY_KYSIGNON_HMAC_SECRET", strings.Repeat("s", size))
		_, err := config.LoadFromEnv()
		if (err != nil) != (size < 16) {
			t.Errorf("directory secret length %d: %v", size, err)
		}
		if err != nil && !strings.Contains(err.Error(), "KY_KYSIGNON_HMAC_SECRET") {
			t.Errorf("error does not identify its field: %v", err)
		}
	}
	for name, raw := range before {
		after, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatalf("directory secret validation changed %s", name)
		}
	}
}
