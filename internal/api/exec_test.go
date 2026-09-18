package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

const terminalOrigin = "http://terminal.test"

var terminalSpec = protocol.ExecSpec{Container: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), User: "1000", Argv: []string{"/bin/sh", "secret-argv-must-not-be-audited"}}

type terminalFixture struct {
	s     *api.Server
	st    store.Store
	url   string
	admin *http.Cookie
	ag    logAgent
	ctx   context.Context
}

func newTerminalFixture(t *testing.T) terminalFixture {
	t.Helper()
	return newTerminalFixtureWithStore(t, nil)
}
func newTerminalFixtureWithStore(t *testing.T, wrap func(store.Store) store.Store) terminalFixture {
	t.Helper()
	s, st, cfg := setupTestServerWith(t, func(cfg *config.Config) { cfg.Server.AppURL = terminalOrigin })
	if wrap != nil {
		st = wrap(st)
		s = api.NewServer(cfg, st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	ts := st.Tenancy()
	if err := ts.CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, s, st, "execadmin", "user")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_execadmin", Role: store.RoleOrganizationAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s)
	t.Cleanup(func() { s.BeginShutdown(); httpSrv.Close(); s.WaitDetached() })
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)
	snapshot := protocol.Snapshot{Generation: uint64(time.Now().Unix()) + 1, Containers: []protocol.Container{{ID: terminalSpec.Container, ImageID: terminalSpec.ImageID, Name: "web", State: "running"}}}
	writeEnvelope(t, ctx, ag.conn, protocol.TypeInventory, snapshot)
	writeEnvelope(t, ctx, ag.conn, protocol.TypeHeartbeat, nil)
	readEnvelope(t, ctx, ag.conn)
	return terminalFixture{s, st, httpSrv.URL, admin, ag, ctx}
}
func (f terminalFixture) path() string {
	return "/api/organizations/a/endpoints/" + f.ag.id + "/containers/" + terminalSpec.Container + "/exec"
}
func (f terminalFixture) dial(cookie *http.Cookie, origin string) (*websocket.Conn, *http.Response, error) {
	headers := http.Header{"Origin": {origin}, "Cookie": {cookie.String() + "; ky_csrf=terminal-test-csrf"}}
	return websocket.Dial(f.ctx, "ws"+strings.TrimPrefix(f.url, "http")+f.path(), &websocket.DialOptions{HTTPHeader: headers})
}
func (f terminalFixture) start(t *testing.T, c *websocket.Conn, csrf, confirm string, spec protocol.ExecSpec) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"csrf": csrf, "confirm": confirm, "spec": spec, "size": protocol.TerminalSize{Rows: 24, Columns: 80}})
	if err := c.Write(f.ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}
