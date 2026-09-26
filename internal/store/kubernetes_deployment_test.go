package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

const kubeUID = "0f1e2d3c-4b5a-4968-8776-655443322110"

// kubernetesPlanFixture maps a two-service application (web with a secret, api pinned by digest)
// to namespace shop of an active cluster, with anonymous pulls on.
func kubernetesPlanFixture(t *testing.T, spec ApplicationSpec, values map[string]string) (*SQLStore, TenantAccess, *Application, string, *ApplicationMapping) {
	t.Helper()
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, []string{"shop"}, nil)
	if err := ts.SetAnonymousPull(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	app := kubernetesApp(t, st, a, spec, values)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "shop"}); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, app, cluster, m
}

func twoServiceSpec() ApplicationSpec {
	return ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{
		{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Ports: []ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}, Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web.TOKEN"}}},
		{Name: "api", Image: "ghcr.io/org/api@" + digestOf("a")},
	}}
}

func kubePlanRequest(m *ApplicationMapping) PlanRequest {
	return PlanRequest{InstanceID: m.InstanceID, MappingVersion: m.Version, Revision: m.Preview.Revision, Confirm: m.Preview.Project, MaxFrameBytes: protocol.MaxDeploymentRequestBytes}
}

// A Kubernetes plan resolves every service's image at the registry, names each service's
// objects in the instance's namespace, and builds a frame with the target, the pinned pulls and
// the secret keys, and no Docker field.
func TestKubernetesPlanAndFrame(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "secret-canary"})
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), nil, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cluster plan without a resolver: %v", err)
	}
	pinned := kubePlanRequest(m)
	pinned.PinImages = map[string]string{"web": digestOf("c")}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, pinned, resolver, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cluster plan with pinned image IDs: %v", err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolver.called()) != 1 {
		t.Fatalf("registry calls %v: the pinned api needs none", resolver.called())
	}
	web, api := d.Plan.Services[0], d.Plan.Services[1]
	if d.Plan.Namespace != "shop" || d.Plan.Project != "shop-front" || web.Object == nil || *web.Object != (KubernetesObject{Namespace: "shop", Name: "shop-front-web"}) || api.Object.Name != "shop-front-api" {
		t.Fatalf("plan %+v", d.Plan)
	}
	if web.PullReference != "ghcr.io/org/web@"+digestOf("b") || web.PullDigest != digestOf("b") || api.PullDigest != digestOf("a") || web.ImageID != "" || web.ContainerID != "" || web.Replaces != (protocol.InspectionTarget{}) || len(web.Mounts) != 0 {
		t.Fatalf("services %+v %+v", web, api)
	}
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != "applying" || req.Kubernetes == nil || !reflect.DeepEqual(*req.Kubernetes, protocol.KubernetesTarget{Namespace: "shop", ApplicationID: app.ID, InstanceID: m.InstanceID, SpecDigest: d.SpecDigest}) || len(req.Registries) != 0 || len(req.Volumes) != 0 {
		t.Fatalf("frame %+v", req)
	}
	s := req.Services[0]
	if s.Pull == nil || s.Pull.Tag != "" || s.ContainerName != "" || s.Env["TOKEN"] != "secret-canary" || !slices.Equal(s.SecretKeys, []string{"TOKEN"}) || s.Ports[0] != (protocol.Port{Container: 80, Host: 8080, Protocol: "tcp"}) {
		t.Fatalf("frame service %+v", s)
	}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = cluster
}

// A namespace the manifest revokes between plan and apply must not reach the agent: apply
// re-reads the endpoint's granted list and refuses with the plan's k8s_namespace blocker,
// producing no frame and leaving the row planned.
func TestKubernetesApplyRefusesARevokedNamespace(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"k8s_namespace"}) || applied != nil || req != nil {
		t.Fatalf("revoked namespace: %+v %+v %v", applied, req, err)
	}
	if got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID); err != nil || got.State != "planned" {
		t.Fatalf("the refused apply left the row %+v %v", got, err)
	}
}

// A namespace move is refused once any apply was sent, even one that failed: a timed-out rollout
// leaves its objects applied in the old namespace, where a removal would never look.
func TestKubernetesMoveRefusedAfterAFailedApply(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, []string{"shop", "billing"}); err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeTimedOut, Code: protocol.ResultStepFailed,
		Steps: []protocol.DeploymentStep{
			{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
			{Service: "api", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
			{Service: "web", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded},
			{Service: "web", Step: protocol.StepStart, Outcome: protocol.OutcomeTimedOut, Code: "rollout_timeout", Detail: "pod=ImagePullBackOff"},
			{Service: "api", Step: protocol.StepCreate, Outcome: protocol.OutcomeSkipped},
			{Service: "api", Step: protocol.StepStart, Outcome: protocol.OutcomeSkipped},
		},
		Services: []protocol.DeploymentIdentity{kubeIdentity(d.Plan.Services[0])}}
	if err := ts.SettleDeployment(ctx, cluster, res); err != nil {
		t.Fatal(err)
	}
	if instance, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID); err != nil || instance.CurrentRevision != 0 {
		t.Fatalf("instance: %+v %v", instance, err)
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "billing"}); !errors.Is(err, ErrApplicationAdopted) {
		t.Fatalf("move after a timed-out apply: %v", err)
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "shop"}); err != nil {
		t.Fatalf("review in place: %v", err)
	}
}

