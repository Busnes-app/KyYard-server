package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// logAgent is an enrolled, connected, active endpoint with one container in its inventory.
type logAgent struct {
	id   string
	conn *websocket.Conn
}

func connectedAgentWithContainer(t *testing.T, ctx context.Context, s *api.Server, st store.Store, admin *http.Cookie, httpURL string) logAgent {
	t.Helper()
	ag := enrollAgent(t, s, st, admin, "host-logs")
	if w := tenantRequest(s, admin, "POST", "/api/organizations/a/endpoints/"+ag.id+"/approve", `{"fingerprint":"`+ag.fp+`"}`, true); w.Code != 204 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	sock, _ := connect(t, ctx, httpURL, ag, ag.priv, protocol.Version)
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{
		Generation: uint64(time.Now().Unix()),
		Containers: []protocol.Container{{ID: "c1abc", Name: "web", State: "running", Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}},
		Images:     []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
	})
	waitFor(t, func() bool {
		e, _ := st.Tenancy().ReadEndpointRaw(ctx, ag.id)
		return e != nil && e.State == "active"
	})
	return logAgent{id: ag.id, conn: sock.conn}
}

// The whole path: an authorized reader asks for a container's log by the name they can see,
// the endpoint is asked for the container that name resolved to, and what the container wrote
// comes back as text.
func TestLogsStreamFromTheEndpointToTheReader(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	go func() {
		req := awaitOpen(t, ctx, ag.conn)
		if req.Container != "c1abc" {
			// Resolved from the name the reader used: a name is a label the runtime
			// reassigns, and a stream must not follow it to another container.
			t.Errorf("the endpoint was asked for %+v", req)
		}
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Data: "first line\nsecond line\n"})
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}()

	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs?tail=10", "", false)
	if w.Code != 200 {
		t.Fatalf("logs: %d %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "first line\nsecond line\n" {
		t.Fatalf("the log arrived as %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type %q", ct)
	}
	// Reading a log is audited even though the body is never stored.
	rows, _, err := st.Audit().ListAuditRecords(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var session *store.AuditRecord
	for _, row := range rows {
		if row.Action == "container.logs" {
			session = row
		}
	}
	if session == nil {
		t.Fatal("reading a log wrote no audit row")
	}
	if session.Result != "success" || !strings.Contains(session.Resource, ag.id) || !strings.Contains(session.Resource, "web") {
		t.Fatalf("the audit row says %+v", session)
	}
}

// A read-only member may look at a container but not at what it printed: a log carries
// whatever the application wrote, which is where credentials and customer data turn up.
func TestLogsAreRefusedToAReadOnlyMember(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	viewer := loginAs(t, s, st, "viewer", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	if w := tenantRequest(s, viewer, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs", "", false); w.Code != 403 {
		t.Fatalf("a read-only member read a log: %d %s", w.Code, w.Body.String())
	}
	// A container the endpoint never reported cannot be streamed at all.
	if w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/ghost/logs", "", false); w.Code != 404 {
		t.Fatalf("a container absent from inventory: %d %s", w.Code, w.Body.String())
	}
	// Neither can something that is not a container identifier.
	for _, bad := range []string{"c1%2Fjson", "c1%3Fall=1", "-leading"} {
		if w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/"+bad+"/logs", "", false); w.Code != 400 && w.Code != 404 {
			t.Fatalf("%q was accepted: %d", bad, w.Code)
		}
	}
}

