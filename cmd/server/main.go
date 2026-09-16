package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Busness-app/ky-primitives/password"
	"github.com/Busness-app/ky-primitives/recoveryclient"
	"github.com/Busness-app/kyyard-server/internal/api"
	"github.com/Busness-app/kyyard-server/internal/backup"
	"github.com/Busness-app/kyyard-server/internal/config"
	"github.com/Busness-app/kyyard-server/internal/crypto"
	"github.com/Busness-app/kyyard-server/internal/store"
)

// appVersion is what the capsule manifest records for this build.
const appVersion = "1.0.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			if err := healthcheck(); err != nil {
				log.Fatal(err)
			}
			return
		case "init-admin":
			runInitAdmin(os.Args[2:])
			return
		case "backup-drill":
			runBackupDrill(os.Args[2:])
			return
		case "export-capsule":
			runExportCapsule(os.Args[2:])
			return
		case "deposit":
			runDeposit()
			return
		case "restore":
			runRestore(os.Args[2:])
			return
		case "version":
			fmt.Println("kyyard-server v1.0.0 (KyYard control plane scaffold)")
			return
		}
	}

	runServer()
}

// shutdownTimeout drains in-flight HTTP requests. Short on purpose: it is spent before the
// backup wait below, and both must fit inside the deployment's stop_grace_period.
const shutdownTimeout = 5 * time.Second

// backupWaitTimeout bounds the wait for detached backup work. recoveryclient caps one deposit
// at 15 minutes (its uploadTimeout, for a container of at most capsule.MaxContainerBytes,
// 384 MiB); the extra two minutes cover sealing and the local copy either side of the upload.
// docker-compose.yml's stop_grace_period must exceed shutdownTimeout + backupWaitTimeout, and
// TestComposeGracePeriodCoversTheShutdownBudget holds the two in step.
const backupWaitTimeout = 17 * time.Minute

func runServer() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	if cfg.Backup.AllowPrivateRecovery {
		log.Printf("[BACKUP] KY_BACKUP_ALLOW_PRIVATE_RECOVERY is on: RFC1918 and CGNAT destinations admitted; loopback, link-local and other reserved addresses remain refused (HTTPS still required)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("Failed to initialize database (%s): %v", cfg.Database.Driver, err)
	}
	defer st.Close()

	// Ensure default admin user exists if database is empty
	count, err := st.Users().CountUsers(ctx)
	if err != nil {
		log.Fatalf("Failed to inspect bootstrap state: %v", err)
	}
	if count == 0 {
		adminPass := os.Getenv("KY_ADMIN_PASSWORD")
		generated := adminPass == ""
		if generated {
			adminPass = crypto.RandomHex(12)
		}
		hash, err := password.Hash(adminPass)
		if err != nil {
			log.Fatalf("Failed to hash bootstrap admin password: %v", err)
		}
		if err := st.Users().CreateUser(ctx, &store.User{
			ID:                 fmt.Sprintf("usr_%s", crypto.RandomHex(12)),
			Username:           "admin",
			DisplayName:        "Administrator",
			PasswordHash:       hash,
			Role:               "admin",
			Status:             "active",
			SSOProvider:        "local",
			MustChangePassword: true,
		}); err != nil {
			log.Fatalf("Failed to create bootstrap admin: %v", err)
		}
		if generated {
			log.Printf("[SECURITY] Initial bootstrap: Created admin account. Username: admin | Password: %s", adminPass)
		}
	}

	srv := api.NewServer(cfg, st)
	backupDone := make(chan struct{})
	go backupLoop(ctx, cfg, st, backupDone)

	addr := net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      srv,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Graceful shutdown channel
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[KYYARD] %s listening on http://%s (DB: %s)", cfg.Server.AppName, addr, cfg.Database.Driver)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stop
	log.Println("[KYYARD] Shutting down gracefully...")
	srv.BeginShutdown()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), backupWaitTimeout)
	defer waitCancel()
	waitForBackupWork(waitCtx, backupDone, srv.WaitDetached)
	log.Println("[KYYARD] Server stopped")
}

// waitForBackupWork blocks until the scheduler loop and every detached handler have finished,
// or until ctx expires. Backup work ignores cancellation once bytes are moving: the scheduler's
// run, and the pair, pin-key and deposit handlers, all detach from their caller. They are waited
// out before the store closes, or they write into a closed store -- a key pinned on disk with no
// row recording it, or a capsule at KyRecovery with no receipt this side.
//
// Both waits start before either blocks, and both are bounded by the one context rather than a
// timer channel: a timer channel delivers its value once, so whichever wait consumed it would
// leave the other unbounded -- exactly the stuck-deposit case this is written for. Starting them
// together matters as much: waited one after the other, a hung scheduled deposit spends the whole
// budget on its own and the handler wait is read only once the deadline has already passed,
// giving a live detached handler no time at all. Past the deadline the work is abandoned and said
// so; a SIGKILL would have been silent.
func waitForBackupWork(ctx context.Context, backupDone <-chan struct{}, waitDetached func()) {
	handlersDone := make(chan struct{})
	go func() { defer close(handlersDone); waitDetached() }()

	select {
	case <-backupDone:
	default:
		log.Println("[KYYARD] waiting for the scheduled backup in flight...")
		select {
		case <-backupDone:
		case <-ctx.Done():
			log.Printf("[KYYARD] abandoning a scheduled deposit still running after %s; its receipt may be unrecorded", backupWaitTimeout)
		}
	}
	select {
	case <-handlersDone:
	case <-ctx.Done():
		log.Printf("[KYYARD] abandoning a detached backup handler still running after %s; its writes may be unrecorded", backupWaitTimeout)
	}
}

