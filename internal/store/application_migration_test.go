package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func migrationSpec() ApplicationSpec {
	return ApplicationSpec{Kind: "compose.v1", Volumes: []DeclaredVolume{{Name: "data"}}, Services: []ApplicationService{
		{Name: "db", Image: "ghcr.io/org/db:1", Restart: "always", Volumes: []ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}}, Environment: map[string]ApplicationSecretRef{"PASSWORD": {SecretRef: "db.PASSWORD"}}},
		{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Ports: []ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}},
	}}
}

// migrationFixture imports spec as "shop" with values, adopts and maps one container per service
// on a Docker endpoint, and enrolls a cluster granting namespace shop whose inventory reports the
// StorageClasses standard (the default) and fast.
func migrationFixture(t *testing.T, spec ApplicationSpec, values map[string]string) (*SQLStore, TenantAccess, *Application, string) {
	t.Helper()
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app, err := ts.ImportApplication(ctx, a, "shop", spec, values, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Volumes: []protocol.Volume{{Name: "shop_data"}}}
	bindings := map[string]string{}
	for i, s := range spec.Services {
		c := protocol.Container{ID: strings.Repeat("a", 63) + string("a0123456789"[i]), Name: "shop-" + s.Name, ImageID: "sha256:" + strings.Repeat("b", 63) + string("b0123456789"[i]), CreatedAt: time.Now().UTC().Add(-time.Hour), ComposeProject: "shop", Mounts: []protocol.Mount{}, Networks: []string{"shop_default"}}
		snapshot.Containers = append(snapshot.Containers, c)
		snapshot.Images = append(snapshot.Images, protocol.Image{ID: c.ImageID, Tags: []string{s.Image}})
		bindings[s.Name] = c.ID
	}
	endpoint := activeEndpointWith(t, ts, a, snapshot.Containers, nil)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := mappingRequest(m)
	r.Bindings = bindings
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
	cluster := activeCluster(t, ts, a, []string{"shop"}, nil)
	putStorageClasses(t, ts, cluster, []protocol.StorageClass{{Name: "standard", Default: true}, {Name: "fast"}})
	return st, a, app, cluster
}

// putStorageClasses reports a fresh cluster snapshot with classes and no workload.
func putStorageClasses(t *testing.T, ts TenancyStore, endpoint string, classes []protocol.StorageClass) {
	t.Helper()
	snap := protocol.Snapshot{Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.31.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: []protocol.Workload{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}, StorageClasses: classes}}
	raw, _ := json.Marshal(snap)
	generation := uint64(time.Now().Unix()) - 1000 + clusterGeneration.Add(1)
	if ok, err := ts.AcceptInventory(context.Background(), endpoint, generation, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("cluster inventory: %v %v", ok, err)
	}
}

func analysis(revision int, ready bool) MigrationAnalysis {
	return MigrationAnalysis{Revision: revision, Report: []byte(`{"version":1}`), Ready: ready}
}

var dataChoice = KubernetesExtension{Volumes: map[string]KubernetesVolume{"data": {StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}}}

