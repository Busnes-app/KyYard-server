package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func TestApplicationComparisonObservations(t *testing.T) {
	st, a, app, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	snapshot.Containers[0].Image = "nginx:1"
	snapshot.Containers[0].Labels = map[string]string{"com.docker.compose.service": "web", "secret-canary": "must-not-appear"}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ts.CompareApplication(ctx, a, app.ID)
	if err != nil || c.Availability != "available" || c.Services[0].Observed != 1 || c.Containers[0].ImageComparison != "same_reference" {
		t.Fatalf("comparison: %+v %v", c, err)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "canary") || strings.Contains(string(raw), "must-not-appear") {
		t.Fatal("labels leaked")
	}
	// A new desired revision does not rewrite adoption or claim runtime changes.
	_, err = ts.AppendApplicationRevision(ctx, a, app.ID, 1, ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:2"}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err = ts.CompareApplication(ctx, a, app.ID)
	if err != nil || c.Revision != 2 || c.AdoptedRevision != 1 || c.Containers[0].ImageComparison != "different_reference" {
		t.Fatalf("revision comparison: %+v %v", c, err)
	}
	original := snapshot.Containers[0]
	for _, test := range []struct {
		name             string
		alter            func(*protocol.Container)
		ownership, image string
	}{
		{"missing labels", func(c *protocol.Container) { c.Labels = nil }, "adopted", "unknown"},
		{"changed image identity", func(c *protocol.Container) { c.ImageID = "different" }, "identity_changed", "unknown"},
		{"changed creation", func(c *protocol.Container) { c.CreatedAt = c.CreatedAt.Add(time.Second) }, "identity_changed", "unknown"},
		{"changed project", func(c *protocol.Container) { c.ComposeProject = "other" }, "project_changed", "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot.Containers[0] = original
			test.alter(&snapshot.Containers[0])
			putAdoptionSnapshot(t, st, endpoint, snapshot)
			c, err := ts.CompareApplication(ctx, a, app.ID)
			if err != nil || c.Containers[0].Ownership != test.ownership || c.Containers[0].ImageComparison != test.image || c.Services[0].Observed != 0 {
				t.Fatalf("%+v %v", c, err)
			}
		})
	}
	snapshot.Containers[0] = original
	snapshot.Containers[0].ID = strings.Repeat("c", 64)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	c, err = ts.CompareApplication(ctx, a, app.ID)
	if err != nil || len(c.Containers) != 2 || c.Containers[0].Ownership != "missing" || c.Containers[1].Ownership != "unowned" || c.Services[0].Observed != 0 {
		t.Fatalf("replacement: %+v %v", c, err)
	}
	var commands int
	if err = st.db.QueryRow(`SELECT COUNT(*) FROM endpoint_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatal("comparison sent commands")
	}
	if err = ts.ReleaseApplication(ctx, a, app.ID, instance.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.CompareApplication(ctx, a, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released: %v", err)
	}
}

func TestApplicationComparisonUnavailableAndScoped(t *testing.T) {
	st, a, app, endpoint, snapshot := adoptionFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []TenantAccess{{ActorID: a.ActorID, OrganizationID: a.OrganizationID, EnvironmentID: "foreign"}, {ActorID: a.ActorID, OrganizationID: "foreign", EnvironmentID: a.EnvironmentID}, {ActorID: "foreign", OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID}} {
		if _, err = ts.CompareApplication(ctx, scope, app.ID); err == nil {
			t.Fatal("foreign scope accepted")
		}
	}
	if _, err = ts.CompareApplication(ctx, a, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*protocol.Snapshot)
	}{
		{"truncated", func(s *protocol.Snapshot) { s.Truncated = []string{"containers"}; s.Containers = nil }},
		{"no engine", func(s *protocol.Snapshot) { s.Engine.Version = "" }},
		{"duplicate IDs", func(s *protocol.Snapshot) { s.Containers = append(s.Containers, s.Containers[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := snapshot
			test.change(&copy)
			putAdoptionSnapshot(t, st, endpoint, copy)
			c, err := ts.CompareApplication(ctx, a, app.ID)
			if err != nil || c.Availability != "incomplete" || len(c.Containers) != 0 {
				t.Fatalf("%+v %v", c, err)
			}
		})
	}
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	_, err = st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_inventory SET received_at=? WHERE endpoint_id=?`), time.Now().Add(-time.Hour), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ts.CompareApplication(ctx, a, app.ID)
	if err != nil || c.Availability != "stale" || len(c.Containers) != 0 {
		t.Fatalf("stale: %+v %v", c, err)
	}
	if err = ts.RevokeEndpoint(ctx, a, endpoint); err != nil {
		t.Fatal(err)
	}
	c, err = ts.CompareApplication(ctx, a, app.ID)
	if err != nil || c.Availability != "unavailable" || len(c.Containers) != 0 {
		t.Fatalf("revoked: %+v %v", c, err)
	}
}