// runBackup seals one capsule and delivers it to every configured destination, for the CLI:
// it builds its own RunConfig because a one-shot failure is reported to the operator and ends.
func runBackup(ctx context.Context, cfg *config.Config, st store.Store) (recoveryclient.Result, error) {
	rc, err := backup.RunConfig(cfg, appVersion)
	if err != nil {
		return recoveryclient.Result{}, err
	}
	client := recoveryclient.NewClient(recoveryclient.Options{AllowPrivate: cfg.Backup.AllowPrivateRecovery})
	return recoveryclient.Run(ctx, rc, backup.Settings(ctx, st.Settings()),
		func() (recoveryclient.Payload, error) { return backup.Collect(ctx, cfg, appVersion) }, client)
}

// backupLoop polls the admin's schedule once a minute; a change in the UI needs no restart
// and a restart never loses its place, the last attempt is in the database. The wait honours
// shutdown; the run does not, and done is closed only once the loop is between runs, so
// SIGTERM cannot land between KyRecovery storing a capsule and the receipt being written.
func backupLoop(ctx context.Context, cfg *config.Config, st store.Store, done chan<- struct{}) {
	defer close(done)
	// Built once: a deployment key that cannot seal is a configuration fault, not a run that
	// might succeed next minute. Run never gets far enough to stamp the attempt, so retrying
	// would log and audit a failure every tick forever.
	rc, err := backup.RunConfig(cfg, appVersion)
	if err != nil {
		log.Printf("[BACKUP] scheduler disabled: %v", err)
		return
	}
	client := recoveryclient.NewClient(recoveryclient.Options{AllowPrivate: cfg.Backup.AllowPrivateRecovery})
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next, on, err := recoveryclient.NextRun(cfg.Backup.DepositInterval, backup.Settings(ctx, st.Settings()))
		if err != nil {
			log.Printf("[BACKUP] schedule unreadable: %s", recoveryclient.AuditSafe(err.Error()))
			continue
		}
		if !on || time.Now().Before(next) {
			continue
		}
		runCtx := context.WithoutCancel(ctx)
		res, err := recoveryclient.Run(runCtx, rc, backup.Settings(runCtx, st.Settings()),
			func() (recoveryclient.Payload, error) { return backup.Collect(runCtx, cfg, appVersion) }, client)
		if errors.Is(err, recoveryclient.ErrNotPaired) || errors.Is(err, recoveryclient.ErrNoDestination) {
			continue // never configured; nothing to report
		}
		recordRun(runCtx, st, "system", res, err)
	}
}

// recordRun audits one run the same way the admin route does, under the actor that started it.
func recordRun(ctx context.Context, st store.Store, actor string, res recoveryclient.Result, err error) {
	action, outcome, details := recoveryclient.Outcome(res, err)
	details["outcome"] = outcome
	_ = st.Audit().LogAudit(ctx, &store.AuditRecord{UserID: actor, Action: action,
		Resource: res.Manifest.CapsuleID, Details: api.AuditDetails(details)})
	if err != nil {
		log.Printf("[BACKUP] %s: %s", actor, recoveryclient.AuditSafe(err.Error()))
		return
	}
	log.Printf("[BACKUP] %s: capsule %s (%d bytes) local=%q deposited=%t", actor, res.Manifest.CapsuleID, res.SizeBytes, res.LocalPath, res.Receipt != nil)
}

// runDeposit seals and delivers one capsule now, for cron or an operator at a shell.
func runDeposit() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("DB error: %v", err)
	}
	defer st.Close()

	res, err := runBackup(ctx, cfg, st)
	recordRun(ctx, st, "cli", res, err)
	if err != nil {
		log.Fatalf("Backup: %v", err)
	}
	if res.Receipt != nil {
		log.Printf("✓ Capsule %s deposited at %s; digest %s", res.Manifest.CapsuleID, res.Receipt.DepositedAt.Format(time.RFC3339), res.Receipt.Digest)
	}
}

