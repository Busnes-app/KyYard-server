package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A cluster plan pinned by digest references pulls exactly those digests and asks no registry;
// pins that are not canonical digest references, or that leave a service unpinned, are invalid.
func TestPlanDeploymentPinsAClusterDigest(t *testing.T) {
	st, a, app, _, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	web, api := "ghcr.io/org/web@"+digestOf("c"), "ghcr.io/org/api@"+digestOf("a")
	for name, pins := range map[string]map[string]string{
		"one service unpinned": {"web": web},
		"a tag":                {"web": "ghcr.io/org/web:2", "api": api},
		"an image ID":          {"web": digestOf("c"), "api": api},
		"a tag and a digest":   {"web": "ghcr.io/org/web:1@" + digestOf("c"), "api": api},
		"not canonical":        {"web": "ghcr.io/org/web@" + digestOf("c"), "api": "GHCR.io/org/api@" + digestOf("a")},
		"an unknown service":   {"web": web, "api": api, "db": web},
		"a renamed service":    {"db": web, "api": api},
		"another repository":   {"web": "ghcr.io/org/api@" + digestOf("c"), "api": api},
		"another host":         {"web": "docker.io/org/web@" + digestOf("c"), "api": api},
	} {
		req := kubePlanRequest(m)
		req.PinImages = pins
		if _, err := ts.PlanDeployment(ctx, a, app.ID, req, nil, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The registry would answer another digest for web: the pin wins, and nothing asks it.
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	req := kubePlanRequest(m)
	req.PinImages = map[string]string{"web": web, "api": api}
	for _, r := range []DigestResolver{resolver, nil} {
		d, err := ts.PlanDeployment(ctx, a, app.ID, req, r, imageCheckKey, false)
		if err != nil {
			t.Fatal(err)
		}
		if ps := d.Plan.Services[0]; ps.PullReference != web || ps.PullDigest != digestOf("c") || ps.Reference != "ghcr.io/org/web:1" || d.Plan.Services[1].PullReference != api {
			t.Fatalf("pinned plan %+v", d.Plan.Services)
		}
		_, frame, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
		if err != nil {
			t.Fatal(err)
		}
		if p := frame.Services[0].Pull; p == nil || p.Reference != web || p.Digest != digestOf("c") || p.Tag != "" {
			t.Fatalf("frame pull %+v", p)
		}
		if err := ts.FailDeployment(ctx, d.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if calls := resolver.called(); len(calls) != 0 {
		t.Fatalf("registry asked %v", calls)
	}
}

// rewritePlan edits a settled deployment's stored plan or revision, standing in for history the
// API cannot produce in one test.
func rewritePlan(t *testing.T, st *SQLStore, id string, revision int, edit func(*DeploymentPlan)) {
	t.Helper()
	var raw string
	if err := st.db.QueryRowContext(context.Background(), st.rebind(`SELECT plan FROM deployments WHERE id=?`), id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan DeploymentPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	edit(&plan)
	out, _ := json.Marshal(plan)
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE deployments SET plan=?,revision=? WHERE id=?`), string(out), revision, id); err != nil {
		t.Fatal(err)
	}
}

// A cluster deployment rolls back to the digests the previous succeeded apply pinned, at its
// revision, only while it is still the latest apply and the prior plan names the same namespace,
// services and claims with a digest for every service and a revision that still validates.
func TestClusterRollbackTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*DeploymentPlan)
		rev    int
		reason string
	}{
		{"eligible", func(*DeploymentPlan) {}, 1, ""},
		{"another namespace", func(p *DeploymentPlan) { p.Namespace = "billing" }, 1, RollbackServiceSetChanged + "," + RollbackNamespaceChanged},
		{"another service set", func(p *DeploymentPlan) { p.Services = p.Services[:1] }, 1, RollbackServiceSetChanged},
		{"a claim the failed plan lacks", func(p *DeploymentPlan) {
			p.Claims = []protocol.KubernetesClaim{{Name: "shop-front-data", Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}}
		}, 1, RollbackServiceSetChanged + "," + RollbackClaimsChanged},
		{"a mount the failed plan lacks", func(p *DeploymentPlan) {
			p.Services[0].ClaimMounts = []protocol.KubernetesMount{{Claim: "shop-front-data", MountPath: "/data"}}
		}, 1, RollbackServiceSetChanged + "," + RollbackClaimsChanged},
		{"a service without a digest", func(p *DeploymentPlan) { p.Services[1].PullDigest = "" }, 1, RollbackNoPriorIdentity},
		{"a revision that is gone", func(*DeploymentPlan) {}, 99, RollbackPriorDefinitionInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
			ctx := context.Background()
			ts := st.Tenancy()
			prior := clusterApply(t, st, a, app, cluster, digestOf("b"))
			failed := clusterApply(t, st, a, app, cluster, digestOf("c"))
			rewritePlan(t, st, prior.ID, tc.rev, tc.edit)
			rb, reason, err := ts.RollbackTarget(ctx, a, app.ID, failed.ID)
			if err != nil || reason != tc.reason {
				t.Fatalf("reason %q %v, want %q", reason, err, tc.reason)
			}
			if tc.reason != "" {
				return
			}
			want := &Rollback{InstanceID: m.InstanceID, MappingVersion: m.Version, Project: "shop-front", Revision: 1, Images: map[string]string{"web": "ghcr.io/org/web@" + digestOf("b"), "api": "ghcr.io/org/api@" + digestOf("a")}}
			if !reflect.DeepEqual(rb, want) {
				t.Fatalf("rollback %+v, want %+v", rb, want)
			}
		})
	}
	// A failed apply between the two is no rollback target: the older succeeded one is.
	t.Run("a failed apply between", func(t *testing.T) {
		st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
		ctx := context.Background()
		ts := st.Tenancy()
		clusterApply(t, st, a, app, cluster, digestOf("b"))
		resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("d")}}}
		d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
			t.Fatal(err)
		}
		if err := ts.FailDeployment(ctx, d.ID, "test"); err != nil {
			t.Fatal(err)
		}
		failed := clusterApply(t, st, a, app, cluster, digestOf("c"))
		rb, reason, err := ts.RollbackTarget(ctx, a, app.ID, failed.ID)
		if err != nil || reason != "" || rb.Images["web"] != "ghcr.io/org/web@"+digestOf("b") {
			t.Fatalf("rollback %+v %q %v", rb, reason, err)
		}
	})
	// Any apply sent after the validated one may have changed the cluster: the rollback frame has
	// no compare-and-swap, so only a later plan never applied leaves it eligible.
	for name, later := range map[string]func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster string){
		"a later succeeded apply": func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster string) {
			clusterApply(t, st, a, app, cluster, digestOf("d"))
		},
		"a later applying apply": func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster string) {
			laterApply(t, st, a, app)
		},
		"a later unknown apply": func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster string) {
			laterApply(t, st, a, app)
			if _, err := st.Tenancy().AbandonDeployments(context.Background(), cluster); err != nil {
				t.Fatal(err)
			}
		},
		"a later failed apply": func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, cluster string) {
			if err := st.Tenancy().FailDeployment(context.Background(), laterApply(t, st, a, app), "test"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, a, app, cluster, _ := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
			clusterApply(t, st, a, app, cluster, digestOf("b"))
			failed := clusterApply(t, st, a, app, cluster, digestOf("c"))
			later(t, st, a, app, cluster)
			if _, reason, err := st.Tenancy().RollbackTarget(context.Background(), a, app.ID, failed.ID); err != nil || reason != RollbackServiceSetChanged {
				t.Fatalf("reason %q %v", reason, err)
			}
		})
	}
	// A later plan never applied changes nothing.
	t.Run("a later planned apply", func(t *testing.T) {
		st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
		ctx := context.Background()
		clusterApply(t, st, a, app, cluster, digestOf("b"))
		failed := clusterApply(t, st, a, app, cluster, digestOf("c"))
		resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("d")}}}
		if _, err := st.Tenancy().PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false); err != nil {
			t.Fatal(err)
		}
		if _, reason, err := st.Tenancy().RollbackTarget(ctx, a, app.ID, failed.ID); err != nil || reason != "" {
			t.Fatalf("reason %q %v", reason, err)
		}
	})
}

// laterApply plans and sends a cluster apply that has not settled, returning its ID.
func laterApply(t *testing.T, st *SQLStore, a TenantAccess, app *Application) string {
	t.Helper()
	ctx := context.Background()
	ts := st.Tenancy()
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("d")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, m.Preview.Project, imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	return d.ID
}
