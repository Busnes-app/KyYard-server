package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// rollbackOpen selects, as v, an automated validation whose rollback is not yet decided: its window
// open, its failing verdict awaiting the decision, or its rollback in flight. Manual and rollback
// validations never roll back.
const rollbackOpen = `(v.automated=1 AND v.is_rollback=0 AND v.policy_run_id IS NOT NULL AND v.rollback_outcome='' AND (v.phase<>'done' OR v.verdict IN ('unhealthy','exited','restarting')))`

// Rollback is what an automated rollback re-applies: the revision the failed deployment replaced,
// each service pinned to the image it ran before.
type Rollback struct {
	InstanceID     string
	MappingVersion int
	Project        string
	Revision       int
	// Images maps each service to its plan's Replaces image ID.
	Images map[string]string
}

// RollbackTarget decides, as a with application.deploy re-checked, whether the failed deployment
// can be rolled back, and to what. The target comes from the deployment's own plan: each service's
// Replaces identity is the container it recreated. An ineligible one returns a reason from the
// fixed vocabulary. A rollback deployment is never rolled back (ErrNotFound). See
// docs/application-schema.md, Rollback.
func (t *tenancyStore) RollbackTarget(ctx context.Context, a TenantAccess, app, deployment string) (*Rollback, string, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, "", ErrInvalid
	}
	depID, err := uuid.Parse(deployment)
	if err != nil {
		return nil, "", ErrNotFound
	}
	var out *Rollback
	reason := ""
	err = t.readTenant(ctx, a, permissions.ApplicationDeploy, func(tx *sql.Tx) error {
		out, reason = nil, ""
		var instance, endpoint, planRaw, resultRaw, rolledBack string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT d.instance_id,d.endpoint_id,d.plan,COALESCE(d.result,''),COALESCE(v.rollback_deployment_id,'') FROM deployments d JOIN deployment_validations v ON v.deployment_id=d.id WHERE d.organization_id=? AND d.environment_id=? AND d.application_id=? AND d.id=? AND v.is_rollback=0`), a.OrganizationID, a.EnvironmentID, appID.String(), depID.String()).Scan(&instance, &endpoint, &planRaw, &resultRaw, &rolledBack)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var previous, adopted, version int
		var project string
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT previous_revision,revision,mapping_version,project FROM application_instances WHERE id=?`), instance).Scan(&previous, &adopted, &version, &project)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if rolledBack != "" {
			reason = RollbackAlreadyRolledBack
			return nil
		}
		// An expired planned row needs no filter: idx_deployments_live holds one planned or applying
		// row per instance and every later plan replaces it, so a named rollback plan still present
		// is newer than any deployment it could be confused with.
		var inFlight int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployment_validations v WHERE v.instance_id=? AND v.deployment_id<>? AND ((v.is_rollback=1 AND v.phase<>'done') OR EXISTS (SELECT 1 FROM deployments r WHERE r.id=v.rollback_deployment_id AND r.state IN ('planned','applying')))`), instance, depID.String()).Scan(&inFlight); err != nil {
			return err
		}
		if inFlight > 0 {
			reason = RollbackInFlight
			return nil
		}
		var plan DeploymentPlan
		var result storedDeploymentResult
		if json.Unmarshal([]byte(planRaw), &plan) != nil || json.Unmarshal([]byte(resultRaw), &result) != nil {
			return ErrRevisionCorrupt
		}
		// A Kubernetes plan replaces no recorded container, so it has nothing to roll back to.
		if len(plan.Services) == 0 || plan.Namespace != "" {
			reason = RollbackNoPriorIdentity
			return nil
		}
		// The revision the deployment replaced. previous_revision 0 is the first update after
		// adoption, which replaced what adoption recorded: the instance's adopted revision.
		revision := previous
		if revision == 0 {
			revision = adopted
		}
		if revision < 1 {
			return ErrInvalid // the planner reads 0 as the latest revision
		}
		images := map[string]string{}
		missing := false
		for _, ps := range plan.Services {
			images[ps.Name] = ps.Replaces.ImageID
			missing = missing || ps.Replaces.ImageID == ""
		}
		var specRaw, digest string
		err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, appID.String(), revision).Scan(&specRaw, &digest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var spec ApplicationSpec
		if err != nil || applicationSpecDigest([]byte(specRaw)) != digest || json.Unmarshal([]byte(specRaw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
			reason = RollbackPriorDefinitionInvalid
			return nil
		}
		// The prior spec, this plan and the instance's bindings name the same services, each still
		// bound to the container this deployment created: nothing has replaced them since.
		bound, err := t.boundServices(ctx, tx, instance)
		if err != nil {
			return err
		}
		created := map[string]string{}
		for _, id := range result.Services {
			created[id.Service] = id.ContainerID
		}
		same := len(spec.Services) == len(images) && len(bound) == len(images) && len(created) == len(images)
		for _, s := range spec.Services {
			_, planned := images[s.Name]
			same = same && planned && created[s.Name] != "" && bound[s.Name].containerID == created[s.Name]
		}
		if !same {
			reason = RollbackServiceSetChanged
			return nil
		}
		if missing {
			reason = RollbackNoPriorIdentity
			return nil
		}
		var raw string
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT snapshot FROM endpoint_inventory WHERE endpoint_id=?`), endpoint).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var snapshot protocol.Snapshot
		_ = json.Unmarshal([]byte(raw), &snapshot) // unreadable reads as empty: nothing is on the host
		onHost := imagesOnHost(snapshot)
		for _, id := range images {
			if !onHost[id] {
				reason = RollbackPriorImagesMissing
				return nil
			}
		}
		out = &Rollback{InstanceID: instance, MappingVersion: version, Project: project, Revision: revision, Images: images}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, reason, nil
}