// A migration starts only from a Docker source to a granted namespace of a cluster, once per
// source; the source reads its inputs; choices are held to its named volumes and the cluster's
// classes while analyzed; the destination copies the analyzed revision with the choices and its
// values, mapped to the namespace; two confirmations with notes close it, and every write is
// audited on the source and the destination.
func TestMigrationLifecycle(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "migration-secret-canary"})
	ctx := context.Background()
	ts := st.Tenancy()
	start := MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}

	src, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if src.Revision != 1 || src.Project != "shop" || len(src.Containers) != 2 || src.Containers["db"].Name != "shop-db" || src.Destination.Project != KubernetesProject(src.Destination.Name) || !strings.HasPrefix(src.Destination.Name, "shop on cluster-") || len(src.Destination.StorageClasses) != 2 || !slices.Equal(src.Destination.Namespaces, []string{"shop"}) || len(src.Volumes) != 1 {
		t.Fatalf("source %+v", src)
	}
	if _, err := ts.ReadMigrationSource(ctx, a, app.ID, src.EndpointID); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("a Docker destination: %v", err)
	}
	for name, tc := range map[string]struct {
		start MigrationStart
		an    MigrationAnalysis
		want  error
	}{
		"ungranted namespace": {MigrationStart{DestinationEndpointID: cluster, Namespace: "billing"}, analysis(1, false), ErrNamespaceUnknown},
		"docker destination":  {MigrationStart{DestinationEndpointID: src.EndpointID, Namespace: "shop"}, analysis(1, false), ErrRuntimeUnsupported},
		"stale analysis":      {start, analysis(2, false), ErrMigrationStale},
		"empty report":        {start, MigrationAnalysis{Revision: 1, Report: nil}, ErrInvalid},
		"oversized report":    {start, MigrationAnalysis{Revision: 1, Report: []byte(`"` + strings.Repeat("x", MaxMigrationReportBytes) + `"`)}, ErrInvalid},
	} {
		if _, err := ts.CreateMigration(ctx, a, app.ID, tc.start, tc.an); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	m, err := ts.CreateMigration(ctx, a, app.ID, start, analysis(1, false))
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != MigrationAnalyzed || m.Role != "source" || m.ApplicationName != "shop" || m.Ready || m.SourceRevision != 1 || len(m.Choices.Volumes) != 0 || string(m.Report) != `{"version":1}` {
		t.Fatalf("created %+v", m)
	}
	if _, err := ts.CreateMigration(ctx, a, app.ID, start, analysis(1, false)); !errors.Is(err, ErrMigrationOpen) {
		t.Fatalf("a second open migration: %v", err)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrMigrationNotReady) {
		t.Fatalf("destination before ready: %v", err)
	}
	for name, tc := range map[string]struct {
		v    KubernetesVolume
		vol  string
		want error
	}{
		"unknown volume":   {dataChoice.Volumes["data"], "logs", ErrVolumeUnknown},
		"bad size":         {KubernetesVolume{StorageClass: "fast", Size: "10G", AccessMode: protocol.AccessReadWriteOnce}, "data", ErrSizeInvalid},
		"unreported class": {KubernetesVolume{StorageClass: "gold", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}, "data", ErrStorageClassUnknown},
		"read write many":  {KubernetesVolume{StorageClass: "fast", Size: "10Gi", AccessMode: "ReadWriteMany"}, "data", ErrInvalid},
		"class grammar":    {KubernetesVolume{StorageClass: "Fast!", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}, "data", ErrStorageClassUnknown},
	} {
		if _, err := ts.AnalyzeMigration(ctx, a, app.ID, KubernetesExtension{Volumes: map[string]KubernetesVolume{tc.vol: tc.v}}, analysis(1, true)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	defaulted := KubernetesExtension{Volumes: map[string]KubernetesVolume{"data": {Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}}}
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, defaulted, analysis(1, true)); err != nil {
		t.Fatalf("the default class while one is reported: %v", err)
	}
	putStorageClasses(t, ts, cluster, []protocol.StorageClass{{Name: "fast"}})
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, defaulted, analysis(1, true)); !errors.Is(err, ErrStorageClassUnknown) {
		t.Fatalf("the default class with none reported: %v", err)
	}
	m, err = ts.AnalyzeMigration(ctx, a, app.ID, dataChoice, MigrationAnalysis{Revision: 1, Report: []byte(`{"version":1,"ready":true}`), Ready: true})
	if err != nil || !m.Ready || m.Choices.Volumes["data"] != dataChoice.Volumes["data"] || string(m.Report) != `{"version":1,"ready":true}` {
		t.Fatalf("analyzed %+v %v", m, err)
	}

	m, err = ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != MigrationDestinationCreated || m.DestinationApplicationID == "" || m.DestinationApplicationName != src.Destination.Name {
		t.Fatalf("destination %+v", m)
	}
	rev, err := ts.ReadApplicationRevision(ctx, a, m.DestinationApplicationID, 1)
	if err != nil || rev.Spec.Kubernetes == nil || rev.Spec.Kubernetes.Volumes["data"] != dataChoice.Volumes["data"] || len(rev.Spec.Services) != 2 || rev.Spec.Services[0].Environment["PASSWORD"].SecretRef != "db.PASSWORD" {
		t.Fatalf("destination revision %+v %v", rev, err)
	}
	if source, _ := ts.ReadApplicationRevision(ctx, a, app.ID, 1); source.Spec.Kubernetes != nil {
		t.Fatal("the source definition changed")
	}
	values, err := ts.ResolveApplicationSecrets(ctx, a, m.DestinationApplicationID, 1, imageCheckKey)
	if err != nil || values["db.PASSWORD"] != "migration-secret-canary" {
		t.Fatalf("copied values %v %v", values, err)
	}
	mapped, err := ts.ReadApplicationMapping(ctx, a, m.DestinationApplicationID)
	if err != nil || mapped.Namespace != "shop" || mapped.Runtime != protocol.RuntimeKubernetes || mapped.Preview.Project != src.Destination.Project || mapped.Version != 1 || mapped.MappedRevision != 1 {
		t.Fatalf("destination mapping %+v %v", mapped, err)
	}
	if dest, err := ts.ReadMigration(ctx, a, m.DestinationApplicationID); err != nil || dest.ID != m.ID || dest.Role != "destination" || dest.ApplicationID != app.ID {
		t.Fatalf("read as the destination %+v %v", dest, err)
	}
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, dataChoice, analysis(1, true)); !errors.Is(err, ErrMigrationState) {
		t.Fatalf("choices after the destination: %v", err)
	}
	if _, err := ts.ConfirmMigration(ctx, a, app.ID, "cutover", "switched"); !errors.Is(err, ErrMigrationState) {
		t.Fatalf("cutover before validation: %v", err)
	}
	for _, note := range []string{"", "   ", "two\nlines", strings.Repeat("n", MaxMigrationNoteRunes+1), "bidi ‮"} {
		if _, err := ts.ConfirmMigration(ctx, a, app.ID, "validated", note); !errors.Is(err, ErrInvalid) {
			t.Errorf("note %q: %v", note, err)
		}
	}
	if _, err := ts.ConfirmMigration(ctx, a, app.ID, "abandoned", "no"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown step: %v", err)
	}
	if m, err = ts.ConfirmMigration(ctx, a, app.ID, "validated", "Checked the orders page on the cluster: ✓"); err != nil || m.Status != MigrationValidated || m.ValidatedBy != "actor" || m.ValidatedAt == nil {
		t.Fatalf("validated %+v %v", m, err)
	}
	if m, err = ts.ConfirmMigration(ctx, a, app.ID, "cutover", "DNS moved to the ingress"); err != nil || m.Status != MigrationCutoverConfirmed || m.ConfirmedBy != "actor" || m.CutoverNote != "DNS moved to the ingress" {
		t.Fatalf("cutover %+v %v", m, err)
	}
	if _, err := ts.ReadMigration(ctx, a, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a closed migration read as the source: %v", err)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{app.ID + "/migration/" + m.ID, m.DestinationApplicationID + "/migration/" + m.ID} {
		if !slices.ContainsFunc(records, func(r *AuditRecord) bool {
			return r.Action == "application.migrate" && r.Resource == resource && r.Result == "success"
		}) {
			t.Errorf("no application.migrate row on %s", resource)
		}
	}
	for _, r := range records {
		if strings.Contains(r.Details, "canary") || strings.Contains(r.Resource, "canary") {
			t.Fatalf("a secret reached the audit trail: %+v", r)
		}
	}
}

