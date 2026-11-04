package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A drill that fails a check exits 1, so automation does not read FAILED as success. The child
// is this test binary re-run into main, without the KY_PORT and KY_DB_DRIVER the drill requires.
func TestFailedBackupDrillExitsNonZero(t *testing.T) {
	if os.Getenv("KYYARD_TEST_DRILL") == "1" {
		os.Args = []string{"kyyard-server", "backup-drill"}
		main()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFailedBackupDrillExitsNonZero$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KY_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "KYYARD_TEST_DRILL=1", "KY_DATA_DIR="+t.TempDir(), "PUSH_RELAY_URL=", "APNS_RELAY_URL=")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "Status:   FAILED") {
		t.Fatalf("exit %v, output %q; want exit 1 after a FAILED summary", err, out)
	}
}
