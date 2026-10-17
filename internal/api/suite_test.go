package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/password"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/auth"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// fakeRunner speaks runner protocol v1 on a Unix socket in a private directory.
type fakeRunner struct {
	socket string
	mu     sync.Mutex
	starts []map[string]any
	reads  int
	// refuse, when set, is the status and code every start is refused with.
	refuse struct {
		status int
		code   string
	}
	// ops holds the operation each read returns; a missing id answers 404.
	ops map[string]string
}

const fakeStatus = `{"runner":{"release_set":"0.5.0","runner_id":"5a1e5b8e-4b7a-4c55-9a37-2f1c4f0e8b11","plan_revision":7,"plan_owner":"runner"},"operation":null,"targets":[{"name":"nas","kind":"docker"}],"apps":[{"name":"kynotes","category":"ky","version":"1.4.0","target":"nas","state":"installed","health":{"ok":true,"checked":"2026-10-16T10:00:00Z"},"last_result":{"step":"kynotes.health","status":"ok","time":"2026-10-16T10:00:00Z"}}]}`

func startFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	dir, err := os.MkdirTemp("", "runner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeRunner{socket: filepath.Join(dir, "runner.sock"), ops: map[string]string{}}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, fakeStatus) })
	mux.HandleFunc("GET /v1/catalog", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"release_set":"0.5.0","apps":[{"name":"hello","description":"Hello","category":"third-party","version":"1.11.0","depends_on":[],"installed":false,"upgrade":{"min_from":"1.0.0","no_skip":[],"migrates_data":false}}]}`)
	})
	mux.HandleFunc("GET /v1/plan", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"revision":7,"stack":"revision: 7\n"}`)
	})
	mux.HandleFunc("POST /v1/operations", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.starts = append(f.starts, body)
		if f.refuse.status != 0 {
			w.WriteHeader(f.refuse.status)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": f.refuse.code, "message": "refused‮ by the runner"}})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"id": body["id"]})
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reads++
		op, ok := f.ops[r.PathValue("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, op)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f
}

func (f *fakeRunner) started() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.starts...)
}

// suiteServer is a test server pointed at socket ("" leaves suite management off).
func suiteServer(t *testing.T, socket string) (*api.Server, store.Store, *config.Config) {
	return setupTestServerWith(t, func(c *config.Config) { c.Suite.RunnerSocket = socket })
}

// suiteCaller creates u with accountPassword and returns a JSON caller carrying its session
// and, when csrf is set, the matching CSRF header.
func suiteCaller(t *testing.T, srv *api.Server, st store.Store, cfg *config.Config, u *store.User) func(method, path string, body any, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	u.PasswordHash, _ = password.Hash(accountPassword)
	if err := st.Users().CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	if _, _, err := auth.NewSessionManager(st, cfg.Security).IssueSession(ctx, login, httptest.NewRequest("POST", "/api/auth/login", nil), u); err != nil {
		t.Fatal(err)
	}
	cookies := login.Result().Cookies()
	return func(method, path string, body any, csrf bool) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		for _, c := range cookies {
			req.AddCookie(c)
			if c.Name == auth.CSRFCookieName && csrf {
				req.Header.Set(auth.HeaderCSRF, c.Value)
			}
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}
}

func adminUser(id string) *store.User {
	return &store.User{ID: id, Username: id, Role: "admin", Status: "active", SSOProvider: "local"}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	return out
}

func wantCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	want(t, w, status)
	if got := decodeBody(t, w)["code"]; got != code {
		t.Fatalf("code %v, want %s: %s", got, code, w.Body)
	}
}

func install(confirm string) map[string]any {
	return map[string]any{"kind": "install", "apps": []string{"hello"}, "target": "nas", "confirm": confirm, "password": accountPassword}
}

