package store

import (
	"context"
	"errors"
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
			m := &ApplicationMapping{Preview: &AdoptionPreview{}, Version: 1, MappedRevision: 1, Bindings: map[string]string{"web": "owned"}}
			m.Preview.Revision = 1
			snapshot := protocol.Snapshot{Images: tc.images}
			if tc.truncated {
				snapshot.Truncated = []string{"images"}
			}
			p := buildDeploymentPreflight(m, ApplicationSpec{Services: []ApplicationService{{Name: "web", Image: tc.ref}}}, snapshot)
			if p.Executable || !slices.Contains(p.Blockers, "runtime_verification_required") {
				t.Fatal("diagnostic enabled deployment")
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
			p := buildDeploymentPreflight(m, ApplicationSpec{Services: []ApplicationService{{Name: "web", Image: "nginx:1", Ports: []ApplicationPort{{Published: 8080, Target: 80, Protocol: "tcp", HostIP: tc.desiredIP}}}}}, protocol.Snapshot{Containers: []protocol.Container{{ID: container, Ports: []protocol.Port{{Host: 8080, Container: 80, Protocol: tc.observedProtocol, HostIP: tc.observedIP}}}}})
			if slices.Contains(p.Services[0].Blockers, "reported_port_overlap") != tc.conflict {
				t.Fatalf("unexpected overlap: %+v", p)
			}
		})
	}
	p := buildDeploymentPreflight(&ApplicationMapping{Preview: &AdoptionPreview{}}, ApplicationSpec{Services: []ApplicationService{{Name: "web", Image: "nginx:1", Ports: []ApplicationPort{{Published: 8080, Protocol: "tcp"}, {Published: 8080, Protocol: "tcp"}}}, {Name: "db", Image: "postgres:17", Ports: []ApplicationPort{{Published: 8080, Protocol: "tcp"}}}}}, protocol.Snapshot{})
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
	if p.Executable || len(p.Blockers) != 1 || len(p.Services[0].Blockers) != 0 || p.Services[0].ImageID != snapshot.Images[0].ID {
		t.Fatalf("preflight: %+v", p)
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
	if err != nil || !slices.Contains(p.Blockers, "mapping_requires_review") {
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
