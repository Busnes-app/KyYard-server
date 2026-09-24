package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func TestDeploymentPreflightImages(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	digest := "nginx@sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, ref, blocker string
		images             []protocol.Image
		truncated          bool
	}{
		{name: "tag", ref: "nginx:1", images: []protocol.Image{{ID: id, Tags: []string{"nginx:1"}}}},
		{name: "digest", ref: digest, images: []protocol.Image{{ID: id, Digests: []string{digest}}}},
		{name: "ID", ref: id, images: []protocol.Image{{ID: id}}},
		{name: "implicit tag", ref: "nginx", blocker: "explicit_image_reference_required"},
		{name: "missing exact reference", ref: "nginx:1", blocker: "image_not_reported", images: []protocol.Image{{ID: id, Tags: []string{"docker.io/library/nginx:1"}}}},
		{name: "ambiguous", ref: "nginx:1", blocker: "image_reference_ambiguous", images: []protocol.Image{{ID: id, Tags: []string{"nginx:1"}}, {ID: "sha256:" + strings.Repeat("b", 64), Tags: []string{"nginx:1"}}}},
		{name: "invalid identity", ref: "nginx:1", blocker: "image_identity_invalid", images: []protocol.Image{{ID: "short", Tags: []string{"nginx:1"}}}},
		{name: "incomplete", ref: "nginx:1", blocker: "image_inventory_incomplete", truncated: true, images: []protocol.Image{{ID: id, Tags: []string{"nginx:1"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owned := AdoptedContainer{ID: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), CreatedAt: time.Unix(1700000000, 0), Mounts: []protocol.Mount{}}
			m := &ApplicationMapping{Preview: &AdoptionPreview{Containers: []AdoptedContainer{owned}}, Version: 1, MappedRevision: 1, Bindings: map[string]string{"web": owned.ID}}
			m.Preview.Revision = 1
			snapshot := protocol.Snapshot{Images: tc.images}
			if tc.truncated {
				snapshot.Truncated = []string{"images"}
			}
			p := buildDeploymentPreflight(m, ApplicationSpec{Services: []ApplicationService{{Name: "web", Image: tc.ref}}}, snapshot, true)
			if p.Executable != (tc.blocker == "") {
				t.Fatalf("executable must mean no blocker remains: %+v", p)
			}
			row := p.Services[0]
			if tc.blocker == "" {
				if row.ImageID != id || len(row.Blockers) != 0 {
					t.Fatalf("resolution: %+v", row)
				}
			} else if row.ImageID != "" || !slices.Contains(row.Blockers, tc.blocker) {
				t.Fatalf("expected %s: %+v", tc.blocker, row)
			}
		})
	}
}

func TestDeploymentPreflightReplacementIdentity(t *testing.T) {
	containerID := strings.Repeat("c", 64)
	for _, tc := range []struct {
		name    string
		imageID string
		blocker string
	}{
		{name: "valid identity", imageID: "sha256:" + strings.Repeat("d", 64)},
		{name: "missing sha256 prefix", imageID: strings.Repeat("b", 64), blocker: "replacement_identity_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owned := AdoptedContainer{ID: containerID, ImageID: tc.imageID, CreatedAt: time.Unix(1700000000, 0), Mounts: []protocol.Mount{}}
			m := &ApplicationMapping{Preview: &AdoptionPreview{Containers: []AdoptedContainer{owned}}, Version: 1, MappedRevision: 1, Bindings: map[string]string{"web": containerID}}
			m.Preview.Revision = 1
			snapshot := protocol.Snapshot{Images: []protocol.Image{{ID: "sha256:" + strings.Repeat("a", 64), Tags: []string{"nginx:1"}}}}
			p := buildDeploymentPreflight(m, ApplicationSpec{Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}, snapshot, true)
			row := p.Services[0]
			if tc.blocker == "" {
				if row.InspectionTarget == nil || slices.Contains(row.Blockers, "replacement_identity_invalid") {
					t.Fatalf("valid identity flagged: %+v", row)
				}
			} else if row.InspectionTarget != nil || !slices.Contains(row.Blockers, tc.blocker) {
				t.Fatalf("expected %s: %+v", tc.blocker, row)
			}
		})
	}
}