// Only an organization administrator migrates; a reader may read the migration.
func TestMigrationAuthorization(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, false)); err != nil {
		t.Fatal(err)
	}
	for _, role := range []TenantRole{RoleEnvironmentAdmin, RoleDeveloper} {
		if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: "actor", Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.AbandonMigration(ctx, a, app.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s abandoned: %v", role, err)
		}
		if _, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s read the source: %v", role, err)
		}
		if m, err := ts.ReadMigration(ctx, a, app.ID); err != nil || m.Status != MigrationAnalyzed {
			t.Errorf("%s read: %+v %v", role, m, err)
		}
	}
}

// The destination refuses a name already taken and a source whose definition moved on since the
// analysis; nothing is created either way.
func TestMigrationDestination(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
	src, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster)
	if err != nil {
		t.Fatal(err)
	}
	squatter, err := ts.ImportApplication(ctx, a, src.Destination.Name, twoServiceSpec(), map[string]string{"web.TOKEN": "x"}, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrApplicationNameTaken) {
		t.Fatalf("a taken name: %v", err)
	}
	if err := ts.DiscardApplication(ctx, a, squatter.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, migrationSpec(), map[string]string{"db.PASSWORD": "changed"}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrMigrationStale) {
		t.Fatalf("a stale analysis: %v", err)
	}
	apps, err := ts.ListApplications(ctx, a, 0, 100)
	if err != nil || len(apps) != 1 {
		t.Fatalf("applications %+v %v", apps, err)
	}
}

