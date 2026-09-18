package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

type ApplicationComparison struct {
	InstanceID      string                `json:"instance_id"`
	EndpointID      string                `json:"endpoint_id"`
	EndpointName    string                `json:"endpoint_name"`
	Project         string                `json:"project"`
	Revision        int                   `json:"revision"`
	AdoptedRevision int                   `json:"adopted_revision"`
	Digest          string                `json:"digest"`
	ReceivedAt      *time.Time            `json:"received_at"`
	Availability    string                `json:"availability"`
	Services        []ComparisonService   `json:"services"`
	Containers      []ComparisonContainer `json:"containers"`
}
type ComparisonService struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Observed int    `json:"observed"`
}
type ComparisonContainer struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Ownership       string `json:"ownership"`
	Service         string `json:"service"`
	Image           string `json:"image"`
	ImageComparison string `json:"image_comparison"`
	State           string `json:"state"`
}

// CompareApplication reports observations, never a deployment plan or grant.
// See docs/application-schema.md, Observed comparison. No secrets are resolved.
func (t *tenancyStore) CompareApplication(ctx context.Context, a TenantAccess, app string) (*ApplicationComparison, error) {
	parsed, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	out := &ApplicationComparison{Services: []ComparisonService{}, Containers: []ComparisonContainer{}}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		var specRaw, state, runtime string
		var inventory sql.NullString
		var received, observed sql.NullTime
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.id,i.endpoint_id,e.name,i.project,i.revision,a.latest_revision,r.spec,r.digest,e.state,e.runtime,v.snapshot,v.received_at,v.observed_at FROM applications a JOIN application_instances i ON i.application_id=a.id JOIN application_revisions r ON r.application_id=a.id AND r.number=a.latest_revision JOIN endpoints e ON e.id=i.endpoint_id LEFT JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE a.organization_id=? AND a.environment_id=? AND a.id=?`), a.OrganizationID, a.EnvironmentID, parsed.String()).Scan(&out.InstanceID, &out.EndpointID, &out.EndpointName, &out.Project, &out.AdoptedRevision, &out.Revision, &specRaw, &out.Digest, &state, &runtime, &inventory, &received, &observed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var spec ApplicationSpec
		if applicationSpecDigest([]byte(specRaw)) != out.Digest || json.Unmarshal([]byte(specRaw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
			return ErrRevisionCorrupt
		}
		for _, s := range spec.Services {
			out.Services = append(out.Services, ComparisonService{Name: s.Name, Image: s.Image})
		}
		if received.Valid {
			out.ReceivedAt = &received.Time
		}
		out.Availability = "unavailable"
		if state != "active" || runtime != "docker" || !inventory.Valid || !received.Valid || !observed.Valid {
			return nil
		}
		out.Availability = "stale"
		if time.Since(received.Time) > 3*time.Minute || time.Until(received.Time) > time.Minute || time.Since(observed.Time) > 5*time.Minute || time.Until(observed.Time) > 5*time.Minute {
			return nil
		}
		var snapshot protocol.Snapshot
		out.Availability = "incomplete"
		if json.Unmarshal([]byte(inventory.String), &snapshot) != nil || snapshot.Engine.Version == "" || len(snapshot.Containers) > protocol.MaxContainers {
			return nil
		}
		for _, part := range snapshot.Truncated {
			if part == "containers" {
				return nil
			}
		}
		current := map[string]protocol.Container{}
		for _, c := range snapshot.Containers {
			if c.ID == "" {
				return nil
			}
			if _, duplicate := current[c.ID]; duplicate {
				return nil
			}
			current[c.ID] = c
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name,image_id,created_at FROM application_resources WHERE instance_id=? AND endpoint_id=? ORDER BY container_id`), out.InstanceID, out.EndpointID)
		if err != nil {
			return err
		}
		owned := map[string]AdoptedContainer{}
		for rows.Next() {
			var c AdoptedContainer
			if err = rows.Scan(&c.ID, &c.Name, &c.ImageID, &c.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			owned[c.ID] = c
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// A concurrent release can remove immutable resources after the metadata read.
		if len(owned) == 0 {
			return ErrAdoptionChanged
		}
		out.Availability = "available"
		for id, saved := range owned {
			c, found := current[id]
			row := ComparisonContainer{ID: id, Name: saved.Name, Ownership: "missing", ImageComparison: "unknown"}
			if found {
				row.Name = c.Name
				row.Image = c.Image
				row.State = c.State
				switch {
				// PostgreSQL timestamps preserve microseconds; inventory JSON may contain nanoseconds.
				case c.ImageID != saved.ImageID || c.CreatedAt.UnixMicro() != saved.CreatedAt.UnixMicro():
					row.Ownership = "identity_changed"
				case c.ComposeProject != out.Project:
					row.Ownership = "project_changed"
				default:
					row.Ownership = "adopted"
					row.Service = c.Labels["com.docker.compose.service"]
					for i, s := range out.Services {
						if row.Service == s.Name {
							out.Services[i].Observed++
							if c.Image != "" {
								row.ImageComparison = "different_reference"
								if c.Image == s.Image {
									row.ImageComparison = "same_reference"
								}
							}
							break
						}
					}
				}
			}
			out.Containers = append(out.Containers, row)
		}
		for id, c := range current {
			if _, exists := owned[id]; !exists && c.ComposeProject == out.Project {
				out.Containers = append(out.Containers, ComparisonContainer{ID: id, Name: c.Name, Ownership: "unowned", Service: c.Labels["com.docker.compose.service"], Image: c.Image, State: c.State, ImageComparison: "unknown"})
			}
		}
		sort.Slice(out.Containers, func(i, j int) bool { return out.Containers[i].ID < out.Containers[j].ID })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
