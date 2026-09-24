package docker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const cfgBind = "/srv/shop/config"

// withBind gives the old container a read-only bind and a named volume, as Compose would.
func withBind(f *fakeDeployEngine) {
	f.oldContainer["Mounts"] = []any{
		map[string]any{"Type": "bind", "Source": cfgBind, "Destination": "/etc/shop", "Mode": "ro", "RW": false},
		map[string]any{"Type": "volume", "Name": "shop_data", "Source": "/var/lib/docker/volumes/shop_data/_data", "Destination": "/data", "RW": true},
	}
}

func mountedWeb() protocol.DeploymentService {
	s := webService()
	s.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: "shop_data", Target: "/data"}, {Kind: protocol.MountVolume, Source: "ext", Target: "/ext"}, {Kind: protocol.MountBind, Source: cfgBind, Target: "/etc/shop", ReadOnly: true}}
	return s
}

func mountedDB() protocol.DeploymentService {
	db := webService()
	db.Name, db.ContainerName, db.Replaces.ContainerID, db.Ports = "db", "shop-db-1", otherOldID, nil
	db.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: "shop_data", Target: "/var/lib/db"}}
	return db
}

func stepList(res protocol.DeploymentResult) string {
	got := []string{}
	for _, s := range res.Steps {
		got = append(got, s.Service+" "+s.Step+" "+s.Outcome)
	}
	return strings.Join(got, ",")
}

