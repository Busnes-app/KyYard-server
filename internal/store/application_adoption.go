package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

var ErrAdoptionChanged = errors.New("adoption preview changed or inventory unavailable")
var ErrApplicationAdopted = errors.New("application has an adopted instance")

type AdoptedContainer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ImageID   string    `json:"image_id"`
	CreatedAt time.Time `json:"created_at"`
}
type AdoptionPreview struct {
	ApplicationID   string             `json:"application_id"`
	ApplicationName string             `json:"application_name"`
	Revision        int                `json:"revision"`
	EndpointID      string             `json:"endpoint_id"`
	EndpointName    string             `json:"endpoint_name"`
	Project         string             `json:"project"`
	Containers      []AdoptedContainer `json:"containers"`
	Digest          string             `json:"digest"`
}
type AdoptionRequest struct {
	EndpointID string `json:"endpoint_id"`
	Project    string `json:"project"`
	Digest     string `json:"digest"`
	Confirm    string `json:"confirm"`
}
type ApplicationInstance struct {
	ContainerCount int                `json:"container_count"`
	EndpointName   string             `json:"endpoint_name"`
	ID             string             `json:"id"`
	ApplicationID  string             `json:"application_id"`
	EndpointID     string             `json:"endpoint_id"`
	Project        string             `json:"project"`
	Revision       int                `json:"revision"`
	CreatedBy      string             `json:"created_by"`
	CreatedAt      time.Time          `json:"created_at"`
	Containers     []AdoptedContainer `json:"containers"`
}

