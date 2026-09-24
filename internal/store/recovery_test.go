package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/backup"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

// planAdopted adopts the project, maps its web service and plans the revision.
func planAdopted(t *testing.T, ts store.TenancyStore, a store.TenantAccess, appID, endpointID, project, containerID string, revision int) *store.Deployment {
	t.Helper()
	ctx := context.Background()
	preview, err := ts.PreviewApplicationAdoption(ctx, a, appID, endpointID, project)
	mustTenant(t, err)
	instance, err := ts.AdoptApplication(ctx, a, appID, store.AdoptionRequest{EndpointID: endpointID, Project: project, Digest: preview.Digest, Confirm: project})
	mustTenant(t, err)
	mapping, err := ts.ReadApplicationMapping(ctx, a, appID)
	mustTenant(t, err)
	mustTenant(t, ts.SetApplicationMapping(ctx, a, appID, store.MappingRequest{InstanceID: instance.ID, Version: mapping.Version, Digest: mapping.Preview.Digest, Confirm: project, Bindings: map[string]string{"web": containerID}}))
	d, err := ts.PlanDeployment(ctx, a, appID, store.PlanRequest{InstanceID: instance.ID, MappingVersion: 1, Revision: revision, Confirm: project}, nil, nil, false)
	mustTenant(t, err)
	return d
}

