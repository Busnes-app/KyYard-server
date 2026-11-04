package backup_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/backup"
)

// A database larger than one capsule member travels as ordered parts and the drill checks the
// database a restore joins from them.
func TestALargeDatabaseTravelsInPartsAndPassesTheDrill(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	defer backup.SetPartBytesForTest(4096)()
	cfg, _ := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	parts := 0
	for _, f := range payload.Files {
		if f.Path == "data/ky_server.db" {
			t.Fatal("the database travelled whole")
		}
		if strings.HasPrefix(f.Path, "data/ky_server.db.part-") {
			parts++
			if len(f.Data) > 4096 {
				t.Fatalf("%s is %d bytes", f.Path, len(f.Data))
			}
		}
	}
	if parts < 2 {
		t.Fatalf("%d parts", parts)
	}
	result, err := backup.RunDrill(context.Background(), cfg, payload)
	if err != nil || !result.Passed {
		t.Fatalf("drill: %+v %v", result, err)
	}
}

// JoinDatabase joins parts in order and refuses a gap or a whole database beside them.
func TestJoinDatabase(t *testing.T) {
	write := func(dir string, files map[string]string) {
		for name, data := range files {
			p := filepath.Join(dir, name)
			_ = os.MkdirAll(filepath.Dir(p), 0700)
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	dir := t.TempDir()
	write(dir, map[string]string{"data/ky_server.db.part-000": "ab", "data/ky_server.db.part-001": "cd", "data/ky_server.db.part-002": "e"})
	if joined, err := backup.JoinDatabase(dir, true); err != nil || !joined {
		t.Fatalf("join: %t %v", joined, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "data/ky_server.db"))
	if err != nil || string(got) != "abcde" {
		t.Fatalf("joined %q %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "data/ky_server.db.part-*")); len(left) != 0 {
		t.Fatalf("parts left: %v", left)
	}
	if joined, err := backup.JoinDatabase(dir, true); err != nil || joined {
		t.Fatalf("a whole database is not joined again: %t %v", joined, err)
	}
	for _, files := range []map[string]string{
		{"data/ky_server.db.part-000": "a", "data/ky_server.db.part-002": "c"},
		{"data/ky_server.db": "x", "data/ky_server.db.part-000": "a"},
	} {
		dir := t.TempDir()
		write(dir, files)
		if _, err := backup.JoinDatabase(dir, true); err == nil {
			t.Fatalf("joined %v", files)
		}
	}
}
