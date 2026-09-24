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
	s.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: "shop_data", Target: "/data"}, {Kind: protocol.MountVolume, Source: "shop_ext", Target: "/ext"}, {Kind: protocol.MountBind, Source: cfgBind, Target: "/etc/shop", ReadOnly: true}}
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
	req.Volumes = []string{"shop_data", "shop_ext"}
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
	for i, want := range []struct{ name, label string }{{"shop_data", "data"}, {"shop_ext", "ext"}} {
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
		{"Type": "volume", "Source": "shop_ext", "Target": "/ext", "ReadOnly": false},
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

// Only 201 is a created volume; any other status fails the step before a container is touched.
func TestDeployVolumeFailureTouchesNoContainer(t *testing.T) {
	for _, status := range []int{500, 200} {
		f := newFakeDeployEngine(t)
		withBind(f)
		f.volumeStatus = status
		req := request(mountedWeb(), mountedDB())
		req.Volumes = []string{"shop_data", "shop_ext"}
		res := f.client().Deploy(context.Background(), req)
		if res.Outcome != protocol.OutcomeFailed || res.Steps[1].Step != protocol.StepVolume || res.Steps[1].Detail != "volume create failed" || res.Detail != "service web, step volume: volume create failed" {
			t.Fatalf("%d: outcome: %+v", status, res)
		}
		want := "web precondition succeeded,web volume failed,web volume skipped,web image skipped,db precondition skipped,db image skipped," +
			"web rename skipped,web create skipped,web stop skipped,web start skipped,web remove skipped," +
			"db rename skipped,db create skipped,db stop skipped,db start skipped,db remove skipped"
		if got := stepList(res); got != want {
			t.Fatalf("%d steps:\n got %v\nwant %v", status, got, want)
		}
		for _, c := range f.steps() {
			if !strings.HasPrefix(c, "GET ") && c != "POST /volumes/create" {
				t.Fatalf("%d: a container was touched: %v", status, f.steps())
			}
		}
	}
}

// An existing volume is mounted only when the old container already mounts it or it is this
// project's plain local volume; a foreign one, or a local volume that is a host path by another
// name, is denied before any container is touched.
func TestDeployVolumeOwnership(t *testing.T) {
	const notOwned, notPresent = "volume is not owned by this project", "volume mount not present on the container"
	project := map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.volume": "cache"}
	hostPath := map[string]string{"type": "none", "o": "bind", "device": "/"}
	hostRoot := map[string]any{"Name": "hostroot", "Driver": "local", "Options": hostPath}
	roOnOld := map[string]any{"Type": "volume", "Name": "hostroot", "Destination": "/cache", "RW": false}
	for name, tc := range map[string]struct {
		volume   string // the frame mounts it at target (default /cache) and lists it in Volumes
		target   string
		ro       bool
		oldMount map[string]any // added to the old container's mounts (shop_data at /data, rw, is there already)
		existing map[string]any
		created  map[string]any // what a racing create answers with instead of the new volume
		outcome  string
		detail   string
		creates  int
	}{
		"absent is created":                 {volume: "shop_cache", outcome: protocol.OutcomeSucceeded, creates: 1},
		"project-owned, new to the service": {volume: "shop_cache", existing: map[string]any{"Name": "shop_cache", "Driver": "local", "Labels": project}, outcome: protocol.OutcomeSucceeded},
		"foreign project denied":            {volume: "shop_cache", existing: map[string]any{"Name": "shop_cache", "Driver": "local", "Labels": map[string]string{"com.docker.compose.project": "billing"}}, outcome: protocol.OutcomeDenied, detail: notOwned},
		"unlabelled denied":                 {volume: "kyyard_data", existing: map[string]any{"Name": "kyyard_data", "Driver": "local"}, outcome: protocol.OutcomeDenied, detail: notOwned},
		"local host path denied":            {volume: "shop_cache", existing: map[string]any{"Name": "shop_cache", "Driver": "local", "Options": hostPath}, outcome: protocol.OutcomeDenied, detail: notOwned},
		"project host path denied":          {volume: "shop_cache", existing: map[string]any{"Name": "shop_cache", "Driver": "local", "Labels": project, "Options": hostPath}, outcome: protocol.OutcomeDenied, detail: notOwned},
		"project other driver denied":       {volume: "shop_cache", existing: map[string]any{"Name": "shop_cache", "Driver": "nfs", "Labels": project}, outcome: protocol.OutcomeDenied, detail: notOwned},
		"already mounted, same place":       {volume: "shop_data", target: "/data", existing: map[string]any{"Name": "shop_data", "Driver": "local", "Options": hostPath}, outcome: protocol.OutcomeSucceeded},
		"already mounted, other target":     {volume: "shop_data", existing: map[string]any{"Name": "shop_data", "Driver": "local", "Options": hostPath}, outcome: protocol.OutcomeDenied, detail: notPresent},
		"host path read-only, frame rw":     {volume: "hostroot", oldMount: roOnOld, existing: hostRoot, outcome: protocol.OutcomeDenied, detail: notPresent},
		"host path read-only, frame ro":     {volume: "hostroot", ro: true, oldMount: roOnOld, existing: hostRoot, outcome: protocol.OutcomeSucceeded},
		"racing foreign create denied":      {volume: "shop_cache", created: map[string]any{"Name": "shop_cache", "Driver": "local", "Options": hostPath}, outcome: protocol.OutcomeDenied, detail: notOwned, creates: 1},
		"absent external denied":            {volume: "shared", outcome: protocol.OutcomeDenied, detail: "volume does not exist"},
		"absent other project denied":       {volume: "billing_cache", outcome: protocol.OutcomeDenied, detail: "volume does not exist"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			withBind(f) // the old container mounts shop_data at /data
			if tc.oldMount != nil {
				f.oldContainer["Mounts"] = append(f.oldContainer["Mounts"].([]any), tc.oldMount)
			}
			f.volumes = map[string]any{}
			if tc.existing != nil {
				f.volumes[tc.volume] = tc.existing
			}
			target := tc.target
			if target == "" {
				target = "/cache"
			}
			s := webService()
			s.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: tc.volume, Target: target, ReadOnly: tc.ro}}
			req := request(s)
			req.Volumes = []string{tc.volume}
			f.createdInstead = tc.created // free when inspected, taken by the time the create lands
			res := f.client().Deploy(context.Background(), req)
			if res.Steps[1].Step != protocol.StepVolume || res.Steps[1].Outcome != tc.outcome || res.Steps[1].Detail != tc.detail || res.Outcome != tc.outcome {
				t.Fatalf("%+v", res)
			}
			creates := 0
			for _, c := range f.steps() {
				if c == "POST /volumes/create" {
					creates++
				} else if !strings.HasPrefix(c, "GET ") && tc.outcome != protocol.OutcomeSucceeded {
					t.Fatalf("a container was touched: %v", f.steps())
				}
			}
			if creates != tc.creates {
				t.Fatalf("%d creates: %v", creates, f.steps())
			}
		})
	}
}