// Turning anonymous pulls off between plan and apply stops a cluster apply the same way it stops
// a Docker one: the kubelet still needs a registry row or anonymous pull to resolve the tag.
func TestKubernetesApplyRefusesAnonymousPullTurnedOff(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.SetAnonymousPull(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	applied, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	if !errors.Is(err, ErrAdoptionChanged) || applied != nil || req != nil {
		t.Fatalf("anonymous pull off: %+v %+v %v", applied, req, err)
	}
	_ = cluster
}

// Every definition a Deployment cannot express stops at the plan with its code, all at once,
// and so does a namespace the manifest no longer grants.
func TestKubernetesPlanBlockers(t *testing.T) {
	spec := ApplicationSpec{Kind: "compose.v1", Volumes: []DeclaredVolume{{Name: "data"}}, Services: []ApplicationService{
		{Name: "db", Image: "ghcr.io/org/db:1", Volumes: []ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}}},
		{Name: "web", Image: "ghcr.io/org/web:1", Restart: "on-failure", Ports: []ApplicationPort{{Target: 80, Published: 80, HostIP: "127.0.0.1", Protocol: "tcp"}}},
		{Name: "api-", Image: "ghcr.io/org/api:1"},
		{Name: "ok", Image: "ghcr.io/org/ok:1"},
	}}
	st, a, app, cluster, m := kubernetesPlanFixture(t, spec, nil)
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{}}
	_, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"kubernetes_unsupported"}) {
		t.Fatalf("blocked: %v", err)
	}
	want := map[string][]string{"db": {"k8s_volume"}, "web": {"k8s_host_ip", "k8s_restart"}, "api-": {"k8s_name"}}
	if len(blocked.Services) != len(want) {
		t.Fatalf("services %+v", blocked.Services)
	}
	for _, s := range blocked.Services {
		if !slices.Equal(s.Unsupported, want[s.Name]) || !slices.Equal(s.Blockers, []string{"kubernetes_unsupported"}) {
			t.Errorf("%s: %+v", s.Name, s)
		}
	}
	if len(resolver.called()) != 0 {
		t.Fatal("a blocked plan asked the registry")
	}
	if _, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	pre, err := ts.PreflightApplication(ctx, a, app.ID)
	if err != nil || !slices.Contains(pre.Blockers, "k8s_namespace") || pre.Executable {
		t.Fatalf("namespace no longer granted: %+v %v", pre, err)
	}
}

// A cluster agent needs kubernetes.deploy and nothing else to take a plan.
func TestKubernetesPlanCapability(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.SetEndpointCapabilities(ctx, cluster, []string{protocol.CapabilityKubernetesInventory}); err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	_, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || !slices.Equal(blocked.Blockers, []string{"agent_deploy_unsupported"}) {
		t.Fatalf("without kubernetes.deploy: %v", err)
	}
}

// kubeIdentity is what a cluster agent reports for a planned service.
func kubeIdentity(ps PlannedService) protocol.DeploymentIdentity {
	return protocol.DeploymentIdentity{Service: ps.Name, Kind: protocol.KindDeployment, Namespace: ps.Object.Namespace, Name: ps.Object.Name, UID: kubeUID, Generation: 1, ImageDigest: ps.PullDigest}
}