func runInitAdmin(args []string) {
	fs := flag.NewFlagSet("init-admin", flag.ExitOnError)
	username := fs.String("username", "admin", "Admin username")
	passwordFlag := fs.String("password", "", "Admin password (minimum 12 characters)")
	_ = fs.Parse(args)

	if *passwordFlag == "" || len(*passwordFlag) < 12 {
		log.Fatal("Error: -password is required and must be at least 12 characters")
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("DB error: %v", err)
	}
	defer st.Close()

	hash, err := password.Hash(*passwordFlag)
	if err != nil {
		log.Fatalf("Password hashing error: %v", err)
	}

	existing, err := st.Users().GetUserByUsername(ctx, *username)
	if err == nil && existing != nil {
		if err := st.Users().ResetAdminPassword(ctx, existing.ID, hash); err != nil {
			log.Fatalf("Failed to update admin: %v", err)
		}
		log.Printf("✓ Admin user %q password successfully reset", *username)
		return
	}

	user := &store.User{
		ID:                 fmt.Sprintf("usr_%s", crypto.RandomHex(12)),
		Username:           *username,
		DisplayName:        "Administrator",
		PasswordHash:       hash,
		Role:               "admin",
		Status:             "active",
		SSOProvider:        "local",
		MustChangePassword: true,
	}

	if err := st.Users().CreateUser(ctx, user); err != nil {
		log.Fatalf("Failed to create admin: %v", err)
	}
	log.Printf("✓ Admin user %q created successfully", *username)
}

// collectFiles is what every CLI seal uses; the sealed-only members are safe here and nowhere else.
func collectFiles(ctx context.Context, cfg *config.Config) recoveryclient.Payload {
	payload, err := backup.Collect(ctx, cfg, appVersion)
	if err != nil {
		log.Fatalf("Failed to collect backup files: %v", err)
	}
	return payload
}

func runBackupDrill(args []string) {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("DB error: %v", err)
	}
	defer st.Close()

	payload := collectFiles(ctx, cfg)
	result, err := backup.RunDrill(ctx, cfg, payload)
	if err != nil {
		log.Fatalf("Drill execution error: %v", err)
	}

	fmt.Printf("\n=== Feature 0: KyBackup Restore Drill Summary ===\n")
	fmt.Printf("Status:   %s\n", map[bool]string{true: "PASSED (OK)", false: "FAILED"}[result.Passed])
	fmt.Printf("Duration: %d ms\n", result.DurationMs)
	for _, check := range result.Checks {
		status := "✓"
		if !check.Passed {
			status = "✗"
		}
		fmt.Printf("  [%s] %s: %s\n", status, check.Name, check.Message)
	}
	fmt.Println("==================================================")
}

func runExportCapsule(args []string) {
	fs := flag.NewFlagSet("export-capsule", flag.ExitOnError)
	out := fs.String("out", "", "output path (default <capsule-id>.kycap in the current directory)")
	_ = fs.Parse(args)

	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		log.Fatalf("DB error: %v", err)
	}
	defer st.Close()

	key, err := recoveryclient.LoadRecoveryKey(cfg.Database.DataDir, backup.Settings(ctx, st.Settings()))
	if err != nil {
		log.Fatalf("Recovery key: %v", err)
	}
	raw, m, err := recoveryclient.Seal(collectFiles(ctx, cfg), key)
	if err != nil {
		log.Fatalf("Seal: %v", err)
	}
	path := *out
	if path == "" {
		path = recoveryclient.FilenameSafe(m.CapsuleID) + ".kycap"
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		log.Fatalf("Write: %v", err)
	}
	log.Printf("✓ Capsule %s sealed to recovery key %s, written to %s (%d bytes)", m.CapsuleID, m.RecoveryKeyID, path, len(raw))
}

// restore is the product-side half of the ceremony, owned by the lib: k custodian shares
// combined, used once, dropped; a capsule from another service refused before the key is
// touched; the authenticated manifest printed for comparison with KyRecovery's record.
func restore(capsulePath, targetDir, expectService string, shares []string, stdout io.Writer) error {
	return recoveryclient.Restore(capsulePath, targetDir, expectService, shares, stdout)
}

// stdinIsTerminal reports whether a human is typing, so a pipeline gets no stray prompt.
func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func runRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	capsulePath := fs.String("capsule", "", "path to the .kycap file")
	target := fs.String("to", "", "empty directory to restore into")
	service := fs.String("service", "", "expected service name (default: $KY_APP_NAME)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: kyyard-server restore -capsule <file.kycap> -to <dir> [-service <name>]\n\n"+
			"Custodian shares are read from stdin, one ky2-... share per line, and never from\n"+
			"the command line: argv is world-readable and lands in shell history.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *capsulePath == "" || *target == "" {
		fs.Usage()
		os.Exit(2)
	}
	if *service == "" {
		// Not config.LoadFromEnv: it mints <DataDir>/encryption.key as a side effect, and a
		// recovery host has no business growing a key of its own mid-ceremony.
		*service = os.Getenv("KY_APP_NAME")
	}
	if *service == "" {
		*service = config.DefaultAppName
	}
	if *service == "" {
		log.Fatal("Error: -service is required when KY_APP_NAME is not set")
	}

	if stdinIsTerminal() {
		fmt.Fprintln(os.Stderr, "Paste custodian shares, one per line, then Ctrl-D:")
	}
	shares, err := recoveryclient.ReadShares(os.Stdin)
	if err != nil {
		log.Fatalf("Reading shares: %v", err)
	}
	if len(shares) == 0 {
		log.Fatal("Error: no custodian shares on stdin")
	}
	if err := restore(*capsulePath, *target, *service, shares, os.Stdout); err != nil {
		log.Fatalf("Restore failed: %v", err)
	}
}
