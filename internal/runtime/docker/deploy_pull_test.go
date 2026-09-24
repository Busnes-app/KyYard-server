package docker_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func pulledService(reference string) protocol.DeploymentService {
	s := webService()
	s.ImageID, s.Pull = "", &protocol.ImagePull{Reference: reference + "@" + pullDigest, Digest: pullDigest}
	return s
}

func TestDeployPullsThePinnedDigest(t *testing.T) {
	f := newFakeDeployEngine(t)
	req := request(pulledService("ghcr.io/org/app"))
	// "a?" puts a '/' in the standard alphabet; the daemon decodes URL-safe and would drop it.
	req.Registries = map[string]protocol.RegistryAuth{"ghcr.io": {Username: "u", Secret: "a?"}}
	res := f.client().Deploy(context.Background(), req)
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("outcome: %+v", res)
	}
	inspect := "GET /images/" + url.PathEscape("ghcr.io/org/app@"+pullDigest) + "/json"
	want := []string{"GET /info", "GET /containers/" + oldID + "/json", "GET /images/" + oldImage + "/json", "POST /images/create", inspect, "POST /containers/" + oldID + "/rename", "POST /containers/create", "POST /containers/" + oldID + "/stop", "POST /containers/" + newID + "/start", "GET /containers/" + newID + "/json", "DELETE /containers/" + oldID}
	if got := f.steps(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order:\n got %v\nwant %v", got, want)
	}
	if q, _ := url.ParseQuery(f.calls[3].Query); len(q) != 2 || q.Get("fromImage") != "ghcr.io/org/app" || q.Get("tag") != pullDigest {
		t.Fatalf("pull query: %q", f.calls[3].Query)
	}
	raw, err := base64.URLEncoding.DecodeString(f.pullAuth[0])
	if err != nil || string(raw) != `{"username":"u","password":"a?","serveraddress":"ghcr.io"}` {
		t.Fatalf("X-Registry-Auth: %q %v", raw, err)
	}
	var body struct{ Image string }
	if err := json.Unmarshal([]byte(f.calls[6].Body), &body); err != nil || body.Image != newImage {
		t.Fatalf("create body image: %q %v", body.Image, err)
	}
	steps := []string{}
	for _, s := range res.Steps {
		steps = append(steps, s.Step)
	}
	if strings.Join(steps, ",") != "precondition,pull,rename,create,stop,start,remove" {
		t.Fatalf("steps: %v", steps)
	}
	if len(res.Services) != 1 || res.Services[0].ImageID != newImage || res.Services[0].ImageDigest != pullDigest {
		t.Fatalf("identity: %+v", res.Services)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}

	f = newFakeDeployEngine(t)
	if res := f.client().Deploy(context.Background(), request(pulledService("ghcr.io/org/app"))); res.Outcome != protocol.OutcomeSucceeded || len(f.pullAuth) != 1 || f.pullAuth[0] != "" {
		t.Fatalf("anonymous pull: %+v auth=%q", res, f.pullAuth)
	}
}

// The daemon reports a Docker Hub image under its short name; the digest must still be byte-equal.
func TestDeployPullVerifiesDockerHubSpelling(t *testing.T) {
	for name, tc := range map[string]struct {
		digests []string
		outcome string
	}{
		"short name":       {[]string{"alpine@" + pullDigest}, protocol.OutcomeSucceeded},
		"full name":        {[]string{"docker.io/library/alpine@" + pullDigest}, protocol.OutcomeSucceeded},
		"index host":       {[]string{"index.docker.io/library/alpine@" + pullDigest}, protocol.OutcomeSucceeded},
		"other repository": {[]string{"other/repo@" + pullDigest}, protocol.OutcomeFailed},
		"other digest":     {[]string{"alpine@sha256:" + strings.Repeat("b", 64)}, protocol.OutcomeFailed},
		"no digests":       {nil, protocol.OutcomeFailed},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			f.pulled = map[string]any{"Id": newImage, "RepoDigests": tc.digests}
			res := f.client().Deploy(context.Background(), request(pulledService("docker.io/library/alpine")))
			if res.Outcome != tc.outcome {
				t.Fatalf("%+v", res)
			}
			if tc.outcome == protocol.OutcomeFailed && (res.Steps[1].Step != protocol.StepPull || res.Steps[1].Detail != "pulled image does not match") {
				t.Fatalf("pull step: %+v", res.Steps[1])
			}
		})
	}
}

