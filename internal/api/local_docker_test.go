package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

// A real Unix-socket Engine fixture exercises bootstrap, authenticated transport,
// inventory, authorized commands, restart generation and live revocation together.
func TestLocalDockerConnectsWithoutEnrollment(t *testing.T) {
	s, st, cfg := setupTestServer(t)
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	engine := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1.41/info":
			_, _ = w.Write([]byte(`{"Name":"fixture-host","NCPU":2,"MemTotal":1024}`))
		case "/v1.41/version":
			_, _ = w.Write([]byte(`{"Version":"test","ApiVersion":"1.41","Os":"linux","Arch":"amd64"}`))
		case "/v1.41/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"c1","Names":["/fixture"],"Image":"fixture:1","ImageID":"sha256:abc","State":"exited","Status":"Exited"}]`))
		case "/v1.41/containers/c1/json":
			_, _ = w.Write([]byte(`{"Id":"c1","Image":"sha256:abc","State":{"Status":"exited","Running":false}}`))
		case "/v1.41/containers/c1/start":
			starts.Add(1)
			w.WriteHeader(204)
		case "/v1.41/volumes":
			_, _ = w.Write([]byte(`{"Volumes":[]}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})}
	go func() { _ = engine.Serve(listener) }()
	defer engine.Close()
	cfg.Server.DockerSocket = socket
	admin := loginAs(t, s, st, "local-admin", "admin")
	outsider := loginAs(t, s, st, "outsider", "admin")
	ctx := context.Background()
	if err := st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: store.InitialOrganizationID, UserID: "usr_local-admin", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	access := store.TenantAccess{ActorID: "usr_local-admin", OrganizationID: store.InitialOrganizationID}
	run := func() (context.CancelFunc, <-chan error) {
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- s.RunLocalDocker(cctx) }()
		return cancel, done
	}
	waitInventory := func(after uint64) *store.Inventory {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for time.Now().Before(until) {
			inv, err := st.Tenancy().ReadInventory(ctx, access, store.LocalDockerEndpointID)
			if err == nil && inv.Generation > after {
				return inv
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("local inventory did not arrive")
		return nil
	}
	cancel, done := run()
	defer cancel()
	inv := waitInventory(0)
	if !strings.Contains(string(inv.Snapshot), "fixture") {
		t.Fatal("missing container", string(inv.Snapshot))
	}
	path := "/api/organizations/" + store.InitialOrganizationID + "/endpoints/" + store.LocalDockerEndpointID
	if w := tenantRequest(s, outsider, "GET", path+"/inventory", "", false); w.Code != 403 {
		t.Fatal("platform role bypassed tenant", w.Code)
	}
	if w := tenantRequest(s, outsider, "POST", path+"/commands", `{"action":"container.start","container":"c1"}`, true); w.Code != 403 {
		t.Fatal("outsider operated local Docker", w.Code)
	}
	w := tenantRequest(s, admin, "POST", path+"/commands", `{"action":"container.start","container":"c1"}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var cmd store.Command
	if err := json.Unmarshal(w.Body.Bytes(), &cmd); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(3 * time.Second)
	for starts.Load() == 0 && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if starts.Load() != 1 {
		t.Fatal("authorized command not delivered")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("local agent did not stop")
	}
	s.WaitDetached()
	cancel, done = run()
	defer cancel()
	waitInventory(inv.Generation)
	if w := tenantRequest(s, admin, "POST", path+"/revoke", `{}`, true); w.Code != 204 {
		t.Fatal("revoke", w.Code, w.Body.String())
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("revoked local agent stayed connected")
	}
	s.WaitDetached()
	if err := s.RunLocalDocker(ctx); !errors.Is(err, store.ErrForbidden) {
		t.Fatal("revocation lost at restart", err)
	}
	// No standalone private identity was added to the data directory.
	if _, err := os.Stat(filepath.Join(cfg.Database.DataDir, "local-agent", "identity.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("derived identity persisted", err)
	}
}

func TestLocalDockerDisabledOrMissingSocket(t *testing.T) {
	s, _, cfg := setupTestServer(t)
	cfg.Server.DockerSocket = ""
	if err := s.RunLocalDocker(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.Server.DockerSocket = filepath.Join(t.TempDir(), "missing.sock")
	if err := s.RunLocalDocker(context.Background()); err == nil || !strings.Contains(err.Error(), "mount the host Docker socket") {
		t.Fatal("missing actionable socket error", err)
	}
}
