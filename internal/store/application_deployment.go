package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const DeploymentPlanTTL = 10 * time.Minute
const MaxDeploymentPlanBytes = 64 * 1024

type PlanRequest struct {
	InstanceID     string `json:"instance_id"`
	MappingVersion int    `json:"mapping_version"`
	Revision       int    `json:"revision"`
	Confirm        string `json:"confirm"`
}
type PlannedService struct {
	Name        string                    `json:"name"`
	Reference   string                    `json:"reference"`
	ImageID     string                    `json:"image_id"`
	ImageDigest string                    `json:"image_digest"`
	ContainerID string                    `json:"container_id"`
	Replaces    protocol.InspectionTarget `json:"replaces"`
	Restart     string                    `json:"restart"`
	Ports       []ApplicationPort         `json:"ports"`
	SecretRefs  []string                  `json:"secret_refs"`
}
type DeploymentPlan struct {
	Project  string           `json:"project"`
	Services []PlannedService `json:"services"`
}
type Deployment struct {
	ID             string         `json:"id"`
	ApplicationID  string         `json:"application_id"`
	InstanceID     string         `json:"instance_id"`
	EndpointID     string         `json:"endpoint_id"`
	State          string         `json:"state"`
	Revision       int            `json:"revision"`
	SpecDigest     string         `json:"spec_digest"`
	MappingVersion int            `json:"mapping_version"`
	Plan           DeploymentPlan `json:"plan"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	Expired        bool           `json:"expired"`
}

// PreflightBlockedError names the findings that stopped a plan. A plan never guesses past them.
type PreflightBlockedError struct{ Blockers []string }

func (e *PreflightBlockedError) Error() string {
	return "deployment preflight blocked: " + strings.Join(e.Blockers, ",")
}

// PlanDeployment persists an executable preview. It is minted only from a clean preflight and
// records every identity apply must recheck. It sends no command and reads no secret value.
// See docs/application-schema.md, Deployment plans.
func (t *tenancyStore) PlanDeployment(ctx context.Context, a TenantAccess, app string, r PlanRequest) (*Deployment, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	var out *Deployment
	err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, id.String()+"/deployments", func(tx *sql.Tx) error {
		p, m, spec, snapshot, digest, err := t.preflight(ctx, tx, a, id.String(), true)
		if err != nil {
			return err
		}
		if r.InstanceID != m.InstanceID || r.MappingVersion != m.Version || r.Revision != p.Revision || r.Confirm != m.Preview.Project {
			return ErrAdoptionChanged
		}
		blockers := []string{}
		for _, b := range p.Blockers {
			if b != "runtime_verification_required" {
				blockers = append(blockers, b)
			}
		}
		plan := DeploymentPlan{Project: m.Preview.Project, Services: []PlannedService{}}
		// The preflight resolved each reference to one full image ID, which is the pin. Record
		// the repository digest beside it only when inventory reported exactly one; it is
		// advisory, for a later registry pull.
		digests := map[string]string{}
		for _, im := range snapshot.Images {
			if len(im.Digests) == 1 {
				digests[im.ID] = im.Digests[0]
			}
		}
		for i, s := range spec.Services {
			row := p.Services[i]
			for _, b := range row.Blockers {
				blockers = append(blockers, b)
			}
			refs := make([]string, 0, len(s.Environment))
			for _, ref := range s.Environment {
				refs = append(refs, ref.SecretRef)
			}
			slices.Sort(refs)
			ps := PlannedService{Name: s.Name, Reference: s.Image, ImageID: row.ImageID, ImageDigest: digests[row.ImageID], ContainerID: row.ContainerID, Restart: s.Restart, Ports: s.Ports, SecretRefs: refs}
			if ps.Ports == nil {
				ps.Ports = []ApplicationPort{}
			}
			if row.InspectionTarget != nil {
				ps.Replaces = *row.InspectionTarget
			}
			plan.Services = append(plan.Services, ps)
		}
		if len(blockers) > 0 {
			slices.Sort(blockers)
			return &PreflightBlockedError{Blockers: slices.Compact(blockers)}
		}
		raw, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		if len(raw) > MaxDeploymentPlanBytes {
			return ErrInvalid
		}
		now := time.Now().UTC()
		out = &Deployment{ID: uuid.NewString(), ApplicationID: id.String(), InstanceID: m.InstanceID, EndpointID: m.Preview.EndpointID, State: "planned", Revision: p.Revision, SpecDigest: digest, MappingVersion: m.Version, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: now.Add(DeploymentPlanTTL)}
		if _, err = tx.ExecContext(ctx, t.store.rebind(`DELETE FROM deployments WHERE organization_id=? AND environment_id=? AND instance_id=?`), a.OrganizationID, a.EnvironmentID, m.InstanceID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), out.ID, a.OrganizationID, a.EnvironmentID, out.ApplicationID, out.InstanceID, out.EndpointID, plan.Project, out.State, out.Revision, out.SpecDigest, out.MappingVersion, string(raw), out.CreatedBy, out.CreatedAt, out.ExpiresAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const deploymentColumns = `id,application_id,instance_id,endpoint_id,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at`

func scanDeployment(rows interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var raw string
	if err := rows.Scan(&d.ID, &d.ApplicationID, &d.InstanceID, &d.EndpointID, &d.State, &d.Revision, &d.SpecDigest, &d.MappingVersion, &raw, &d.CreatedBy, &d.CreatedAt, &d.ExpiresAt); err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(raw), &d.Plan) != nil {
		return nil, ErrRevisionCorrupt
	}
	d.Expired = !time.Now().Before(d.ExpiresAt)
	return &d, nil
}

func (t *tenancyStore) ReadDeployment(ctx context.Context, a TenantAccess, app, id string) (*Deployment, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	if _, err = uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *Deployment
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, err = scanDeployment(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+deploymentColumns+` FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *tenancyStore) ListDeployments(ctx context.Context, a TenantAccess, app string) ([]Deployment, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	out := []Deployment{}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT `+deploymentColumns+` FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? ORDER BY created_at DESC, id LIMIT 100`), a.OrganizationID, a.EnvironmentID, appID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDeployment(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