// The control-plane state the spec lists (registries, applications with revisions and values, instances with resources and mappings, image checks, deployments, endpoints with keys and capabilities, enrollment tokens, commands and revoked identities) survives a snapshot restore with its secrets
// usable under the key from the capsule, and startup reconcile settles only what it owns.
func TestRestoreCarriesControlPlaneState(t *testing.T) {
	ctx := context.Background()
	db := testdb.Config(t)
	if db.Driver != "sqlite" {
		t.Skip("capsules require SQLite")
	}
	db.DataDir = t.TempDir()
	cfg := &config.Config{Database: db}
	cfg.Security.EncryptionKey = make([]byte, 32)
	_, err := rand.Read(cfg.Security.EncryptionKey)
	mustTenant(t, err)
	cfg.Security.InstanceKey = make([]byte, 32)
	cfg.Security.SessionSecret = strings.Repeat("00", 32)
	key := cfg.Security.EncryptionKey
	st, err := store.Open(ctx, db)
	mustTenant(t, err)
	defer st.Close()
	ts := st.Tenancy()
	mustTenant(t, ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "a"}))
	mustTenant(t, ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "prod"}))
	tenantUser(t, st, "actor", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "actor", Role: store.RoleOrganizationAdmin, Status: "active"}))
	a := store.TenantAccess{ActorID: "actor", OrganizationID: "a", EnvironmentID: "env-a"}
	orgAccess := store.TenantAccess{ActorID: "actor", OrganizationID: "a"}

	// Registry with a credential.
	registryCredential := "registry-restore-canary"
	_, err = ts.PutRegistry(ctx, orgAccess, store.RegistryInput{Host: "ghcr.io", Name: "GitHub", Username: "bot", Credential: &registryCredential}, key, false)
	mustTenant(t, err)

	// Applications with revisions and encrypted values.
	shop, err := ts.ImportApplication(ctx, a, "shop", desired("nginx:1"), map[string]string{"database-password": "shop-canary-1"}, key)
	mustTenant(t, err)
	_, err = ts.ReplaceApplicationRevision(ctx, a, shop.ID, 1, desired("nginx:2"), map[string]string{"database-password": "shop-canary-2"}, key)
	mustTenant(t, err)
	blog, err := ts.ImportApplication(ctx, a, "blog", desired("nginx:1"), map[string]string{"database-password": "blog-canary"}, key)
	mustTenant(t, err)

	// An active endpoint with a key, capabilities and inventory, and a revoked one.
	enroll := func(name string) (*store.Endpoint, ed25519.PublicKey) {
		tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
		mustTenant(t, err)
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		mustTenant(t, err)
		e, err := ts.Enroll(ctx, store.EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: name})
		mustTenant(t, err)
		mustTenant(t, ts.ApproveEndpoint(ctx, a, e.ID, e.Fingerprint))
		return e, pub
	}
	host, hostKey := enroll("restore-host")
	gone, _ := enroll("revoked-host")
	mustTenant(t, ts.RevokeEndpoint(ctx, a, gone.ID))
	capabilities := []string{"compose.v1", "logs"}
	mustTenant(t, ts.SetEndpointCapabilities(ctx, host.ID, capabilities))
	shopContainer, blogContainer := strings.Repeat("a", 64), strings.Repeat("f", 64)
	now := time.Now().UTC()
	// report sends the host's inventory with shop's web container as given.
	report := func(generation int64, shopWeb protocol.Container) {
		raw, err := json.Marshal(protocol.Snapshot{Engine: protocol.Engine{Version: "1"},
			Containers: []protocol.Container{
				shopWeb,
				{ID: blogContainer, Name: "blog-web", ImageID: "sha256:" + strings.Repeat("b", 64), ComposeProject: "blog", CreatedAt: now},
			},
			Images: []protocol.Image{
				{ID: "sha256:" + strings.Repeat("c", 64), Tags: []string{"nginx:2"}},
				{ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"nginx:1"}},
			}})
		mustTenant(t, err)
		accepted, err := ts.AcceptInventory(ctx, host.ID, uint64(generation), time.Now().UTC(), raw)
		mustTenant(t, err)
		if !accepted {
			t.Fatal("inventory refused")
		}
	}
	report(now.Unix(), protocol.Container{ID: shopContainer, Name: "shop-web", ImageID: "sha256:" + strings.Repeat("b", 64), ComposeProject: "shop", CreatedAt: now})
	// Issued and never used, so it must still enroll after restore.
	spareToken, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)

	// shop: adopted, mapped, applied and settled succeeded. blog: left applying.
	succeeded := planAdopted(t, ts, a, shop.ID, host.ID, "shop", shopContainer, 2)
	_, _, err = ts.ApplyDeployment(ctx, a, shop.ID, succeeded.ID, "shop", key, protocol.MaxDeploymentRequestBytes)
	mustTenant(t, err)
	newContainer := strings.Repeat("e", 64)
	mustTenant(t, ts.SettleDeployment(ctx, host.ID, protocol.DeploymentResult{Deployment: succeeded.ID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{{Service: "web", ContainerID: newContainer, ImageID: succeeded.Plan.Services[0].ImageID, CreatedUnix: 1800000000}}}))
	// The host reports the container the apply created, as a live agent would.
	report(now.Unix()+1, protocol.Container{ID: newContainer, Name: "shop-web", ImageID: succeeded.Plan.Services[0].ImageID, ComposeProject: "shop", CreatedAt: time.Unix(1800000000, 0).UTC()})
	applying := planAdopted(t, ts, a, blog.ID, host.ID, "blog", blogContainer, 1)
	_, _, err = ts.ApplyDeployment(ctx, a, blog.ID, applying.ID, "blog", key, protocol.MaxDeploymentRequestBytes)
	mustTenant(t, err)

	// A settled apply clears the instance's checks, so the cached verdict is written after it.
	shopInstances, err := ts.ListApplicationInstances(ctx, a, host.ID)
	mustTenant(t, err)
	var shopInstance string
	for _, i := range shopInstances {
		if i.ApplicationID == shop.ID {
			shopInstance = i.ID
		}
	}
	checkedAt := now.Truncate(time.Second)
	rawDB, err := sql.Open("sqlite", db.DSN)
	mustTenant(t, err)
	_, err = rawDB.ExecContext(ctx, `INSERT INTO image_checks(instance_id,service_name,reference,verdict,checked_at) VALUES(?,?,?,?,?)`, shopInstance, "web", "nginx:2", "update_available", checkedAt)
	mustTenant(t, err)
	mustTenant(t, rawDB.Close())

	// One command dispatched, one never sent.
	dispatched, err := ts.CreateCommand(ctx, a, host.ID, protocol.ActionRestart, "shop-web", "", protocol.Expectation{})
	mustTenant(t, err)
	mustTenant(t, ts.MarkCommandDispatched(ctx, dispatched.ID))
	queued, err := ts.CreateCommand(ctx, a, host.ID, protocol.ActionRestart, "blog-web", "", protocol.Expectation{})
	mustTenant(t, err)

	payload, err := backup.Collect(ctx, cfg, "test")
	mustTenant(t, err)
	path, restoredKey := restoreThroughCapsule(t, payload)
	restored, err := store.Open(ctx, config.DatabaseConfig{Driver: "sqlite", DSN: path})
	mustTenant(t, err)
	defer restored.Close()
	rs := restored.Tenancy()
	n, err := rs.ReconcileAfterStart(ctx)
	mustTenant(t, err)
	if n != 2 {
		t.Fatalf("reconcile settled %d commands, want 2", n)
	}

	access, err := rs.ResolveRegistryAccess(ctx, orgAccess, permissions.ApplicationDeploy, "ghcr.io/org/app:v1", restoredKey, true)
	mustTenant(t, err)
	if access.Credential == nil || access.Credential.Username != "bot" || access.Credential.Secret != registryCredential {
		t.Fatal("restore lost the registry credential")
	}
	for _, c := range []struct {
		app      string
		revision int
		want     string
	}{{shop.ID, 1, "shop-canary-1"}, {shop.ID, 2, "shop-canary-2"}, {blog.ID, 1, "blog-canary"}} {
		values, err := rs.ResolveApplicationSecrets(ctx, a, c.app, c.revision, restoredKey)
		mustTenant(t, err)
		if values["database-password"] != c.want {
			t.Fatalf("restore lost the values of %s revision %d", c.app, c.revision)
		}
	}
	checks, err := rs.ReadImageChecks(ctx, a, shop.ID)
	mustTenant(t, err)
	if checks.InstanceID != shopInstance || len(checks.Services) != 1 || checks.Services[0].Verdict != "update_available" || !checks.Services[0].CheckedAt.Equal(checkedAt) {
		t.Fatalf("restore lost the image check: %+v", checks)
	}
	instances, err := rs.ListApplicationInstances(ctx, a, host.ID)
	mustTenant(t, err)
	resources := map[string]string{}
	for _, i := range instances {
		if len(i.Containers) != 1 {
			t.Fatalf("instance %s has %d containers after restore", i.ID, len(i.Containers))
		}
		resources[i.ApplicationID] = i.Containers[0].ID
	}
	for _, m := range []struct {
		app, container string
		revision       int
	}{{shop.ID, newContainer, 2}, {blog.ID, blogContainer, 1}} {
		mapping, err := rs.ReadApplicationMapping(ctx, a, m.app)
		mustTenant(t, err)
		if mapping.Version != 1 || mapping.MappedRevision != m.revision || len(mapping.Bindings) != 1 || mapping.Bindings["web"] != m.container {
			t.Fatalf("restore lost the mapping of %s: %+v", m.app, mapping)
		}
	}
	if len(instances) != 2 || resources[shop.ID] != newContainer || resources[blog.ID] != blogContainer {
		t.Fatalf("restore lost application resources: %+v", instances)
	}
	got, err := rs.ReadDeployment(ctx, a, shop.ID, succeeded.ID)
	mustTenant(t, err)
	if got.State != "succeeded" || got.Plan.Services[0].ImageID != "sha256:"+strings.Repeat("c", 64) || got.Result == nil || len(got.Result.Services) != 1 || got.Result.Services[0].ContainerID != newContainer {
		t.Fatalf("succeeded deployment after restore: %+v", got)
	}
	got, err = rs.ReadDeployment(ctx, a, blog.ID, applying.ID)
	mustTenant(t, err)
	if got.State != "applying" || got.Result != nil {
		t.Fatalf("reconcile touched the applying deployment: %+v", got)
	}

	endpoints, err := rs.ListEndpoints(ctx, a, 0, 10)
	mustTenant(t, err)
	byID := map[string]store.Endpoint{}
	for _, e := range endpoints {
		byID[e.ID] = e
	}
	if e := byID[host.ID]; e.State != "active" || !slices.Equal(e.Capabilities, capabilities) {
		t.Fatalf("active endpoint after restore: %+v", e)
	}
	if e := byID[gone.ID]; e.State != "revoked" || e.RevokedAt == nil {
		t.Fatalf("revoked endpoint after restore: %+v", e)
	}
	identity, err := rs.AgentIdentity(ctx, host.ID, host.Fingerprint)
	mustTenant(t, err)
	if !hostKey.Equal(ed25519.PublicKey(identity.PublicKey)) {
		t.Fatal("restore lost the endpoint key")
	}
	if _, err := rs.AgentIdentity(ctx, gone.ID, gone.Fingerprint); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("revoked identity authenticates after restore: %v", err)
	}

	for _, id := range []string{dispatched.ID, queued.ID} {
		cmd, err := rs.ReadCommand(ctx, a, host.ID, id)
		mustTenant(t, err)
		if cmd.Outcome != protocol.OutcomeUnknown || cmd.Detail != "the server restarted before a result arrived" || cmd.SettledAt == nil {
			t.Fatalf("command %s after reconcile: %+v", id, cmd)
		}
	}
	reconciled := func() []store.AuditRecord {
		records, err := rs.ReadAudit(ctx, a, 0, 200)
		mustTenant(t, err)
		var out []store.AuditRecord
		for _, r := range records {
			if r.Action == "endpoint.commands.reconciled" {
				out = append(out, r)
			}
		}
		return out
	}
	audits := reconciled()
	if len(audits) != 1 || audits[0].Resource != host.ID || audits[0].Details != "commands=2" || audits[0].Result != "unknown" {
		t.Fatalf("reconcile audit: %+v", audits)
	}
	n, err = rs.ReconcileAfterStart(ctx)
	mustTenant(t, err)
	if n != 0 || len(reconciled()) != 1 {
		t.Fatalf("second reconcile changed state: %d settled", n)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	mustTenant(t, err)
	if _, err := rs.Enroll(ctx, store.EnrollmentRequest{Token: spareToken.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, spareToken.Secret)), Name: "late-host"}); err != nil {
		t.Fatalf("restore lost the unused enrollment token: %v", err)
	}
}