func TestSuiteNotConfigured(t *testing.T) {
	srv, st, cfg := suiteServer(t, "")
	call := suiteCaller(t, srv, st, cfg, adminUser("usr_off"))
	w := call("GET", "/api/suite/status", nil, false)
	want(t, w, 200)
	if got := decodeBody(t, w); len(got) != 1 || got["configured"] != false {
		t.Fatalf("status: %v", got)
	}
	for _, path := range []string{"/api/suite/catalog", "/api/suite/plan", "/api/suite/operations/5a1e5b8e-4b7a-4c55-9a37-2f1c4f0e8b11"} {
		wantCode(t, call("GET", path, nil, false), 404, "not_configured")
	}
	wantCode(t, call("POST", "/api/suite/operations", install("hello"), true), 404, "not_configured")
}

func TestSuiteReadsPassThrough(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	call := suiteCaller(t, srv, st, cfg, adminUser("usr_read"))
	status := decodeBody(t, call("GET", "/api/suite/status", nil, false))
	if status["configured"] != true || status["runner"].(map[string]any)["plan_owner"] != "runner" || len(status["apps"].([]any)) != 1 {
		t.Fatalf("status: %v", status)
	}
	if apps := decodeBody(t, call("GET", "/api/suite/catalog", nil, false))["apps"].([]any); apps[0].(map[string]any)["name"] != "hello" {
		t.Fatalf("catalog: %v", apps)
	}
	if plan := decodeBody(t, call("GET", "/api/suite/plan", nil, false)); plan["revision"] != float64(7) {
		t.Fatalf("plan: %v", plan)
	}
}

func TestSuiteRunnerUnreachable(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	os.Remove(runner.socket)
	call := suiteCaller(t, srv, st, cfg, adminUser("usr_gone"))
	wantCode(t, call("GET", "/api/suite/status", nil, false), 502, "runner_unreachable")
	w := call("POST", "/api/suite/operations", install("hello"), true)
	wantCode(t, w, 502, "runner_unreachable")
	rows := auditRows(t, st, "suite.operation")
	if len(rows) != 1 || rows[0].Result != "unknown" {
		t.Fatalf("audit: %+v", rows)
	}
	// The runner may have seen it, so the row stays open for a read to settle.
	op, err := st.Suite().ReadSuiteOperation(context.Background(), rows[0].Resource)
	if err != nil || op.State != "running" || op.ErrorCode != "runner_unreachable" {
		t.Fatalf("row: %+v %v", op, err)
	}
}

func TestSuiteStartRequiresCSRF(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	call := suiteCaller(t, srv, st, cfg, adminUser("usr_csrf"))
	want(t, call("POST", "/api/suite/operations", install("hello"), false), 403)
	if len(runner.started()) != 0 {
		t.Fatal("a request without CSRF reached the runner")
	}
}

