package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// ApplyDeployment turns a planned row into an applying one and builds the frame the agent
// executes. The values it resolves exist only in the returned request. See
// docs/application-schema.md, Deploy.
func (t *tenancyStore) ApplyDeployment(ctx context.Context, a TenantAccess, app, id, confirm string, key []byte) (*Deployment, *protocol.DeploymentRequest, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 {
		return nil, nil, ErrInvalid
	}
	planID, err := uuid.Parse(id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	var out *Deployment
	var req *protocol.DeploymentRequest
	err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, appID.String()+"/deployments/"+planID.String()+"/apply", func(tx *sql.Tx) error {
		// Application row first, the lock order PlanDeployment and ReleaseApplication take.
		lock := ""
		if t.store.driver == "postgres" {
			lock = " FOR UPDATE"
		}
		var head int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, appID.String()).Scan(&head); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		d, err := scanDeployment(tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+deploymentColumns+` FROM deployments WHERE organization_id=? AND environment_id=? AND application_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), planID.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if d.State == "applying" {
			return ErrDeploymentInProgress
		}
		if confirm != d.Plan.Project {
			return ErrInvalid
		}
		if d.State != "planned" || d.Expired || head != d.Revision {
			return ErrAdoptionChanged
		}
		var mappingVersion int
		var endpointState string
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.mapping_version,e.state FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.id=? AND i.endpoint_id=? AND i.organization_id=? AND i.environment_id=?`), d.InstanceID, d.EndpointID, a.OrganizationID, a.EnvironmentID).Scan(&mappingVersion, &endpointState)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && mappingVersion != d.MappingVersion) {
			return ErrAdoptionChanged
		}
		if err != nil {
			return err
		}
		if endpointState != "active" {
			return ErrEndpointOffline
		}
		spec, values, digest, err := t.resolveApplicationValues(ctx, tx, a, appID.String(), d.Revision, key)
		if err != nil {
			return err
		}
		if digest != d.SpecDigest || len(spec.Services) != len(d.Plan.Services) {
			return ErrAdoptionChanged
		}
		names := map[string]string{}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,name FROM application_resources WHERE instance_id=? AND endpoint_id=?`), d.InstanceID, d.EndpointID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var cid, name string
			if err := rows.Scan(&cid, &name); err != nil {
				rows.Close()
				return err
			}
			names[cid] = name
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		now := time.Now().UTC()
		req = &protocol.DeploymentRequest{Deployment: d.ID, Endpoint: d.EndpointID, Project: d.Plan.Project, Revision: d.Revision, Deadline: now.Add(DeploymentApplyDeadline), Services: []protocol.DeploymentService{}}
		for i, ps := range d.Plan.Services {
			name, ok := names[ps.ContainerID]
			if !ok || spec.Services[i].Name != ps.Name {
				return ErrAdoptionChanged
			}
			svc := protocol.DeploymentService{Name: ps.Name, ContainerName: name, ImageID: ps.ImageID, Replaces: ps.Replaces, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}}
			for _, p := range ps.Ports {
				svc.Ports = append(svc.Ports, protocol.Port{Container: p.Target, Host: p.Published, Protocol: p.Protocol, HostIP: p.HostIP})
			}
			for envName, ref := range spec.Services[i].Environment {
				svc.Env[envName] = values[ref.SecretRef]
			}
			req.Services = append(req.Services, svc)
		}
		if err := req.Validate(now); err != nil {
			return ErrInvalid
		}
		// The agent closes the session on a frame past its bound, so refuse it while the row
		// is still planned rather than send one that can only end unknown.
		if raw, err := json.Marshal(req); err != nil || len(raw) > protocol.MaxDeploymentRequestBytes {
			return ErrInvalid
		}
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='applying',applied_by=?,applied_at=?,deadline=? WHERE id=? AND state='planned'`), a.ActorID, now, req.Deadline, d.ID)
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
		d.State, d.AppliedBy, d.AppliedAt, d.Deadline = "applying", a.ActorID, &now, &req.Deadline
		out = d
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, req, nil
}

// FailDeployment records that the frame never left the server. An unknown row qualifies too:
// a disconnect may have abandoned it first, but the server knows nothing reached the agent.
func (t *tenancyStore) FailDeployment(ctx context.Context, id, detail string) error {
	_, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='failed',detail=?,settled_at=? WHERE id=? AND state IN ('applying','unknown')`), protocol.CleanText(detail, 255), time.Now().UTC(), id)
	return err
}

// auditResults maps a deployment outcome onto the audit table's result vocabulary; the
// outcome itself goes in the details.
var auditResults = map[string]string{protocol.OutcomeSucceeded: "success", protocol.OutcomeFailed: "failure", protocol.OutcomeTimedOut: "failure", protocol.OutcomeDenied: "denied", protocol.OutcomeUnknown: "unknown"}

// settleable selects a deployment row d that may still take a result: applying, or unknown
// while its instance exists and no newer row for it has been applied or settled.
const settleable = `(d.state='applying' OR (d.state='unknown' AND EXISTS (SELECT 1 FROM application_instances i WHERE i.id=d.instance_id) AND NOT EXISTS (SELECT 1 FROM deployments n WHERE n.instance_id=d.instance_id AND n.created_at>d.created_at AND n.state<>'planned')))`

// lockDeploymentApplication takes the application row lock before the deployment row, the
// order ApplyDeployment and PlanDeployment use, so a late result cannot interleave with an
// apply of a newer plan. It returns the row-lock suffix for the deployment select. PostgreSQL
// only: SQLite already serializes writers.
func (t *tenancyStore) lockDeploymentApplication(ctx context.Context, tx *sql.Tx, endpointID, id string) (string, error) {
	if t.store.driver != "postgres" {
		return "", nil
	}
	var app string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT a.id FROM applications a JOIN deployments d ON d.application_id=a.id WHERE d.id=? AND d.endpoint_id=? FOR UPDATE OF a`), id, endpointID).Scan(&app)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return " FOR UPDATE", nil
}