func TestDeploymentPreflightPorts(t *testing.T) {
	for _, tc := range []struct {
		name, observedIP, desiredIP, observedProtocol string
		own, conflict                                 bool
	}{
		{"wildcard", "0.0.0.0", "127.0.0.1", "tcp", false, true},
		{"IPv6 wildcard conservative", "::", "127.0.0.1", "tcp", false, true},
		{"desired wildcard", "127.0.0.1", "", "tcp", false, true},
		{"same specific", "127.0.0.1", "127.0.0.1", "tcp", false, true},
		{"different specific", "127.0.0.2", "127.0.0.1", "tcp", false, false},
		{"different protocol", "", "", "udp", false, false},
		{"mapped replacement", "", "", "tcp", true, false},
		{"invalid observed conservative", "bad", "127.0.0.1", "tcp", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &ApplicationMapping{Preview: &AdoptionPreview{}, Bindings: map[string]string{"web": "own"}}
			container := "other"
			if tc.own {
				container = "own"
			}
			p := buildDeploymentPreflight(m, ApplicationSpec{Services: []ApplicationService{{Name: "web", Image: "nginx:1", Ports: []ApplicationPort{{Published: 8080, Target: 80, Protocol: "tcp", HostIP: tc.desiredIP}}}}}, protocol.Snapshot{Containers: []protocol.Container{{ID: container, Ports: []protocol.Port{{Host: 8080, Container: 80, Protocol: tc.observedProtocol, HostIP: tc.observedIP}}}}}, true)
			if slices.Contains(p.Services[0].Blockers, "reported_port_overlap") != tc.conflict {
				t.Fatalf("unexpected overlap: %+v", p)
			}
		})
	}
	p := buildDeploymentPreflight(&ApplicationMapping{Preview: &AdoptionPreview{}}, ApplicationSpec{Services: []ApplicationService{{Name: "web", Image: "nginx:1", Ports: []ApplicationPort{{Published: 8080, Protocol: "tcp"}, {Published: 8080, Protocol: "tcp"}}}, {Name: "db", Image: "postgres:17", Ports: []ApplicationPort{{Published: 8080, Protocol: "tcp"}}}}}, protocol.Snapshot{}, true)
	for _, s := range p.Services {
		if !slices.Contains(s.Blockers, "desired_port_overlap") {
			t.Fatal("duplicate desired binding missed")
		}
	}
}

func TestApplicationPreflightScopeAndFreshness(t *testing.T) {
	st, a, app, endpoint, snapshot, m := mappingFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	snapshot.Images = []protocol.Image{{ID: "sha256:" + strings.Repeat("d", 64), Tags: []string{"nginx:1"}}}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if err := ts.SetApplicationMapping(ctx, a, app.ID, mappingRequest(m)); err != nil {
		t.Fatal(err)
	}
	p, err := ts.PreflightApplication(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Executable || len(p.Blockers) != 0 || len(p.Services[0].Blockers) != 0 || p.Services[0].ImageID != snapshot.Images[0].ID {
		t.Fatalf("preflight: %+v", p)
	}
	container := snapshot.Containers[0]
	target := p.Services[0].InspectionTarget
	if p.EndpointID != endpoint || target == nil || target.ContainerID != container.ID || target.ImageID != container.ImageID || target.CreatedUnix != container.CreatedAt.Unix() {
		t.Fatalf("inspection must bind the owned identity, not desired image: %+v", p)
	}
	foreign := a
	foreign.EnvironmentID = "foreign"
	if _, err = ts.PreflightApplication(ctx, foreign, app.ID); err == nil {
		t.Fatal("foreign scope accepted")
	}
	if _, err = ts.AppendApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}); err != nil {
		t.Fatal(err)
	}
	p, err = ts.PreflightApplication(ctx, a, app.ID)
	if err != nil || p.Executable || !slices.Contains(p.Blockers, "mapping_requires_review") {
		t.Fatalf("stale mapping: %+v %v", p, err)
	}
	original := snapshot.Containers[0]
	snapshot.Containers[0].ImageID = "changed"
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err = ts.PreflightApplication(ctx, a, app.ID); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("changed identity accepted")
	}
	snapshot.Containers[0] = original
	snapshot.Truncated = []string{"containers"}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err = ts.PreflightApplication(ctx, a, app.ID); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("partial containers accepted")
	}
	snapshot.Truncated = nil
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	if _, err = st.db.Exec(st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), time.Now().Add(-4*time.Minute), endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.PreflightApplication(ctx, a, app.ID); !errors.Is(err, ErrAdoptionChanged) {
		t.Fatal("stale inventory accepted")
	}
	var commands int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM endpoint_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatal("preflight dispatched command")
	}
}

