package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

// activeCluster enrolls, approves and reports a Kubernetes endpoint whose manifest granted
// namespaces, with the cluster deployment capabilities and workloads in its inventory.
func activeCluster(t *testing.T, ts TenancyStore, a TenantAccess, namespaces []string, workloads []protocol.Workload) string {
	t.Helper()
	ctx := context.Background()
	tok, err := ts.CreateEnrollmentToken(ctx, a, protocol.RuntimeKubernetes, "", namespaces...)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enrolled, err := ts.Enroll(ctx, EnrollmentRequest{Token: tok.Secret, PublicKey: pub, Proof: ed25519.Sign(priv, protocol.Preimage(protocol.ContextEnroll, tok.Secret)), Name: "cluster-" + tok.ID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.ApproveEndpoint(ctx, a, enrolled.ID, enrolled.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetEndpointCapabilities(ctx, enrolled.ID, []string{protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesClaims, protocol.CapabilityKubernetesRemove}); err != nil {
		t.Fatal(err)
	}
	putClusterInventory(t, ts, enrolled.ID, workloads)
	return enrolled.ID
}

var clusterGeneration atomic.Uint64

// putClusterInventory reports a fresh cluster snapshot holding workloads and the StorageClasses
// standard (the default) and fast.
func putClusterInventory(t *testing.T, ts TenancyStore, endpoint string, workloads []protocol.Workload) {
	t.Helper()
	if workloads == nil {
		workloads = []protocol.Workload{}
	}
	snap := protocol.Snapshot{Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.31.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: workloads, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}, StorageClasses: []protocol.StorageClass{{Name: "standard", Default: true}, {Name: "fast"}}}}
	raw, _ := json.Marshal(snap)
	// Each report must raise the generation, and none may be ahead of the clock.
	generation := uint64(time.Now().Unix()) - 1000 + clusterGeneration.Add(1)
	if ok, err := ts.AcceptInventory(context.Background(), endpoint, generation, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("cluster inventory: %v %v", ok, err)
	}
}

// Namespaces are normalized once, travel from the token to the endpoint, and are replaced only
// through the audited manifest write; a Docker endpoint has none and takes none.
func TestDeployNamespacesFromTokenToEndpoint(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, []string{"shop", "billing"}, nil)
	e, err := ts.ReadEndpoint(ctx, a, cluster)
	if err != nil || !slices.Equal(e.DeployNamespaces, []string{"billing", "shop"}) {
		t.Fatalf("enrolled namespaces: %v %v", e.DeployNamespaces, err)
	}
	host := activeEndpointWith(t, ts, a, nil, nil)
	if e, err := ts.ReadEndpoint(ctx, a, host); err != nil || e.DeployNamespaces == nil || len(e.DeployNamespaces) != 0 {
		t.Fatalf("docker namespaces: %v %v", e.DeployNamespaces, err)
	}
	if _, err := ts.CreateEnrollmentToken(ctx, a, protocol.RuntimeDocker, "", "shop"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("docker token with namespaces: %v", err)
	}
	tooMany := make([]string, MaxDeployNamespaces+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("ns-%d", i)
	}
	for name, list := range map[string][]string{
		"repeated":         {"shop", "shop"},
		"not a label":      {"Shop"},
		"agent namespace":  {"kyyard-agent"},
		"system namespace": {"kube-system"},
		"too many":         tooMany,
	} {
		if _, err := ts.CreateEnrollmentToken(ctx, a, protocol.RuntimeKubernetes, "", list...); !errors.Is(err, ErrInvalid) {
			t.Errorf("token %s: %v", name, err)
		}
		if _, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, list); !errors.Is(err, ErrInvalid) {
			t.Errorf("manifest %s: %v", name, err)
		}
	}
	updated, err := ts.SetEndpointDeployNamespaces(ctx, a, cluster, []string{"web", "shop"})
	if err != nil || !slices.Equal(updated.DeployNamespaces, []string{"shop", "web"}) {
		t.Fatalf("manifest namespaces: %+v %v", updated, err)
	}
	if e, _ := ts.ReadEndpoint(ctx, a, cluster); !slices.Equal(e.DeployNamespaces, []string{"shop", "web"}) {
		t.Fatalf("stored %v", e.DeployNamespaces)
	}
	if _, err := ts.SetEndpointDeployNamespaces(ctx, a, host, []string{"shop"}); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("docker manifest: %v", err)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(records, func(r *AuditRecord) bool {
		return r.Action == "endpoint.enroll" && r.Resource == cluster+"/manifest" && r.Details == "namespaces=shop,web" && r.Result == "success"
	}) {
		t.Fatal("the manifest write left no audit row naming its namespaces")
	}
}

