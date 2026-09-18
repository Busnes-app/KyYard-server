package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/backup"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

func TestApplicationRevisionsSurviveBackup(t *testing.T) {
	ctx := context.Background()
	db := testdb.Config(t)
	if db.Driver != "sqlite" {
		t.Skip("capsules require SQLite")
	}
	db.DataDir = t.TempDir()
	cfg := &config.Config{Database: db}
	cfg.Security.EncryptionKey = make([]byte, 32)
	cfg.Security.InstanceKey = make([]byte, 32)
	cfg.Security.SessionSecret = strings.Repeat("00", 32)
	st, err := store.Open(ctx, db)
	mustTenant(t, err)
	defer st.Close()
	ts := st.Tenancy()
	mustTenant(t, ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "a"}))
	mustTenant(t, ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "prod"}))
	tenantUser(t, st, "actor", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: store.RoleOrganizationAdmin, Status: "active"}))
	a := store.TenantAccess{ActorID: "actor", OrganizationID: "a", EnvironmentID: "env-a"}
	app, err := ts.CreateApplication(ctx, a, "shop", desired("nginx:1"))
	mustTenant(t, err)
	_, err = ts.AppendApplicationRevision(ctx, a, app.ID, 1, desired("nginx:2"))
	mustTenant(t, err)
	secretApp, err := ts.ImportApplication(ctx, a, "secret-shop", desired("nginx:1"), map[string]string{"database-password": "backup-secret-canary"}, cfg.Security.EncryptionKey)
	mustTenant(t, err)
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	endpoint, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "backup-host"})
	mustTenant(t, err)
	mustTenant(t, ts.ApproveEndpoint(ctx, a, endpoint.ID, endpoint.Fingerprint))
	raw, err := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: strings.Repeat("a", 64), Name: "shop-web", ImageID: "sha256:" + strings.Repeat("b", 64), ComposeProject: "shop", CreatedAt: time.Now().UTC()}}})
	mustTenant(t, err)
	_, err = ts.AcceptInventory(ctx, endpoint.ID, uint64(time.Now().Unix()), time.Now(), raw)
	mustTenant(t, err)
	preview, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint.ID, "shop")
	mustTenant(t, err)
	instance, err := ts.AdoptApplication(ctx, a, app.ID, store.AdoptionRequest{EndpointID: endpoint.ID, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	mustTenant(t, err)
	payload, err := backup.Collect(ctx, cfg, "test")
	mustTenant(t, err)
	path := filepath.Join(t.TempDir(), "restored.db")
	found := false
	var restoredKey []byte
	for _, file := range payload.Files {
		if file.Path == "data/encryption.key" {
			restoredKey, err = hex.DecodeString(strings.TrimSpace(string(file.Data)))
			mustTenant(t, err)
		}
		if file.Path == "data/ky_server.db" {
			mustTenant(t, os.WriteFile(path, file.Data, 0600))
			found = true
		}
	}
	if !found {
		t.Fatal("missing snapshot")
	}
	restored, err := store.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: path})
	mustTenant(t, err)
	defer restored.Close()
	for _, number := range []int{1, 2} {
		before, err := ts.ReadApplicationRevision(ctx, a, app.ID, number)
		mustTenant(t, err)
		after, err := restored.Tenancy().ReadApplicationRevision(ctx, a, app.ID, number)
		mustTenant(t, err)
		if before.ID != after.ID || before.Digest != after.Digest || before.Spec.Services[0].Image != after.Spec.Services[0].Image || after.Spec.Services[0].Environment["DATABASE_PASSWORD"].SecretRef != "database-password" {
			t.Fatal("backup lost revision")
		}
	}
	rows, err := restored.Tenancy().ListApplications(ctx, a, 0, 10)
	mustTenant(t, err)
	if len(rows) != 2 {
		t.Fatal("backup lost application head")
	}
	instances, err := restored.Tenancy().ListApplicationInstances(ctx, a, endpoint.ID)
	mustTenant(t, err)
	if len(instances) != 1 || instances[0].ID != instance.ID || len(instances[0].Containers) != 1 || instances[0].Containers[0].ID != strings.Repeat("a", 64) {
		t.Fatal("backup lost adoption ownership")
	}
	values, err := restored.Tenancy().ResolveApplicationSecrets(ctx, a, secretApp.ID, 1, restoredKey)
	mustTenant(t, err)
	if values["database-password"] != "backup-secret-canary" {
		t.Fatal("backup lost encrypted values")
	}
}
