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

type ApplicationMapping struct {
	InstanceID     string            `json:"instance_id"`
	Version        int               `json:"version"`
	MappedRevision int               `json:"mapped_revision"`
	Preview        *AdoptionPreview  `json:"preview"`
	Services       []string          `json:"services"`
	Bindings       map[string]string `json:"bindings"`
	// Runtime is the endpoint's. A Kubernetes mapping names its Namespace and the namespaces
	// the cluster's manifest grants; it has no containers and no bindings.
	Runtime          string   `json:"runtime"`
	Namespace        string   `json:"namespace,omitempty"`
	DeployNamespaces []string `json:"deploy_namespaces,omitempty"`
}

// MappingRequest binds services to adopted containers (Docker), or, with EndpointID and
// Namespace and nothing else, maps the application to a namespace of a Kubernetes endpoint.
type MappingRequest struct {
	InstanceID string            `json:"instance_id"`
	Version    int               `json:"version"`
	Digest     string            `json:"digest"`
	Confirm    string            `json:"confirm"`
	Bindings   map[string]string `json:"bindings"`
	EndpointID string            `json:"endpoint_id"`
	Namespace  string            `json:"namespace"`
}

// A mapping assigns desired service names only to already adopted, unchanged IDs.
// It grants no runtime authority. See docs/application-schema.md, Service mapping.
func (t *tenancyStore) applicationMapping(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool) (*ApplicationMapping, error) {
	out := &ApplicationMapping{Services: []string{}, Bindings: map[string]string{}}
	var endpoint, project, namespaces string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.id,i.endpoint_id,i.project,i.mapping_version,i.mapped_revision,i.namespace,e.runtime,e.deploy_namespaces FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&out.InstanceID, &endpoint, &project, &out.Version, &out.MappedRevision, &out.Namespace, &out.Runtime, &namespaces)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// An instance's shape must be its endpoint's runtime's: containers on Docker, a namespace
	// on Kubernetes.
	if (out.Runtime == protocol.RuntimeKubernetes) != (out.Namespace != "") {
		return nil, ErrRuntimeUnsupported
	}
	if out.Runtime == protocol.RuntimeKubernetes {
		out.DeployNamespaces = decodeNamespaces(namespaces)
		if out.Preview, err = t.kubernetesPreview(ctx, tx, a, app, endpoint, project, lock); err != nil {
			return nil, err
		}
		return out, t.mappedServices(ctx, tx, a, app, out)
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
	return out, t.mappedServices(ctx, tx, a, app, out)
}

// mappedServices lists the services of the revision out's preview names.
func (t *tenancyStore) mappedServices(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, out *ApplicationMapping) error {
	var raw, digest string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE application_id=? AND number=? AND organization_id=? AND environment_id=?`), app, out.Preview.Revision, a.OrganizationID, a.EnvironmentID).Scan(&raw, &digest)
	if err != nil {
		return err
	}
	var spec ApplicationSpec
	if applicationSpecDigest([]byte(raw)) != digest || json.Unmarshal([]byte(raw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return ErrRevisionCorrupt
	}
	for _, s := range spec.Services {
		out.Services = append(out.Services, s.Name)
	}
	return nil
}

// kubernetesPreview is adoptionPreview for a Kubernetes instance: the application and endpoint
// it maps, taking the same locks in the same order, with no containers and no digest.
func (t *tenancyStore) kubernetesPreview(ctx context.Context, tx *sql.Tx, a TenantAccess, app, endpoint, project string, lock bool) (*AdoptionPreview, error) {
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
	if lock {
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE endpoints SET name=name WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, endpoint); err != nil {
			return nil, err
		}
	}
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT name FROM endpoints WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, endpoint).Scan(&p.EndpointName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// KubernetesProject is the project a Kubernetes instance takes from its application's name:
// lower-case letters and digits, other runs as one '-', at most 63 characters, "app" when
// nothing is left. It names the instance's objects and labels.
func KubernetesProject(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > protocol.MaxKubeObjectName {
		out = strings.TrimRight(out[:protocol.MaxKubeObjectName], "-")
	}
	if out == "" {
		return "app"
	}
	return out
}

// mapKubernetes creates the application's instance on a Kubernetes endpoint, or moves it to
// another listed namespace while nothing has been applied there. Every call is a review: the
// mapping version rises and the mapped revision becomes the latest.
func (t *tenancyStore) mapKubernetes(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, r MappingRequest) error {
	if r.InstanceID != "" || r.Version != 0 || r.Digest != "" || r.Confirm != "" || len(r.Bindings) > 0 || r.EndpointID == "" {
		return ErrInvalid
	}
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var name string
	var head int
	if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT name,latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, app).Scan(&name, &head); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var runtime, raw string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT runtime,deploy_namespaces FROM endpoints WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, r.EndpointID).Scan(&runtime, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if runtime != protocol.RuntimeKubernetes {
		return ErrRuntimeUnsupported
	}
	if !slices.Contains(decodeNamespaces(raw), r.Namespace) {
		return ErrNamespaceUnknown
	}
	var instance, endpoint, namespace string
	var current int
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,endpoint_id,namespace,current_revision FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&instance, &endpoint, &namespace, &current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_instances(id,organization_id,environment_id,application_id,endpoint_id,project,revision,created_by,created_at,mapping_version,mapped_revision,namespace) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), uuid.NewString(), a.OrganizationID, a.EnvironmentID, app, r.EndpointID, KubernetesProject(name), head, a.ActorID, time.Now().UTC(), 1, head, r.Namespace)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case endpoint != r.EndpointID || namespace == "" || (namespace != r.Namespace && current > 0):
		// Mapped elsewhere, adopted on Docker, or applied in the namespace it would leave.
		return ErrApplicationAdopted
	default:
		var applying int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployments WHERE organization_id=? AND environment_id=? AND instance_id=? AND state='applying'`), a.OrganizationID, a.EnvironmentID, instance).Scan(&applying); err != nil {
			return err
		}
		if applying > 0 {
			return ErrDeploymentInProgress
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_instances SET namespace=?,mapping_version=mapping_version+1,mapped_revision=? WHERE id=?`), r.Namespace, head, instance); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE applications SET removed_at=NULL WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, app)
	return err
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
		if r.EndpointID != "" || r.Namespace != "" {
			return t.mapKubernetes(ctx, tx, a, id.String(), r)
		}
		p, err := t.applicationMapping(ctx, tx, a, id.String(), true)
		if err != nil {
			return err
		}
		if p.Runtime != protocol.RuntimeDocker {
			return ErrRuntimeUnsupported
		}
		if r.InstanceID != p.InstanceID || r.Version != p.Version || r.Digest != p.Preview.Digest || r.Confirm != p.Preview.Project {
			return ErrAdoptionChanged
		}
		// A running deployment rebinds these resources when it settles.
		var applying int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM deployments WHERE organization_id=? AND environment_id=? AND instance_id=? AND state='applying'`), a.OrganizationID, a.EnvironmentID, p.InstanceID).Scan(&applying); err != nil {
			return err
		}
		if applying > 0 {
			return ErrDeploymentInProgress
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