// SettleDeployment is the one writer for outcomes, scoped to the endpoint that ran it. An
// applying row always settles. An unknown row (abandoned or swept) settles only while its
// instance exists and no newer row for it has been applied or settled, so a late answer never
// rewrites state someone has since acted on; a newer plan, built against the containers the
// late answer replaces, is expired instead. Anything else is ErrNotFound.
func (t *tenancyStore) SettleDeployment(ctx context.Context, endpointID string, res protocol.DeploymentResult) error {
	if res.Validate() != nil {
		return ErrInvalid
	}
	raw, err := json.Marshal(storedDeploymentResult{Steps: res.Steps, Services: res.Services})
	if err != nil || len(raw) > MaxDeploymentResultStoredBytes {
		return ErrInvalid
	}
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lock, err := t.lockDeploymentApplication(ctx, tx, endpointID, res.Deployment)
	if err != nil {
		return err
	}
	var org, env, appID, instance, planRaw string
	var revision int
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id,d.instance_id,d.revision,d.plan FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), res.Deployment, endpointID).Scan(&org, &env, &appID, &instance, &revision, &planRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var plan DeploymentPlan
	if json.Unmarshal([]byte(planRaw), &plan) != nil {
		return ErrRevisionCorrupt
	}
	replaced := map[string]PlannedService{}
	for _, ps := range plan.Services {
		replaced[ps.Name] = ps
	}
	// Each identity names a distinct planned service and runs its pinned image; a success
	// accounts for every planned service. Checked in full before any write.
	seen := map[string]bool{}
	for _, idn := range res.Services {
		ps, ok := replaced[idn.Service]
		if !ok || seen[idn.Service] || idn.ImageID != ps.ImageID {
			return ErrInvalid
		}
		seen[idn.Service] = true
	}
	if res.Outcome == protocol.OutcomeSucceeded && len(seen) != len(plan.Services) {
		return ErrInvalid
	}
	for _, idn := range res.Services {
		ps := replaced[idn.Service]
		var name string
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT name FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`), instance, endpointID, ps.ContainerID).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdoptionChanged
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM application_resources WHERE instance_id=? AND endpoint_id=? AND container_id=?`), instance, endpointID, ps.ContainerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_resources(instance_id,endpoint_id,container_id,name,image_id,created_at,service_name) VALUES(?,?,?,?,?,?,?)`), instance, endpointID, idn.ContainerID, name, idn.ImageID, time.Unix(idn.CreatedUnix, 0).UTC(), idn.Service); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	updated, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state=?,detail=?,result=?,settled_at=? WHERE id=? AND state IN ('applying','unknown')`), res.Outcome, protocol.CleanText(res.Detail, 255), string(raw), now, res.Deployment)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if res.Outcome == protocol.OutcomeSucceeded {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET previous_revision=current_revision,current_revision=? WHERE id=?`), revision, instance); err != nil {
			return err
		}
	}
	// Only an unknown row can have a newer plan (a live one refuses planning).
	if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET expires_at=? WHERE instance_id=? AND state='planned' AND created_at>(SELECT created_at FROM deployments WHERE id=?)`), now, instance, res.Deployment); err != nil {
		return err
	}
	// In the same transaction: a settled row without its audit row cannot exist.
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), "agent:"+endpointID, string(permissions.ApplicationDeploy), protocol.CleanText(appID+"/deployments/"+res.Deployment, 255), "outcome="+res.Outcome, now, "organization", org, env, uuid.NewString(), auditResults[res.Outcome]); err != nil {
		return err
	}
	return tx.Commit()
}

// RefuseDeploymentResult records that the endpoint answered with a result that does not fit
// the plan: the host may have acted, so the row becomes unknown with the caller's fixed detail.
// It selects rows as SettleDeployment does, and a repeat (already unknown with this detail)
// changes nothing and writes no audit row, so an agent re-sending the result cannot grow the log.
func (t *tenancyStore) RefuseDeploymentResult(ctx context.Context, endpointID, id, detail string) error {
	tx, err := t.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lock, err := t.lockDeploymentApplication(ctx, tx, endpointID, id)
	if err != nil {
		return err
	}
	var org, env, appID string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.organization_id,d.environment_id,d.application_id FROM deployments d WHERE d.id=? AND d.endpoint_id=? AND `+settleable+lock), id, endpointID).Scan(&org, &env, &appID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	detail = protocol.CleanText(detail, 255)
	updated, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='unknown',detail=? WHERE id=? AND (state='applying' OR (state='unknown' AND detail<>?))`), detail, id, detail)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return nil
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?)`), "agent:"+endpointID, string(permissions.ApplicationDeploy), protocol.CleanText(appID+"/deployments/"+id, 255), "outcome=refused", now, "organization", org, env, uuid.NewString(), auditResults[protocol.OutcomeUnknown]); err != nil {
		return err
	}
	return tx.Commit()
}

// AbandonDeployments marks what was applying when the connection ended as unknown. The
// agent keeps the real answer and re-sends it on reconnect.
func (t *tenancyStore) AbandonDeployments(ctx context.Context, endpointID string) (int64, error) {
	res, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='unknown',detail=? WHERE endpoint_id=? AND state='applying'`), "the connection ended before a result arrived", endpointID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