func (f terminalFixture) open(t *testing.T) (*websocket.Conn, protocol.ExecOpen) {
	t.Helper()
	c, _, err := f.dial(f.admin, terminalOrigin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	f.start(t, c, "terminal-test-csrf", "web", terminalSpec)
	frame := readEnvelope(t, f.ctx, f.ag.conn)
	if frame.Type != protocol.TypeExecOpen {
		t.Fatalf("open: %s", frame.Type)
	}
	var grant protocol.ExecOpen
	if err := json.Unmarshal(frame.Payload, &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Validate(time.Now()) != nil || grant.Actor != "usr_execadmin" || grant.Endpoint != f.ag.id || grant.Spec.User != "1000" {
		t.Fatalf("bad scope: %+v", grant)
	}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeExecReady, protocol.ExecStream{Stream: grant.Stream})
	if ready := readEnvelope(t, f.ctx, c); ready.Type != protocol.TypeExecReady {
		t.Fatal("not ready")
	}
	return c, grant
}
func TestBrowserExecBinaryResizeAuditAndExit(t *testing.T) {
	f := newTerminalFixture(t)
	c, g := f.open(t)
	data := []byte{0, 255, 27, '[', '3', '1', 'm'}
	writeEnvelope(t, f.ctx, c, protocol.TypeExecInput, protocol.ExecData{Stream: g.Stream, Data: data})
	got := readEnvelope(t, f.ctx, f.ag.conn)
	var input protocol.ExecData
	_ = json.Unmarshal(got.Payload, &input)
	if got.Type != protocol.TypeExecInput || !bytes.Equal(input.Data, data) {
		t.Fatal("input changed")
	}
	writeEnvelope(t, f.ctx, c, protocol.TypeExecResize, protocol.ExecResize{Stream: g.Stream, Size: protocol.TerminalSize{Rows: 40, Columns: 120}})
	if got := readEnvelope(t, f.ctx, f.ag.conn); got.Type != protocol.TypeExecResize {
		t.Fatal("no resize")
	}
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeExecOutput, protocol.ExecData{Stream: g.Stream, Data: data})
	got = readEnvelope(t, f.ctx, c)
	_ = json.Unmarshal(got.Payload, &input)
	if got.Type != protocol.TypeExecOutput || !bytes.Equal(input.Data, data) {
		t.Fatal("output changed")
	}
	exit := 7
	writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeExecClose, protocol.ExecClose{Stream: g.Stream, Reason: "secret-agent-error-must-not-leak", ExitCode: &exit})
	got = readEnvelope(t, f.ctx, c)
	if got.Type != protocol.TypeExecClose || strings.Contains(string(got.Payload), "secret-agent") {
		t.Fatal("invalid close")
	}
	waitFor(t, func() bool {
		rows, _, err := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		opened, closed := false, false
		for _, row := range rows {
			if strings.Contains(row.Details, "secret-") {
				t.Fatal("secret audited")
			}
			if row.CorrelationID == g.Stream && row.Action == "container.exec.open" {
				opened = row.EnvironmentID == "env-a" && row.UserID == "usr_execadmin"
			}
			if row.CorrelationID == g.Stream && row.Action == "container.exec.close" {
				closed = row.Result == "failure" && strings.Contains(row.Details, "exit_code=7")
			}
		}
		return opened && closed
	})
}
func TestBrowserExecRefusesOriginAndRoles(t *testing.T) {
	f := newTerminalFixture(t)
	for _, origin := range []string{"", "null", "https://terminal.test", "http://terminal.test.evil", "http://terminal.test/", "http://terminal.test:80"} {
		c, res, err := f.dial(f.admin, origin)
		if c != nil {
			c.CloseNow()
		}
		if err == nil || res == nil || res.StatusCode != 403 {
			t.Fatalf("origin %q: %v %+v", origin, err, res)
		}
	}
	for _, role := range []store.TenantRole{store.RoleEnvironmentAdmin, store.RoleOperator, store.RoleDeveloper, store.RoleReadOnly, "absent"} {
		user := loginAs(t, f.s, f.st, string(role), "admin")
		if role != "absent" {
			if err := f.st.Tenancy().SetMembership(f.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_" + string(role), Role: role, Status: "active"}); err != nil {
				t.Fatal(err)
			}
		}
		c, res, err := f.dial(user, terminalOrigin)
		if c != nil {
			c.CloseNow()
		}
		if err == nil || res == nil || res.StatusCode != 403 {
			t.Fatalf("role %s: %v", role, err)
		}
	}
}
func TestBrowserExecRefusesCSRFAndChangedTarget(t *testing.T) {
	for _, test := range []string{"csrf", "name", "image", "user", "container"} {
		t.Run(test, func(t *testing.T) {
			f := newTerminalFixture(t)
			c, _, err := f.dial(f.admin, terminalOrigin)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			csrf, confirm, spec := "terminal-test-csrf", "web", terminalSpec
			switch test {
			case "csrf":
				csrf = ""
			case "name":
				confirm = "other"
			case "image":
				spec.ImageID = "sha256:" + strings.Repeat("c", 64)
			case "user":
				spec.User = ""
			case "container":
				spec.Container = strings.Repeat("d", 64)
			}
			f.start(t, c, csrf, confirm, spec)
			for {
				_, _, err = c.Read(f.ctx)
				if err != nil {
					break
				}
			}
			// A heartbeat round-trip proves no exec was queued before the refusal.
			writeEnvelope(t, f.ctx, f.ag.conn, protocol.TypeHeartbeat, nil)
			if got := readEnvelope(t, f.ctx, f.ag.conn); got.Type != protocol.TypeHeartbeat {
				t.Fatalf("refusal dispatched %s", got.Type)
			}
		})
	}
}
func TestBrowserExecRevocationAndDisconnect(t *testing.T) {
	for _, kind := range []string{"session", "membership", "disable", "disconnect", "shutdown", "endpoint", "endpoint-store"} {
		t.Run(kind, func(t *testing.T) {
			f := newTerminalFixture(t)
			c, _ := f.open(t)
			began := time.Now()
			switch kind {
			case "session":
				if err := f.st.Sessions().DeleteSession(f.ctx, crypto.SHA256Hex([]byte(f.admin.Value))); err != nil {
					t.Fatal(err)
				}
			case "membership":
				if err := f.st.Tenancy().SetMembership(f.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_execadmin", Role: store.RoleReadOnly, Status: "active"}); err != nil {
					t.Fatal(err)
				}
			case "disable":
				u, err := f.st.Users().GetUserByID(f.ctx, "usr_execadmin")
				if err != nil {
					t.Fatal(err)
				}
				u.Status = "disabled"
				if err := f.st.Users().UpdateUser(f.ctx, u); err != nil {
					t.Fatal(err)
				}
			case "disconnect":
				c.CloseNow()
			case "shutdown":
				f.s.BeginShutdown()
			case "endpoint-store":
				if err := f.st.Tenancy().RevokeEndpoint(f.ctx, store.TenantAccess{ActorID: "usr_execadmin", OrganizationID: "a"}, f.ag.id); err != nil {
					t.Fatal(err)
				}
			case "endpoint":
				w := tenantRequest(f.s, f.admin, "POST", "/api/organizations/a/endpoints/"+f.ag.id+"/revoke", "", true)
				if w.Code != 204 {
					t.Fatal(w.Code)
				}
			}
			ctx, cancel := context.WithTimeout(f.ctx, time.Second)
			defer cancel()
			_, raw, err := f.ag.conn.Read(ctx)
			if kind != "shutdown" && kind != "endpoint" {
				if err != nil {
					t.Fatal(err)
				}
				var frame protocol.Envelope
				_ = json.Unmarshal(raw, &frame)
				if frame.Type != protocol.TypeExecCancel {
					t.Fatalf("not cancelled: %s", frame.Type)
				}
			}
			if time.Since(began) > time.Second {
				t.Fatal("revocation exceeded one second")
			}
		})
	}
}

