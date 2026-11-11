package backup_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/ky-primitives/recoverykey"
	"github.com/Busnes-app/kyyard-server/internal/backup"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// stalledDepositor is an upload that never completes: it returns only when its context ends,
// as the real client's HTTP request does.
type stalledDepositor struct {
	once    sync.Once
	entered chan struct{}
}

func (d *stalledDepositor) Deposit(ctx context.Context, _, _ string, _ []byte) (recoveryclient.Receipt, error) {
	d.once.Do(func() { close(d.entered) })
	<-ctx.Done()
	return recoveryclient.Receipt{}, fmt.Errorf("%w: %w", recoveryclient.ErrRemote, ctx.Err())
}

// storingDepositor answers as KyRecovery does for bytes it stored.
type storingDepositor struct{ deposits int }

func (d *storingDepositor) Deposit(_ context.Context, _, _ string, raw []byte) (recoveryclient.Receipt, error) {
	d.deposits++
	m, err := capsule.ReadUnverifiedManifest(raw)
	if err != nil {
		return recoveryclient.Receipt{}, err
	}
	sum := sha256.Sum256(raw)
	return recoveryclient.Receipt{CapsuleID: m.CapsuleID, Digest: hex.EncodeToString(sum[:]), SizeBytes: int64(len(raw)), DepositedAt: time.Now().UTC()}, nil
}

// pairedInstance is sqliteInstance with a pinned key, a KyRecovery pairing and a local
// backup directory, plus the private key that opens what it seals.
func pairedInstance(t *testing.T) (*config.Config, store.Store, recoveryclient.RunConfig, recoverykey.PrivateKey) {
	t.Helper()
	cfg, st := sqliteInstance(t)
	cfg.Backup.Dir = filepath.Join(t.TempDir(), "capsules")
	cfg.Backup.Keep = 3
	cfg.Backup.DepositInterval = time.Hour
	settings := backup.Settings(context.Background(), st.Settings())
	priv, err := recoverykey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryclient.StoreRecoveryKey(cfg.Database.DataDir, settings, recoveryclient.RecoveryKey{Public: priv.Public(), Threshold: 2, TotalShares: 3}); err != nil {
		t.Fatal(err)
	}
	rc, err := backup.RunConfig(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryclient.StorePairing(settings, rc.Sealer, "https://recovery.busnes.app", "kyrec_live_t"); err != nil {
		t.Fatal(err)
	}
	return cfg, st, rc, priv
}

// runInterrupted starts a run, cancels it once started reports the moment has come, and
// returns the run's error. It fails the test if the run takes longer than two seconds to stop.
func runInterrupted(t *testing.T, cfg *config.Config, st store.Store, rc recoveryclient.RunConfig, client recoveryclient.Depositor, started <-chan struct{}) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := backup.Run(ctx, cfg, rc, st.Settings(), client)
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("the run ended before it could be interrupted: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the run never reached the step to interrupt")
	}
	cancel()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("the run was still going two seconds after it was cancelled")
		return nil
	}
}

