package config_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/config"
)

func TestKeysPersistAndStayPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv("KY_DATA_DIR", dir)
	t.Setenv("KY_SESSION_SECRET", "")
	t.Setenv("KY_ENCRYPTION_KEY", "")
	first, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	second, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if first.Security.SessionSecret != second.Security.SessionSecret || !bytes.Equal(first.Security.EncryptionKey, second.Security.EncryptionKey) || !bytes.Equal(first.Security.InstanceKey, second.Security.InstanceKey) {
		t.Fatal("keys changed after reload")
	}
	private := ed25519.NewKeyFromSeed(first.Security.InstanceKey)
	message := []byte("instance identity persists")
	if !ed25519.Verify(ed25519.NewKeyFromSeed(second.Security.InstanceKey).Public().(ed25519.PublicKey), message, ed25519.Sign(private, message)) {
		t.Fatal("instance identity changed")
	}
	for _, name := range []string{"encryption.key", "session.key", "instance.key"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		key, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(key) != 32 {
			t.Fatal("invalid persisted key", name)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("key permissions", name, err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("directory permissions", err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(first.Security.SessionSecret)) || bytes.Contains(encoded, []byte("session_secret")) || bytes.Contains(encoded, []byte("instance_key")) {
		t.Fatal("config JSON exposed a secret")
	}
}

func TestKeyOverridesDoNotReplacePersistedKeys(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	t.Setenv("KY_SESSION_SECRET", "")
	t.Setenv("KY_ENCRYPTION_KEY", "")
	original, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KY_SESSION_SECRET", strings.Repeat("ab", 32))
	t.Setenv("KY_ENCRYPTION_KEY", strings.Repeat("cd", 32))
	override, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if override.Security.SessionSecret != strings.Repeat("ab", 32) || !bytes.Equal(override.Security.EncryptionKey, bytes.Repeat([]byte{0xcd}, 32)) {
		t.Fatal("override ignored")
	}
	t.Setenv("KY_SESSION_SECRET", "")
	t.Setenv("KY_ENCRYPTION_KEY", "")
	restored, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Security.SessionSecret != original.Security.SessionSecret || !bytes.Equal(restored.Security.EncryptionKey, original.Security.EncryptionKey) {
		t.Fatal("override replaced persisted keys")
	}
	t.Setenv("KY_SESSION_SECRET", "not-a-secret-to-print")
	if _, err := config.LoadFromEnv(); err == nil || strings.Contains(err.Error(), "not-a-secret-to-print") {
		t.Fatal("invalid override accepted or leaked", err)
	}
}

func TestUnsafeKeysFailWithoutReplacement(t *testing.T) {
	for _, name := range []string{"encryption.key", "session.key", "instance.key"} {
		for _, kind := range []string{"empty", "truncated", "malformed", "permissive", "symlink", "directory"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("KY_DATA_DIR", dir)
				t.Setenv("KY_SESSION_SECRET", "")
				t.Setenv("KY_ENCRYPTION_KEY", "")
				path := filepath.Join(dir, name)
				data := strings.Repeat("ab", 32)
				mode := os.FileMode(0600)
				switch kind {
				case "empty":
					data = ""
				case "truncated":
					data = "ab"
				case "malformed":
					data = "secret-invalid-marker"
				case "permissive":
					mode = 0644
				}
				if kind == "symlink" {
					if err := os.Symlink(filepath.Join(dir, "missing-target"), path); err != nil {
						t.Fatal(err)
					}
				} else if kind == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(data), mode); err != nil {
					t.Fatal(err)
				}
				if kind != "symlink" && kind != "directory" {
					if err := os.Chmod(path, mode); err != nil {
						t.Fatal(err)
					}
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm() != mode {
						t.Fatalf("fixture mode: got %04o, want %04o", info.Mode().Perm(), mode)
					}
				}
				if _, err := config.LoadFromEnv(); err == nil || strings.Contains(err.Error(), "secret-invalid-marker") {
					t.Fatal("unsafe key accepted or leaked", err)
				}
				if kind != "symlink" && kind != "directory" {
					after, err := os.ReadFile(path)
					if err != nil || string(after) != data {
						t.Fatal("bad key was replaced", err)
					}
				}
			})
		}
	}
}

func TestDataDirectorySafety(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KY_DATA_DIR", dir)
	if _, err := config.LoadFromEnv(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("existing directory not secured", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KY_DATA_DIR", link)
	if _, err := config.LoadFromEnv(); err == nil {
		t.Fatal("symlink data directory accepted")
	}
}

func TestBootstrapKeyProcess(t *testing.T) {
	if os.Getenv("KY_KEY_TEST_CHILD") != "1" {
		return
	}
	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sum := sha256.Sum256(append(append([]byte(cfg.Security.SessionSecret), cfg.Security.EncryptionKey...), cfg.Security.InstanceKey...))
	fmt.Printf("%x", sum)
	os.Exit(0)
}

func TestConcurrentFirstBootAgreesOnKeys(t *testing.T) {
	t.Setenv("KY_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("KY_SESSION_SECRET", "")
	t.Setenv("KY_ENCRYPTION_KEY", "")
	t.Setenv("KY_KEY_TEST_CHILD", "1")
	var wg sync.WaitGroup
	results := make(chan string, 4)
	for range 4 {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapKeyProcess$")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("first boot: %v: %s", err, out)
				return
			}
			results <- string(out)
		})
	}
	wg.Wait()
	close(results)
	first := ""
	for result := range results {
		if first == "" {
			first = result
		} else if result != first {
			t.Fatal("concurrent processes used different keys")
		}
	}
}