// The refusals that protect data the definition cannot express carry fixed details.
func TestDeployVolumeRefusalDetails(t *testing.T) {
	for detail, mutate := range map[string]func(*fakeDeployEngine){
		"anonymous volumes": func(f *fakeDeployEngine) {
			f.oldContainer["Mounts"] = []any{map[string]any{"Type": "volume", "Name": strings.Repeat("ab", 32), "Destination": "/data", "RW": true}}
		},
		"volumes-from": func(f *fakeDeployEngine) {
			f.oldContainer["HostConfig"].(map[string]any)["VolumesFrom"] = []string{"other"}
		},
		"volume driver": func(f *fakeDeployEngine) { f.oldContainer["HostConfig"].(map[string]any)["VolumeDriver"] = "nfs" },
		"mount options": func(f *fakeDeployEngine) {
			f.oldContainer["HostConfig"].(map[string]any)["Mounts"] = []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{"Subpath": "app"}}}
		},
	} {
		f := newFakeDeployEngine(t)
		mutate(f)
		res := f.client().Deploy(context.Background(), request(webService()))
		if res.Outcome != protocol.OutcomeDenied || res.Steps[0].Detail != "the container has configuration the definition cannot express: "+detail {
			t.Fatalf("%s: %+v", detail, res.Steps[0])
		}
	}
	// The daemon's defaults are not options: a local driver, rprivate binds, empty option objects.
	f := newFakeDeployEngine(t)
	f.oldContainer["HostConfig"].(map[string]any)["VolumeDriver"] = "local"
	f.oldContainer["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/srv", "Destination": "/srv", "RW": true, "Mode": "z", "Propagation": "rprivate"}, map[string]any{"Type": "volume", "Name": "shop_data", "Destination": "/data", "RW": true, "Mode": "z"}}
	f.oldContainer["HostConfig"].(map[string]any)["Mounts"] = []any{map[string]any{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": map[string]any{}}, map[string]any{"Type": "bind", "Source": "/srv", "Target": "/srv", "BindOptions": map[string]any{"Propagation": "rprivate", "CreateMountpoint": true}}}
	s := webService()
	s.Mounts = []protocol.Mount{} // the definition drops both
	if res := f.client().Deploy(context.Background(), request(s)); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("defaults refused: %+v", res.Steps[0])
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
			req.Volumes = []string{"shop_data", "shop_ext"}
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

// Every volume mount must be listed for ensuring, so none escapes the ownership check.
func TestDeployRefusesAnUnlistedVolumeMount(t *testing.T) {
	f := newFakeDeployEngine(t)
	withBind(f)
	req := request(mountedWeb())
	req.Volumes = []string{"shop_data"} // mountedWeb also mounts shop_ext
	res := f.client().Deploy(context.Background(), req)
	if res.Outcome != protocol.OutcomeDenied || len(res.Steps) != 0 || len(f.calls) != 0 {
		t.Fatalf("%+v calls=%v", res, f.steps())
	}
}

// A volume that is not the project's own is kept only where each service already has it: same
// name, target and read-only flag on that service's old container. It cannot move to another
// service, nor gain write access. A project-owned volume may be shared freely.
func TestDeployKeepOnlyVolumePerService(t *testing.T) {
	const notPresent = "volume mount not present on the container"
	ext := func(rw bool) map[string]any {
		return map[string]any{"Type": "volume", "Name": "shared_ext", "Destination": "/ext", "RW": rw}
	}
	for name, tc := range map[string]struct {
		volume             map[string]any
		webOld, dbOld      []any
		webMounts, dbMount bool
		outcome            string
		failing            string // "service step" of the refused step
		detail             string
	}{
		"both kept in place": {map[string]any{"Name": "shared_ext", "Driver": "local"}, []any{ext(true)}, []any{ext(true)}, true, true, protocol.OutcomeSucceeded, "", ""},
		"only the other service's container has it": {map[string]any{"Name": "shared_ext", "Driver": "local"}, []any{}, []any{ext(true)}, true, true, protocol.OutcomeDenied,
			"web volume", "volume is not owned by this project"},
		"moved to a service that lacks it": {map[string]any{"Name": "shared_ext", "Driver": "local"}, []any{ext(true)}, []any{}, false, true, protocol.OutcomeDenied,
			"db volume", "volume is not owned by this project"},
		"added to a second service": {map[string]any{"Name": "shared_ext", "Driver": "local"}, []any{ext(true)}, []any{}, true, true, protocol.OutcomeDenied,
			"db precondition", notPresent},
		"project-owned added to a second service": {map[string]any{"Name": "shared_ext", "Driver": "local", "Labels": map[string]string{"com.docker.compose.project": "shop"}}, []any{}, []any{}, true, true, protocol.OutcomeSucceeded, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			f.volumes = map[string]any{"shared_ext": tc.volume}
			f.oldContainer["Mounts"], f.otherMounts = tc.webOld, tc.dbOld
			web, db := webService(), mountedDB()
			web.Mounts, db.Mounts = []protocol.Mount{}, []protocol.Mount{} // present: nil is a server older than mounts
			if tc.webMounts {
				web.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: "shared_ext", Target: "/ext"}}
			}
			if tc.dbMount {
				db.Mounts = []protocol.Mount{{Kind: protocol.MountVolume, Source: "shared_ext", Target: "/ext"}}
			}
			req := request(web, db)
			req.Volumes = []string{"shared_ext"}
			res := f.client().Deploy(context.Background(), req)
			if res.Outcome != tc.outcome {
				t.Fatalf("%+v", res)
			}
			for _, s := range res.Steps {
				if s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped && (s.Service+" "+s.Step != tc.failing || s.Detail != tc.detail) {
					t.Fatalf("refused at %+v, want %s %q", s, tc.failing, tc.detail)
				}
			}
			for _, c := range f.steps() {
				if c == "POST /volumes/create" || (tc.outcome != protocol.OutcomeSucceeded && !strings.HasPrefix(c, "GET ")) {
					t.Fatalf("unexpected call: %v", f.steps())
				}
			}
		})
	}
}

