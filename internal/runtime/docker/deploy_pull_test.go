package docker_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

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
	req.Registries = map[string]protocol.RegistryAuth{"ghcr.io": {Username: "u", Secret: "s"}}
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
	raw, err := base64.StdEncoding.DecodeString(f.pullAuth[0])
	if err != nil || string(raw) != `{"username":"u","password":"s","serveraddress":"ghcr.io"}` {
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
		"mismatch": {func(f *fakeDeployEngine) { f.pulled["RepoDigests"] = []string{daemonText + "@" + pullDigest} }, "pulled image does not match"},
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
