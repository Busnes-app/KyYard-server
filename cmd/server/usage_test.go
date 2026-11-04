package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// An argument the binary does not know must not fall through to the server: a second server on
// the live data directory reconciles the running one's in-flight commands before its listener
// fails. The child is this test binary re-run into main.
func TestUnknownCommandStartsNothing(t *testing.T) {
	if os.Getenv("KYYARD_TEST_MAIN") == "1" {
		os.Args = append([]string{"kyyard-server"}, strings.Fields(os.Getenv("KYYARD_TEST_ARGS"))...)
		main()
		os.Exit(0)
	}
	for _, tc := range []struct {
		args string
		code int
		out  string
	}{
		{"--help", 0, "usage: kyyard-server"},
		{"help", 0, "usage: kyyard-server"},
		{"init_admin", 2, `unknown command "init_admin"`},
		{"-config x", 2, `unknown command "-config"`},
	} {
		t.Run(tc.args, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestUnknownCommandStartsNothing$")
			cmd.Env = append(os.Environ(), "KYYARD_TEST_MAIN=1", "KYYARD_TEST_ARGS="+tc.args, "KY_DATA_DIR="+dir)
			out, err := cmd.CombinedOutput()
			code := 0
			if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.code || !strings.Contains(string(out), tc.out) {
				t.Fatalf("exit %d, output %q; want exit %d with %q", code, out, tc.code, tc.out)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("the data directory was touched: %v", entries)
			}
		})
	}
}

// A second server on a data directory a running one holds is refused before it opens the store.
func TestDataDirLockRefusesASecondServer(t *testing.T) {
	dir := t.TempDir()
	first, err := lockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDataDir(dir); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock: %v", err)
	}
	_ = first.Close()
	again, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after the first server ended: %v", err)
	}
	_ = again.Close()
}
