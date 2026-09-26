package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/google/uuid"
)

const DeploymentPlanTTL = 10 * time.Minute
const MaxDeploymentPlanBytes = 64 * 1024
const DeploymentApplyDeadline = 10 * time.Minute
const MaxDeploymentResultStoredBytes = 160 * 1024

type PlanRequest struct {
	InstanceID     string `json:"instance_id"`
	MappingVersion int    `json:"mapping_version"`
	Revision       int    `json:"revision"`
	Confirm        string `json:"confirm"`
	// Update names services to pull from the registry, pinned to its digest at plan time.
	Update []string `json:"update"`
	// Set by the API, never read from a client: the largest frame the endpoint's agent accepts,
	// and one live inspection per mapped container, keyed by container ID.
	MaxFrameBytes int                                     `json:"-"`
	Inspections   map[string]protocol.ContainerInspection `json:"-"`
	// PinImages is set only by a validation's rollback: service to image ID, taken instead of
	// resolving the service's tag. The ID must be on the host; nothing is pulled.
	PinImages map[string]string `json:"-"`
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
	// Set for a pulled service: apply sends the pull and no image ID.
	PullReference string `json:"pull_reference,omitempty"`
	PullDigest    string `json:"pull_digest,omitempty"`
	// Mounts are the preflight's resolved list; binds on it are already on the replaced container.
	Mounts []protocol.Mount `json:"mounts,omitempty"`
	// DroppedMounts are the replaced container's mounts the recreate leaves off, for approval.
	DroppedMounts []protocol.Mount `json:"dropped_mounts,omitempty"`
	// Object is the Deployment (and Service) a Kubernetes plan applies for this service; nil on
	// Docker. Replaces, ImageID and Mounts are then empty and the service always pulls.
	Object *KubernetesObject `json:"object,omitempty"`
	// ClaimMounts mount the plan's claims into a Kubernetes service.
	ClaimMounts []protocol.KubernetesMount `json:"claim_mounts,omitempty"`
}

// KubernetesObject names a service's objects: the Deployment and Service <name>, the
// ConfigMap <name>-env and the Secret <name>-secret.
type KubernetesObject struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// DeploymentPlan is what a row decided: the services an apply replaces, or the containers a
// removal stops and deletes (Services then empty).
type DeploymentPlan struct {
	Project    string              `json:"project"`
	Services   []PlannedService    `json:"services"`
	Containers []RemovalPlanTarget `json:"containers,omitempty"`
	// Volumes are the named volumes the agent ensures, host names in first-use order.
	Volumes []string `json:"volumes,omitempty"`
	// Namespace is set exactly for a Kubernetes plan or removal.
	Namespace string `json:"namespace,omitempty"`
	// Claims are the PersistentVolumeClaims a Kubernetes apply ensures.
	Claims []protocol.KubernetesClaim `json:"claims,omitempty"`
}
type RemovalPlanTarget struct {
	Service     string `json:"service"`
	ContainerID string `json:"container_id"`
	ImageID     string `json:"image_id"`
	CreatedUnix int64  `json:"created_unix"`
	Name        string `json:"name"`
}
type Deployment struct {
	ID             string         `json:"id"`
	ApplicationID  string         `json:"application_id"`
	InstanceID     string         `json:"instance_id"`
	EndpointID     string         `json:"endpoint_id"`
	EndpointName   string         `json:"endpoint_name"`
	Kind           string         `json:"kind"`
	State          string         `json:"state"`
	Revision       int            `json:"revision"`
	SpecDigest     string         `json:"spec_digest"`
	MappingVersion int            `json:"mapping_version"`
	Plan           DeploymentPlan `json:"plan"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	Expired        bool           `json:"expired"`
	AppliedBy      string         `json:"applied_by"`
	AppliedAt      *time.Time     `json:"applied_at"`
	Deadline       *time.Time     `json:"deadline"`
	SettledAt      *time.Time     `json:"settled_at"`
	Detail         string         `json:"detail"`
	// CorrelationID is the plan's request ID: the frame and every audit row of the deployment carry it.
	CorrelationID string                     `json:"correlation_id"`
	Result        *protocol.DeploymentResult `json:"result"` // nil until settled
	// Validation is the deployment's health validation: nil for a plan, a removal or an apply
	// that did not succeed.
	Validation *Validation `json:"validation,omitempty"`
	// MigrationID names the open migration whose destination this apply deployed, set when the
	// row turned applying.
	MigrationID string `json:"migration_id,omitempty"`
}

// storedDeploymentResult is the shape kept in the result column: the result code and the
// step-by-step record, codes and parameters only. The outcome lives in the row's state.
type storedDeploymentResult struct {
	Code     string                        `json:"code,omitempty"`
	Steps    []protocol.DeploymentStep     `json:"steps"`
	Services []protocol.DeploymentIdentity `json:"services"`
}

// legacyResult reads a result produced by a binary built before outcome codes: it carries no
// request ID, and a step or result that did not succeed carries no code. Each missing code becomes
// protocol.CodeLegacy and its free text is dropped. A result with a request ID is a current
// agent's and is taken as sent.
func legacyResult(res protocol.DeploymentResult) protocol.DeploymentResult {
	if res.RequestID != "" {
		return res
	}
	res.Steps = slices.Clone(res.Steps)
	for i, s := range res.Steps {
		if s.Code == "" && s.Outcome != protocol.OutcomeSucceeded && s.Outcome != protocol.OutcomeSkipped {
			res.Steps[i].Code, res.Steps[i].Detail = protocol.CodeLegacy, ""
		}
	}
	if res.Code == "" && res.Outcome != protocol.OutcomeSucceeded {
		res.Code = protocol.CodeLegacy
	}
	return res
}

// PreflightBlockedError names the findings that stopped a plan. A plan never guesses past them.
type PreflightBlockedError struct {
	Blockers []string
	// Services are the planned services that carry blockers of their own.
	Services []BlockedService
}

// BlockedService names a service a refused plan blocked on: its own blockers and the codes a
// live inspection reported, nothing else about it.
type BlockedService struct {
	Name        string            `json:"name"`
	Blockers    []string          `json:"blockers"`
	Unsupported []string          `json:"unsupported,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
}

