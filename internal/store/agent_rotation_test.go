package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/Busness-app/kyyard-server/internal/agent/protocol"
	"github.com/Busness-app/kyyard-server/internal/store"
)

func rotateSig(current ed25519.PrivateKey, newPub ed25519.PublicKey) []byte {
	return ed25519.Sign(current, protocol.Preimage(protocol.ContextRotate, protocol.RawFingerprint(current.Public().(ed25519.PublicKey)), newPub))
}

func TestRotationWaitsForAcknowledgementAndAllowsOnePendingKey(t *testing.T) {
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	ts := st.Tenancy()
	a.EnvironmentID = "env-a"
	tok, err := ts.CreateEnrollmentToken(ctx, a, "docker", "")
	mustTenant(t, err)
	key := newAgentKey(t)
	e, err := ts.Enroll(ctx, key.request(tok.Secret, "host"))
	mustTenant(t, err)
	org := store.TenantAccess{ActorID: a.ActorID, OrganizationID: "a"}
	newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)

	// Pending endpoints cannot rotate; approval first.
	if _, err := ts.RotateEndpointKey(ctx, e.ID, newPub, rotateSig(key.priv, newPub), "10.0.0.1"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("pending endpoint rotated: %v", err)
	}
	mustTenant(t, ts.ApproveEndpoint(ctx, org, e.ID, e.Fingerprint))
	// A signature by a key that is not the approved one is refused.
	if _, err := ts.RotateEndpointKey(ctx, e.ID, newPub, rotateSig(newPriv, newPub), "10.0.0.1"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("self-signed rotation accepted: %v", err)
	}
	fp, err := ts.RotateEndpointKey(ctx, e.ID, newPub, rotateSig(key.priv, newPub), "10.0.0.1")
	mustTenant(t, err)
	if fp != protocol.Fingerprint(newPub) {
		t.Fatal("fingerprint")
	}
	// Only one pending key at a time.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := ts.RotateEndpointKey(ctx, e.ID, otherPub, rotateSig(key.priv, otherPub), "10.0.0.1"); !errors.Is(err, store.ErrRotationPending) {
		t.Fatalf("second pending key accepted: %v", err)
	}
	// The pending key does not authenticate; the approved one still does.
	if _, err := ts.AgentIdentity(ctx, e.ID, fp); !errors.Is(err, store.ErrKeyPendingReview) {
		t.Fatalf("pending key authenticated: %v", err)
	}
	if _, err := ts.AgentIdentity(ctx, e.ID, e.Fingerprint); err != nil {
		t.Fatalf("approved key refused during rotation: %v", err)
	}
	got, err := ts.ReadEndpoint(ctx, org, e.ID)
	mustTenant(t, err)
	if got.PendingFingerprint != fp || got.Fingerprint != e.Fingerprint || len(got.Alerts) != 1 || got.Alerts[0].Kind != "rotation_pending" {
		t.Fatalf("rotation not surfaced: %+v", got)
	}
	// Acknowledging the wrong fingerprint changes nothing; the right one retires the rest.
	if err := ts.AcknowledgeEndpointKey(ctx, org, e.ID, e.Fingerprint); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("acknowledged the approved key: %v", err)
	}
	viewer := store.TenantAccess{ActorID: a.ActorID, OrganizationID: "b"}
	if err := ts.AcknowledgeEndpointKey(ctx, viewer, e.ID, fp); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("cross-tenant acknowledgement: %v", err)
	}
	mustTenant(t, ts.AcknowledgeEndpointKey(ctx, org, e.ID, fp))
	if _, err := ts.AgentIdentity(ctx, e.ID, e.Fingerprint); !errors.Is(err, store.ErrKeyRetired) {
		t.Fatalf("old key not retired: %v", err)
	}
	if _, err := ts.AgentIdentity(ctx, e.ID, fp); err != nil {
		t.Fatalf("new key refused: %v", err)
	}
	got, err = ts.ReadEndpoint(ctx, org, e.ID)
	mustTenant(t, err)
	if got.Fingerprint != fp || got.PendingFingerprint != "" || len(got.Alerts) != 0 {
		t.Fatalf("post-acknowledgement state: %+v", got)
	}
	// Acknowledging twice is refused.
	if err := ts.AcknowledgeEndpointKey(ctx, org, e.ID, fp); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("double acknowledgement: %v", err)
	}
	// A duplicate connection blocks rotation until an operator clears the event.
	mustTenant(t, ts.RecordEndpointEvent(ctx, got, "high", "duplicate_connection", "from 10.0.0.9"))
	thirdPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := ts.RotateEndpointKey(ctx, e.ID, thirdPub, rotateSig(newPriv, thirdPub), "10.0.0.1"); !errors.Is(err, store.ErrRotationBlocked) {
		t.Fatalf("rotation not blocked by duplicate connection: %v", err)
	}
	got, _ = ts.ReadEndpoint(ctx, org, e.ID)
	if len(got.Alerts) != 1 || got.Alerts[0].Kind != "duplicate_connection" {
		t.Fatalf("duplicate event not surfaced: %+v", got)
	}
	if err := ts.AcknowledgeEndpointEvent(ctx, org, e.ID, got.Alerts[0].ID+999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown event acknowledged: %v", err)
	}
	mustTenant(t, ts.AcknowledgeEndpointEvent(ctx, org, e.ID, got.Alerts[0].ID))
	if _, err := ts.RotateEndpointKey(ctx, e.ID, thirdPub, rotateSig(newPriv, thirdPub), "10.0.0.1"); err != nil {
		t.Fatalf("rotation still blocked after clearing: %v", err)
	}
	// Capabilities replace the recorded set and refuse unsafe text.
	mustTenant(t, ts.SetEndpointCapabilities(ctx, e.ID, []string{"docker.containers", "docker.logs"}))
	if err := ts.SetEndpointCapabilities(ctx, e.ID, []string{"bad\ncap"}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unsafe capability accepted: %v", err)
	}
	many := make([]string, 1000)
	for i := range many {
		many[i] = "cap." + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
	}
	if err := ts.SetEndpointCapabilities(ctx, e.ID, many); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("1000 capabilities accepted: %v", err)
	}
	if err := ts.SetEndpointCapabilities(ctx, e.ID, []string{strings.Repeat("c", 255), strings.Repeat("d", 255), strings.Repeat("e", 255), strings.Repeat("f", 255), strings.Repeat("g", 255), strings.Repeat("h", 255), strings.Repeat("i", 255), strings.Repeat("j", 255), strings.Repeat("k", 255), strings.Repeat("l", 255), strings.Repeat("m", 255), strings.Repeat("n", 255), strings.Repeat("o", 255), strings.Repeat("p", 255), strings.Repeat("q", 255), strings.Repeat("r", 255), strings.Repeat("s", 255)}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("oversized capability set accepted: %v", err)
	}
	if err := ts.RecordEndpointEvent(ctx, got, "high", "bad\nkind", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unsafe event text accepted: %v", err)
	}
	got, _ = ts.ReadEndpoint(ctx, org, e.ID)
	if len(got.Capabilities) != 2 || got.Capabilities[0] != "docker.containers" {
		t.Fatalf("capabilities: %+v", got.Capabilities)
	}
	records, err := ts.ReadAudit(ctx, org, 0, 200)
	mustTenant(t, err)
	var rotated bool
	for _, r := range records {
		if r.Action == "endpoint.rotate" && r.UserID == "agent:"+e.ID && r.Result == "success" {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("rotation not audited")
	}
}
