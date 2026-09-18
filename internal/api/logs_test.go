package api_test

import (
	"context"
	"encoding/json"
	"fmt"
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

// An endpoint serves only so many streams at once. The limit is answered immediately, with a
// reason, rather than by opening readers the host will not serve.
func TestAnEndpointServesOnlySoManyStreams(t *testing.T) {
	s, st, httpSrv := logServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin := loginAs(t, s, st, "envadmin", "user")
	_ = st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleOrganizationAdmin, Status: "active"})
	ag := connectedAgentWithContainer(t, ctx, s, st, admin, httpSrv.URL)
	path := "/api/organizations/a/endpoints/" + ag.id + "/containers/web/logs?follow=1"

	held, closeHeld := context.WithCancel(ctx)
	defer closeHeld()
	var waiting []chan struct{}
	for i := 0; i < 2; i++ {
		_, done := logRequest(held, s, admin, path)
		waiting = append(waiting, done)
		awaitOpen(t, ctx, ag.conn)
	}
	w := tenantRequest(s, admin, "GET", path, "", false)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a third stream was opened: %d %s", w.Code, w.Body.String())
	}
	closeHeld()
	for _, done := range waiting {
		<-done
	}
	// And the places come back: the limit is what is open now, not a quota that runs out.
	go func() {
		req := awaitOpen(t, ctx, ag.conn)
		writeEnvelope(t, ctx, ag.conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}()
	if w := tenantRequest(s, admin, "GET", path, "", false); w.Code != 200 {
		t.Fatalf("a stream after the others closed: %d %s", w.Code, w.Body.String())
	}
}