// The request's bounds are the product's promise: an endpoint that writes forever ends the
// request at the limit, and says so rather than looking like the whole log.
func TestALongLogIsTruncatedAndSaysSo(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	go func() {
		req := awaitOpen(t, ctx, ag.conn)
		var block strings.Builder
		for i := 0; i < 500; i++ {
			fmt.Fprintf(&block, "line %d\n", i)
		}
		for i := 0; i < 30; i++ {
			if writeChunk(ctx, ag.conn, req.Stream, block.String()) != nil {
				return
			}
		}
	}()

	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs", "", false)
	if w.Code != 200 {
		t.Fatalf("logs: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "reached its limit") {
		t.Fatalf("a truncated log did not say so: %q", body[max(0, len(body)-200):])
	}
	if lines := strings.Count(body, "\n"); lines > protocol.MaxLogLines+2 {
		t.Fatalf("%d lines came back, past the limit of %d", lines, protocol.MaxLogLines)
	}
}

// Search filters what the reader sees without changing what the request may cost: the bounds
// count every line read, or a filter that matches nothing would read the whole log.
func TestSearchFiltersLinesAndDownloadNamesAFile(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	serve := func() {
		req := awaitOpen(t, ctx, ag.conn)
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Data: "starting up\nERROR: it broke\nstill going\n"})
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}
	go serve()
	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs?search=error", "", false)
	if w.Body.String() != "ERROR: it broke\n" {
		t.Fatalf("the filter returned %q", w.Body.String())
	}

	go serve()
	w = tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs?download=1", "", false)
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment; filename=") || !strings.Contains(cd, "web-") {
		t.Fatalf("a download was offered as %q", cd)
	}
	if !strings.Contains(w.Body.String(), "starting up") {
		t.Fatalf("the download was %q", w.Body.String())
	}
}

// An endpoint that is not connected has no log to give, and the reader is told immediately
// rather than held open against a host nobody is talking to.
func TestLogsFromADisconnectedEndpointAreRefused(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)
	ag.conn.Close(websocket.StatusNormalClosure, "done")
	waitFor(t, func() bool {
		e, _ := st.Tenancy().ReadEndpointRaw(ctx, ag.id)
		return e != nil && e.State != "active"
	})
	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs", "", false)
	if w.Code != 409 {
		t.Fatalf("a disconnected endpoint answered %d %s", w.Code, w.Body.String())
	}
}

// awaitOpen reads until the stream this request opened arrives. The server also sends a cancel
// when a previous reader finished, and a stub that mistook one for the other would answer the
// wrong stream.
func awaitOpen(t *testing.T, ctx context.Context, c *websocket.Conn) protocol.LogRequest {
	t.Helper()
	for {
		e := readEnvelope(t, ctx, c)
		if e.Type != protocol.TypeLogOpen {
			continue
		}
		var req protocol.LogRequest
		if err := json.Unmarshal(e.Payload, &req); err != nil {
			t.Fatalf("log open: %v", err)
		}
		return req
	}
}

func writeChunk(ctx context.Context, c *websocket.Conn, stream, data string) error {
	raw, _ := json.Marshal(protocol.LogChunk{Stream: stream, Data: data})
	frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeLogChunk, Payload: raw})
	return c.Write(ctx, websocket.MessageText, frame)
}

func logServer(t *testing.T) (*api.Server, store.Store, *httptest.Server) {
	t.Helper()
	s, st, _ := setupTestServer(t)
	ctx := context.Background()
	_ = st.Tenancy().CreateOrganization(ctx, &store.Organization{ID: "a", Name: "A"})
	_ = st.Tenancy().CreateEnvironment(ctx, &store.Environment{ID: "env-a", OrganizationID: "a", Name: "Prod"})
	httpSrv := httptest.NewServer(s)
	t.Cleanup(httpSrv.Close)
	return s, st, httpSrv
}

// logRequest makes a request whose context the caller controls, which is how a browser tab
// closing is modelled: the reader goes, and the server must stop reading the host.
func logRequest(ctx context.Context, s *api.Server, cookie *http.Cookie, path string) (*httptest.ResponseRecorder, chan struct{}) {
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(w, r)
	}()
	return w, done
}