// kubernetesApp imports an application named "Shop Front" with services and returns it.
func kubernetesApp(t *testing.T, st *SQLStore, a TenantAccess, spec ApplicationSpec, values map[string]string) *Application {
	t.Helper()
	app, err := st.Tenancy().ImportApplication(context.Background(), a, "Shop Front", spec, values, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// Mapping to a cluster creates the instance in a listed namespace with a project taken from
// the application's name; the namespace may move only while no apply was sent there.
func TestKubernetesMapping(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, []string{"shop", "staging"}, nil)
	app := kubernetesApp(t, st, a, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1"}}}, nil)
	body := MappingRequest{EndpointID: cluster, Namespace: "prod"}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, body); !errors.Is(err, ErrNamespaceUnknown) {
		t.Fatalf("unlisted namespace: %v", err)
	}
	for name, r := range map[string]MappingRequest{
		"bindings too": {EndpointID: cluster, Namespace: "shop", Bindings: map[string]string{"web": "x"}},
		"version too":  {EndpointID: cluster, Namespace: "shop", Version: 1},
		"no endpoint":  {Namespace: "shop"},
	} {
		if err := ts.SetApplicationMapping(ctx, a, app.ID, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	host := activeEndpointWith(t, ts, a, nil, nil)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: host, Namespace: "shop"}); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("namespace on a Docker endpoint: %v", err)
	}
	body.Namespace = "shop"
	if err := ts.SetApplicationMapping(ctx, a, app.ID, body); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil || m.Runtime != protocol.RuntimeKubernetes || m.Namespace != "shop" || m.Version != 1 || m.MappedRevision != 1 || m.Preview.Project != "shop-front" || m.Preview.EndpointID != cluster || len(m.Preview.Containers) != 0 || len(m.Bindings) != 0 || !slices.Equal(m.Services, []string{"web"}) || !slices.Equal(m.DeployNamespaces, []string{"shop", "staging"}) {
		t.Fatalf("mapping: %+v %v", m, err)
	}
	instance, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID)
	if err != nil || instance.Namespace != "shop" || instance.PreviousRevision != 0 || instance.CurrentRevision != 0 || instance.ContainerCount != 0 {
		t.Fatalf("instance: %+v %v", instance, err)
	}
	// A Docker body cannot bind containers to a cluster instance.
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{InstanceID: m.InstanceID, Version: m.Version, Confirm: "shop-front", Bindings: map[string]string{}}); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("docker body on a cluster instance: %v", err)
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: cluster, Namespace: "staging"}); err != nil {
		t.Fatalf("move before any apply: %v", err)
	}
	if m, _ := ts.ReadApplicationMapping(ctx, a, app.ID); m.Namespace != "staging" || m.Version != 2 {
		t.Fatalf("moved: %+v", m)
	}
	other := activeCluster(t, ts, a, []string{"shop"}, nil)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, MappingRequest{EndpointID: other, Namespace: "shop"}); !errors.Is(err, ErrApplicationAdopted) {
		t.Fatalf("second endpoint: %v", err)
	}
	// A move after an apply was sent: TestKubernetesMoveRefusedAfterAFailedApply.
}

// A Docker-adopted instance whose endpoint reads as a cluster is refused as the wrong runtime,
// not read as either shape.
func TestMappingRefusesAnInstanceOfTheOtherRuntime(t *testing.T) {
	st, a, app, endpoint, _, _ := mappingFixture(t)
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE endpoints SET runtime='kubernetes' WHERE id=?`), endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Tenancy().ReadApplicationMapping(context.Background(), a, app.ID); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("read: %v", err)
	}
}

func TestKubernetesProject(t *testing.T) {
	for in, want := range map[string]string{
		"Shop Front":            "shop-front",
		"  web__api!!":          "web-api",
		"Ünï":                   "n",
		"***":                   "app",
		strings.Repeat("a", 70): strings.Repeat("a", 63),
	} {
		if got := KubernetesProject(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// cluster_health reads only the nodes and the cut lists of the stored snapshot: a list it does
// not need, even one that would not decode, does not blank the health of every endpoint read.
func TestClusterHealthReadsOnlyNodes(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, nil, nil)
	for raw, want := range map[string]string{
		`{"kubernetes":{"nodes":[{"name":"n1","ready":true}],"pods":"not a list"},"containers":7}`: protocol.HealthHealthy,
		`{"kubernetes":{"nodes":[{"name":"n1","ready":false}]}}`:                                   protocol.HealthDegraded,
		`{"kubernetes":{"nodes":[{"name":"n1","ready":true}]},"truncated":["nodes"]}`:              protocol.HealthUnknown,
		`{"containers":[]}`: protocol.HealthUnknown,
		`not json`:          protocol.HealthUnknown,
	} {
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET snapshot=? WHERE endpoint_id=?`), raw, cluster); err != nil {
			t.Fatal(err)
		}
		e, err := ts.ReadEndpoint(ctx, a, cluster)
		if err != nil || e.ClusterHealth != want {
			t.Errorf("%s: %q %v", raw, e.ClusterHealth, err)
		}
	}
}

// The pre-checks the API runs before its runtime gate write nothing when allowed (the operation
// audits itself) and exactly one denied row when refused.
func TestAccessChecksAuditOnlyDenials(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	cluster := activeCluster(t, ts, a, nil, nil)
	app := kubernetesApp(t, st, a, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1"}}}, nil)
	rows := func(action string) []string {
		t.Helper()
		records, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range records {
			if r.Action == action {
				out = append(out, r.Result)
			}
		}
		return out
	}
	if err := ts.CheckEndpointAccess(ctx, a, permissions.ContainerOperate, cluster); err != nil {
		t.Fatal(err)
	}
	if err := ts.CheckApplicationAccess(ctx, a, permissions.ApplicationAdopt, app.ID); err != nil {
		t.Fatal(err)
	}
	if len(rows("container.operate")) != 0 || len(rows("application.adopt")) != 0 {
		t.Fatalf("an allowed check wrote a row: %v %v", rows("container.operate"), rows("application.adopt"))
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: a.ActorID, Role: RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CheckEndpointAccess(ctx, a, permissions.ContainerOperate, cluster); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer endpoint check: %v", err)
	}
	if err := ts.CheckApplicationAccess(ctx, a, permissions.ApplicationAdopt, app.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer application check: %v", err)
	}
	if !slices.Equal(rows("container.operate"), []string{"denied"}) || !slices.Equal(rows("application.adopt"), []string{"denied"}) {
		t.Fatalf("denials: %v %v", rows("container.operate"), rows("application.adopt"))
	}
}
