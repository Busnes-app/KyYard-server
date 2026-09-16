package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestDirectoryTighteningIsReportedAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	if err := secureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"data directory", dir, "0755", "0700"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q in permission-change log: %s", want, &output)
		}
	}
	output.Reset()
	// A private but more restrictive mode must not be changed either.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if err := secureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0500 || output.Len() != 0 {
		t.Fatal("already-private directory was changed or logged")
	}
}