// A cluster result settles with Deployment identities in the planned namespace, names and
// digests; anything else is refused, and a success advances the instance's revisions.
func TestSettleKubernetesDeployment(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil {
		t.Fatal(err)
	}
	result := func(ids ...protocol.DeploymentIdentity) protocol.DeploymentResult {
		return protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: ids}
	}
	web, api := kubeIdentity(d.Plan.Services[0]), kubeIdentity(d.Plan.Services[1])
	for name, bad := range map[string]protocol.DeploymentIdentity{
		"other namespace": func() protocol.DeploymentIdentity { i := web; i.Namespace = "billing"; return i }(),
		"other name":      func() protocol.DeploymentIdentity { i := web; i.Name = "shop-front-api"; return i }(),
		"other digest":    func() protocol.DeploymentIdentity { i := web; i.ImageDigest = digestOf("f"); return i }(),
		"a container":     {Service: "web", ContainerID: strings.Repeat("e", 64), ImageID: digestOf("e"), CreatedUnix: 1700000000, ImageDigest: digestOf("b")},
	} {
		if err := ts.SettleDeployment(ctx, cluster, result(bad, api)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ts.SettleDeployment(ctx, cluster, result(web)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a success missing a service: %v", err)
	}
	if err := ts.SettleDeployment(ctx, cluster, result(web, api)); err != nil {
		t.Fatal(err)
	}
	got, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || got.State != protocol.OutcomeSucceeded || len(got.Result.Services) != 2 || got.Result.Services[0].UID != kubeUID || got.Validation == nil {
		t.Fatalf("settled: %+v %v", got, err)
	}
	instance, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID)
	if err != nil || instance.CurrentRevision != 1 || instance.PreviousRevision != 0 {
		t.Fatalf("instance: %+v %v", instance, err)
	}
	// The instance has no recorded container to go back to.
	_, reason, err := ts.RollbackTarget(ctx, a, app.ID, d.ID)
	if err != nil || reason != RollbackNoPriorIdentity {
		t.Fatalf("rollback: %q %v", reason, err)
	}
}

