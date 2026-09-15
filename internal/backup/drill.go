package backup

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Busness-app/ky-primitives/capsule"
	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/ky_server_base/internal/config"
	_ "modernc.org/sqlite"
)

// Checks validates the recipe from the capsule that was actually opened. A malformed
// recipe is a failed drill, never permission to omit a required product check.
func Checks(dir string, opened capsule.Manifest) []recoveryclient.Check {
	recipe, ok := opened.VerificationRecipe.(map[string]any)
	if !ok {
		return recipeFailure("Expected a recipe object")
	}
	required, err := recipeStrings(recipe["required_files"])
	if err != nil {
		return recipeFailure("required_files: " + err.Error())
	}
	sqlitePaths, err := recipeStrings(recipe["sqlite_paths"])
	if err != nil {
		return recipeFailure("sqlite_paths: " + err.Error())
	}
	env, err := recipeStrings(recipe["expected_env"])
	if err != nil {
		return recipeFailure("expected_env: " + err.Error())
	}
	if enabled, ok := recipe["check_sqlite_integrity"].(bool); !ok || !enabled {
		return recipeFailure("check_sqlite_integrity must be true")
	}
	for _, name := range []string{"data/ky_server.db", "config/settings.json", encryptionKeyPath} {
		if !slices.Contains(required, name) {
			return recipeFailure("required_files omits " + name)
		}
	}
	if !slices.Contains(sqlitePaths, "data/ky_server.db") {
		return recipeFailure("sqlite_paths omits the database")
	}
	for _, name := range []string{"KY_PORT", "KY_DB_DRIVER"} {
		if !slices.Contains(env, name) {
			return recipeFailure("expected_env omits " + name)
		}
	}
	members := make(map[string]bool, len(opened.Files))
	for _, file := range opened.Files {
		members[file.Path] = true
		if !slices.Contains(required, file.Path) {
			return recipeFailure("required_files omits a capsule member")
		}
	}
	for _, paths := range [][]string{required, sqlitePaths} {
		for _, name := range paths {
			if _, safe := drillPath(dir, name); !safe || !members[name] {
				return recipeFailure("File check must name a clean relative capsule member")
			}
		}
	}
	var checks []recoveryclient.Check
	allFound := true
	for _, name := range required {
		full, _ := drillPath(dir, name)
		fi, err := os.Lstat(full)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			allFound = false
			checks = append(checks, recoveryclient.Check{Name: "Required File: " + name, Message: "File missing, empty or not regular"})
		}
	}
	if allFound {
		checks = append(checks, recoveryclient.Check{Name: "Required Files", Passed: true, Message: fmt.Sprintf("All %d required files verified", len(required))})
	}
	for _, name := range sqlitePaths {
		full, _ := drillPath(dir, name)
		checks = append(checks, sqliteIntegrityCheck(name, full))
	}
	for _, name := range env {
		_, found := os.LookupEnv(name)
		message := "Missing"
		if found {
			message = "Configured"
		}
		checks = append(checks, recoveryclient.Check{Name: "Environment: " + name, Passed: found, Message: message})
	}
	return checks
}

func recipeFailure(message string) []recoveryclient.Check {
	return []recoveryclient.Check{{Name: "Verification Recipe", Message: message}}
}

// Open JSON-decodes lists as []any. []string also supports in-memory fixtures.
func recipeStrings(value any) ([]string, error) {
	var result []string
	switch list := value.(type) {
	case []string:
		result = list
	case []any:
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("expected strings")
			}
			result = append(result, s)
		}
	default:
		return nil, fmt.Errorf("expected a nonempty string list")
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("expected a nonempty string list")
	}
	for _, item := range result {
		if strings.TrimSpace(item) == "" || strings.ContainsRune(item, 0) {
			return nil, fmt.Errorf("invalid empty or NUL-containing member")
		}
	}
	return result, nil
}

func sqliteIntegrityCheck(name, path string) recoveryclient.Check {
	fail := func(message string) recoveryclient.Check {
		return recoveryclient.Check{Name: "SQLite Integrity: " + name, Message: message}
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return fail("Database missing, empty or not regular")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fail("Invalid database path")
	}
	// URL encoding prevents a filename's '?' or '#' from changing SQLite's options.
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fail("Failed to open database")
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check;").Scan(&result); err != nil || result != "ok" {
		return fail("PRAGMA integrity_check failed")
	}
	return recoveryclient.Check{Name: "SQLite Integrity: " + name, Passed: true, Message: "PRAGMA integrity_check passed ok"}
}

func drillPath(root, name string) (string, bool) {
	clean := filepath.Clean(name)
	if clean != name || clean == "." || !filepath.IsLocal(name) || strings.ContainsAny(name, "\\\x00") {
		return "", false
	}
	return filepath.Join(root, name), true
}

// DrillRoot keeps opened instance data beneath the deployment's data directory.
func DrillRoot(cfg *config.Config) string { return filepath.Join(cfg.Database.DataDir, "drill") }