// A failed pull ends the run before any container is touched, with a fixed detail: the daemon's
// text and the credential never reach the result.
func TestDeployPullFailuresTouchNothing(t *testing.T) {
	const daemonText = "daemon-text-canary"
	for name, tc := range map[string]struct {
		mutate func(*fakeDeployEngine)
		detail string
	}{
		"401": {func(f *fakeDeployEngine) { f.pullStatus, f.pullBody = 401, daemonText }, "unauthorized"},
		"403": {func(f *fakeDeployEngine) { f.pullStatus, f.pullBody = 403, daemonText }, "unauthorized"},
		"404": {func(f *fakeDeployEngine) { f.pullStatus, f.pullBody = 404, daemonText }, "not found"},
		"500": {func(f *fakeDeployEngine) { f.pullStatus, f.pullBody = 500, daemonText }, "pull failed"},
		"stream error": {func(f *fakeDeployEngine) {
			f.pullBody = `{"status":"Pulling"}` + "\n" + `{"error":"` + daemonText + `"}` + "\n"
		}, "pull failed"},
		"mismatch":    {func(f *fakeDeployEngine) { f.pulled["RepoDigests"] = []string{daemonText + "@" + pullDigest} }, "pulled image does not match"},
		"id shape":    {func(f *fakeDeployEngine) { f.pulled["Id"] = "abc" }, "pulled image does not match"},
		"inspect 404": {func(f *fakeDeployEngine) { f.pulledStatus = 404 }, "the runtime refused with status 404"},
		"inspect 500": {func(f *fakeDeployEngine) { f.pulledStatus = 500 }, "the runtime refused with status 500"},
		// A line past the per-line bound ends the scan with an error, never a success.
		"stream cut": {func(f *fakeDeployEngine) { f.pullBody = `{"status":"` + strings.Repeat("x", 1<<20+1) + `"}` + "\n" }, "the runtime call failed"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			tc.mutate(f)
			req := request(pulledService("ghcr.io/org/app"))
			req.Registries = map[string]protocol.RegistryAuth{"ghcr.io": {Username: "user-canary", Secret: "secret-canary"}}
			res := f.client().Deploy(context.Background(), req)
			if res.Outcome != protocol.OutcomeFailed || res.Steps[1].Step != protocol.StepPull || res.Steps[1].Outcome != protocol.OutcomeFailed || res.Steps[1].Detail != tc.detail || len(res.Services) != 0 {
				t.Fatalf("%+v", res)
			}
			for _, s := range res.Steps[2:] {
				if s.Outcome != protocol.OutcomeSkipped {
					t.Fatalf("later step ran: %+v", s)
				}
			}
			for _, c := range f.steps() {
				if !strings.HasPrefix(c, "GET ") && c != "POST /images/create" {
					t.Fatalf("a container was touched: %v", f.steps())
				}
			}
			raw, _ := json.Marshal(res)
			for _, leak := range []string{daemonText, "user-canary", "secret-canary", f.pullAuth[0]} {
				if strings.Contains(string(raw), leak) {
					t.Fatalf("%q reached the result: %s", leak, raw)
				}
			}
			if err := res.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// All pulls run before any replacement: a second service's refused pull leaves the first
// service's container untouched although its own pull succeeded.
func TestDeploySecondPullFailureTouchesNothing(t *testing.T) {
	f := newFakeDeployEngine(t)
	f.pullStatusFor = map[string]int{"ghcr.io/org/db": 401}
	db := pulledService("ghcr.io/org/db")
	db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", otherOldID, nil
	res := f.client().Deploy(context.Background(), request(pulledService("ghcr.io/org/app"), db))
	if res.Outcome != protocol.OutcomeFailed || res.Detail != "service db, step pull: unauthorized" || len(res.Services) != 0 {
		t.Fatalf("%+v", res)
	}
	for _, c := range f.steps() {
		if !strings.HasPrefix(c, "GET ") && c != "POST /images/create" {
			t.Fatalf("a container was touched: %v", f.steps())
		}
	}
	got := []string{}
	for _, s := range res.Steps {
		got = append(got, s.Service+" "+s.Step+" "+s.Outcome)
	}
	if strings.Join(got[:4], ",") != "web precondition succeeded,web pull succeeded,db precondition succeeded,db pull failed" || len(got) != 14 {
		t.Fatalf("steps: %v", got)
	}
}

// A pull that would leave too little time for every replacement is refused before it is sent.
func TestDeployPullRefusedWithoutTimeToReplace(t *testing.T) {
	const replace = 2*30*time.Second + 3*20*time.Second // replaceBudget
	for name, tc := range map[string]struct {
		services []protocol.DeploymentService
		left     time.Duration
	}{
		"one service":  {[]protocol.DeploymentService{pulledService("ghcr.io/org/app")}, replace + 10*time.Second},
		"two services": {[]protocol.DeploymentService{pulledService("ghcr.io/org/app"), webService()}, 2*replace + 10*time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			if len(tc.services) == 2 {
				tc.services[1].Name, tc.services[1].ContainerName, tc.services[1].Replaces.ContainerID, tc.services[1].Ports = "db", "shop-db-1", otherOldID, nil
			}
			req := request(tc.services...)
			req.Deadline = time.Now().Add(tc.left)
			res := f.client().Deploy(context.Background(), req)
			if res.Outcome != protocol.OutcomeTimedOut || res.Steps[1].Step != protocol.StepPull || res.Steps[1].Outcome != protocol.OutcomeTimedOut {
				t.Fatalf("%+v", res)
			}
			for _, c := range f.steps() {
				if !strings.HasPrefix(c, "GET ") {
					t.Fatalf("pulled or touched a container: %v", f.steps())
				}
			}
		})
	}
}