// Removing a cluster instance names its services and target, not containers; a success releases
// the instance and marks the application removed.
func TestRemoveKubernetesApplication(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	d, req, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop-front"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Plan.Namespace != "shop" || len(req.Containers) != 0 || !slices.Equal(req.Services, []string{"api", "web"}) || req.Kubernetes == nil || req.Kubernetes.Namespace != "shop" || req.Kubernetes.InstanceID != m.InstanceID {
		t.Fatalf("removal %+v %+v", d.Plan, req)
	}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Services: []protocol.DeploymentIdentity{}, Steps: []protocol.DeploymentStep{
		{Service: "api", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
		{Service: "api", Step: protocol.StepRemove, Outcome: protocol.OutcomeSkipped},
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSucceeded},
	}}
	bad := res
	bad.Steps = append(slices.Clone(res.Steps), protocol.DeploymentStep{Service: "web", Step: protocol.StepStop, Outcome: protocol.OutcomeSucceeded})
	if err := ts.SettleDeployment(ctx, cluster, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a Docker step in a cluster removal: %v", err)
	}
	if err := ts.SettleDeployment(ctx, cluster, res); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("instance kept: %v", err)
	}
	var removed int
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM applications WHERE id=? AND removed_at IS NOT NULL`), app.ID).Scan(&removed); err != nil || removed != 1 {
		t.Fatalf("application not marked removed: %d %v", removed, err)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(records, func(r *AuditRecord) bool {
		return r.Action == string(permissions.ApplicationDestroy) && strings.Contains(r.Details, "services=2")
	}) {
		t.Fatal("the removal's audit row does not count its services")
	}
}

// A cluster instance's update check reads the running digest off its Deployment in the
// inventory, only when the Deployment carries the instance's label.
func TestKubernetesImageCheckReadsTheWorkload(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1"}}}, nil)
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("c")}}}
	check := func() ImageCheck {
		t.Helper()
		out, err := ts.CheckImageUpdates(ctx, a, app.ID, resolver, imageCheckKey, false)
		if err != nil || len(out.Services) != 1 {
			t.Fatalf("check: %+v %v", out, err)
		}
		return out.Services[0]
	}
	if c := check(); c.Verdict != "unknown_local" {
		t.Fatalf("no workload yet: %+v", c)
	}
	running := protocol.Workload{Kind: protocol.KindDeployment, Namespace: "shop", Name: "shop-front-web", Images: []string{"ghcr.io/org/web@" + digestOf("b")}, Instance: m.InstanceID, Application: app.ID}
	foreign := running
	foreign.Instance = ""
	putClusterInventory(t, ts, cluster, []protocol.Workload{foreign})
	if c := check(); c.Verdict != "unknown_local" {
		t.Fatalf("an unlabelled Deployment was read: %+v", c)
	}
	putClusterInventory(t, ts, cluster, []protocol.Workload{running})
	if c := check(); c.Verdict != "update_available" || c.LocalDigest != digestOf("b") || c.RemoteDigest != digestOf("c") {
		t.Fatalf("labelled Deployment: %+v", c)
	}
}

func claimSpec(k *KubernetesExtension) ApplicationSpec {
	return ApplicationSpec{Kind: "compose.v1", Volumes: []DeclaredVolume{{Name: "data"}, {Name: "logs"}}, Services: []ApplicationService{
		{Name: "db", Image: "ghcr.io/org/db:1", Volumes: []ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}, {Kind: "named", Source: "logs", Target: "/var/log/db", ReadOnly: true}}},
		{Name: "web", Image: "ghcr.io/org/web:1"},
	}, Kubernetes: k}
}

var bothChosen = &KubernetesExtension{Volumes: map[string]KubernetesVolume{
	"data": {StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce},
	"logs": {Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce},
}}

// Chosen named volumes plan as claims <project>-<volume> mounted by their one service, and the
// frame carries both; the claims are what the agent creates and nothing else.
func TestKubernetesPlanClaims(t *testing.T) {
	st, a, app, _, m := kubernetesPlanFixture(t, claimSpec(bothChosen), nil)
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/db:1": {digest: digestOf("b")}, "ghcr.io/org/web:1": {digest: digestOf("c")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	claims := []protocol.KubernetesClaim{
		{Name: "shop-front-data", StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce},
		{Name: "shop-front-logs", Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce},
	}
	mounts := []protocol.KubernetesMount{{Claim: "shop-front-data", MountPath: "/var/lib/db"}, {Claim: "shop-front-logs", MountPath: "/var/log/db", ReadOnly: true}}
	if !reflect.DeepEqual(d.Plan.Claims, claims) || !reflect.DeepEqual(d.Plan.Services[0].ClaimMounts, mounts) || len(d.Plan.Services[1].ClaimMounts) != 0 {
		t.Fatalf("plan %+v %+v", d.Plan.Claims, d.Plan.Services)
	}
	_, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Kubernetes.Claims, claims) || !reflect.DeepEqual(req.Services[0].Volumes, mounts) || len(req.Services[1].Volumes) != 0 || len(req.Volumes) != 0 || len(req.Services[0].Mounts) != 0 {
		t.Fatalf("frame %+v %+v", req.Kubernetes, req.Services)
	}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// A named volume without a choice is k8s_volume with detail choice_required; a bind keeps
// k8s_volume without one; a volume two services mount is k8s_volume_shared.
func TestKubernetesPlanVolumeBlockers(t *testing.T) {
	unchosen := claimSpec(&KubernetesExtension{Volumes: map[string]KubernetesVolume{"data": bothChosen.Volumes["data"]}})
	bind := claimSpec(bothChosen)
	bind.Services[0].Volumes = append(bind.Services[0].Volumes, ApplicationVolume{Kind: "bind", Source: "/srv/db", Target: "/etc/db"})
	shared := claimSpec(bothChosen)
	shared.Services[1].Volumes = []ApplicationVolume{{Kind: "named", Source: "data", Target: "/data"}}
	for name, tc := range map[string]struct {
		spec ApplicationSpec
		want map[string]BlockedService
	}{
		"no choice": {unchosen, map[string]BlockedService{"db": {Unsupported: []string{"k8s_volume"}, Details: map[string]string{"k8s_volume": "choice_required"}}}},
		"a bind":    {bind, map[string]BlockedService{"db": {Unsupported: []string{"k8s_volume"}}}},
		"shared":    {shared, map[string]BlockedService{"db": {Unsupported: []string{"k8s_volume_shared"}}, "web": {Unsupported: []string{"k8s_volume_shared"}}}},
	} {
		st, a, app, _, m := kubernetesPlanFixture(t, tc.spec, nil)
		_, err := st.Tenancy().PlanDeployment(context.Background(), a, app.ID, kubePlanRequest(m), &fakeResolver{reply: map[string]fakeReply{}}, imageCheckKey, false)
		var blocked *PreflightBlockedError
		if !errors.As(err, &blocked) || len(blocked.Services) != len(tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
		for _, s := range blocked.Services {
			if want := tc.want[s.Name]; !slices.Equal(s.Unsupported, want.Unsupported) || !reflect.DeepEqual(s.Details, want.Details) {
				t.Errorf("%s: %s %+v", name, s.Name, s)
			}
		}
	}
}

// A cluster removal reports each kept claim as a skipped volume step with detail retained, and
// settles; any other volume step is refused.
func TestRemoveKubernetesRetainsClaims(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	d, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop-front"})
	if err != nil {
		t.Fatal(err)
	}
	steps := []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepVolume, Outcome: protocol.OutcomeSkipped, Detail: protocol.DetailRetained},
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Services: []protocol.DeploymentIdentity{}, Steps: steps}
	bad := res
	bad.Steps = append(slices.Clone(steps[:2]), protocol.DeploymentStep{Service: "web", Step: protocol.StepVolume, Outcome: protocol.OutcomeSucceeded})
	if err := ts.SettleDeployment(ctx, cluster, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a volume step that is not a kept claim: %v", err)
	}
	if err := ts.SettleDeployment(ctx, cluster, res); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("instance kept: %v", err)
	}
}
