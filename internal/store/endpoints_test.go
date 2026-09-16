package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
)

type agentKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newAgentKey(t *testing.T) agentKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	mustTenant(t, err)
	return agentKey{pub, priv}
}

func (k agentKey) request(token []byte, name string) store.EnrollmentRequest {
	return store.EnrollmentRequest{Token: token, PublicKey: k.pub, Proof: ed25519.Sign(k.priv, protocol.Preimage(protocol.ContextEnroll, token)), Name: name, Facts: map[string]string{"hostname": "host-1", "os": "linux", "secret": "must-be-dropped"}, IPAddress: "10.0.0.9"}
}

func TestEnrollmentTokenIsSingleUseAndBound(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	a.EnvironmentID = "env-a"
	if _, err := ts.CreateEnrollmentToken(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a"}, "docker", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("token without environment: %v", err)
	}
	if _, err := ts.CreateEnrollmentToken(ctx, a, "podman", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unknown runtime: %v", err)
	}
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	if len(tok.Secret) != protocol.TokenSize || tok.EnvironmentID != "env-a" {
		t.Fatalf("token shape: %+v", tok)
	}
	key := newAgentKey(t)

	// Wrong proof, wrong key for the proof, and a forged token all refuse identically.
	bad := key.request(tok.Secret, "host")
	bad.Proof[0] ^= 1
	if _, err := ts.Enroll(ctx, bad); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("bad proof: %v", err)
	}
	other := newAgentKey(t)
	mixed := key.request(tok.Secret, "host")
	mixed.PublicKey = other.pub
	if _, err := ts.Enroll(ctx, mixed); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("proof from another key: %v", err)
	}
	forged := make([]byte, protocol.TokenSize)
	if _, err := ts.Enroll(ctx, key.request(forged, "host")); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("forged token: %v", err)
	}
	// A name carrying control characters could forge lines in the approval dialog.
	if _, err := ts.Enroll(ctx, key.request(tok.Secret, "host\nwith key fingerprint\n"+strings.Repeat("0", 64))); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("control characters in name accepted: %v", err)
	}
	if _, err := ts.AddEnvironment(ctx, a, "env\x7fname"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("control character in environment name accepted: %v", err)
	}
	for _, hostile := range []string{"h1\nwith key fingerprint\n" + strings.Repeat("0", 64), "h1\u202e" + strings.Repeat("0", 64), strings.Repeat("h", 256)} {
		bad := key.request(tok.Secret, "host")
		bad.Facts = map[string]string{"hostname": hostile}
		if _, err := ts.Enroll(ctx, bad); !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("hostile fact value accepted: %v", err)
		}
	}
	for _, sep := range []string{"\u2028", "\u2029", "\u0085", "\u202e", "\u2066"} {
		if _, err := ts.Enroll(ctx, key.request(tok.Secret, "host"+sep+strings.Repeat("0", 64))); !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("line separator %q in name accepted: %v", sep, err)
		}
	}
	// A refused attempt did not consume the token. Two winners are impossible.
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := newAgentKey(t)
			_, err := ts.Enroll(ctx, k.request(tok.Secret, "racer"))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("unexpected race error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("token consumed %d times", wins)
	}
	if _, err := ts.Enroll(ctx, key.request(tok.Secret, "again")); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("consumed token reused: %v", err)
	}

	list, err := ts.ListEndpoints(ctx, a, 0, 50)
	mustTenant(t, err)
	if len(list) != 1 || list[0].State != "pending" || list[0].Runtime != "docker" || list[0].EnvironmentID != "env-a" || list[0].Facts["hostname"] != "host-1" {
		t.Fatalf("enrolled endpoint wrong: %+v", list)
	}
	if _, leaked := list[0].Facts["secret"]; leaked {
		t.Fatal("unknown fact key stored")
	}
	// A second token cannot reuse the name inside the organization.
	tok2, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	if _, err := ts.Enroll(ctx, newAgentKey(t).request(tok2.Secret, "racer")); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	// The token binds its environment: it never lands in another organization or environment.
	foreign := store.TenantAccess{ActorID: a.ActorID, OrganizationID: "b"}
	if _, err := ts.ListEndpoints(ctx, foreign, 0, 50); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("cross-tenant endpoint list: %v", err)
	}
	if _, err := ts.ReadEndpoint(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a", EnvironmentID: "env-b"}, list[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("mixed environment read: %v", err)
	}
	records, err := ts.ReadAudit(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a"}, 0, 200)
	mustTenant(t, err)
	var enrolled bool
	for _, r := range records {
		if r.Action == "agent.enroll" && r.UserID == "agent:"+list[0].ID && r.EnvironmentID == "env-a" && r.Result == "success" {
			enrolled = true
		}
	}
	if !enrolled {
		t.Fatalf("enrollment not audited: %+v", records)
	}
}