// Adoption is a control-plane ownership record, not proof of configuration parity
// or permission to execute later. See docs/application-schema.md, Adoption and import.
func (t *tenancyStore) adoptionPreview(ctx context.Context, tx *sql.Tx, a TenantAccess, app, endpoint, project string, lock bool) (*AdoptionPreview, error) {
	if a.EnvironmentID == "" || !validTenantName(project) {
		return nil, ErrInvalid
	}
	p := &AdoptionPreview{ApplicationID: app, EndpointID: endpoint, Project: project, Containers: []AdoptedContainer{}}
	q := `SELECT name,latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`
	if lock && t.store.driver == "postgres" {
		q += " FOR UPDATE"
	}
	if err := tx.QueryRowContext(ctx, t.store.rebind(q), a.OrganizationID, a.EnvironmentID, app).Scan(&p.ApplicationName, &p.Revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// Same lock order as discard: application first. Inventory/revocation lock the
	// endpoint, so no snapshot or identity transition can overtake this adoption.
	if lock {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET name=name WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, endpoint); err != nil {
			return nil, err
		}
	}
	var raw, state, runtime string
	var received, observed time.Time
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.name,e.state,e.runtime,i.received_at,i.observed_at,i.snapshot FROM endpoints e JOIN endpoint_inventory i ON i.endpoint_id=e.id WHERE e.organization_id=? AND e.environment_id=? AND e.id=?`), a.OrganizationID, a.EnvironmentID, endpoint).Scan(&p.EndpointName, &state, &runtime, &received, &observed, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != "active" || runtime != "docker" || time.Since(received) > 3*time.Minute || time.Until(received) > time.Minute || time.Since(observed) > 5*time.Minute || time.Until(observed) > 5*time.Minute {
		return nil, ErrAdoptionChanged
	}
	var snapshot protocol.Snapshot
	if json.Unmarshal([]byte(raw), &snapshot) != nil || snapshot.Engine.Version == "" {
		return nil, ErrAdoptionChanged
	}
	for _, part := range snapshot.Truncated {
		if part == "containers" {
			return nil, ErrAdoptionChanged
		}
	}
	seen := map[string]bool{}
	for _, c := range snapshot.Containers {
		if c.ComposeProject != project {
			continue
		}
		id, err := hex.DecodeString(c.ID)
		if err != nil || len(id) != 32 || c.ID != hex.EncodeToString(id) || c.CreatedAt.IsZero() || c.ImageID == "" || seen[c.ID] {
			return nil, ErrAdoptionChanged
		}
		seen[c.ID] = true
		p.Containers = append(p.Containers, AdoptedContainer{ID: c.ID, Name: c.Name, ImageID: c.ImageID, CreatedAt: c.CreatedAt})
	}
	if len(p.Containers) == 0 || len(p.Containers) > protocol.MaxContainers {
		return nil, ErrAdoptionChanged
	}
	sort.Slice(p.Containers, func(i, j int) bool { return p.Containers[i].ID < p.Containers[j].ID })
	// The digest is an optimistic concurrency precondition, not an authorization token.
	encoded, _ := json.Marshal(p)
	sum := sha256.Sum256(encoded)
	p.Digest = hex.EncodeToString(sum[:])
	return p, nil
}
func (t *tenancyStore) PreviewApplicationAdoption(ctx context.Context, a TenantAccess, app, endpoint, project string) (*AdoptionPreview, error) {
	var p *AdoptionPreview
	err := t.readTenant(ctx, a, permissions.ApplicationAdopt, func(tx *sql.Tx) error {
		var err error
		p, err = t.adoptionPreview(ctx, tx, a, app, endpoint, project, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}
func (t *tenancyStore) AdoptApplication(ctx context.Context, a TenantAccess, app string, r AdoptionRequest) (*ApplicationInstance, error) {
	parsed, err := uuid.Parse(app)
	if err != nil {
		return nil, ErrInvalid
	}
	app = parsed.String()
	var result *ApplicationInstance
	instanceID := uuid.NewString()
	err = t.withTenantTarget(ctx, a, permissions.ApplicationAdopt, app+"/instances/"+instanceID+"/endpoints/"+r.EndpointID, func(tx *sql.Tx) error {
		p, err := t.adoptionPreview(ctx, tx, a, app, r.EndpointID, r.Project, true)
		if err != nil {
			return err
		}
		if r.Digest != p.Digest || r.Confirm != p.Project {
			return ErrAdoptionChanged
		}
		var claimed int
		if err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM application_resources WHERE endpoint_id=?`), p.EndpointID).Scan(&claimed); err != nil {
			return err
		}
		if claimed+len(p.Containers) > protocol.MaxContainers {
			return ErrApplicationLimit
		}
		instance := ApplicationInstance{ContainerCount: len(p.Containers), ID: instanceID, EndpointName: p.EndpointName, ApplicationID: app, EndpointID: p.EndpointID, Project: p.Project, Revision: p.Revision, CreatedBy: a.ActorID, CreatedAt: time.Now().UTC(), Containers: p.Containers}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_instances(id,organization_id,environment_id,application_id,endpoint_id,project,revision,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`), instance.ID, a.OrganizationID, a.EnvironmentID, app, instance.EndpointID, instance.Project, instance.Revision, instance.CreatedBy, instance.CreatedAt)
		if err != nil {
			return err
		}
		for _, c := range instance.Containers {
			if _, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_resources(instance_id,endpoint_id,container_id,name,image_id,created_at) VALUES(?,?,?,?,?,?)`), instance.ID, instance.EndpointID, c.ID, c.Name, c.ImageID, c.CreatedAt); err != nil {
				return err
			}
		}
		result = &instance
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
func (t *tenancyStore) ReleaseApplication(ctx context.Context, a TenantAccess, app, id, confirm string) error {
	parsed, err := uuid.Parse(app)
	if err != nil {
		return ErrInvalid
	}
	app = parsed.String()
	parsedInstance, err := uuid.Parse(id)
	if err != nil {
		return ErrAdoptionChanged
	}
	id = parsedInstance.String()
	return t.withTenantTarget(ctx, a, permissions.ApplicationRelease, app+"/instances/"+id, func(tx *sql.Tx) error {
		if a.EnvironmentID == "" {
			return ErrInvalid
		}
		// Conditional deletion pins the exact instance the operator reviewed. A later
		// re-adoption cannot be released by retrying an old request.
		res, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=? AND id=? AND project=?`), a.OrganizationID, a.EnvironmentID, app, id, confirm)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrAdoptionChanged
		}
		return nil
	})
}
func (t *tenancyStore) ListApplicationInstances(ctx context.Context, a TenantAccess, endpoint string) ([]ApplicationInstance, error) {
	out := []ApplicationInstance{}
	err := t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT i.id,i.application_id,i.endpoint_id,i.project,i.revision,i.created_by,i.created_at,e.name,(SELECT COUNT(*) FROM application_resources r WHERE r.instance_id=i.id) FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND (?='' OR i.environment_id=?) AND (?='' OR i.endpoint_id=?) ORDER BY i.id LIMIT 100`), a.OrganizationID, a.EnvironmentID, a.EnvironmentID, endpoint, endpoint)
		if err != nil {
			return err
		}
		for rows.Next() {
			var i ApplicationInstance
			i.Containers = []AdoptedContainer{}
			if err = rows.Scan(&i.ID, &i.ApplicationID, &i.EndpointID, &i.Project, &i.Revision, &i.CreatedBy, &i.CreatedAt, &i.EndpointName, &i.ContainerCount); err != nil {
				rows.Close()
				return err
			}
			out = append(out, i)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if endpoint == "" {
			return nil
		}
		for n := range out {
			resources, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name,image_id,created_at FROM application_resources WHERE instance_id=? AND endpoint_id=? ORDER BY container_id`), out[n].ID, out[n].EndpointID)
			if err != nil {
				return err
			}
			for resources.Next() {
				var c AdoptedContainer
				if err = resources.Scan(&c.ID, &c.Name, &c.ImageID, &c.CreatedAt); err != nil {
					resources.Close()
					return err
				}
				out[n].Containers = append(out[n].Containers, c)
			}
			err = resources.Err()
			resources.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
