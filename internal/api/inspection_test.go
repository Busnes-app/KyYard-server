package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func inspectionFixture(t *testing.T) terminalFixture {
	f := newTerminalFixture(t)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHello, protocol.Hello{Capabilities: []string{"container.inspect"}})
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) + 2, ObservedAt: time.Now(), Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: terminalSpec.Container, ImageID: terminalSpec.ImageID, CreatedAt: time.Unix(1700000000, 0), State: "running"}}})
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, f.ctx, f.ag.conn)
	return f
}
func beginInspection(f terminalFixture) <-chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ch <- tenantRequest(f.s, f.admin, "GET", strings.TrimSuffix(f.path(), "exec")+"inspection", "", true)
	}()
	return ch
}
func inspectionGrant(t *testing.T, f terminalFixture) protocol.InspectionOpen {
	t.Helper()
	frame := readEnvelope(t, f.ctx, f.ag.conn)
	var req protocol.InspectionOpen
	if frame.Type != protocol.TypeInspectionOpen || json.Unmarshal(frame.Payload, &req) != nil || req.Validate(time.Now()) != nil {
		t.Fatalf("invalid inspection request: %+v", frame)
	}
	return req
}
func inspectionReply(req protocol.InspectionOpen) protocol.InspectionResult {
	return protocol.InspectionResult{Request: req.Request, Status: "ok", Result: &protocol.ContainerInspection{Target: req.Target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: protocol.ImagePlatform{OS: "linux", Architecture: "amd64"}, Ports: []protocol.Port{}}}
}
func TestInspectionAPIReadOnlyAndScoped(t *testing.T) {
	f := inspectionFixture(t)
	path := strings.TrimSuffix(f.path(), "exec") + "inspection"
	for _, tc := range []struct {
		path   string
		status int
	}{{strings.Replace(path, "/a/", "/foreign/", 1), 403}, {strings.Replace(path, terminalSpec.Container, strings.Repeat("c", 64), 1), 404}} {
		if w := tenantRequest(f.s, f.admin, "GET", tc.path, "", true); w.Code != tc.status {
			t.Fatalf("scope: %d %s", w.Code, w.Body.String())
		}
	}
	if w := tenantRequest(f.s, nil, "GET", path, "", true); w.Code != 401 {
		t.Fatal("unauthenticated inspection")
	}
	if err := f.st.Tenancy().SetMembership(f.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_execadmin", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	response := beginInspection(f)
	req := inspectionGrant(t, f)
	if req.Target.ContainerID != terminalSpec.Container || req.Target.ImageID != terminalSpec.ImageID || req.Target.CreatedUnix != 1700000000 {
		t.Fatal("inventory identity not pinned")
	}
	// The same actor cannot occupy both endpoint slots.
	if w := tenantRequest(f.s, f.admin, "GET", path, "", true); w.Code != 429 {
		t.Fatalf("admission: %d", w.Code)
	}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInspectionResult, inspectionReply(req))
	w := <-response
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	var got protocol.ContainerInspection
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ConfigurationVerified || got.Target != req.Target {
		t.Fatal("bad result")
	}
}
func TestInspectionAPIDeniesRevokedReaderAndDisconnectedAgent(t *testing.T) {
	for _, revoke := range []bool{true, false} {
		t.Run(map[bool]string{true: "membership", false: "disconnect"}[revoke], func(t *testing.T) {
			f := inspectionFixture(t)
			response := beginInspection(f)
			_ = inspectionGrant(t, f)
			want := 409
			if revoke {
				want = 403
				if err := f.st.Tenancy().SetMembership(f.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_execadmin", Role: store.RoleReadOnly, Status: "disabled"}); err != nil {
					t.Fatal(err)
				}
			} else {
				f.ag.conn.CloseNow()
			}
			select {
			case w := <-response:
				if w.Code != want {
					t.Fatalf("got %d %s", w.Code, w.Body.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request did not stop")
			}
		})
	}
}
func TestInspectionAPIRefusesUntrustedResult(t *testing.T) {
	for _, bad := range []string{"identity", "verified", "state", "unknown_field"} {
		t.Run(bad, func(t *testing.T) {
			f := inspectionFixture(t)
			response := beginInspection(f)
			req := inspectionGrant(t, f)
			reply := inspectionReply(req)
			switch bad {
			case "identity":
				reply.Result.Target.ImageID = "sha256:" + strings.Repeat("c", 64)
			case "verified":
				reply.Result.ConfigurationVerified = true
			case "state":
				reply.Result.State = "secret-canary"
			}
			if bad == "unknown_field" {
				raw, _ := json.Marshal(reply)
				var v map[string]any
				json.Unmarshal(raw, &v)
				v["secret"] = "secret-canary"
				writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInspectionResult, v)
			} else {
				writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInspectionResult, reply)
			}
			select {
			case w := <-response:
				if w.Code == 200 || strings.Contains(w.Body.String(), "secret-canary") {
					t.Fatalf("untrusted result: %d %s", w.Code, w.Body.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("invalid result did not terminate")
			}
		})
	}
}

func TestInspectionAPIRefusesChangedInventoryAndUnsupportedAgent(t *testing.T) {
	f := inspectionFixture(t)
	response := beginInspection(f)
	req := inspectionGrant(t, f)
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) + 3, ObservedAt: time.Now(), Engine: protocol.Engine{Version: "1"}, Containers: []protocol.Container{{ID: terminalSpec.Container, ImageID: "sha256:" + strings.Repeat("c", 64), CreatedAt: time.Unix(1700000000, 0), State: "running"}}})
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeInspectionResult, inspectionReply(req))
	if w := <-response; w.Code != 409 {
		t.Fatalf("changed target accepted: %d %s", w.Code, w.Body.String())
	}
	if err := f.st.Tenancy().SetEndpointCapabilities(f.ctx, f.ag.id, nil); err != nil {
		t.Fatal(err)
	}
	if w := tenantRequest(f.s, f.admin, "GET", strings.TrimSuffix(f.path(), "exec")+"inspection", "", true); w.Code != 501 {
		t.Fatalf("old agent: %d", w.Code)
	}
}
func TestInspectionAPICancellation(t *testing.T) {
	f := inspectionFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	r := httptest.NewRequest("GET", strings.TrimSuffix(f.path(), "exec")+"inspection", nil).WithContext(ctx)
	r.AddCookie(f.admin)
	finished := make(chan struct{})
	go func() { f.s.ServeHTTP(httptest.NewRecorder(), r); close(finished) }()
	grant := inspectionGrant(t, f)
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cancelled HTTP request retained admission")
	}
	frame := readEnvelope(t, f.ctx, f.ag.conn)
	var stopped protocol.InspectionCancel
	if frame.Type != protocol.TypeInspectionCancel || json.Unmarshal(frame.Payload, &stopped) != nil || stopped.Request != grant.Request {
		t.Fatal("agent did not receive request cancellation")
	}
}