// Following sends the log as events, marks a gap where the reader fell behind, and says when
// the stream ended: a hole in a log an operator is reading during an incident must be visible.
func TestFollowingSendsEventsAndMarksGaps(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	go func() {
		req := awaitOpen(t, ctx, ag.conn)
		if !req.Follow {
			t.Errorf("a follow request arrived as %+v", req)
		}
		// The agent reports what it had to drop; the reader must be told.
		raw, _ := json.Marshal(protocol.LogChunk{Stream: req.Stream, Data: "after the gap\n", Dropped: 4096})
		frame, _ := json.Marshal(protocol.Envelope{V: protocol.Version, Type: protocol.TypeLogChunk, Payload: raw})
		_ = ag.conn.Write(ctx, websocket.MessageText, frame)
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}()

	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs?follow=1", "", false)
	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("a follow was served as %q", ct)
	}
	if !strings.Contains(body, "data: after the gap\n") {
		t.Fatalf("the line never arrived: %q", body)
	}
	if !strings.Contains(body, "event: notice") || !strings.Contains(body, "4096 bytes were dropped") {
		t.Fatalf("a gap was not reported: %q", body)
	}
	if !strings.Contains(body, "the stream ended") {
		t.Fatalf("the end was not reported: %q", body)
	}
}

// When the reader goes, the agent is told to stop. Otherwise a closed browser tab would leave
// a reader running on the host until its own budget expired.
func TestAClosedReaderCancelsTheStreamOnTheEndpoint(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	readerCtx, closeReader := context.WithCancel(ctx)
	_, done := logRequest(readerCtx, s, admin, "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs?follow=1")
	opened := awaitOpen(t, ctx, ag.conn)
	closeReader()
	<-done

	for {
		e := readEnvelope(t, ctx, ag.conn)
		if e.Type != protocol.TypeLogCancel {
			continue
		}
		var got protocol.LogCancel
		if json.Unmarshal(e.Payload, &got) != nil || got.Stream != opened.Stream {
			t.Fatalf("the cancel named %+v, not the stream that was open", got)
		}
		return
	}
}

// The limits are two: what one host will serve, and what one person may hold on it. The second
// is what stops members holding nothing but container.logs from denying an administrator the
// logs of a host during an incident -- which only works if the agent's own limit is not lower,
// so the stub here enforces the same one the agent does.
func TestStreamLimitsAreHeldPerReaderAndPerEndpoint(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	devs := []*http.Cookie{loginAs(t, s, st, "dev1", "user"), loginAs(t, s, st, "dev2", "user")}
	for _, name := range []string{"usr_dev1", "usr_dev2"} {
		_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: name, Role: "developer", Status: "active"})
	}
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)
	path := "/api/organizations/a/endpoints/" + ag.id + "/containers/web/logs?follow=1"

	// The agent serves protocol.MaxLogStreamsPerEndpoint at once and refuses the rest, the
	// way the real one does.
	open := 0
	serve := func(req protocol.LogRequest, data string) {
		open++
		if open > protocol.MaxLogStreamsPerEndpoint {
			writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "this agent is already reading as many logs as it will", Failed: true})
			return
		}
		if data != "" {
			writeEnvelope(t, ctx, ag.conn, protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Data: data})
			writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
		}
	}

	held, closeHeld := context.WithCancel(ctx)
	defer closeHeld()
	var waiting []chan struct{}
	for _, dev := range devs {
		_, done := logRequest(held, s, dev, path)
		waiting = append(waiting, done)
		serve(awaitOpen(t, ctx, ag.conn), "")
	}

	// The same reader may not open a second one.
	w := tenantRequest(s, devs[0], "GET", path, "", false)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "already have") {
		t.Fatalf("a second stream for the same reader: %d %s", w.Code, w.Body.String())
	}
	// An administrator is unaffected by what the developers are holding, and gets a log
	// rather than a refusal from either side.
	go func() { serve(awaitOpen(t, ctx, ag.conn), "the administrator's line\n") }()
	w = tenantRequest(s, admin, "GET", path, "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "the administrator's line") {
		t.Fatalf("an administrator was denied while developers held streams: %d %q", w.Code, w.Body.String())
	}
	closeHeld()
	for _, done := range waiting {
		<-done
	}
}