// Every refusal is audited, reaches no runner and records no operation. Each case is its own
// account, so the attempt budget never answers first.
func TestSuiteStartRefusals(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	wrong := func(field string, value any) map[string]any {
		b := install("hello")
		b[field] = value
		return b
	}
	cases := []struct {
		name   string
		mfa    bool
		body   map[string]any
		status int
		code   string
	}{
		{"wrong confirm", false, install("hell"), 400, "confirm_mismatch"},
		{"upgrade-all confirms suite", false, map[string]any{"kind": "upgrade", "apps": []string{}, "confirm": "hello", "password": accountPassword}, 400, "confirm_mismatch"},
		{"wrong password", false, wrong("password", "not-the-password"), 401, "password_incorrect"},
		{"no target", false, wrong("target", ""), 400, "invalid_request"},
		{"two apps", false, map[string]any{"kind": "uninstall", "apps": []string{"a", "b"}, "confirm": "a", "password": accountPassword}, 400, "invalid_request"},
		{"acknowledgement off upgrade and install", false, map[string]any{"kind": "uninstall", "apps": []string{"hello"}, "confirm": "hello", "acknowledge_no_backup": true, "password": accountPassword}, 400, "invalid_request"},
		{"unknown field", false, wrong("force", true), 400, "invalid_request"},
		{"missing TOTP", true, install("hello"), 401, "code_invalid"},
		{"wrong TOTP", true, wrong("code", "abcdef"), 401, "code_invalid"},
		{"right TOTP, wrong password", true, wrong("password", "not-the-password"), 401, "password_incorrect"},
	}
	for i, c := range cases {
		u := adminUser("usr_case" + strconv.Itoa(i))
		if c.mfa {
			u, _ = enabledMFAUser(t, cfg, u.ID)
			u.Role = "admin"
		}
		call := suiteCaller(t, srv, st, cfg, u)
		w := call("POST", "/api/suite/operations", c.body, true)
		if w.Code != c.status || decodeBody(t, w)["code"] != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.name, w.Code, w.Body, c.status, c.code)
		}
	}
	if len(runner.started()) != 0 {
		t.Fatal("a refused request reached the runner")
	}
	rows := auditRows(t, st, "suite.operation")
	if len(rows) != len(cases) {
		t.Fatalf("audit rows %d, want %d", len(rows), len(cases))
	}
	for _, r := range rows {
		if !strings.Contains(r.Details, "refused=") || strings.Contains(r.Details, accountPassword) || strings.Contains(r.Details, "not-the-password") || r.Result == "success" {
			t.Fatalf("audit row: %+v", r)
		}
		if _, err := st.Suite().ReadSuiteOperation(context.Background(), r.Resource); err != store.ErrNotFound {
			t.Fatalf("refusal recorded an operation: %v", err)
		}
	}
}

func TestSuiteStartWithTOTP(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	u, secret := enabledMFAUser(t, cfg, "usr_totp_ok")
	u.Role = "admin"
	call := suiteCaller(t, srv, st, cfg, u)
	code, _ := stepCodes(t, secret)
	body := install("hello")
	body["code"] = code
	want(t, call("POST", "/api/suite/operations", body, true), 202)
	// The code is spent: replaying it is refused.
	wantCode(t, call("POST", "/api/suite/operations", body, true), 401, "code_invalid")
	if n := len(runner.started()); n != 1 {
		t.Fatalf("runner got %d starts, want 1", n)
	}
}

// Failed re-authentication spends the per-account password-change budget.
func TestSuiteStartSpendsThePasswordChangeBudget(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	call := suiteCaller(t, srv, st, cfg, adminUser("usr_budget"))
	wrong := install("hello")
	wrong["password"] = "not-the-password"
	for range 5 {
		want(t, call("POST", "/api/suite/operations", wrong, true), 401)
	}
	wantCode(t, call("POST", "/api/suite/operations", install("hello"), true), 429, "too_many_attempts")
	if api.AllowAccountAttemptForTest(srv, "password-change-user:usr_budget", 5, time.Minute) {
		t.Fatal("suite attempts did not spend the password-change budget")
	}
	if len(runner.started()) != 0 {
		t.Fatal("a throttled request reached the runner")
	}
}

