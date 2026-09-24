package backup_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/backup"
	"github.com/Busnes-app/kyyard-server/internal/store/migrations"
)

func TestChecksFailsOnAScratchDirMissingTheDatabase(t *testing.T) {
	cfg, _ := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	scratch := t.TempDir()
	// Recreate every required file except the database, so the missing member is the only
	// difference from a real drill's scratch directory.
	for _, f := range payload.Files {
		if f.Path == "data/ky_server.db" {
			continue
		}
		full := filepath.Join(scratch, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	checks := backup.Checks(scratch, manifestFor(payload))
	var sawMissing bool
	for _, c := range checks {
		if c.Name == "Required File: data/ky_server.db" {
			sawMissing = true
			if c.Passed {
				t.Error("missing database reported as passed")
			}
		}
		if c.Passed && c.Name == "Required Files" {
			t.Error("Required Files reported all-present with the database missing")
		}
	}
	for _, check := range checks {
		if check.Name == "SQLite Integrity: data/ky_server.db" && check.Passed {
			t.Error("missing SQLite database passed integrity checking")
		}
	}
	if _, err := os.Stat(filepath.Join(scratch, "data/ky_server.db")); !os.IsNotExist(err) {
		t.Fatalf("integrity check created the missing database: %v", err)
	}
	if !sawMissing {
		t.Fatal("no check reported the missing database member")
	}
}

func TestChecksPassesOnACompleteScratchDir(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	cfg, _ := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	for _, f := range payload.Files {
		full := filepath.Join(scratch, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	checks := backup.Checks(scratch, manifestFor(payload))
	if len(checks) == 0 {
		t.Fatal("no checks ran")
	}
	for _, c := range checks {
		if !c.Passed {
			t.Errorf("check %q failed: %s", c.Name, c.Message)
		}
	}
}

func TestDrillRootIsUnderTheDataDir(t *testing.T) {
	cfg, _ := payloadConfig(t)
	root := backup.DrillRoot(cfg)
	rel, err := filepath.Rel(cfg.Database.DataDir, root)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("drill root %q is not under data dir %q", root, cfg.Database.DataDir)
	}
}

func manifestFor(payload recoveryclient.Payload) capsule.Manifest {
	m := capsule.Manifest{UnverifiedManifest: capsule.UnverifiedManifest{VerificationRecipe: payload.VerificationRecipe}}
	for _, f := range payload.Files {
		m.Files = append(m.Files, capsule.FileEntry{Path: f.Path})
	}
	return m
}

func TestDrillChecksDecodedManifest(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	cfg, _ := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	root := backup.DrillRoot(cfg)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	var scratch string
	result, err := recoveryclient.Drill(context.Background(), root, payload, func(dir string, opened capsule.Manifest) []recoveryclient.Check {
		scratch = dir
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("scratch permissions %o", info.Mode().Perm())
		}
		recipe, ok := opened.VerificationRecipe.(map[string]any)
		if !ok {
			t.Fatalf("recipe type %T", opened.VerificationRecipe)
		}
		if _, ok := recipe["required_files"].([]any); !ok {
			t.Fatalf("list was not JSON decoded: %T", recipe["required_files"])
		}
		// Mutating the original recipe cannot change what the opened capsule checks.
		payload.VerificationRecipe["required_files"] = nil
		return backup.Checks(dir, opened)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("drill failed: %+v", result)
	}
	for _, name := range []string{"Required Files", "SQLite Integrity: data/ky_server.db", "Schema Version: data/ky_server.db", "Environment: KY_PORT", "Environment: KY_DB_DRIVER"} {
		found := false
		for _, c := range result.Checks {
			if c.Name == name && c.Passed {
				found = true
			}
		}
		if !found {
			t.Errorf("missing passing check %q: %+v", name, result.Checks)
		}
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch survived: %v", err)
	}
}

func TestDrillRejectsMalformedRecipes(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	cfg, _ := payloadConfig(t)
	original, err := backup.Collect(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"missing required":     func(r map[string]any) { delete(r, "required_files") },
		"null required":        func(r map[string]any) { r["required_files"] = nil },
		"empty required":       func(r map[string]any) { r["required_files"] = []string{} },
		"wrong required type":  func(r map[string]any) { r["required_files"] = "data/ky_server.db" },
		"mixed required":       func(r map[string]any) { r["required_files"] = []any{"data/ky_server.db", 42} },
		"omitted key":          func(r map[string]any) { r["required_files"] = []string{"data/ky_server.db", "config/settings.json"} },
		"missing sqlite flag":  func(r map[string]any) { delete(r, "check_sqlite_integrity") },
		"false sqlite flag":    func(r map[string]any) { r["check_sqlite_integrity"] = false },
		"wrong sqlite flag":    func(r map[string]any) { r["check_sqlite_integrity"] = "true" },
		"missing sqlite paths": func(r map[string]any) { delete(r, "sqlite_paths") },
		"empty sqlite paths":   func(r map[string]any) { r["sqlite_paths"] = []string{} },
		"null sqlite paths":    func(r map[string]any) { r["sqlite_paths"] = nil },
		"mixed sqlite paths":   func(r map[string]any) { r["sqlite_paths"] = []any{"data/ky_server.db", false} },
		"sqlite omits db":      func(r map[string]any) { r["sqlite_paths"] = []string{"config/settings.json"} },
		"missing env":          func(r map[string]any) { delete(r, "expected_env") },
		"empty env":            func(r map[string]any) { r["expected_env"] = []string{} },
		"null env":             func(r map[string]any) { r["expected_env"] = nil },
		"wrong env":            func(r map[string]any) { r["expected_env"] = true },
		"mixed env":            func(r map[string]any) { r["expected_env"] = []any{"KY_PORT", 1} },
		"omitted env":          func(r map[string]any) { r["expected_env"] = []string{"KY_PORT"} },
		"missing schema":       func(r map[string]any) { delete(r, "schema_version") },
		"string schema":        func(r map[string]any) { r["schema_version"] = fmt.Sprint(migrations.Latest()) },
		"fractional schema":    func(r map[string]any) { r["schema_version"] = float64(migrations.Latest()) + 0.5 },
	}
	for _, path := range []string{"", ".", "../outside", "/etc/passwd", "data/../data/ky_server.db", "data//ky_server.db", "data\\ky_server.db", "data/not-in-manifest", "data/\x00db"} {
		cases["unsafe path "+path] = func(r map[string]any) {
			r["required_files"] = append(append([]string{}, r["required_files"].([]string)...), path)
		}
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			payload := original
			payload.VerificationRecipe = maps.Clone(original.VerificationRecipe)
			mutate(payload.VerificationRecipe)
			result, err := backup.RunDrill(context.Background(), cfg, payload)
			if err != nil {
				t.Fatal(err)
			}
			if result.Passed {
				t.Fatalf("malformed recipe passed: %+v", result)
			}
			var opened, failed bool
			for _, c := range result.Checks {
				if c.Name == "Directory Unpack" && c.Passed {
					opened = true
				}
				if c.Name == "Verification Recipe" && !c.Passed {
					failed = true
					if strings.HasSuffix(name, "schema") && c.Message != "schema_version must be the latest migration number" {
						t.Fatalf("schema message: %q", c.Message)
					}
				}
			}
			if !opened || !failed {
				t.Fatalf("did not fail the opened recipe: %+v", result)
			}
			entries, err := os.ReadDir(backup.DrillRoot(cfg))
			if err != nil || len(entries) != 0 {
				t.Fatalf("scratch not cleaned: %v %v", entries, err)
			}
		})
	}
	for _, recipe := range []any{nil, "invalid", []any{}} {
		m := manifestFor(original)
		m.VerificationRecipe = recipe
		checks := backup.Checks(t.TempDir(), m)
		if len(checks) != 1 || checks[0].Passed {
			t.Fatalf("nonobject recipe passed: %+v", checks)
		}
	}
}

func TestDrillRejectsDamagedPayload(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	for _, kind := range []string{"missing database", "empty database", "corrupt database", "missing environment", "corrupt session key", "corrupt instance key"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := payloadConfig(t)
			payload, err := backup.Collect(context.Background(), cfg, "test")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing environment" {
				payload.VerificationRecipe["expected_env"] = []string{"KY_PORT", "KY_DB_DRIVER", "KY_DRILL_TEST_MISSING"}
				t.Setenv("KY_DRILL_TEST_MISSING", "temporary")
				os.Unsetenv("KY_DRILL_TEST_MISSING")
			}
			for i, f := range payload.Files {
				if (kind == "corrupt session key" && f.Path == "data/session.key") || (kind == "corrupt instance key" && f.Path == "data/instance.key") {
					payload.Files[i].Data = []byte("not a valid key")
				}
				if f.Path == "data/ky_server.db" {
					switch kind {
					case "missing database":
						payload.Files = append(payload.Files[:i:i], payload.Files[i+1:]...)
					case "empty database":
						payload.Files[i].Data = nil
					case "corrupt database":
						payload.Files[i].Data = []byte("not a sqlite database")
					}
					if kind != "corrupt session key" && kind != "corrupt instance key" {
						break
					}
				}
			}
			result, err := backup.RunDrill(context.Background(), cfg, payload)
			if err != nil {
				t.Fatal(err)
			}
			if result.Passed {
				t.Fatalf("damaged payload passed: %+v", result)
			}
		})
	}
}