type countingExecStore struct {
	store.Store
	tenancy *countingExecTenancy
}

func (s countingExecStore) Tenancy() store.TenancyStore { return s.tenancy }

type countingExecTenancy struct {
	store.TenancyStore
	checks atomic.Int64
}

func (s *countingExecTenancy) StillAllowed(ctx context.Context, a store.TenantAccess, action permissions.Action, endpoint string) error {
	if action == permissions.ContainerExec {
		s.checks.Add(1)
	}
	return s.TenancyStore.StillAllowed(ctx, a, action, endpoint)
}
func TestBrowserExecAuthorizationCostBound(t *testing.T) {
	var counter *countingExecTenancy
	f := newTerminalFixtureWithStore(t, func(st store.Store) store.Store {
		counter = &countingExecTenancy{TenancyStore: st.Tenancy()}
		return countingExecStore{Store: st, tenancy: counter}
	})
	c, g := f.open(t)
	before, began := counter.checks.Load(), time.Now()
	for i := 0; i < 1000; i++ {
		writeEnvelope(t, f.ctx, c, protocol.TypeExecInput, protocol.ExecData{Stream: g.Stream, Data: []byte("x")})
		if got := readEnvelope(t, f.ctx, f.ag.conn); got.Type != protocol.TypeExecInput {
			t.Fatalf("input: %s", got.Type)
		}
	}
	if got, bound := counter.checks.Load()-before, int64(time.Since(began)/(250*time.Millisecond))+2; got > bound {
		t.Fatalf("1000 inputs caused %d authorization checks; time-based bound %d", got, bound)
	}
}
func TestBrowserExecRefusesOriginAndRolesDenialBudget(t *testing.T) {
	f := newTerminalFixture(t)
	user := loginAs(t, f.s, f.st, "limited", "user")
	if err := f.st.Tenancy().SetMembership(f.ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_limited", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		c, res, err := f.dial(user, terminalOrigin)
		if c != nil {
			c.CloseNow()
		}
		if err == nil || res == nil || (res.StatusCode != 403 && res.StatusCode != 429) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	rows, _, err := f.st.Audit().ListAuditRecords(f.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if row.UserID == "usr_limited" && row.Action == "container.exec" && row.Result == "denied" {
			count++
		}
	}
	if count == 0 || count > 10 {
		t.Fatalf("denial writes: %d, want 1..10", count)
	}
}
