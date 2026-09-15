package backup_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busness-app/ky_server_base/internal/backup"
	"golang.org/x/sys/unix"
)

// A separate process holds the same lock used by the HTTP/CLI wrapper. Killing it
// proves the kernel releases ownership without a stale lock-file recovery scheme.
func TestDrillLockProcess(t *testing.T) {
	if os.Getenv("KY_DRILL_LOCK_HELPER") != "1" {
		return
	}
	dir := os.Getenv("KY_DRILL_LOCK_DIR")
	f, err := os.OpenFile(filepath.Join(dir, "drill.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestRunDrillSerializesProcessesAndReleasesAfterExit(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	cfg, _ := payloadConfig(t)
	payload, err := backup.Collect(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	root := backup.DrillRoot(cfg)
	stale := filepath.Join(root, "recoveryclient-drill-active")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stale, "active-data")
	if err := os.WriteFile(marker, []byte("active"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDrillLockProcess$")
	cmd.Env = append(os.Environ(), "KY_DRILL_LOCK_HELPER=1", "KY_DRILL_LOCK_DIR="+cfg.Database.DataDir)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("helper did not acquire lock: %q %v", line, err)
	}
	if _, err := backup.RunDrill(context.Background(), cfg, payload); !errors.Is(err, backup.ErrDrillBusy) {
		t.Fatalf("concurrent process accepted: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("busy drill swept active scratch: %v", err)
	}
	other, _ := payloadConfig(t)
	result, err := backup.RunDrill(context.Background(), other, payload)
	if err != nil || !result.Passed {
		t.Fatalf("independent data dir blocked: %+v %v", result, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// A permissive existing root is repaired before the library opens any plaintext.
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	result, err = backup.RunDrill(context.Background(), cfg, payload)
	if err != nil || !result.Passed {
		t.Fatalf("lock survived process exit: %+v %v", result, err)
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("scratch root not private: %v %v", info, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch left behind: %v %v", entries, err)
	}
	// Cancellation must also release the lock.
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := backup.RunDrill(canceled, cfg, payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled drill: %v", err)
	}
	result, err = backup.RunDrill(context.Background(), cfg, payload)
	if err != nil || !result.Passed {
		t.Fatalf("lock survived cancellation: %+v %v", result, err)
	}
}
