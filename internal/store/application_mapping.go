package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

type ApplicationMapping struct {
	InstanceID     string            `json:"instance_id"`
	Version        int               `json:"version"`
	MappedRevision int               `json:"mapped_revision"`
	Preview        *AdoptionPreview  `json:"preview"`
	Services       []string          `json:"services"`
	Bindings       map[string]string `json:"bindings"`
}
type MappingRequest struct {
	InstanceID string            `json:"instance_id"`
	Version    int               `json:"version"`
	Digest     string            `json:"digest"`
	Confirm    string            `json:"confirm"`
	Bindings   map[string]string `json:"bindings"`
}

// A mapping assigns desired service names only to already adopted, unchanged IDs.
// It grants no runtime authority. See docs/application-schema.md, Service mapping.
func (t *tenancyStore) applicationMapping(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool) (*ApplicationMapping, error) {
	out := &ApplicationMapping{Services: []string{}, Bindings: map[string]string{}}
	var endpoint, project string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,endpoint_id,project,mapping_version,mapped_revision FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&out.InstanceID, &endpoint, &project, &out.Version, &out.MappedRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out.Preview, err = t.adoptionPreview(ctx, tx, a, app, endpoint, project, lock)
	if err != nil {
		return nil, err
	}
	current := map[string]AdoptedContainer{}
	for _, c := range out.Preview.Containers {
		current[c.ID] = c
	}
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name,image_id,created_at,service_name FROM application_resources WHERE instance_id=? AND endpoint_id=? ORDER BY container_id`), out.InstanceID, endpoint)
	if err != nil {
		return nil, err
	}
	owned := []AdoptedContainer{}
	for rows.Next() {
		var c AdoptedContainer
		var service string
		if err = rows.Scan(&c.ID, &c.Name, &c.ImageID, &c.CreatedAt, &service); err != nil {
			rows.Close()
			return nil, err
		}
		now, found := current[c.ID]
		if !found || now.ImageID != c.ImageID || now.CreatedAt.UnixMicro() != c.CreatedAt.UnixMicro() {
			rows.Close()
			return nil, ErrAdoptionChanged
		}
		owned = append(owned, now)
		if service != "" {
			out.Bindings[service] = c.ID
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(owned) == 0 {
		return nil, ErrAdoptionChanged
	}
	// Keep the digest of the complete observed project, but expose only owned choices.
	out.Preview.Containers = owned
	var raw, digest string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE application_id=? AND number=? AND organization_id=? AND environment_id=?`), app, out.Preview.Revision, a.OrganizationID, a.EnvironmentID).Scan(&raw, &digest)
	if err != nil {
		return nil, err
	}
	var spec ApplicationSpec
	if applicationSpecDigest([]byte(raw)) != digest || json.Unmarshal([]byte(raw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return nil, ErrRevisionCorrupt
	}
	for _, s := range spec.Services {
		out.Services = append(out.Services, s.Name)
	}
	return out, nil
}
func (t *tenancyStore) ReadApplicationMapping(ctx context.Context, a TenantAccess, app string) (*ApplicationMapping, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	var out *ApplicationMapping
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		var err error
		out, err = t.applicationMapping(ctx, tx, a, id.String(), false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (t *tenancyStore) SetApplicationMapping(ctx context.Context, a TenantAccess, app string, r MappingRequest) error {
	id, err := uuid.Parse(app)
	if err != nil {
		return ErrInvalid
	}
	return t.withTenantTarget(ctx, a, permissions.ApplicationAdopt, id.String()+"/service-mapping", func(tx *sql.Tx) error {
		if a.EnvironmentID == "" || r.Version < 0 || r.Version >= 1000000000 || len(r.Bindings) > 100 {
			return ErrInvalid
		}
		p, err := t.applicationMapping(ctx, tx, a, id.String(), true)
		if err != nil {
			return err
		}
		if r.InstanceID != p.InstanceID || r.Version != p.Version || r.Digest != p.Preview.Digest || r.Confirm != p.Preview.Project {
			return ErrAdoptionChanged
		}
		services := map[string]bool{}
		for _, s := range p.Services {
			services[s] = true
		}
		owned := map[string]bool{}
		for _, c := range p.Preview.Containers {
			owned[c.ID] = true
		}
		used := map[string]bool{}
		for service, container := range r.Bindings {
			if !services[service] || !owned[container] || used[container] {
				return ErrInvalid
			}
			used[container] = true
		}
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET mapping_version=mapping_version+1,mapped_revision=? WHERE id=? AND mapping_version=?`), p.Preview.Revision, p.InstanceID, r.Version)
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
		if _, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE application_resources SET service_name='' WHERE instance_id=?`), p.InstanceID); err != nil {
			return err
		}
		for service, container := range r.Bindings {
			res, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE application_resources SET service_name=? WHERE instance_id=? AND endpoint_id=? AND container_id=?`), service, p.InstanceID, p.Preview.EndpointID, container)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrAdoptionChanged
			}
		}
		return nil
	})
}