func TestChecksSQLiteFilenameIsNotADSN(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	cfg, _ := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	const name = "data/extra?mode=rw#database.db"
	payload.Files = append(payload.Files, recoveryclient.File{Path: name, Data: payload.Files[0].Data, Mode: 0600})
	payload.VerificationRecipe["required_files"] = append(payload.VerificationRecipe["required_files"].([]string), name)
	payload.VerificationRecipe["sqlite_paths"] = []string{"data/ky_server.db", name}
	result, err := backup.RunDrill(context.Background(), cfg, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("escaped filename failed: %+v", result)
	}
}

// A capsule from another schema must fail the drill, not migrate silently on first start.
func TestDrillChecksSchemaVersion(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	cfg, _ := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	for _, f := range payload.Files {
		full := filepath.Join(scratch, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(scratch, "data/ky_server.db")
	exec := func(query string, args ...any) {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	schemaCheck := func() (int, recoveryclient.Check) {
		t.Helper()
		checks := backup.Checks(scratch, manifestFor(payload))
		for i, c := range checks {
			if c.Name == "Schema Version: data/ky_server.db" {
				if i == 0 || checks[i-1].Name != "SQLite Integrity: data/ky_server.db" {
					t.Fatalf("schema check does not follow the database integrity check: %+v", checks)
				}
				return i, c
			}
		}
		t.Fatalf("no schema version check: %+v", checks)
		return 0, recoveryclient.Check{}
	}
	latest := migrations.Latest()

	if _, c := schemaCheck(); !c.Passed {
		t.Fatalf("current schema failed: %s", c.Message)
	}

	exec("DELETE FROM schema_migrations WHERE version = ?", latest)
	if _, c := schemaCheck(); c.Passed || c.Message != fmt.Sprintf("database schema is version %d, capsule expects %d", latest-1, latest) {
		t.Fatalf("behind schema: %+v", c)
	}

	exec("INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, 'x', CURRENT_TIMESTAMP), (?, 'future', CURRENT_TIMESTAMP)", latest, latest+1)
	if _, c := schemaCheck(); c.Passed || c.Message != fmt.Sprintf("database schema is version %d, capsule expects %d", latest+1, latest) {
		t.Fatalf("ahead schema: %+v", c)
	}

	exec("DROP TABLE schema_migrations")
	if _, c := schemaCheck(); c.Passed || !strings.Contains(c.Message, "schema_migrations") {
		t.Fatalf("missing table: %+v", c)
	}
}