func TestEndpointApprovalBindsFingerprintAndTerminalStates(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	a.EnvironmentID = "env-a"
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	key := newAgentKey(t)
	e, err := ts.Enroll(ctx, key.request(tok.Secret, "host"))
	mustTenant(t, err)
	if e.Fingerprint != protocol.Fingerprint(key.pub) {
		t.Fatal("fingerprint not derived from the enrolled key")
	}
	org := store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a"}
	// Read roles may see it; they may not approve. Environment admins may.
	tenantUser(t, st, "viewer", "user", "local", "active")
	mustTenant(t, ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "viewer", Role: store.RoleReadOnly, Status: "active"}))
	viewer := store.TenantAccess{ActorID: "viewer", OrganizationID: "a"}
	if _, err := ts.ReadEndpoint(ctx, viewer, e.ID); err != nil {
		t.Fatalf("read-only could not read endpoint: %v", err)
	}
	if err := ts.ApproveEndpoint(ctx, viewer, e.ID, e.Fingerprint); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read-only approved: %v", err)
	}
	if _, err := ts.CreateEnrollmentToken(ctx, store.TenantAccess{ActorID: "viewer", OrganizationID: "a", EnvironmentID: "env-a"}, "docker", ""); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read-only minted a token: %v", err)
	}
	// The reviewed fingerprint must be the enrolled one.
	if err := ts.ApproveEndpoint(ctx, org, e.ID, protocol.Fingerprint(newAgentKey(t).pub)); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("approved a fingerprint nobody enrolled: %v", err)
	}
	mustTenant(t, ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint))
	got, err := ts.ReadEndpoint(ctx, org, e.ID)
	mustTenant(t, err)
	if got.State != "approved" || got.ApprovedBy != a.ActorID || got.ApprovedAt == nil || got.Fingerprint != e.Fingerprint {
		t.Fatalf("approval not recorded: %+v", got)
	}
	if err := ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second approval: %v", err)
	}
	if err := ts.RejectEndpoint(ctx, org, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reject of a non-pending endpoint: %v", err)
	}
	mustTenant(t, ts.RenameEndpoint(ctx, org, e.ID, "renamed"))
	// The environment cannot go while a live endpoint is in it.
	if err := ts.RemoveEnvironment(ctx, a); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("environment removed with a live endpoint: %v", err)
	}
	mustTenant(t, ts.RevokeEndpoint(ctx, org, e.ID))
	got, err = ts.ReadEndpoint(ctx, org, e.ID)
	mustTenant(t, err)
	if got.State != "revoked" || got.RevokedAt == nil || got.Fingerprint != "" {
		t.Fatalf("revocation not terminal: %+v", got)
	}
	if err := ts.RevokeEndpoint(ctx, org, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	if err := ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("approve after revoke: %v", err)
	}
	mustTenant(t, ts.RemoveEnvironment(ctx, a))
	if _, err := ts.ReadEndpoint(ctx, org, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked endpoint survived its environment: %v", err)
	}

	// A pending enrollment can be rejected; it is terminal too.
	tok2, err := ts.CreateEnrollmentToken(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "b", EnvironmentID: "env-b"}, "docker", "")
	if !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("token for an organization without membership: %v", err)
	}
	mustTenant(t, ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a2", OrganizationID: "a", Name: "Second"}))
	tok2, err = ts.CreateEnrollmentToken(ctx, store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a", EnvironmentID: "env-a2"}, "kubernetes", "")
	mustTenant(t, err)
	e2, err := ts.Enroll(ctx, newAgentKey(t).request(tok2.Secret, "host"))
	mustTenant(t, err)
	mustTenant(t, ts.RejectEndpoint(ctx, org, e2.ID))
	got, err = ts.ReadEndpoint(ctx, org, e2.ID)
	mustTenant(t, err)
	if got.State != "revoked" || got.Runtime != "kubernetes" {
		t.Fatalf("rejection: %+v", got)
	}
}