func TestSuiteStartRecordsAuditsAndSettles(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	call := suiteCaller(t, srv, st, cfg, adminUser("usr_ok"))
	w := call("POST", "/api/suite/operations", install("hello"), true)
	want(t, w, 202)
	accepted := decodeBody(t, w)
	id := accepted["id"].(string)
	starts := runner.started()
	got, _ := json.Marshal(starts)
	if string(got) != `[{"acknowledge_no_backup":false,"actor":"usr_ok","apps":["hello"],"id":"`+id+`","kind":"install","target":"nas"}]` {
		t.Fatalf("runner request: %s", got)
	}
	rows := auditRows(t, st, "suite.operation")
	if len(rows) != 1 || rows[0].Result != "success" || rows[0].UserID != "usr_ok" || rows[0].Resource != id || rows[0].Details != "kind=install apps=hello target=nas" {
		t.Fatalf("audit: %+v", rows)
	}
	if accepted["state"] != "running" || accepted["actor_id"] != "usr_ok" {
		t.Fatalf("accepted: %v", accepted)
	}

	runner.mu.Lock()
	runner.ops[id] = `{"id":"` + id + `","kind":"install","apps":["hello"],"actor":"usr_ok","state":"succeeded","started":"2026-10-16T10:00:00Z","finished":"2026-10-16T10:01:00Z","steps":[{"id":"hello.deploy","status":"ok","time":"2026-10-16T10:00:30Z","diagnostics":"pulled"}],"error":null}`
	runner.mu.Unlock()
	op := decodeBody(t, call("GET", "/api/suite/operations/"+id, nil, false))
	steps := op["operation"].(map[string]any)["steps"].([]any)
	if op["state"] != "succeeded" || op["finished"] != "2026-10-16T10:01:00Z" || steps[0].(map[string]any)["diagnostics"] != "pulled" {
		t.Fatalf("settled: %v", op)
	}
	// A settled row is answered from the record.
	call("GET", "/api/suite/operations/"+id, nil, false)
	if runner.reads != 1 {
		t.Fatalf("runner read %d times, want 1", runner.reads)
	}
	stored, err := st.Suite().ReadSuiteOperation(context.Background(), id)
	if err != nil || stored.State != "succeeded" || len(stored.Snapshot) == 0 {
		t.Fatalf("row: %+v %v", stored, err)
	}

	// Upgrade of every app: no apps, no target, the acknowledgement carried.
	want(t, call("POST", "/api/suite/operations", map[string]any{"kind": "upgrade", "apps": []string{}, "confirm": "suite", "acknowledge_no_backup": true, "password": accountPassword}, true), 202)
	upgrade, _ := json.Marshal(runner.started()[1])
	if !strings.Contains(string(upgrade), `"acknowledge_no_backup":true`) || !strings.Contains(string(upgrade), `"apps":[]`) || strings.Contains(string(upgrade), "target") {
		t.Fatalf("upgrade request: %s", upgrade)
	}
}

func TestSuiteStartPassesRunnerRefusals(t *testing.T) {
	for _, code := range []string{"busy", "id_conflict", "ack_required"} {
		runner := startFakeRunner(t)
		runner.refuse.status, runner.refuse.code = 409, code
		srv, st, cfg := suiteServer(t, runner.socket)
		call := suiteCaller(t, srv, st, cfg, adminUser("usr_"+code))
		w := call("POST", "/api/suite/operations", install("hello"), true)
		wantCode(t, w, 409, code)
		if msg := decodeBody(t, w)["error"]; msg != "refused by the runner" {
			t.Fatalf("message not passed through cleaned: %q", msg)
		}
		rows := auditRows(t, st, "suite.operation")
		if len(rows) != 1 || rows[0].Result != "failure" || !strings.HasSuffix(rows[0].Details, "runner="+code) {
			t.Fatalf("audit: %+v", rows)
		}
		op, err := st.Suite().ReadSuiteOperation(context.Background(), rows[0].Resource)
		if err != nil || op.State != "failed" || op.ErrorCode != code || op.FinishedAt == nil {
			t.Fatalf("row: %+v %v", op, err)
		}
	}
}

func TestSuiteLostOperationBecomesUnknown(t *testing.T) {
	runner := startFakeRunner(t)
	srv, st, cfg := suiteServer(t, runner.socket)
	call := suiteCaller(t, srv, st, cfg, adminUser("usr_lost"))
	w := call("POST", "/api/suite/operations", install("hello"), true)
	want(t, w, 202)
	id := decodeBody(t, w)["id"].(string)
	op := decodeBody(t, call("GET", "/api/suite/operations/"+id, nil, false))
	if op["state"] != "unknown" || op["error_code"] != "lost" {
		t.Fatalf("lost operation: %v", op)
	}
	want(t, call("GET", "/api/suite/operations/not-a-uuid", nil, false), 400)
	want(t, call("GET", "/api/suite/operations/5a1e5b8e-4b7a-4c55-9a37-2f1c4f0e8b11", nil, false), 404)
}