// The source of an open migration cannot be removed; abandoning keeps a created destination and
// lets the removal through.
func TestMigrationKeepsTheSource(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
	m, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := ts.ListApplicationInstances(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	source := instances[slices.IndexFunc(instances, func(i ApplicationInstance) bool { return i.ApplicationID == app.ID })]
	if _, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: source.ID, Confirm: "shop"}); !errors.Is(err, ErrMigrationOpen) {
		t.Fatalf("removing a migrating source: %v", err)
	}
	abandoned, err := ts.AbandonMigration(ctx, a, app.ID)
	if err != nil || abandoned.Status != MigrationAbandoned || abandoned.DestinationApplicationID != m.DestinationApplicationID {
		t.Fatalf("abandoned %+v %v", abandoned, err)
	}
	if _, err := ts.ReadApplicationMapping(ctx, a, m.DestinationApplicationID); err != nil {
		t.Fatalf("the destination went with the migration: %v", err)
	}
	if _, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: source.ID, Confirm: "shop"}); err != nil {
		t.Fatalf("removing the source after abandoning: %v", err)
	}
	if _, err := ts.AbandonMigration(ctx, a, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abandoning twice: %v", err)
	}
}

// Every apply of an open migration's destination records the migration; the source's applies
// never do, and a closed migration stops recording.
func TestMigrationIDOnDestinationApplies(t *testing.T) {
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web.TOKEN"}}}}}
	st, a, app, cluster := migrationFixture(t, spec, map[string]string{"web.TOKEN": "t"})
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.SetAnonymousPull(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
	m, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	dest := m.DestinationApplicationID
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	apply := func() *Deployment {
		t.Helper()
		mapped, err := ts.ReadApplicationMapping(ctx, a, dest)
		if err != nil {
			t.Fatal(err)
		}
		d, err := ts.PlanDeployment(ctx, a, dest, kubePlanRequest(mapped), resolver, imageCheckKey, false)
		if err != nil {
			t.Fatal(err)
		}
		applied, _, err := ts.ApplyDeployment(ctx, a, dest, d.ID, mapped.Preview.Project, imageCheckKey, protocol.MaxDeploymentRequestBytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.SettleDeployment(ctx, cluster, protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{kubeIdentity(d.Plan.Services[0])}}); err != nil {
			t.Fatal(err)
		}
		return applied
	}
	first := apply()
	if first.MigrationID != m.ID {
		t.Fatalf("apply of the destination: %q", first.MigrationID)
	}
	if read, err := ts.ReadDeployment(ctx, a, dest, first.ID); err != nil || read.MigrationID != m.ID {
		t.Fatalf("read back %+v %v", read, err)
	}
	if list, err := ts.ListDeployments(ctx, a, dest); err != nil || list[0].MigrationID != m.ID {
		t.Fatalf("listed %+v %v", list, err)
	}
	source, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(source), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if applied, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil || applied.MigrationID != "" {
		t.Fatalf("apply of the source %+v %v", applied, err)
	}
	if _, err := ts.AbandonMigration(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	if later := apply(); later.MigrationID != "" {
		t.Fatalf("an apply after the migration closed: %q", later.MigrationID)
	}
}
