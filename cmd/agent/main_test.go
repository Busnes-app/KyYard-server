package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckFlags(t *testing.T) {
	for _, tc := range []struct {
		kube                   bool
		socket, link, linkFile string
		want                   string
	}{
		{false, "/var/run/docker.sock", "https://y/#kyyard=x", "", ""},
		{true, "", "", "/etc/kyyard/link", ""},
		{true, "/var/run/docker.sock", "", "/etc/kyyard/link", "kubernetes and docker are exclusive"},
		{false, "/var/run/docker.sock", "https://y/#kyyard=x", "/etc/kyyard/link", "--link or --link-file"},
	} {
		err := checkFlags(tc.kube, tc.socket, tc.link, tc.linkFile)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// The link file is read trimmed; a missing one means no link, because the manifest mounts it
// optional and the operator deletes it once spent; anything longer than a link is refused.
func TestReadLinkFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	if link, err := readLinkFile(path); link != "" || err != nil {
		t.Fatalf("missing file: %q %v", link, err)
	}
	if err := os.WriteFile(path, []byte("https://yard.example/#kyyard=abc\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if link, err := readLinkFile(path); link != "https://yard.example/#kyyard=abc" || err != nil {
		t.Fatalf("link %q %v", link, err)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 4097)), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := readLinkFile(big); err == nil {
		t.Fatal("an oversized link file was read")
	}
}

// A writable scratch directory holds the ledgers; one an old manifest never mounted (here, a
// path that does not exist) leaves the agent running without them, said once, and the probe
// leaves nothing behind.
func TestLedgerDir(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, format) }
	if got := ledgerDir(dir, logf); got != dir || len(lines) != 0 {
		t.Fatalf("writable: %q %v", got, lines)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the probe left %v", entries)
	}
	if got := ledgerDir(filepath.Join(dir, "absent"), logf); got != "" || len(lines) != 1 {
		t.Fatalf("unwritable: %q %v", got, lines)
	}
}
