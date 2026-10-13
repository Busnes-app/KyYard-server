package backup_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/applysetup"
	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/backup"
)

func TestInstallerCredentialsSealAndDrillRequiresThem(t *testing.T) {
	t.Setenv("KY_PORT", "8080")
	t.Setenv("KY_DB_DRIVER", "sqlite")
	ctx := context.Background()
	cfg, st := sqliteInstance(t)
	dir := t.TempDir()
	client, directory := filepath.Join(dir, "client"), filepath.Join(dir, "directory")
	for _, p := range []string{client, directory} {
		if err := os.WriteFile(p, []byte("disposable-private-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := applysetup.Common{Schema: applysetup.Schema, Service: "kyyard", SetupID: "271333e2-c221-4601-b249-563b3d828530", SSO: &applysetup.SSO{IssuerURL: "https://identity.example", ClientID: "yard", ClientSecretFile: client, DirectorySecretFile: directory, RedirectURI: "https://yard.example/api/sso/kyidentity/callback"}, Admins: []applysetup.Identity{{Issuer: "https://identity.example", Subject: "owner", Username: "owner-admin"}}}
	if _, err := st.ApplyInstaller(ctx, b, false); err != nil {
		t.Fatal(err)
	}
	payload, err := backup.Collect(ctx, cfg, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"installer/client.secret", "installer/directory.secret", "installer/credential-paths.json"} {
		if file := findFile(payload.Files, name); file == nil || file.Mode != 0600 {
			t.Fatal("missing private credential", name)
		}
		if !slices.Contains(backup.Members(cfg, true), name) {
			t.Fatal("status omitted member", name)
		}
	}
	if err := os.MkdirAll(backup.DrillRoot(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	drill := func(p recoveryclient.Payload) *recoveryclient.DrillResult {
		t.Helper()
		result, err := recoveryclient.Drill(ctx, backup.DrillRoot(cfg), p, func(dir string, opened capsule.Manifest) []recoveryclient.Check { return backup.Checks(dir, opened) })
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := drill(payload); !result.Passed {
		t.Fatal("complete managed restore failed", result)
	}
	for _, omitted := range []string{"installer/client.secret", "installer/directory.secret", "installer/credential-paths.json"} {
		bad := payload
		bad.Files = nil
		bad.VerificationRecipe = maps.Clone(payload.VerificationRecipe)
		required := []string{}
		for _, f := range payload.Files {
			if f.Path != omitted {
				bad.Files = append(bad.Files, f)
				required = append(required, f.Path)
			}
		}
		bad.VerificationRecipe["required_files"] = required
		if result := drill(bad); result.Passed {
			t.Fatal("self-consistent recipe omitted owned credential", omitted)
		}
	}
	bad := payload
	bad.Files = slices.Clone(payload.Files)
	for i := range bad.Files {
		if bad.Files[i].Path == "installer/credential-paths.json" {
			raw, _ := json.Marshal(map[string]string{"installer/client.secret": "/foreign/client", "installer/directory.secret": directory})
			bad.Files[i].Data = raw
		}
	}
	if result := drill(bad); result.Passed {
		t.Fatal("foreign restore mapping accepted")
	}
	for i := range bad.Files {
		if bad.Files[i].Path == "installer/client.secret" {
			bad.Files[i].Data = []byte(strings.Repeat("x", applysetup.MaxSecretBytes+1))
		}
	}
	if result := drill(bad); result.Passed {
		t.Fatal("oversized credential accepted")
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Collect(ctx, cfg, "fixture"); err == nil {
		t.Fatal("backup succeeded without protected directory credential")
	}
}
