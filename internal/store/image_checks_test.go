package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func insertImageCheck(t *testing.T, st *SQLStore, instance string, at time.Time) {
	t.Helper()
	if _, err := st.db.Exec(st.rebind(`INSERT INTO image_checks(instance_id,service_name,reference,local_digest,remote_digest,verdict,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`), instance, "web", "nginx:1", "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), "update_available", "", at); err != nil {
		t.Fatal(err)
	}
}

func imageCheckCount(t *testing.T, st *SQLStore, instance string) (n int) {
	t.Helper()
	if err := st.db.QueryRow(st.rebind(`SELECT COUNT(*) FROM image_checks WHERE instance_id=?`), instance).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReadImageChecksEmptyWithoutInstance(t *testing.T) {
	st, a, app, _, _ := adoptionFixture(t)
	ctx := context.Background()
	out, err := st.Tenancy().ReadImageChecks(ctx, a, app.ID)
	if err != nil || out.InstanceID != "" || out.MappingVersion != 0 || out.Services == nil || len(out.Services) != 0 {
		t.Fatalf("empty read: %+v %v", out, err)
	}
	if _, err := st.Tenancy().ReadImageChecks(ctx, a, "not-a-uuid"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	noEnv := a
	noEnv.EnvironmentID = ""
	if _, err := st.Tenancy().ReadImageChecks(ctx, noEnv, app.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no environment: %v", err)
	}
}

func TestImageChecksClearedBySuccessfulApply(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	at := time.Now().UTC().Truncate(time.Second)
	insertImageCheck(t, st, m.InstanceID, at)
	out, err := ts.ReadImageChecks(ctx, a, app.ID)
	if err != nil || out.InstanceID != m.InstanceID || out.MappingVersion != m.Version || len(out.Services) != 1 {
		t.Fatalf("read: %+v %v", out, err)
	}
	if c := out.Services[0]; c.Service != "web" || c.Reference != "nginx:1" || c.Verdict != "update_available" || c.LocalDigest == "" || c.RemoteDigest == "" || !c.CheckedAt.Equal(at) {
		t.Fatalf("row: %+v", c)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeSucceeded, strings.Repeat("e", 64))); err != nil {
		t.Fatal(err)
	}
	if n := imageCheckCount(t, st, m.InstanceID); n != 0 {
		t.Fatalf("rows outlived the apply: %d", n)
	}
}

func TestImageChecksSurviveAFailedApply(t *testing.T) {
	st, a, app, endpoint, _, m, d, key := applyFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	insertImageCheck(t, st, m.InstanceID, time.Now().UTC())
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", key); err != nil {
		t.Fatal(err)
	}
	if err := ts.SettleDeployment(ctx, endpoint, settledResult(d, protocol.OutcomeFailed, "")); err != nil {
		t.Fatal(err)
	}
	if n := imageCheckCount(t, st, m.InstanceID); n != 1 {
		t.Fatalf("failed apply cleared the check: %d", n)
	}
}

func TestImageChecksCascadeOnRelease(t *testing.T) {
	st, a, app, endpoint, _ := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	insertImageCheck(t, st, instance.ID, time.Now().UTC())
	if err := ts.ReleaseApplication(ctx, a, app.ID, instance.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if n := imageCheckCount(t, st, instance.ID); n != 0 {
		t.Fatalf("rows outlived the release: %d", n)
	}
}
