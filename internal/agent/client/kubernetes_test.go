package client_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/Busnes-app/kyyard-server/internal/testdb"
)

// memIdentities is an IdentityStore in memory, standing in for the cluster's Secret.
type memIdentities struct {
	mu    sync.Mutex
	id    *client.Identity
	saves int
}

func (m *memIdentities) Load() (*client.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.id == nil {
		return nil, nil
	}
	c := *m.id
	return &c, nil
}

func (m *memIdentities) Save(id *client.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *id
	m.id, m.saves = &c, m.saves+1
	return nil
}

// A cluster agent enrolls through its identity store with cluster facts, advertises only the
// cluster capabilities, and its inventory arrives with the cluster lists.
func TestClusterAgentEnrollsAndReportsTheCluster(t *testing.T) {
	t.Setenv("KY_DATA_DIR", t.TempDir())
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbCfg := testdb.Config(t)
	dbCfg.DataDir = cfg.Database.DataDir
	cfg.Database = dbCfg
	cfg.Captcha.Provider = "none"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	httpSrv := httptest.NewServer(api.NewServer(cfg, st))
	defer httpSrv.Close()
	ts := st.Tenancy()
	_ = ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	hash, _ := password.Hash("SuperSecretPass123!")
	_ = st.Users().CreateUser(ctx, &store.User{ID: "usr_admin", Username: "admin", PasswordHash: hash, Role: "user", Status: "active", SSOProvider: "local"})
	_ = ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_admin", Role: store.RoleOrganizationAdmin, Status: "active"})
	jar := login(t, httpSrv.URL, "admin", "SuperSecretPass123!")
	access := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a", EnvironmentID: "env-a"}
	// Minted in the store: the route renders a manifest, which needs an HTTPS address and a
	// pinned image this test server does not have.
	tok, err := ts.CreateEnrollmentToken(ctx, access, protocol.RuntimeKubernetes, "")
	if err != nil {
		t.Fatal(err)
	}

	identities := &memIdentities{}
	facts := map[string]string{"runtime": "kubernetes", "server_version": "v1.36.0", "node_count": "1", "platform": "linux/amd64"}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	id, err := client.Enroll(ctx, httpClient, httpSrv.URL, identities, "cluster-1", base64.RawURLEncoding.EncodeToString(tok.Secret), facts)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if identities.saves != 1 {
		t.Fatalf("enrollment saved %d times", identities.saves)
	}
	e, _ := ts.ReadEndpointRaw(ctx, id.EndpointID)
	if e.Runtime != protocol.RuntimeKubernetes || e.Facts["server_version"] != "v1.36.0" || e.Facts["node_count"] != "1" || e.Facts["platform"] != "linux/amd64" {
		t.Fatalf("endpoint %+v", e)
	}
	post(t, httpSrv.URL+"/api/organizations/a/endpoints/"+id.EndpointID+"/approve", jar, `{"fingerprint":"`+e.Fingerprint+`"}`)

	snapshot := func(context.Context) (*protocol.Snapshot, error) {
		return &protocol.Snapshot{ObservedAt: time.Now().UTC(), Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"}, Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
			Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "kind-control-plane", Ready: true}}}}, nil
	}
	logs := func(context.Context, protocol.LogRequest, func([]byte) error) error { return nil }
	deploy := func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	remove := func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult {
		return protocol.DeploymentResult{}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(runCtx, id, client.Options{HTTPClient: httpClient, Identities: identities, Kubernetes: true, Snapshot: snapshot, Logs: logs, Deploy: deploy, Remove: remove, InventoryEvery: time.Second})
	}()
	waitState(t, ctx, ts, id.EndpointID, "active")
	deadline := time.Now().Add(8 * time.Second)
	for {
		ep, err := ts.ReadEndpoint(ctx, access, id.EndpointID)
		// The server stores them sorted; a hello it refused would store none.
		if err == nil && slices.Equal(ep.Capabilities, []string{protocol.CapabilityKubernetesClaims, protocol.CapabilityKubernetesDeploy, protocol.CapabilityKubernetesInventory, protocol.CapabilityKubernetesRemove, protocol.CapabilityPodLogs}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("capabilities %+v %v", ep, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	inv, err := ts.ReadInventory(ctx, access, id.EndpointID)
	if err != nil || !strings.Contains(string(inv.Snapshot), `"kind-control-plane"`) {
		t.Fatalf("inventory %v: %s", err, inv.Snapshot)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	saved, _ := identities.Load()
	if saved == nil || saved.Generation == 0 {
		t.Fatal("the rising generation was not written back through the store")
	}

	// With no runtime answer at all the facts-only report still has the cluster shape.
	before := saved.Generation
	runCtx, stop = context.WithCancel(ctx)
	defer stop()
	go func() {
		done <- client.Run(runCtx, saved, client.Options{HTTPClient: httpClient, Identities: identities, Kubernetes: true, InventoryEvery: time.Second})
	}()
	deadline = time.Now().Add(8 * time.Second)
	for {
		inv, err := ts.ReadInventory(ctx, access, id.EndpointID)
		if err == nil && inv.Generation > before {
			if !strings.Contains(string(inv.Snapshot), `"kubernetes":{`) || strings.Contains(string(inv.Snapshot), "kind-control-plane") {
				t.Fatalf("facts-only report: %s", inv.Snapshot)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no facts-only report: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("second run: %v", err)
	}
}