// Each volume is ensured once, labelled for Compose, under the first service that mounts it and
// before that service's image step; the replacement mounts what the frame says, and no Binds.
func TestDeployEnsuresVolumesAndMountsThem(t *testing.T) {
	f := newFakeDeployEngine(t)
	withBind(f)
	req := request(mountedWeb(), mountedDB())
	req.Volumes = []string{"shop_data", "ext"}
	res := f.client().Deploy(context.Background(), req)
	if res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("outcome: %+v", res)
	}
	want := "web precondition succeeded,web volume succeeded,web volume succeeded,web image succeeded,db precondition succeeded,db image succeeded," +
		"web rename succeeded,web create succeeded,web stop succeeded,web start succeeded,web remove succeeded," +
		"db rename succeeded,db create succeeded,db stop succeeded,db start succeeded,db remove succeeded"
	if got := stepList(res); got != want {
		t.Fatalf("steps:\n got %v\nwant %v", got, want)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	var volumes []engineCall
	var creates []engineCall
	for _, c := range f.calls {
		switch {
		case strings.HasSuffix(c.Path, "/volumes/create"):
			volumes = append(volumes, c)
		case strings.HasSuffix(c.Path, "/containers/create"):
			creates = append(creates, c)
		}
	}
	if len(volumes) != 2 {
		t.Fatalf("volume creates: %+v", volumes)
	}
	for i, want := range []struct{ name, label string }{{"shop_data", "data"}, {"ext", "ext"}} {
		var body struct {
			Name   string
			Labels map[string]string
		}
		if err := json.Unmarshal([]byte(volumes[i].Body), &body); err != nil {
			t.Fatal(err)
		}
		if body.Name != want.name || len(body.Labels) != 2 || body.Labels["com.docker.compose.project"] != "shop" || body.Labels["com.docker.compose.volume"] != want.label {
			t.Fatalf("volume %d body: %s", i, volumes[i].Body)
		}
	}
	var body struct {
		HostConfig map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(creates[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body.HostConfig["Binds"]; ok {
		t.Fatalf("create sent Binds: %s", creates[0].Body)
	}
	var mounts []map[string]any
	if err := json.Unmarshal(body.HostConfig["Mounts"], &mounts); err != nil {
		t.Fatal(err)
	}
	wantMounts := []map[string]any{
		{"Type": "volume", "Source": "shop_data", "Target": "/data", "ReadOnly": false},
		{"Type": "volume", "Source": "ext", "Target": "/ext", "ReadOnly": false},
		{"Type": "bind", "Source": cfgBind, "Target": "/etc/shop", "ReadOnly": true},
	}
	got, _ := json.Marshal(mounts)
	wanted, _ := json.Marshal(wantMounts)
	if string(got) != string(wanted) {
		t.Fatalf("create mounts:\n got %s\nwant %s", got, wanted)
	}
	// A service without mounts sends none.
	f = newFakeDeployEngine(t)
	if res := f.client().Deploy(context.Background(), request(webService())); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("plain: %+v", res)
	}
	for _, c := range f.calls {
		if strings.HasSuffix(c.Path, "/containers/create") && strings.Contains(c.Body, "Mounts") {
			t.Fatalf("mounts sent for a service without any: %s", c.Body)
		}
		if strings.HasSuffix(c.Path, "/volumes/create") {
			t.Fatal("a volume was created for a frame without volumes")
		}
	}
}

func TestDeployVolumeFailureTouchesNoContainer(t *testing.T) {
	f := newFakeDeployEngine(t)
	withBind(f)
	f.volumeStatus = 500
	req := request(mountedWeb(), mountedDB())
	req.Volumes = []string{"shop_data", "ext"}
	res := f.client().Deploy(context.Background(), req)
	if res.Outcome != protocol.OutcomeFailed || res.Steps[1].Step != protocol.StepVolume || res.Steps[1].Detail != "volume create failed" || res.Detail != "service web, step volume: volume create failed" {
		t.Fatalf("outcome: %+v", res)
	}
	want := "web precondition succeeded,web volume failed,web volume skipped,web image skipped,db precondition skipped,db image skipped," +
		"web rename skipped,web create skipped,web stop skipped,web start skipped,web remove skipped," +
		"db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
	if got := stepList(res); got != want {
		t.Fatalf("steps:\n got %v\nwant %v", got, want)
	}
	for _, c := range f.steps() {
		if !strings.HasPrefix(c, "GET ") && c != "POST /volumes/create" {
			t.Fatalf("a container was touched: %v", f.steps())
		}
	}
}

// A bind the frame keeps must be on the old container with the same source, target and mode;
// volume and bind mounts no longer refuse the recreate, tmpfs still does.
func TestDeployBindPrecondition(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*fakeDeployEngine, *protocol.DeploymentService)
		outcome string
		detail  string
	}{
		"bind present":                   {func(*fakeDeployEngine, *protocol.DeploymentService) {}, protocol.OutcomeSucceeded, ""},
		"bind dropped by the definition": {func(_ *fakeDeployEngine, s *protocol.DeploymentService) { s.Mounts = s.Mounts[:2] }, protocol.OutcomeSucceeded, ""},
		"bind missing": {func(f *fakeDeployEngine, _ *protocol.DeploymentService) {
			f.oldContainer["Mounts"] = f.oldContainer["Mounts"].([]any)[1:]
		}, protocol.OutcomeDenied, "bind mount not present on the container"},
		"bind read-write on the container": {func(f *fakeDeployEngine, _ *protocol.DeploymentService) {
			f.oldContainer["Mounts"].([]any)[0].(map[string]any)["RW"] = true
		}, protocol.OutcomeDenied, "bind mount not present on the container"},
		"bind read-write in the frame": {func(_ *fakeDeployEngine, s *protocol.DeploymentService) { s.Mounts[2].ReadOnly = false }, protocol.OutcomeDenied, "bind mount not present on the container"},
		"bind other source":            {func(_ *fakeDeployEngine, s *protocol.DeploymentService) { s.Mounts[2].Source = "/srv/other" }, protocol.OutcomeDenied, "bind mount not present on the container"},
		"bind other target":            {func(_ *fakeDeployEngine, s *protocol.DeploymentService) { s.Mounts[2].Target = "/etc/other" }, protocol.OutcomeDenied, "bind mount not present on the container"},
		"volume in place of the bind": {func(f *fakeDeployEngine, _ *protocol.DeploymentService) {
			f.oldContainer["Mounts"].([]any)[0].(map[string]any)["Type"] = "volume"
		}, protocol.OutcomeDenied, "bind mount not present on the container"},
		"tmpfs beside them": {func(f *fakeDeployEngine, _ *protocol.DeploymentService) {
			f.oldContainer["Mounts"] = append(f.oldContainer["Mounts"].([]any), map[string]any{"Type": "tmpfs", "Destination": "/run", "RW": true})
		}, protocol.OutcomeDenied, "the container has configuration the definition cannot express: mounts"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			withBind(f)
			s := mountedWeb()
			tc.mutate(f, &s)
			req := request(s)
			req.Volumes = []string{"shop_data", "ext"}
			res := f.client().Deploy(context.Background(), req)
			if res.Outcome != tc.outcome || res.Steps[0].Detail != tc.detail {
				t.Fatalf("%+v", res)
			}
			if tc.outcome == protocol.OutcomeDenied {
				for _, c := range f.calls {
					if c.Method != "GET" {
						t.Fatalf("mutating call after a refused precondition: %+v", c)
					}
				}
			}
		})
	}
}
