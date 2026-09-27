package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func TestApplicationServiceIPsRevisionAndDeployment(t *testing.T) {
	st, a, app, endpoint, _ := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "network-secret-canary"})
	ctx, ts := context.Background(), st.Tenancy()
	ips := map[string]string{"web": "10.96.0.40"}
	if n, err := ts.SetApplicationServiceIPs(ctx, a, app.ID, 1, ips, imageCheckKey); err != nil || n != 2 {
		t.Fatalf("save: %d %v", n, err)
	}
	for n := 1; n <= 2; n++ {
		values, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, n, imageCheckKey)
		if err != nil || values["web.TOKEN"] != "network-secret-canary" {
			t.Fatalf("values %d: %v", n, err)
		}
	}
	before, _ := ts.ReadApplicationRevision(ctx, a, app.ID, 1)
	after, _ := ts.ReadApplicationRevision(ctx, a, app.ID, 2)
	if before.Spec.Kubernetes != nil || after.Spec.Kubernetes.ServiceIPs["web"] != ips["web"] || before.Digest == after.Digest {
		t.Fatal("revision not immutable or IP not digest-bound")
	}
	raw, _ := json.Marshal(after)
	if strings.Contains(string(raw), "network-secret-canary") {
		t.Fatal("revision exposed values")
	}
	if _, err := ts.SetApplicationServiceIPs(ctx, a, app.ID, 1, ips, imageCheckKey); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale save: %v", err)
	}
	// Ordinary Compose replacement preserves the chosen IP and takes explicit values.
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 2, twoServiceSpec(), map[string]string{"web.TOKEN": "replacement-secret"}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	rev, _ := ts.ReadApplicationRevision(ctx, a, app.ID, 3)
	if rev.Spec.Kubernetes.serviceIP("web") != ips["web"] {
		t.Fatal("replacement dropped IP")
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	var blocked *PreflightBlockedError
	if _, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false); !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, "agent_service_ips_unsupported") {
		t.Fatalf("old agent plan: %v", err)
	}
	ep, _ := ts.ReadEndpoint(ctx, a, endpoint)
	upgraded := append(slices.Clone(ep.Capabilities), protocol.CapabilityKubernetesServiceIPs)
	if err := ts.SetEndpointCapabilities(ctx, endpoint, upgraded); err != nil {
		t.Fatal(err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Plan.Services[0].ClusterIP != ips["web"] {
		t.Fatal("plan dropped IP")
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, ep.Capabilities); err != nil {
		t.Fatal(err)
	}
	if _, frame, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, m.Preview.Project, imageCheckKey, protocol.MaxDeploymentRequestBytes); !errors.As(err, &blocked) || frame != nil {
		t.Fatalf("downgraded agent received frame: %v", err)
	}
	if err := ts.SetEndpointCapabilities(ctx, endpoint, upgraded); err != nil {
		t.Fatal(err)
	}
	_, frame, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, m.Preview.Project, imageCheckKey, protocol.MaxDeploymentRequestBytes)
	if err != nil || frame.Services[0].ClusterIP != ips["web"] || frame.Services[0].Env["TOKEN"] != "replacement-secret" {
		t.Fatalf("apply IP/values: %v", err)
	}
	if _, err := ts.SetApplicationServiceIPs(ctx, a, app.ID, 3, map[string]string{}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	cleared, _ := ts.ReadApplicationRevision(ctx, a, app.ID, 4)
	if cleared.Spec.Kubernetes.serviceIP("web") != "" {
		t.Fatal("explicit clear carried the old IP")
	}
}

func TestApplicationServiceIPsValidationAuthorizationAndAtomicity(t *testing.T) {
	st, a, app, _, _ := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "canary"})
	ctx, ts := context.Background(), st.Tenancy()
	for _, ips := range []map[string]string{{"missing": "10.96.0.40"}, {"web": "None"}, {"web": "10.96.0.40/24"}, {"api": "10.96.0.40"}, {"web": ""}} {
		if _, err := ts.SetApplicationServiceIPs(ctx, a, app.ID, 1, ips, imageCheckKey); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid IP map accepted: %v", err)
		}
	}
	foreign := a
	foreign.OrganizationID = "other"
	if _, err := ts.SetApplicationServiceIPs(ctx, foreign, app.ID, 1, nil, imageCheckKey); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-tenant save: %v", err)
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.SetApplicationServiceIPs(ctx, a, app.ID, 1, nil, imageCheckKey); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only save: %v", err)
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DROP TABLE audit_records`); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.SetApplicationServiceIPs(ctx, a, app.ID, 1, map[string]string{"web": "10.96.0.40"}, imageCheckKey); err == nil {
		t.Fatal("save without audit")
	}
	var head, count int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT latest_revision,(SELECT COUNT(*) FROM application_revisions WHERE application_id=applications.id) FROM applications WHERE id=?`), app.ID).Scan(&head, &count); err != nil || head != 1 || count != 1 {
		t.Fatalf("partial save: %d %d %v", head, count, err)
	}
}

func TestStaticServiceIPPreflight(t *testing.T) {
	spec := twoServiceSpec()
	spec.Kubernetes = &KubernetesExtension{ServiceIPs: map[string]string{"web": "10.96.0.40"}}
	m := &ApplicationMapping{InstanceID: "instance", Namespace: "shop", DeployNamespaces: []string{"shop"}, Preview: &AdoptionPreview{Project: "shop"}}
	for _, tc := range []struct {
		svc     protocol.Service
		blocker string
	}{
		{protocol.Service{Namespace: "other", Name: "another", ClusterIP: "10.96.0.40"}, "service_ip_in_use"},
		{protocol.Service{Namespace: "shop", Name: "shop-web", Instance: "instance", ClusterIP: "10.96.0.41"}, "service_ip_immutable"},
		{protocol.Service{Namespace: "shop", Name: "shop-web", Instance: "instance", ClusterIP: "10.96.0.40"}, ""},
	} {
		p := buildKubernetesPreflight(m, spec, protocol.Snapshot{Kubernetes: &protocol.KubernetesInventory{Services: []protocol.Service{tc.svc}}}, nil)
		if tc.blocker == "" && !p.Executable || tc.blocker != "" && !slices.Contains(p.Services[0].Blockers, tc.blocker) {
			t.Fatalf("preflight: %+v", p)
		}
	}
}