// decoded is the frame as the agent receives it, with the mounts key removed when absent.
func decoded(t *testing.T, s protocol.DeploymentService, absent bool) protocol.DeploymentService {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if absent {
		delete(fields, "mounts")
	}
	raw, _ = json.Marshal(fields)
	var out protocol.DeploymentService
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A frame without the mounts key comes from a server older than mounts, which relied on the
// agent refusing any; "mounts":[] is a definition that drops them, already shown as dropped.
func TestDeployFrameWithoutMountsKey(t *testing.T) {
	for name, tc := range map[string]struct {
		absent, mounted bool
		outcome, detail string
	}{
		"absent, container has mounts": {true, true, protocol.OutcomeDenied, "the container has configuration the definition cannot express: mounts"},
		"absent, container has none":   {true, false, protocol.OutcomeSucceeded, ""},
		"empty, container has mounts":  {false, true, protocol.OutcomeSucceeded, ""},
		"empty, container has none":    {false, false, protocol.OutcomeSucceeded, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDeployEngine(t)
			if tc.mounted {
				withBind(f)
			}
			s := webService()
			s.Mounts = []protocol.Mount{}
			s = decoded(t, s, tc.absent)
			if (s.Mounts == nil) != tc.absent {
				t.Fatalf("decoded mounts %#v", s.Mounts)
			}
			res := f.client().Deploy(context.Background(), request(s))
			if res.Outcome != tc.outcome || res.Steps[0].Detail != tc.detail {
				t.Fatalf("%+v", res)
			}
			for _, c := range f.calls {
				if tc.outcome == protocol.OutcomeDenied && c.Method != "GET" {
					t.Fatalf("mutating call after a refused precondition: %+v", c)
				}
			}
		})
	}
}
