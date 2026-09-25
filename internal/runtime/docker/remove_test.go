package docker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

var secondID = strings.Repeat("f", 64)

// fakeRemoveEngine answers the three calls one target needs, for two containers.
type fakeRemoveEngine struct {
	mu           sync.Mutex
	calls        []engineCall
	inspect      map[string]int            // GET /containers/{id}/json status; 200 default
	containers   map[string]map[string]any // its body
	stopStatus   int                       // 204 default; 304 allowed
	removeStatus int                       // 204 default
	srv          *httptest.Server
}

func newFakeRemoveEngine(t *testing.T) *fakeRemoveEngine {
	t.Helper()
	f := &fakeRemoveEngine{inspect: map[string]int{oldID: 200, secondID: 200}, stopStatus: 204, removeStatus: 204, containers: map[string]map[string]any{
		oldID:    {"Id": oldID, "Image": oldImage, "Created": "2023-11-14T22:13:20Z"},
		secondID: {"Id": secondID, "Image": oldImage, "Created": "2023-11-14T22:13:20Z"},
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, engineCall{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, ""})
		f.mu.Unlock()
		p := r.URL.EscapedPath()
		for _, id := range []string{oldID, secondID} {
			switch {
			case r.Method == "GET" && strings.HasSuffix(p, "/containers/"+id+"/json"):
				w.WriteHeader(f.inspect[id])
				_ = json.NewEncoder(w).Encode(f.containers[id])
				return
			case r.Method == "POST" && strings.HasSuffix(p, "/containers/"+id+"/stop"):
				w.WriteHeader(f.stopStatus)
				return
			case r.Method == "DELETE" && strings.HasSuffix(p, "/containers/"+id):
				w.WriteHeader(f.removeStatus)
				return
			}
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(f.srv.Close)
	return f
}
func (f *fakeRemoveEngine) client() *docker.Client { return docker.NewHTTP(f.srv.Client(), f.srv.URL) }
func (f *fakeRemoveEngine) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, c := range f.calls {
		out = append(out, c.Method+" "+c.Path[strings.LastIndex(c.Path, "/v1.41")+len("/v1.41"):])
	}
	return out
}
func removal(ids ...string) protocol.RemovalRequest {
	req := protocol.RemovalRequest{Deployment: deploymentID, RequestID: requestID, Endpoint: "ep_1", Project: "shop", IssuedAt: time.Now(), Deadline: time.Now().Add(5 * time.Minute)}
	for i, id := range ids {
		req.Containers = append(req.Containers, protocol.RemovalTarget{Service: []string{"web", "db"}[i], Target: protocol.InspectionTarget{ContainerID: id, ImageID: oldImage, CreatedUnix: 1700000000}})
	}
	return req
}
func outcomes(res protocol.DeploymentResult) string {
	out := []string{}
	for _, s := range res.Steps {
		out = append(out, s.Service+"/"+s.Step+"="+s.Outcome)
	}
	return strings.Join(out, ",")
}

func TestRemoveStopsThenDeletesEachTarget(t *testing.T) {
	f := newFakeRemoveEngine(t)
	res := f.client().Remove(context.Background(), removal(oldID, secondID), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Deployment != deploymentID || res.Code != "" || res.RequestID != requestID || len(res.Services) != 0 {
		t.Fatalf("outcome: %+v", res)
	}
	want := []string{"GET /containers/" + oldID + "/json", "POST /containers/" + oldID + "/stop", "DELETE /containers/" + oldID,
		"GET /containers/" + secondID + "/json", "POST /containers/" + secondID + "/stop", "DELETE /containers/" + secondID}
	if got := f.steps(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order:\n got %v\nwant %v", got, want)
	}
	for i, q := range []string{"", "t=10", "", "", "t=10", ""} {
		if f.calls[i].Query != q {
			t.Fatalf("call %d query %q, want %q", i, f.calls[i].Query, q)
		}
	}
	if got := outcomes(res); got != "web/precondition=succeeded,web/stop=succeeded,web/remove=succeeded,db/precondition=succeeded,db/stop=succeeded,db/remove=succeeded" {
		t.Fatalf("steps: %s", got)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAcceptsAContainerAlreadyGone(t *testing.T) {
	f := newFakeRemoveEngine(t)
	f.inspect[oldID] = 404
	res := f.client().Remove(context.Background(), removal(oldID), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || len(f.calls) != 1 {
		t.Fatalf("gone: %+v calls=%v", res, f.steps())
	}
	if got := outcomes(res); got != "web/precondition=succeeded,web/stop=skipped,web/remove=skipped" {
		t.Fatalf("steps: %s", got)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	// A gone target does not end the run: the next one is still removed.
	f = newFakeRemoveEngine(t)
	f.inspect[oldID] = 404
	res = f.client().Remove(context.Background(), removal(oldID, secondID), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || len(f.calls) != 4 || !strings.HasSuffix(outcomes(res), "db/remove=succeeded") {
		t.Fatalf("gone then present: %s calls=%v", outcomes(res), f.steps())
	}
}

func TestRemoveRefusesAContainerThatIsNotTheDecidedOne(t *testing.T) {
	for field, v := range map[string]any{"Id": secondID, "Image": newImage, "Created": "2023-11-14T22:13:21Z"} {
		f := newFakeRemoveEngine(t)
		f.containers[oldID][field] = v
		res := f.client().Remove(context.Background(), removal(oldID, secondID), func() {})
		if res.Outcome != protocol.OutcomeDenied || res.Steps[0].Code != "identity_mismatch" || len(f.calls) != 1 {
			t.Fatalf("%s mismatch: %+v calls=%v", field, res, f.steps())
		}
		if got := outcomes(res); got != "web/precondition=denied,web/stop=skipped,web/remove=skipped,db/precondition=skipped,db/stop=skipped,db/remove=skipped" {
			t.Fatalf("%s steps: %s", field, got)
		}
		if err := res.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveStepFailuresStopTheRun(t *testing.T) {
	f := newFakeRemoveEngine(t)
	f.stopStatus = 500
	res := f.client().Remove(context.Background(), removal(oldID), func() {})
	if res.Outcome != protocol.OutcomeFailed || outcomes(res) != "web/precondition=succeeded,web/stop=failed,web/remove=skipped" || len(f.calls) != 2 {
		t.Fatalf("stop 500: %s %+v", outcomes(res), f.steps())
	}
	if res.Steps[1].Code != "runtime_status" || res.Steps[1].Detail != "500" {
		t.Fatalf("stop 500 step: %+v", res.Steps[1])
	}
	f = newFakeRemoveEngine(t)
	f.stopStatus = 304
	if res = f.client().Remove(context.Background(), removal(oldID), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("stop 304: %+v", res)
	}
	f = newFakeRemoveEngine(t)
	f.removeStatus = 404
	if res = f.client().Remove(context.Background(), removal(oldID), func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("remove 404: %+v", res)
	}
	f = newFakeRemoveEngine(t)
	f.removeStatus = 409
	res = f.client().Remove(context.Background(), removal(oldID), func() {})
	if res.Outcome != protocol.OutcomeFailed || res.Steps[2].Code != "dependents" {
		t.Fatalf("remove 409: %+v", res)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRefusesToStartWithoutTimeToFinish(t *testing.T) {
	f := newFakeRemoveEngine(t)
	req := removal(oldID)
	req.Deadline = time.Now().Add(60 * time.Second)
	res := f.client().Remove(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeTimedOut || res.Steps[1].Step != protocol.StepStop || res.Steps[1].Code != "deadline" || res.Steps[2].Outcome != protocol.OutcomeSkipped {
		t.Fatalf("guard: %+v", res)
	}
	if got := f.steps(); len(got) != 1 || got[0] != "GET /containers/"+oldID+"/json" {
		t.Fatalf("calls: %v", got)
	}
}

func TestRemoveRefusesAnInvalidRequestWithoutCalling(t *testing.T) {
	f := newFakeRemoveEngine(t)
	req := removal(oldID)
	req.Containers[0].Target.ContainerID = "../x"
	res := f.client().Remove(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultInvalidRequest || len(f.calls) != 0 || len(res.Steps) != 0 {
		t.Fatalf("invalid request: %+v calls=%d", res, len(f.calls))
	}
}

func TestRemoveTwoTargetsSecondMismatched(t *testing.T) {
	f := newFakeRemoveEngine(t)
	f.containers[secondID]["Image"] = newImage
	res := f.client().Remove(context.Background(), removal(oldID, secondID), func() {})
	if res.Outcome != protocol.OutcomeDenied || res.Code != protocol.ResultStepFailed || res.Steps[3].Code != "identity_mismatch" {
		t.Fatalf("partial: %+v", res)
	}
	if got := outcomes(res); got != "web/precondition=succeeded,web/stop=succeeded,web/remove=succeeded,db/precondition=denied,db/stop=skipped,db/remove=skipped" {
		t.Fatalf("steps: %s", got)
	}
	if got := f.steps(); len(got) != 4 || got[2] != "DELETE /containers/"+oldID {
		t.Fatalf("calls: %v", got)
	}
}

func TestRemoveReportsClockSkewWithoutCalling(t *testing.T) {
	f := newFakeRemoveEngine(t)
	req := removal(oldID)
	req.IssuedAt = time.Now().Add(protocol.MaxClockSkew + time.Minute)
	req.Deadline = req.IssuedAt.Add(time.Minute)
	res := f.client().Remove(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeFailed || res.Code != protocol.ResultClockSkew || res.RequestID != requestID || len(f.calls) != 0 {
		t.Fatalf("skewed removal: %+v", res)
	}
}

// started is called once, before the first stop; a removal of containers already gone never
// changes the host and never calls it.
func TestRemoveCallsStartedBeforeTheFirstStop(t *testing.T) {
	f := newFakeRemoveEngine(t)
	marks := []int{}
	res := f.client().Remove(context.Background(), removal(oldID, secondID), func() {
		f.mu.Lock()
		marks = append(marks, len(f.calls))
		f.mu.Unlock()
	})
	if res.Outcome != protocol.OutcomeSucceeded || len(marks) != 1 || marks[0] != 1 {
		t.Fatalf("started at %v: %+v", marks, res)
	}
	f = newFakeRemoveEngine(t)
	f.inspect[oldID] = 404
	called := false
	if res := f.client().Remove(context.Background(), removal(oldID), func() { called = true }); res.Outcome != protocol.OutcomeSucceeded || called {
		t.Fatalf("started=%v for a container already gone: %+v", called, res)
	}
}