func (e *PreflightBlockedError) Error() string {
	return "deployment preflight blocked: " + strings.Join(e.Blockers, ",")
}

// PlanDeployment persists an executable preview. It is minted only from a clean preflight and
// records every identity apply must recheck. Services named in r.Update are pinned to the
// registry's current digest: that plan reads, resolves with no transaction open, then writes
// only if imageCheckState is unchanged. It sends no command. It decrypts the revision's values
// and credentials to build the frame apply would send, measures it against r.MaxFrameBytes and
// drops it; nothing secret is stored. See docs/application-schema.md, Deployment plans.
func (t *tenancyStore) PlanDeployment(ctx context.Context, a TenantAccess, app string, r PlanRequest, resolver DigestResolver, key []byte, privateAllowed bool) (*Deployment, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 {
		return nil, ErrInvalid
	}
	if len(r.Update) > 0 && len(r.PinImages) > 0 {
		return nil, ErrInvalid
	}
	// A Kubernetes plan resolves every image at the registry, so it takes the update path. This
	// read only picks the path: both paths re-read the instance under authorization and refuse
	// one whose runtime changed since.
	var namespace string
	if err := t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT namespace FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, id.String()).Scan(&namespace); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	kube := namespace != ""
	if kube && (len(r.PinImages) > 0 || resolver == nil) {
		return nil, ErrInvalid
	}
	if len(r.Update) > 0 {
		sorted := slices.Sorted(slices.Values(r.Update))
		if len(r.Update) > protocol.MaxDeploymentServices || len(slices.Compact(sorted)) != len(r.Update) || resolver == nil {
			return nil, ErrInvalid
		}
	}
	// The plan's request ID is the deployment's correlation ID from here on.
	if a.CorrelationID == "" {
		a.CorrelationID = uuid.NewString()
	}
	planID := uuid.NewString()
	target := id.String() + "/deployments/" + planID
	var out *Deployment
	if len(r.Update) == 0 && !kube {
		err = t.withTenantTarget(ctx, a, permissions.ApplicationDeploy, target, func(tx *sql.Tx) error {
			dr, err := t.draftPlan(ctx, tx, a, id.String(), planID, r, true)
			if err != nil {
				return err
			}
			if dr.m.Runtime != protocol.RuntimeDocker {
				return ErrAdoptionChanged
			}
			if err := blocked(dr.blockers, dr.services...); err != nil {
				return err
			}
			if err := t.checkFrame(ctx, tx, a, dr.d, key, r.MaxFrameBytes); err != nil {
				return err
			}
			out = dr.d
			return t.insertPlan(ctx, tx, a, out, dr.m.InstanceID)
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	var work []imageCheckWork
	var pulled []int // plan service index per work item
	var state string
	var capabilities map[string]bool
	var services []BlockedService
	// A read: no lock and no success row, but a denial or failure audits the plan's target.
	err = t.run(ctx, a, permissions.ApplicationDeploy, &target, nil, false, func(tx *sql.Tx) error {
		dr, err := t.draftPlan(ctx, tx, a, id.String(), planID, r, false)
		if err != nil {
			return err
		}
		d, m, blockers := dr.d, dr.m, dr.blockers
		if (m.Runtime == protocol.RuntimeKubernetes) != kube {
			return ErrAdoptionChanged
		}
		services = dr.services
		capabilities = dr.capabilities
		anonymous, err := t.anonymousPull(ctx, tx, a.OrganizationID)
		if err != nil {
			return err
		}
		updates := r.Update
		if kube {
			updates = nil
			for _, ps := range d.Plan.Services {
				updates = append(updates, ps.Name)
			}
		}
		for _, name := range updates {
			i := slices.IndexFunc(d.Plan.Services, func(ps PlannedService) bool { return ps.Name == name })
			if i < 0 || (!kube && m.Bindings[name] == "") {
				blockers = append(blockers, "update_not_mapped")
				continue
			}
			ref, err := registry.ParseReference(d.Plan.Services[i].Reference)
			if err != nil {
				blockers = append(blockers, "registry_unavailable")
				continue
			}
			reg, cred, err := t.registryFor(ctx, tx, a.OrganizationID, ref.Host, key)
			switch {
			case errors.Is(err, ErrNotFound) && !anonymous:
				blockers = append(blockers, "registry_not_configured")
				continue
			case errors.Is(err, ErrNotFound):
			case err != nil:
				return err
			}
			if ref.Digest != "" {
				pinPull(&d.Plan.Services[i], ref, ref.Digest)
				continue
			}
			w := imageCheckWork{Ref: ref}
			if reg != nil {
				w.Cred, w.AllowPrivate = cred, reg.AllowPrivate && privateAllowed
			}
			work = append(work, w)
			pulled = append(pulled, i)
		}
		if err := blocked(blockers, services...); err != nil {
			return err
		}
		// draftPlan's preflight verified m against the latest revision and live inventory.
		state = imageCheckStateOf(m.Version, m.Preview.Revision, m.Preview.Containers)
		out = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	resolveImageChecks(ctx, resolver, work)
	blockers := []string{}
	for n, w := range work {
		switch {
		case w.Row.Verdict == "registry_error":
			blockers = append(blockers, "registry_"+w.Row.Detail)
		case !validSHA256(w.Row.RemoteDigest):
			blockers = append(blockers, "registry_unavailable")
		default:
			pinPull(&out.Plan.Services[pulled[n]], w.Ref, w.Row.RemoteDigest)
		}
	}
	// Only now is it known which services pull.
	blockers = append(blockers, capabilityBlockers(capabilities, out.Plan)...)
	pulls := 0
	for _, ps := range out.Plan.Services {
		if ps.PullDigest != "" {
			pulls++
		}
	}
	details := fmt.Sprintf("pulls=%d", pulls)
	// Uncancelled, so a plan the client abandoned mid-registry still audits a failure.
	wctx := context.WithoutCancel(ctx)
	err = t.withTenantTargetDetails(wctx, a, permissions.ApplicationDeploy, target, &details, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := t.lockApplication(wctx, tx, a, id.String()); err != nil {
			return err
		}
		current, err := t.imageCheckState(wctx, tx, a, id.String(), out.InstanceID)
		if err != nil {
			return err
		}
		if current != state {
			return ErrAdoptionChanged
		}
		if err := blocked(blockers, services...); err != nil {
			return err
		}
		now := time.Now().UTC()
		out.CreatedAt, out.ExpiresAt = now, now.Add(DeploymentPlanTTL)
		if err := t.checkFrame(wctx, tx, a, out, key, r.MaxFrameBytes); err != nil {
			return err
		}
		return t.insertPlan(wctx, tx, a, out, out.InstanceID)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pinPull points a planned service at the registry digest its update pulls. The canonical host
// is what protocol.ImagePull.Host reads back and what the registry row and frame are keyed by.
func pinPull(ps *PlannedService, ref registry.Reference, digest string) {
	ps.PullReference, ps.PullDigest = ref.Host+"/"+ref.Repository+"@"+digest, digest
}

func blocked(blockers []string, services ...BlockedService) error {
	if len(blockers) == 0 {
		return nil
	}
	slices.Sort(blockers)
	return &PreflightBlockedError{Blockers: slices.Compact(blockers), Services: services}
}

// inspectionBlockers checks a mapped service against the live inspection the API made of its
// container at plan time, recording on row the codes of what a recreate would drop.
func inspectionBlockers(row *PreflightService, inspections map[string]protocol.ContainerInspection) []string {
	in, ok := inspections[row.ContainerID]
	switch {
	case !ok:
		return []string{"inspection_unavailable"}
	case in.Target != *row.InspectionTarget:
		return []string{"replacement_identity_changed"}
	case !in.ConfigurationVerified:
		row.Unsupported = in.Unsupported
		return []string{"configuration_unsupported"}
	}
	return nil
}

// lockApplication takes the application row lock settle, remap and revision append contend on
// (FOR UPDATE on PostgreSQL; SQLite's single writer already serializes). A missing row is
// ErrAdoptionChanged.
func (t *tenancyStore) lockApplication(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) error {
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var one int
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM applications WHERE id=? AND organization_id=? AND environment_id=?`+lock), app, a.OrganizationID, a.EnvironmentID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAdoptionChanged
	}
	return err
}

// draft is a plan built in memory from one preflight, with every blocker found for it and the
// endpoint's capabilities, which the update path checks again once its pulls are pinned.
type draft struct {
	d            *Deployment
	m            *ApplicationMapping
	blockers     []string
	capabilities map[string]bool
	// services are the services with blockers of their own, for the refusal.
	services []BlockedService
}

// draftPlan runs the preflight (locking when the plan is written in the same transaction) and
// builds the plan in memory, returning its blockers unrefused so an update can add its own.
func (t *tenancyStore) draftPlan(ctx context.Context, tx *sql.Tx, a TenantAccess, app, planID string, r PlanRequest, lock bool) (*draft, error) {
	p, m, spec, snapshot, digest, err := t.preflight(ctx, tx, a, app, lock, r.Revision, r.PinImages)
	if err != nil {
		return nil, err
	}
	// An unmatched pin would leave the service it meant resolving its tag.
	for name := range r.PinImages {
		if !slices.ContainsFunc(spec.Services, func(s ApplicationService) bool { return s.Name == name }) {
			return nil, ErrInvalid
		}
	}
	if r.InstanceID != m.InstanceID || r.MappingVersion != m.Version || (r.Revision != 0 && r.Revision != p.Revision) || r.Confirm != m.Preview.Project {
		return nil, ErrAdoptionChanged
	}
	capabilities, err := t.endpointCapabilities(ctx, tx, m.Preview.EndpointID)
	if err != nil {
		return nil, err
	}
	blockers := slices.Clone(p.Blockers)
	plan := DeploymentPlan{Project: m.Preview.Project, Services: []PlannedService{}}
	// The preflight resolved each reference to one full image ID, which is the pin. Record
	// the repository digest beside it only when inventory reported exactly one; it is
	// advisory.
	digests := map[string]string{}
	for _, im := range snapshot.Images {
		if len(im.Digests) == 1 {
			digests[im.ID] = im.Digests[0]
		}
	}
	inspected := inspectsVerdicts(capabilities)
	var objects map[string]string
	var mounts map[string][]protocol.KubernetesMount
	if m.Runtime == protocol.RuntimeKubernetes {
		plan.Namespace = m.Namespace
		objects = protocol.KubernetesNames(m.Preview.Project, spec.serviceNames())
		plan.Claims, mounts = kubernetesClaims(m.Preview.Project, spec)
	}
	var refused []BlockedService
	for i, s := range spec.Services {
		row := p.Services[i]
		// Without both inspect capabilities there was nothing to ask; agent_inspect_unsupported
		// says why. A blocked preflight is not inspected either: its own blockers refuse the plan.
		if inspected && p.Executable && row.InspectionTarget != nil {
			row.Blockers = append(row.Blockers, inspectionBlockers(&row, r.Inspections)...)
		}
		blockers = append(blockers, row.Blockers...)
		if len(row.Blockers) > 0 {
			refused = append(refused, BlockedService{Name: row.Name, Blockers: row.Blockers, Unsupported: row.Unsupported, Details: row.Details})
		}
		refs := make([]string, 0, len(s.Environment))
		for _, ref := range s.Environment {
			refs = append(refs, ref.SecretRef)
		}
		slices.Sort(refs)
		ps := PlannedService{Name: s.Name, Reference: s.Image, ImageID: row.ImageID, ImageDigest: digests[row.ImageID], ContainerID: row.ContainerID, Restart: s.Restart, Ports: s.Ports, SecretRefs: refs, Mounts: row.Mounts}
		if ps.Ports == nil {
			ps.Ports = []ApplicationPort{}
		}
		if len(row.DroppedMounts) > 0 {
			ps.DroppedMounts = row.DroppedMounts
		}
		for _, mount := range row.Mounts {
			if mount.Kind == protocol.MountVolume && !slices.Contains(plan.Volumes, mount.Source) {
				plan.Volumes = append(plan.Volumes, mount.Source)
			}
		}
		if row.InspectionTarget != nil {
			ps.Replaces = *row.InspectionTarget
		}
		if plan.Namespace != "" {
			ps.Object, ps.ClaimMounts = &KubernetesObject{Namespace: plan.Namespace, Name: objects[s.Name]}, mounts[s.Name]
		}
		plan.Services = append(plan.Services, ps)
	}
	blockers = append(blockers, capabilityBlockers(capabilities, plan)...)
	now := time.Now().UTC()
	d := &Deployment{ID: planID, ApplicationID: app, InstanceID: m.InstanceID, EndpointID: m.Preview.EndpointID, EndpointName: m.Preview.EndpointName, Kind: "apply", State: "planned", Revision: p.Revision, SpecDigest: digest, MappingVersion: m.Version, Plan: plan, CreatedBy: a.ActorID, CreatedAt: now, ExpiresAt: now.Add(DeploymentPlanTTL), CorrelationID: a.CorrelationID}
	return &draft{d: d, m: m, blockers: blockers, capabilities: capabilities, services: refused}, nil
}

// endpointCapabilities is what the endpoint's agent advertised at its last connect.
func (t *tenancyStore) endpointCapabilities(ctx context.Context, tx *sql.Tx, endpoint string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT capability FROM endpoint_capabilities WHERE endpoint_id=? ORDER BY capability LIMIT ?`), endpoint, MaxCapabilities)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[c] = true
	}
	return out, rows.Err()
}

// inspectsVerdicts reports an agent whose inspections a plan can use: an agent with
// container.inspect but not container.inspect.verdict predates the verdict and answers with
// configuration_verified false and no codes, which the plan could only call unavailable.
func inspectsVerdicts(capabilities map[string]bool) bool {
	return capabilities[protocol.CapabilityContainerInspect] && capabilities[protocol.CapabilityContainerInspectVerdict]
}

// capabilityBlockers refuses a plan the endpoint's agent could not run: no deployments, no live
// inspection for the plan to check, or a pull without deployment.pull. Apply checks again. A
// Kubernetes plan needs kubernetes.deploy only: the kubelet pulls, and nothing is inspected.
func capabilityBlockers(capabilities map[string]bool, plan DeploymentPlan) []string {
	if plan.Namespace != "" {
		if !capabilities[protocol.CapabilityKubernetesDeploy] {
			return []string{"agent_deploy_unsupported"}
		}
		return nil
	}
	var out []string
	if !capabilities[protocol.CapabilityDeploymentApply] {
		out = append(out, "agent_deploy_unsupported")
	}
	if !inspectsVerdicts(capabilities) {
		out = append(out, "agent_inspect_unsupported")
	}
	if !capabilities[protocol.CapabilityDeploymentPull] && slices.ContainsFunc(plan.Services, func(ps PlannedService) bool { return ps.PullDigest != "" }) {
		out = append(out, "agent_pull_unsupported")
	}
	return out
}

// insertPlan replaces the instance's planned row with d, refusing while one is applying.
func (t *tenancyStore) insertPlan(ctx context.Context, tx *sql.Tx, a TenantAccess, d *Deployment, instance string) error {
	raw, err := json.Marshal(d.Plan)
	if err != nil {
		return err
	}
	if len(raw) > MaxDeploymentPlanBytes {
		return ErrInvalid
	}
	var liveID, liveState string
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,state FROM deployments WHERE organization_id=? AND environment_id=? AND instance_id=? AND state IN ('planned','applying')`), a.OrganizationID, a.EnvironmentID, instance).Scan(&liveID, &liveState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if liveState == "applying" {
		return ErrDeploymentInProgress
	}
	if liveState == "planned" {
		if _, err = tx.ExecContext(ctx, t.store.rebind(`DELETE FROM deployments WHERE id=?`), liveID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO deployments(id,organization_id,environment_id,application_id,instance_id,endpoint_id,project,state,revision,spec_digest,mapping_version,plan,created_by,created_at,expires_at,correlation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), d.ID, a.OrganizationID, a.EnvironmentID, d.ApplicationID, d.InstanceID, d.EndpointID, d.Plan.Project, d.State, d.Revision, d.SpecDigest, d.MappingVersion, string(raw), d.CreatedBy, d.CreatedAt, d.ExpiresAt, d.CorrelationID)
	return err
}

// selectDeployments reads rows aliased d with the endpoint's current name (empty once the
// endpoint is gone) and the deployment's validation; the caller appends the WHERE clause.
const selectDeployments = `SELECT d.id,d.application_id,d.instance_id,d.endpoint_id,COALESCE(e.name,''),d.kind,d.state,d.revision,d.spec_digest,d.mapping_version,d.plan,d.created_by,d.created_at,d.expires_at,d.applied_by,d.applied_at,d.deadline,d.settled_at,d.detail,d.result,d.correlation_id,COALESCE(d.migration_id,''),` + validationColumns + ` FROM deployments d LEFT JOIN endpoints e ON e.id=d.endpoint_id LEFT JOIN deployment_validations v ON v.deployment_id=d.id LEFT JOIN deployments rd ON rd.id=v.rollback_deployment_id `

func scanDeployment(rows interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var raw, result string
	var appliedAt, deadline, settledAt sql.NullTime
	var vs validationScan
	if err := rows.Scan(append([]any{&d.ID, &d.ApplicationID, &d.InstanceID, &d.EndpointID, &d.EndpointName, &d.Kind, &d.State, &d.Revision, &d.SpecDigest, &d.MappingVersion, &raw, &d.CreatedBy, &d.CreatedAt, &d.ExpiresAt, &d.AppliedBy, &appliedAt, &deadline, &settledAt, &d.Detail, &result, &d.CorrelationID, &d.MigrationID}, vs.dest()...)...); err != nil {
		return nil, err
	}
	d.Validation = vs.validation()
	if json.Unmarshal([]byte(raw), &d.Plan) != nil {
		return nil, ErrRevisionCorrupt
	}
	if d.Plan.Services == nil {
		d.Plan.Services = []PlannedService{}
	}
	if appliedAt.Valid {
		d.AppliedAt = &appliedAt.Time
	}
	if deadline.Valid {
		d.Deadline = &deadline.Time
	}
	if settledAt.Valid {
		d.SettledAt = &settledAt.Time
	}
	if result != "" {
		var stored storedDeploymentResult
		if json.Unmarshal([]byte(result), &stored) != nil {
			return nil, ErrRevisionCorrupt
		}
		// A result stored before codes reads as legacy.
		res := legacyResult(protocol.DeploymentResult{Deployment: d.ID, Outcome: d.State, Code: stored.Code, Steps: stored.Steps, Services: stored.Services})
		d.Result = &res
	}
	d.Expired = !time.Now().Before(d.ExpiresAt)
	return &d, nil
}

func (t *tenancyStore) ReadDeployment(ctx context.Context, a TenantAccess, app, id string) (*Deployment, error) {
	appID, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrNotFound
	}
	var out *Deployment
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, err = scanDeployment(tx.QueryRowContext(ctx, t.store.rebind(selectDeployments+`WHERE d.organization_id=? AND d.environment_id=? AND d.application_id=? AND d.id=?`), a.OrganizationID, a.EnvironmentID, appID.String(), parsedID.String()))
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
		rows, err := tx.QueryContext(ctx, t.store.rebind(selectDeployments+`WHERE d.organization_id=? AND d.environment_id=? AND d.application_id=? ORDER BY d.created_at DESC, d.id LIMIT 100`), a.OrganizationID, a.EnvironmentID, appID.String())
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