// dueNow reports whether the scheduler would start a run on its next tick.
func dueNow(t *testing.T, cfg *config.Config, st store.Store) bool {
	t.Helper()
	next, on, err := recoveryclient.NextRun(cfg.Backup.DepositInterval, backup.Settings(context.Background(), st.Settings()))
	if err != nil || !on {
		t.Fatalf("schedule: on=%v err=%v", on, err)
	}
	return !time.Now().Before(next)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Shutdown mid-upload: the run stops at once, records no receipt, and is due again, so the
// next start delivers. The local copy was complete before the upload began; whatever is in
// the backup directory afterwards must be a whole capsule, never a partial one.
func TestInterruptedUploadIsDueAgainAndLeavesOnlyWholeCapsules(t *testing.T) {
	cfg, st, rc, priv := pairedInstance(t)
	settings := backup.Settings(context.Background(), st.Settings())

	stalled := &stalledDepositor{entered: make(chan struct{})}
	err := runInterrupted(t, cfg, st, rc, stalled, stalled.entered)
	if !errors.Is(err, backup.ErrInterrupted) || !errors.Is(err, recoveryclient.ErrRemote) {
		t.Fatalf("interrupted run returned %v, want ErrInterrupted wrapping the upload error", err)
	}
	if _, ok, _ := recoveryclient.LastDeposit(settings); ok {
		t.Error("an interrupted upload recorded a receipt")
	}
	if !dueNow(t, cfg, st) {
		t.Error("the interrupted run kept its attempt stamp: the next run would wait a whole interval")
	}
	names := dirNames(t, cfg.Backup.Dir)
	if len(names) != 1 {
		t.Fatalf("backup directory holds %v, want exactly the one local copy", names)
	}
	raw, _ := os.ReadFile(filepath.Join(cfg.Backup.Dir, names[0]))
	if _, files, err := capsule.Open(raw, priv, t.TempDir()); err != nil || len(files) < 2 {
		t.Fatalf("the local copy left by an interrupted run does not open: %v (%d files)", err, len(files))
	}

	// The next start: the same schedule now delivers.
	next := &storingDepositor{}
	if _, err := backup.Run(context.Background(), cfg, rc, st.Settings(), next); err != nil {
		t.Fatalf("the run after the interruption failed: %v", err)
	}
	if _, ok, _ := recoveryclient.LastDeposit(settings); !ok || next.deposits != 1 {
		t.Fatalf("the run after the interruption did not deliver (deposits=%d)", next.deposits)
	}
	if dueNow(t, cfg, st) {
		t.Fatal("a completed run did not stamp its attempt")
	}

	// Interrupted again, with an attempt on record: the earlier stamp comes back unchanged.
	before, _ := settings.Get("backup_last_attempt")
	if err := settings.Set("backup_last_attempt", "2026-01-02T03:04:05Z"); err != nil {
		t.Fatal(err)
	}
	stalled = &stalledDepositor{entered: make(chan struct{})}
	if err := runInterrupted(t, cfg, st, rc, stalled, stalled.entered); !errors.Is(err, backup.ErrInterrupted) {
		t.Fatalf("second interrupted run returned %v", err)
	}
	if after, _ := settings.Get("backup_last_attempt"); after != "2026-01-02T03:04:05Z" {
		t.Errorf("attempt stamp after an interrupted run is %q, want the one from before it (previous completed run was %q)", after, before)
	}
}

// Shutdown mid-snapshot: VACUUM INTO is interrupted, the scratch directory is removed, and
// nothing reaches the backup directory or KyRecovery.
func TestInterruptedSnapshotLeavesNothingBehind(t *testing.T) {
	cfg, st, rc, _ := pairedInstance(t)
	db, err := sql.Open("sqlite", cfg.Database.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ballast (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	for range 48 { // 48 MiB: under the capsule member cap, large enough to interrupt mid-copy
		if _, err := db.Exec(`INSERT INTO ballast VALUES (randomblob(1048576))`); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	snapshotStarted := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			if m, _ := filepath.Glob(filepath.Join(cfg.Database.DataDir, "snapshot-*", "ky_server.db")); len(m) > 0 {
				close(snapshotStarted)
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	client := &storingDepositor{}
	if err := runInterrupted(t, cfg, st, rc, client, snapshotStarted); !errors.Is(err, backup.ErrInterrupted) {
		t.Fatalf("interrupted run returned %v, want ErrInterrupted", err)
	}
	if m, _ := filepath.Glob(filepath.Join(cfg.Database.DataDir, "snapshot-*")); len(m) > 0 {
		t.Errorf("the interrupted snapshot left %v in the data directory", m)
	}
	if names := dirNames(t, cfg.Backup.Dir); len(names) > 0 {
		t.Errorf("the interrupted snapshot left %v in the backup directory", names)
	}
	if client.deposits != 0 {
		t.Error("a capsule was uploaded from an interrupted snapshot")
	}
	if !dueNow(t, cfg, st) {
		t.Error("the interrupted run kept its attempt stamp")
	}
}

// A dropped admin connection is not a shutdown: a run that fails for its own reasons keeps
// its stamp, so a broken destination is retried once per interval and not every minute.
func TestFailedRunKeepsItsAttemptStamp(t *testing.T) {
	cfg, st, rc, _ := pairedInstance(t)
	failing := depositorFunc(func() error { return fmt.Errorf("%w: refused", recoveryclient.ErrRemote) })
	if _, err := backup.Run(context.Background(), cfg, rc, st.Settings(), failing); err == nil || errors.Is(err, backup.ErrInterrupted) {
		t.Fatalf("failing run returned %v", err)
	}
	if dueNow(t, cfg, st) {
		t.Error("a failed run gave its attempt stamp back; it would retry every tick")
	}
}

type depositorFunc func() error

func (f depositorFunc) Deposit(context.Context, string, string, []byte) (recoveryclient.Receipt, error) {
	return recoveryclient.Receipt{}, f()
}