// Binds are preserve-only: a definition bind needs the identical mount (source, target and
// read-only, compared as reported) on the mapped container. Named volumes never block.
func TestDeploymentPreflightMounts(t *testing.T) {
	bind := func(source, target string, ro bool) protocol.Mount {
		return protocol.Mount{Kind: protocol.MountBind, Source: source, Target: target, ReadOnly: ro}
	}
	volume := func(source, target string, ro bool) protocol.Mount {
		return protocol.Mount{Kind: protocol.MountVolume, Source: source, Target: target, ReadOnly: ro}
	}
	for _, tc := range []struct {
		name        string
		want        ApplicationVolume
		has         []protocol.Mount
		truncated   bool
		blocker     string
		dropped     []protocol.Mount
		unsupported []protocol.Mount
	}{
		{name: "identical bind", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data"}, has: []protocol.Mount{bind("/srv/data", "/data", false)}},
		{name: "identical read-only bind", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data", ReadOnly: true}, has: []protocol.Mount{bind("/srv/data", "/data", true)}},
		{name: "new bind", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data"}, has: []protocol.Mount{}, blocker: "bind_mount_new"},
		{name: "trailing slash is another path", want: ApplicationVolume{Kind: "bind", Source: "/data", Target: "/data"}, has: []protocol.Mount{bind("/data/", "/data", false)}, blocker: "bind_mount_new", dropped: []protocol.Mount{bind("/data/", "/data", false)}},
		{name: "other target", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data"}, has: []protocol.Mount{bind("/srv/data", "/var/data", false)}, blocker: "bind_mount_new", dropped: []protocol.Mount{bind("/srv/data", "/var/data", false)}},
		{name: "read-only to read-write", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data"}, has: []protocol.Mount{bind("/srv/data", "/data", true)}, blocker: "bind_mount_new", dropped: []protocol.Mount{bind("/srv/data", "/data", true)}},
		{name: "read-write to read-only", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data", ReadOnly: true}, has: []protocol.Mount{bind("/srv/data", "/data", false)}, blocker: "bind_mount_new", dropped: []protocol.Mount{bind("/srv/data", "/data", false)}},
		{name: "a volume of the same name is not the bind", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data"}, has: []protocol.Mount{volume("/srv/data", "/data", false)}, blocker: "bind_mount_new", dropped: []protocol.Mount{volume("/srv/data", "/data", false)}},
		{name: "new named volume", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, has: []protocol.Mount{}},
		{name: "identical named volume", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, has: []protocol.Mount{volume("shop_data", "/data", false)}},
		{name: "named volume over a bind", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, has: []protocol.Mount{bind("/srv/data", "/data", false)}, dropped: []protocol.Mount{bind("/srv/data", "/data", false)}},
		{name: "only the volume source differs", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, has: []protocol.Mount{volume("shop_old", "/data", false)}, dropped: []protocol.Mount{volume("shop_old", "/data", false)}},
		{name: "only the volume target differs", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, has: []protocol.Mount{volume("shop_data", "/old", false)}, dropped: []protocol.Mount{volume("shop_data", "/old", false)}},
		{name: "volume read-only to read-write", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, has: []protocol.Mount{volume("shop_data", "/data", true)}, dropped: []protocol.Mount{volume("shop_data", "/data", true)}},
		{name: "anonymous volume", has: []protocol.Mount{volume(strings.Repeat("f", 64), "/cache", false)}, blocker: "mount_unsupported", unsupported: []protocol.Mount{volume(strings.Repeat("f", 64), "/cache", false)}},
		{name: "tmpfs", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, has: []protocol.Mount{volume("shop_data", "/data", false), {Kind: protocol.MountOther, Target: "/tmp"}}, blocker: "mount_unsupported", unsupported: []protocol.Mount{{Kind: protocol.MountOther, Target: "/tmp"}}},
		{name: "unreported", want: ApplicationVolume{Kind: "named", Source: "data", Target: "/data"}, blocker: "mounts_unreported"},
		{name: "truncated", want: ApplicationVolume{Kind: "bind", Source: "/srv/data", Target: "/data"}, has: []protocol.Mount{bind("/srv/data", "/data", false)}, truncated: true, blocker: "mounts_unreported"},
		{name: "unreported without volumes", blocker: "mounts_unreported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owned := AdoptedContainer{ID: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), CreatedAt: time.Unix(1700000000, 0), Mounts: tc.has, MountsTruncated: tc.truncated}
			m := &ApplicationMapping{Preview: &AdoptionPreview{Project: "shop", Revision: 1, Containers: []AdoptedContainer{owned}}, Version: 1, MappedRevision: 1, Bindings: map[string]string{"web": owned.ID}}
			service := ApplicationService{Name: "web", Image: "nginx:1"}
			if tc.want.Kind != "" {
				service.Volumes = []ApplicationVolume{tc.want}
			}
			spec := ApplicationSpec{Services: []ApplicationService{service}, Volumes: []DeclaredVolume{{Name: "data"}}}
			p := buildDeploymentPreflight(m, spec, protocol.Snapshot{Images: []protocol.Image{{ID: "sha256:" + strings.Repeat("a", 64), Tags: []string{"nginx:1"}}}}, true)
			row := p.Services[0]
			if tc.blocker == "" {
				if !p.Executable || len(row.Blockers) != 0 {
					t.Fatalf("blocked: %+v", row)
				}
			} else if p.Executable || !reflect.DeepEqual(row.Blockers, []string{tc.blocker}) {
				t.Fatalf("expected only %s: %+v", tc.blocker, row)
			}
			binds := []protocol.Mount{}
			for _, d := range tc.dropped {
				if d.Kind == protocol.MountBind {
					binds = append(binds, d)
				}
			}
			if !slices.Equal(row.DroppedMounts, tc.dropped) || !slices.Equal(row.DroppedBinds, binds) || !slices.Equal(row.UnsupportedMounts, tc.unsupported) {
				t.Fatalf("dropped %+v, binds %+v, unsupported %+v", row.DroppedMounts, row.DroppedBinds, row.UnsupportedMounts)
			}
		})
	}
}

// The resolved list names volumes by host name (project prefix unless external); every mount
// on the container the definition does not list identically is reported, binds also on their own.
func TestDeploymentPreflightResolvesAndReportsDroppedMounts(t *testing.T) {
	kept := protocol.Mount{Kind: protocol.MountBind, Source: "/srv/web", Target: "/srv"}
	oldBind := protocol.Mount{Kind: protocol.MountBind, Source: "/old", Target: "/old", ReadOnly: true}
	readOnly := protocol.Mount{Kind: protocol.MountVolume, Source: "shop_data", Target: "/var/lib/data", ReadOnly: true}
	owned := AdoptedContainer{ID: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), CreatedAt: time.Unix(1700000000, 0), Mounts: []protocol.Mount{kept, oldBind, readOnly}}
	m := &ApplicationMapping{Preview: &AdoptionPreview{Project: "shop", Revision: 1, Containers: []AdoptedContainer{owned}}, Version: 1, MappedRevision: 1, Bindings: map[string]string{"web": owned.ID}}
	spec := ApplicationSpec{Volumes: []DeclaredVolume{{Name: "data"}, {Name: "shared", External: true}}, Services: []ApplicationService{{Name: "web", Image: "nginx:1", Volumes: []ApplicationVolume{
		{Kind: "named", Source: "data", Target: "/var/lib/data"},
		{Kind: "named", Source: "shared", Target: "/shared", ReadOnly: true},
		{Kind: "bind", Source: "/srv/web", Target: "/srv"},
	}}}}
	p := buildDeploymentPreflight(m, spec, protocol.Snapshot{Images: []protocol.Image{{ID: "sha256:" + strings.Repeat("a", 64), Tags: []string{"nginx:1"}}}, Volumes: []protocol.Volume{{Name: "shared"}}}, true)
	row := p.Services[0]
	if !p.Executable {
		t.Fatalf("blocked: %+v", p)
	}
	want := []protocol.Mount{{Kind: protocol.MountVolume, Source: "shop_data", Target: "/var/lib/data"}, {Kind: protocol.MountVolume, Source: "shared", Target: "/shared", ReadOnly: true}, kept}
	if !reflect.DeepEqual(row.Mounts, want) {
		t.Fatalf("mounts: %+v", row.Mounts)
	}
	// Read-only to read-write widens access: the old mount shows as dropped beside the new one.
	if !reflect.DeepEqual(row.DroppedBinds, []protocol.Mount{oldBind}) || !reflect.DeepEqual(row.DroppedMounts, []protocol.Mount{oldBind, readOnly}) {
		t.Fatalf("dropped: %+v %+v", row.DroppedBinds, row.DroppedMounts)
	}
}

// An external volume is mounted, never created: it must be in a complete volume list.
func TestDeploymentPreflightExternalVolumes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		external  bool
		volumes   []protocol.Volume
		truncated bool
		missing   bool
	}{
		{name: "present", external: true, volumes: []protocol.Volume{{Name: "shared"}}},
		{name: "absent", external: true, volumes: []protocol.Volume{{Name: "shop_shared"}}, missing: true},
		{name: "list truncated", external: true, volumes: []protocol.Volume{{Name: "shared"}}, truncated: true, missing: true},
		{name: "project volume is never checked", volumes: nil, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owned := AdoptedContainer{ID: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), CreatedAt: time.Unix(1700000000, 0), Mounts: []protocol.Mount{}}
			m := &ApplicationMapping{Preview: &AdoptionPreview{Project: "shop", Revision: 1, Containers: []AdoptedContainer{owned}}, Version: 1, MappedRevision: 1, Bindings: map[string]string{"web": owned.ID}}
			spec := ApplicationSpec{Volumes: []DeclaredVolume{{Name: "shared", External: tc.external}}, Services: []ApplicationService{{Name: "web", Image: "nginx:1", Volumes: []ApplicationVolume{{Kind: "named", Source: "shared", Target: "/shared"}}}}}
			snapshot := protocol.Snapshot{Images: []protocol.Image{{ID: "sha256:" + strings.Repeat("a", 64), Tags: []string{"nginx:1"}}}, Volumes: tc.volumes}
			if tc.truncated {
				snapshot.Truncated = []string{"volumes"}
			}
			row := buildDeploymentPreflight(m, spec, snapshot, true).Services[0]
			if tc.missing != slices.Equal(row.Blockers, []string{"volume_missing"}) || (!tc.missing && len(row.Blockers) != 0) {
				t.Fatalf("blockers: %+v", row.Blockers)
			}
		})
	}
}

// volumesFixture adopts and maps one container per service of spec, each reporting
// mounts[service], with every image tagged and every external volume in the inventory.
func volumesFixture(t *testing.T, spec ApplicationSpec, mounts map[string][]protocol.Mount) (*SQLStore, TenantAccess, *Application, *ApplicationMapping) {
	t.Helper()
	st, a, app, endpoint, snapshot := adoptionFixtureSpec(t, spec, nil, mounts)
	ctx := context.Background()
	ts := st.Tenancy()
	for i, s := range spec.Services {
		snapshot.Images = append(snapshot.Images, protocol.Image{ID: snapshot.Containers[i].ImageID, Tags: []string{s.Image}})
	}
	for _, v := range spec.Volumes {
		if v.External {
			snapshot.Volumes = append(snapshot.Volumes, protocol.Volume{Name: v.Name})
		}
	}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := mappingRequest(m)
	r.Bindings = map[string]string{}
	for i, s := range spec.Services {
		r.Bindings[s.Name] = snapshot.Containers[i].ID
	}
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
	if m, err = ts.ReadApplicationMapping(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	return st, a, app, m
}

func previewMounts(m *ApplicationMapping, name string) []protocol.Mount {
	i := slices.IndexFunc(m.Preview.Containers, func(c AdoptedContainer) bool { return c.Name == name })
	return m.Preview.Containers[i].Mounts
}

func volumesSpec() ApplicationSpec {
	return ApplicationSpec{Kind: "compose.v1", Volumes: []DeclaredVolume{{Name: "data"}, {Name: "shared", External: true}}, Services: []ApplicationService{
		{Name: "web", Image: "nginx:1", Volumes: []ApplicationVolume{{Kind: "named", Source: "data", Target: "/data"}, {Kind: "bind", Source: "/srv/web", Target: "/srv", ReadOnly: true}}},
		{Name: "worker", Image: "worker:1", Volumes: []ApplicationVolume{{Kind: "named", Source: "shared", Target: "/shared"}, {Kind: "named", Source: "data", Target: "/data"}}},
	}}
}

// Adoption records each container's mounts from inventory; the plan carries the resolved
// mounts per service and each named volume once, in first-use order.
func TestPlanDeploymentCarriesMountsAndVolumes(t *testing.T) {
	webBind := protocol.Mount{Kind: protocol.MountBind, Source: "/srv/web", Target: "/srv", ReadOnly: true}
	st, a, app, m := volumesFixture(t, volumesSpec(), map[string][]protocol.Mount{"web": {webBind, {Kind: protocol.MountBind, Source: "/old", Target: "/old"}}})
	ctx := context.Background()
	ts := st.Tenancy()
	if got := previewMounts(m, "shop-web"); len(got) != 2 || got[0] != webBind {
		t.Fatalf("preview mounts: %+v", m.Preview.Containers)
	}
	p, err := ts.PreflightApplication(ctx, a, app.ID)
	if err != nil || !p.Executable {
		t.Fatalf("preflight: %+v %v", p, err)
	}
	if len(p.Services[0].DroppedBinds) != 1 || p.Services[0].DroppedBinds[0].Source != "/old" {
		t.Fatalf("dropped bind: %+v", p.Services[0])
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	web := []protocol.Mount{{Kind: protocol.MountVolume, Source: "shop_data", Target: "/data"}, webBind}
	worker := []protocol.Mount{{Kind: protocol.MountVolume, Source: "shared", Target: "/shared"}, {Kind: protocol.MountVolume, Source: "shop_data", Target: "/data"}}
	if !reflect.DeepEqual(d.Plan.Services[0].Mounts, web) || !reflect.DeepEqual(d.Plan.Services[1].Mounts, worker) || !reflect.DeepEqual(d.Plan.Volumes, []string{"shop_data", "shared"}) {
		t.Fatalf("plan: %+v", d.Plan)
	}
	// The approver sees the bind the definition dropped; the stored plan keeps it.
	if !reflect.DeepEqual(d.Plan.Services[0].DroppedMounts, []protocol.Mount{{Kind: protocol.MountBind, Source: "/old", Target: "/old"}}) || d.Plan.Services[1].DroppedMounts != nil {
		t.Fatalf("dropped on the plan: %+v", d.Plan.Services)
	}
	stored, err := ts.ReadDeployment(ctx, a, app.ID, d.ID)
	if err != nil || !reflect.DeepEqual(stored.Plan, d.Plan) {
		t.Fatalf("stored plan: %+v %v", stored, err)
	}
}

func TestPlanDeploymentRefusesANewBindMount(t *testing.T) {
	st, a, app, m := volumesFixture(t, volumesSpec(), map[string][]protocol.Mount{"web": {{Kind: protocol.MountBind, Source: "/srv/web/", Target: "/srv", ReadOnly: true}}})
	_, err := st.Tenancy().PlanDeployment(context.Background(), a, app.ID, planRequest(m), nil, imageCheckKey, false)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || !reflect.DeepEqual(blocked.Blockers, []string{"bind_mount_new"}) {
		t.Fatalf("new bind: %v", err)
	}
}

// An agent older than mounts reports none: nothing to compare, so nothing is planned.
func TestPlanDeploymentRefusesUnreportedMounts(t *testing.T) {
	st, a, app, m := volumesFixture(t, volumesSpec(), map[string][]protocol.Mount{"web": {{Kind: protocol.MountBind, Source: "/srv/web", Target: "/srv", ReadOnly: true}}, "worker": nil})
	if previewMounts(m, "shop-worker") != nil || previewMounts(m, "shop-web") == nil {
		t.Fatalf("reported and unreported mounts confused: %+v", m.Preview.Containers)
	}
	// The API does not inspect a blocked preflight, and the store adds no inspection blocker.
	r := planRequest(m)
	r.Inspections = nil
	_, err := st.Tenancy().PlanDeployment(context.Background(), a, app.ID, r, nil, imageCheckKey, false)
	var blocked *PreflightBlockedError
	if !errors.As(err, &blocked) || !reflect.DeepEqual(blocked.Blockers, []string{"mounts_unreported"}) {
		t.Fatalf("unreported: %v", err)
	}
}

func TestApplicationSpecDigestCoversVolumes(t *testing.T) {
	spec := volumesSpec()
	_, before, err := encodeApplicationSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Services[0].Volumes[1].ReadOnly = false
	_, after, err := encodeApplicationSpec(spec)
	if err != nil || after == before {
		t.Fatalf("digest ignores a volume change: %v", err)
	}
}

// An inventory whose agent clock disagrees with the server's by more than MaxClockSkew blocks
// preflight and plan; inside the bound it does not.
func TestPreflightBlocksClockSkew(t *testing.T) {
	st, a, app, endpoint, _, m := planFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	skew := func(received, observed time.Duration) {
		t.Helper()
		now := time.Now().UTC()
		if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET received_at=?, observed_at=? WHERE endpoint_id=?`), now.Add(received), now.Add(observed), endpoint); err != nil {
			t.Fatal(err)
		}
	}
	skew(-2*time.Minute, 4*time.Minute) // six minutes apart, each inside freshInventory's windows
	p, err := ts.PreflightApplication(ctx, a, app.ID)
	if err != nil || p.Executable || !slices.Contains(p.Blockers, "clock_skew") {
		t.Fatalf("skewed preflight: %+v %v", p, err)
	}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false); !isBlocked(err, "clock_skew") {
		t.Fatalf("skewed plan: %v", err)
	}
	skew(-time.Minute, 3*time.Minute)
	if p, err = ts.PreflightApplication(ctx, a, app.ID); err != nil || slices.Contains(p.Blockers, "clock_skew") {
		t.Fatalf("four minutes apart: %+v %v", p, err)
	}
}