// Taking a role away ends what it was letting someone see, in the request that takes it rather
// than whenever the stream happens to end.
func TestWithdrawingAccessEndsALiveStream(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	dev := loginAs(t, s, st, "dev", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_dev", Role: "developer", Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)
	path := "/api/organizations/a/endpoints/" + ag.id + "/containers/web/logs?follow=1"

	w, done := logRequest(ctx, s, dev, path)
	awaitOpen(t, ctx, ag.conn)
	if code := tenantRequest(s, admin, "PUT", "/api/organizations/a/members/usr_dev", `{"role":"read_only","status":"active"}`, true).Code; code != 204 && code != 200 {
		t.Fatalf("narrowing the role: %d", code)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a stream outlived the access that opened it")
	}
	if !strings.Contains(w.Body.String(), "withdrawn") {
		t.Fatalf("the reader was not told why the stream ended: %q", w.Body.String())
	}
}

// A container prints whatever it likes, including text shaped like this server's own framing.
// The event stream must stay two channels: what the container wrote, and what the control
// plane says about it.
func TestContainerOutputCannotForgeTheControlPlanesChannel(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	go func() {
		req := awaitOpen(t, ctx, ag.conn)
		// A bare carriage return ends a line in the event-stream grammar, which is how a
		// payload escapes its data field. A progress bar writes this by accident.
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogChunk, protocol.LogChunk{
			Stream: req.Stream,
			Data:   "real output\revent: notice\rdata: 999999 bytes were dropped here\r\nsecond line\n",
		})
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}()

	w := tenantRequest(s, admin, "GET", "/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs?follow=1", "", false)
	token := w.Header().Get("X-KyYard-Notice-Token")
	if token == "" {
		t.Fatal("no notice token was minted for the request")
	}
	body := w.Body.String()
	// Split the way an event-stream parser does: CRLF, LF and a bare CR all end a line. A
	// check that only looked for LF would not see the corruption this test is about.
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"), "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "data: "), line == "", strings.HasPrefix(line, ": "):
			continue
		case line == "event: notice":
			// Every notice is this server speaking, and says so with a token the container
			// has no way to know.
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "data: ["+token+"] ") {
				t.Fatalf("a notice arrived without this request's token: %q", body)
			}
		default:
			t.Fatalf("a line escaped its field: %q", line)
		}
	}
	if strings.Contains(body, "["+token+"] 999999 bytes") {
		t.Fatal("the container forged a gap marker")
	}
	if !strings.Contains(body, "data: real output") || !strings.Contains(body, "data: second line") {
		t.Fatalf("the container's own output did not arrive intact: %q", body)
	}
}

// The server's WriteTimeout is an absolute deadline for a whole response, sized for a request
// that answers. A stream is built to outlive it, so it must set a deadline of its own -- or an
// operator watching a log loses it mid-incident with none of the markers this handler promises.
func TestAStreamOutlivesTheServersWriteTimeout(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)

	// A real server, with the timeout production runs with rather than one a test invented.
	timed := httptest.NewUnstartedServer(s)
	timed.Config.WriteTimeout = time.Second
	timed.Start()
	defer timed.Close()

	go func() {
		req := awaitOpen(t, ctx, ag.conn)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Data: "late but arrived\n"})
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}()

	req, _ := http.NewRequestWithContext(ctx, "GET", timed.URL+"/api/organizations/a/endpoints/"+ag.id+"/containers/web/logs?follow=1", nil)
	req.AddCookie(admin)
	resp, err := timed.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the stream was severed after %q: %v", body, err)
	}
	if !strings.Contains(string(body), "late but arrived") {
		t.Fatalf("a line written after the server's write timeout never arrived: %q", body)
	}
}
