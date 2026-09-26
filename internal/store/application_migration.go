package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// Migration statuses. A migration is open until cutover_confirmed or abandoned, and a source
// has at most one open migration. See docs/application-schema.md, Migration.
const (
	MigrationAnalyzed           = "analyzed"
	MigrationDestinationCreated = "destination_created"
	MigrationValidated          = "validated"
	MigrationCutoverConfirmed   = "cutover_confirmed"
	MigrationAbandoned          = "abandoned"
	MaxMigrationReportBytes     = 65536
	MaxMigrationNoteRunes       = 500
)

var (
	ErrMigrationOpen        = errors.New("the application has an open migration")
	ErrMigrationNotReady    = errors.New("the migration report is not ready")
	ErrMigrationState       = errors.New("the migration's status does not allow this")
	ErrMigrationStale       = errors.New("the source definition changed since the analysis")
	ErrStorageClassUnknown  = errors.New("the destination reports no such StorageClass")
	ErrSizeInvalid          = errors.New("invalid claim size")
	ErrVolumeUnknown        = errors.New("the source mounts no such named volume")
	ErrApplicationNameTaken = errors.New("the destination's name is taken")
)

// ApplicationMigration is one migration of a Docker source to a destination application on a
// cluster. Report is the analyzer's JSON, stored as produced. Role says which end the
// application it was read for is.
type ApplicationMigration struct {
	ID                         string              `json:"id"`
	ApplicationID              string              `json:"application_id"`
	ApplicationName            string              `json:"application_name"`
	DestinationApplicationID   string              `json:"destination_application_id,omitempty"`
	DestinationApplicationName string              `json:"destination_application_name,omitempty"`
	DestinationEndpointID      string              `json:"destination_endpoint_id"`
	Namespace                  string              `json:"namespace"`
	Status                     string              `json:"status"`
	SourceRevision             int                 `json:"source_revision"`
	Ready                      bool                `json:"ready"`
	Report                     json.RawMessage     `json:"report"`
	Choices                    KubernetesExtension `json:"choices"`
	CreatedBy                  string              `json:"created_by"`
	CreatedAt                  time.Time           `json:"created_at"`
	UpdatedAt                  time.Time           `json:"updated_at"`
	ValidatedBy                string              `json:"validated_by,omitempty"`
	ValidatedAt                *time.Time          `json:"validated_at,omitempty"`
	ValidatedNote              string              `json:"validated_note,omitempty"`
	ConfirmedBy                string              `json:"confirmed_by,omitempty"`
	ConfirmedAt                *time.Time          `json:"confirmed_at,omitempty"`
	CutoverNote                string              `json:"cutover_note,omitempty"`
	Role                       string              `json:"role"`
}

// MigrationStart names the cluster and namespace a migration targets.
type MigrationStart struct {
	DestinationEndpointID string `json:"destination_endpoint_id"`
	Namespace             string `json:"namespace"`
}

// MigrationAnalysis is what the analyzer produced for revision Revision of the source.
type MigrationAnalysis struct {
	Revision int
	Report   []byte
	Ready    bool
}

// MigrationSource is everything the analyzer reads: the source's latest revision, its project
// and mapped containers by service from the endpoint's fresh inventory, the endpoint's volumes,
// and the destination.
type MigrationSource struct {
	ApplicationName string
	Revision        int
	Spec            ApplicationSpec
	Project         string
	EndpointID      string
	Containers      map[string]protocol.Container
	Volumes         []protocol.Volume
	Destination     MigrationDestination
}

// MigrationDestination is the cluster side: its namespaces and StorageClasses, and the name and
// project the destination application takes.
type MigrationDestination struct {
	EndpointID     string
	Namespaces     []string
	StorageClasses []protocol.StorageClass
	Name           string
	Project        string
}

const openMigration = `status NOT IN ('cutover_confirmed','abandoned')`

const selectMigration = `SELECT m.id,m.application_id,s.name,COALESCE(m.destination_application_id,''),COALESCE(d.name,''),m.destination_endpoint_id,m.namespace,m.status,m.source_revision,m.ready,m.report,m.choices,m.created_by,m.created_at,m.updated_at,m.validated_by,m.validated_at,m.validated_note,m.confirmed_by,m.confirmed_at,m.cutover_note FROM application_migrations m JOIN applications s ON s.id=m.application_id LEFT JOIN applications d ON d.id=m.destination_application_id `

func scanMigration(row interface{ Scan(...any) error }) (*ApplicationMigration, error) {
	var m ApplicationMigration
	var report, choices string
	var ready int
	var validated, confirmed sql.NullTime
	if err := row.Scan(&m.ID, &m.ApplicationID, &m.ApplicationName, &m.DestinationApplicationID, &m.DestinationApplicationName, &m.DestinationEndpointID, &m.Namespace, &m.Status, &m.SourceRevision, &ready, &report, &choices, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.ValidatedBy, &validated, &m.ValidatedNote, &m.ConfirmedBy, &confirmed, &m.CutoverNote); err != nil {
		return nil, err
	}
	if !json.Valid([]byte(report)) || json.Unmarshal([]byte(choices), &m.Choices) != nil {
		return nil, ErrRevisionCorrupt
	}
	if m.Choices.Volumes == nil {
		m.Choices.Volumes = map[string]KubernetesVolume{}
	}
	m.Ready, m.Report = ready == 1, json.RawMessage(report)
	if validated.Valid {
		m.ValidatedAt = &validated.Time
	}
	if confirmed.Valid {
		m.ConfirmedAt = &confirmed.Time
	}
	return &m, nil
}

// migrationApp parses an application ID and refuses an access without an environment.
func migrationApp(a TenantAccess, app string) (string, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return "", ErrInvalid
	}
	return id.String(), nil
}

// lockSource takes the source application's row lock, the order every deployment writer takes,
// and returns its latest revision.
func (t *tenancyStore) lockSource(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) (int, error) {
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var head int
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, app).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return head, err
}

// dockerSource refuses a source that is not adopted (ErrMappingRequired) or not on Docker.
func (t *tenancyStore) dockerSource(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) error {
	var namespace, runtime string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.namespace,e.runtime FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&namespace, &runtime)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrMappingRequired
	case err != nil:
		return err
	case namespace != "" || runtime != protocol.RuntimeDocker:
		return ErrRuntimeUnsupported
	}
	return nil
}

// migrationDestination reads a Kubernetes endpoint's namespaces and reported StorageClasses and
// the name the destination of app would take on it.
func (t *tenancyStore) migrationDestination(ctx context.Context, tx *sql.Tx, a TenantAccess, sourceName, endpoint string) (MigrationDestination, error) {
	d := MigrationDestination{EndpointID: endpoint, StorageClasses: []protocol.StorageClass{}}
	var runtime, name, namespaces string
	var snapshot sql.NullString
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.runtime,e.name,e.deploy_namespaces,v.snapshot FROM endpoints e LEFT JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE e.organization_id=? AND e.environment_id=? AND e.id=?`), a.OrganizationID, a.EnvironmentID, endpoint).Scan(&runtime, &name, &namespaces, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if runtime != protocol.RuntimeKubernetes {
		return d, ErrRuntimeUnsupported
	}
	d.Namespaces = decodeNamespaces(namespaces)
	var s protocol.Snapshot
	if snapshot.Valid && json.Unmarshal([]byte(snapshot.String), &s) == nil && s.Kubernetes != nil && s.Kubernetes.StorageClasses != nil {
		d.StorageClasses = s.Kubernetes.StorageClasses
	}
	d.Name = sourceName + " on " + name
	d.Project = KubernetesProject(d.Name)
	return d, nil
}

// revisionSpec reads and verifies one revision's spec.
func (t *tenancyStore) revisionSpec(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, number int) (ApplicationSpec, error) {
	var raw, digest string
	var spec ApplicationSpec
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, app, number).Scan(&raw, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return spec, ErrNotFound
	}
	if err != nil {
		return spec, err
	}
	if applicationSpecDigest([]byte(raw)) != digest || json.Unmarshal([]byte(raw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return spec, ErrRevisionCorrupt
	}
	return spec, nil
}

// ReadMigrationSource gathers the analyzer's input for app against the cluster endpoint, under
// application.migrate: the source must be adopted on a Docker endpoint whose inventory is fresh.
func (t *tenancyStore) ReadMigrationSource(ctx context.Context, a TenantAccess, app, endpoint string) (*MigrationSource, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	var out *MigrationSource
	err = t.readTenant(ctx, a, permissions.ApplicationMigrate, func(tx *sql.Tx) error {
		if err := t.dockerSource(ctx, tx, a, app); err != nil {
			return err
		}
		m, err := t.applicationMapping(ctx, tx, a, app, false)
		if err != nil {
			return err
		}
		spec, err := t.revisionSpec(ctx, tx, a, app, m.Preview.Revision)
		if err != nil {
			return err
		}
		var state, raw string
		var received, observed time.Time
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.state,v.snapshot,v.received_at,v.observed_at FROM endpoints e JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE e.organization_id=? AND e.environment_id=? AND e.id=?`), a.OrganizationID, a.EnvironmentID, m.Preview.EndpointID).Scan(&state, &raw, &received, &observed); err != nil {
			return err
		}
		snapshot, current, err := freshInventory(state, raw, received, observed)
		if err != nil {
			return err
		}
		dest, err := t.migrationDestination(ctx, tx, a, m.Preview.ApplicationName, endpoint)
		if err != nil {
			return err
		}
		out = &MigrationSource{ApplicationName: m.Preview.ApplicationName, Revision: m.Preview.Revision, Spec: spec, Project: m.Preview.Project, EndpointID: m.Preview.EndpointID, Containers: map[string]protocol.Container{}, Volumes: snapshot.Volumes, Destination: dest}
		if out.Volumes == nil {
			out.Volumes = []protocol.Volume{}
		}
		for service, id := range m.Bindings {
			out.Containers[service] = current[id]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkAnalysis bounds a report before it is stored.
func checkAnalysis(an MigrationAnalysis) error {
	if an.Revision < 1 || an.Revision > MaxApplicationRevisions || len(an.Report) < 2 || len(an.Report) > MaxMigrationReportBytes || !json.Valid(an.Report) {
		return ErrInvalid
	}
	return nil
}

// CreateMigration starts app's migration to a namespace of a cluster with its first analysis.
// The source must be adopted on Docker, the namespace one the cluster's manifest grants, and the
// source may have no other open migration.
func (t *tenancyStore) CreateMigration(ctx context.Context, a TenantAccess, app string, start MigrationStart, an MigrationAnalysis) (*ApplicationMigration, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	if err := checkAnalysis(an); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	var out *ApplicationMigration
	err = t.withTenantTarget(ctx, a, permissions.ApplicationMigrate, app+"/migration/"+id, func(tx *sql.Tx) error {
		head, err := t.lockSource(ctx, tx, a, app)
		if err != nil {
			return err
		}
		if err := t.dockerSource(ctx, tx, a, app); err != nil {
			return err
		}
		if an.Revision != head {
			return ErrMigrationStale
		}
		dest, err := t.migrationDestination(ctx, tx, a, "", start.DestinationEndpointID)
		if err != nil {
			return err
		}
		if !slices.Contains(dest.Namespaces, start.Namespace) {
			return ErrNamespaceUnknown
		}
		var open int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM application_migrations WHERE organization_id=? AND environment_id=? AND application_id=? AND `+openMigration), a.OrganizationID, a.EnvironmentID, app).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return ErrMigrationOpen
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_migrations(id,organization_id,environment_id,application_id,destination_endpoint_id,namespace,status,source_revision,ready,report,choices,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), id, a.OrganizationID, a.EnvironmentID, app, start.DestinationEndpointID, start.Namespace, MigrationAnalyzed, an.Revision, boolInt(an.Ready), string(an.Report), `{}`, a.ActorID, now, now); err != nil {
			return err
		}
		out, err = t.migrationByID(ctx, tx, a, id, "source")
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (t *tenancyStore) migrationByID(ctx context.Context, tx *sql.Tx, a TenantAccess, id, role string) (*ApplicationMigration, error) {
	m, err := scanMigration(tx.QueryRowContext(ctx, t.store.rebind(selectMigration+`WHERE m.organization_id=? AND m.environment_id=? AND m.id=?`), a.OrganizationID, a.EnvironmentID, id))
	if err != nil {
		return nil, err
	}
	m.Role = role
	return m, nil
}

// openMigrationOf reads app's open migration as its source, ErrNotFound when it has none.
func (t *tenancyStore) openMigrationOf(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) (*ApplicationMigration, error) {
	m, err := scanMigration(tx.QueryRowContext(ctx, t.store.rebind(selectMigration+`WHERE m.organization_id=? AND m.environment_id=? AND m.application_id=? AND m.`+openMigration), a.OrganizationID, a.EnvironmentID, app))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Role = "source"
	return m, nil
}

// ReadMigration returns app's open migration when app is its source, else the migration that
// created app as its destination, whatever its status; ErrNotFound when there is neither.
func (t *tenancyStore) ReadMigration(ctx context.Context, a TenantAccess, app string) (*ApplicationMigration, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	var out *ApplicationMigration
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, err = t.openMigrationOf(ctx, tx, a, app)
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		out, err = scanMigration(tx.QueryRowContext(ctx, t.store.rebind(selectMigration+`WHERE m.organization_id=? AND m.environment_id=? AND m.destination_application_id=? ORDER BY m.created_at DESC LIMIT 1`), a.OrganizationID, a.EnvironmentID, app))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err == nil {
			out.Role = "destination"
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// changeMigration runs op on app's open migration under application.migrate, locked behind the
// application row and audited on <app>/migration/<id>.
func (t *tenancyStore) changeMigration(ctx context.Context, a TenantAccess, app string, op func(tx *sql.Tx, head int, m *ApplicationMigration) error) (*ApplicationMigration, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	target := app + "/migration"
	var out *ApplicationMigration
	err = t.run(ctx, a, permissions.ApplicationMigrate, &target, nil, true, func(tx *sql.Tx) error {
		head, err := t.lockSource(ctx, tx, a, app)
		if err != nil {
			return err
		}
		m, err := t.openMigrationOf(ctx, tx, a, app)
		if err != nil {
			return err
		}
		target = app + "/migration/" + m.ID
		if err := op(tx, head, m); err != nil {
			return err
		}
		out, err = t.migrationByID(ctx, tx, a, m.ID, "source")
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AnalyzeMigration replaces an analyzed migration's choices and report together: the choices
// name named volumes of the analyzed revision, each with a StorageClass the destination reports
// (or "" while it reports a default) and a valid size.
func (t *tenancyStore) AnalyzeMigration(ctx context.Context, a TenantAccess, app string, choices KubernetesExtension, an MigrationAnalysis) (*ApplicationMigration, error) {
	if err := checkAnalysis(an); err != nil {
		return nil, err
	}
	if len(choices.Volumes) > protocol.MaxKubernetesClaims {
		return nil, ErrInvalid
	}
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, head int, m *ApplicationMigration) error {
		if m.Status != MigrationAnalyzed {
			return ErrMigrationState
		}
		if an.Revision != head {
			return ErrMigrationStale
		}
		spec, err := t.revisionSpec(ctx, tx, a, m.ApplicationID, head)
		if err != nil {
			return err
		}
		dest, err := t.migrationDestination(ctx, tx, a, "", m.DestinationEndpointID)
		if err != nil {
			return err
		}
		if err := checkChoices(spec, dest.StorageClasses, choices); err != nil {
			return err
		}
		if choices.Volumes == nil {
			choices.Volumes = map[string]KubernetesVolume{}
		}
		raw, err := json.Marshal(choices)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE application_migrations SET choices=?,report=?,ready=?,source_revision=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`), string(raw), string(an.Report), boolInt(an.Ready), an.Revision, time.Now().UTC(), a.OrganizationID, a.EnvironmentID, m.ID)
		return err
	})
}

// checkChoices holds each choice to a named volume of spec and to the destination's classes.
func checkChoices(spec ApplicationSpec, classes []protocol.StorageClass, choices KubernetesExtension) error {
	named := spec.namedVolumes()
	for name, v := range choices.Volumes {
		if !slices.Contains(named, name) {
			return ErrVolumeUnknown
		}
		if _, ok := protocol.StorageSizeBytes(v.Size); !ok {
			return ErrSizeInvalid
		}
		known := slices.ContainsFunc(classes, func(c protocol.StorageClass) bool {
			return c.Name == v.StorageClass || (v.StorageClass == "" && c.Default)
		})
		if !protocol.ValidStorageClass(v.StorageClass) || !known {
			return ErrStorageClassUnknown
		}
		if v.AccessMode != protocol.AccessReadWriteOnce {
			return ErrInvalid
		}
	}
	return nil
}

// CreateMigrationDestination creates the destination application "<name> on <endpoint>": revision
// 1 is the analyzed revision with the storage choices, its values are the analyzed revision's
// sealed afresh for the new application, and it is mapped to the migration's namespace. The
// migration must be analyzed and ready, and the source must still be at the analyzed revision.
// An audit row on each application records it.
func (t *tenancyStore) CreateMigrationDestination(ctx context.Context, a TenantAccess, app string, key []byte) (*ApplicationMigration, error) {
	if len(key) != 32 {
		return nil, ErrInvalid
	}
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, head int, m *ApplicationMigration) error {
		switch {
		case m.Status != MigrationAnalyzed:
			return ErrMigrationState
		case !m.Ready:
			return ErrMigrationNotReady
		case m.SourceRevision != head:
			return ErrMigrationStale
		}
		if err := t.dockerSource(ctx, tx, a, m.ApplicationID); err != nil {
			return err
		}
		spec, values, _, err := t.resolveApplicationValues(ctx, tx, a, m.ApplicationID, m.SourceRevision, key)
		if err != nil {
			return err
		}
		dest, err := t.migrationDestination(ctx, tx, a, m.ApplicationName, m.DestinationEndpointID)
		if err != nil {
			return err
		}
		if !slices.Contains(dest.Namespaces, m.Namespace) {
			return ErrNamespaceUnknown
		}
		var taken int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT (SELECT COUNT(*) FROM applications WHERE organization_id=? AND environment_id=? AND name=?)+(SELECT COUNT(*) FROM application_instances WHERE organization_id=? AND environment_id=? AND endpoint_id=? AND project=?)`), a.OrganizationID, a.EnvironmentID, dest.Name, a.OrganizationID, a.EnvironmentID, dest.EndpointID, dest.Project).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return ErrApplicationNameTaken
		}
		spec.Kubernetes = nil
		if len(m.Choices.Volumes) > 0 {
			spec.Kubernetes = &KubernetesExtension{Volumes: m.Choices.Volumes}
		}
		now := time.Now().UTC()
		created := Application{ID: uuid.NewString(), OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, Name: dest.Name, LatestRevision: 1, CreatedBy: a.ActorID, CreatedAt: now}
		if err := t.insertApplication(ctx, tx, a, created, spec, values, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_instances(id,organization_id,environment_id,application_id,endpoint_id,project,revision,created_by,created_at,mapping_version,mapped_revision,namespace) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), uuid.NewString(), a.OrganizationID, a.EnvironmentID, created.ID, dest.EndpointID, dest.Project, 1, a.ActorID, now, 1, 1, m.Namespace); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_migrations SET destination_application_id=?,status=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`), created.ID, MigrationDestinationCreated, now, a.OrganizationID, a.EnvironmentID, m.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), a.ActorID, string(permissions.ApplicationMigrate), created.ID+"/migration/"+m.ID, "source="+m.ApplicationID, a.IPAddress, now, "organization", a.OrganizationID, a.EnvironmentID, a.CorrelationID, "success")
		return err
	})
}

// validNote is 1..MaxMigrationNoteRunes characters of printable text, not only spaces.
func validNote(note string) bool {
	n := utf8.RuneCountInString(note)
	return utf8.ValidString(note) && n >= 1 && n <= MaxMigrationNoteRunes && strings.TrimSpace(note) != "" && !strings.ContainsFunc(note, func(r rune) bool {
		return !unicode.IsPrint(r) && r != ' '
	})
}

// ConfirmMigration records an operator's confirmation with its note: "validated" after the
// destination is created, "cutover" after validation, which closes the migration.
func (t *tenancyStore) ConfirmMigration(ctx context.Context, a TenantAccess, app, step, note string) (*ApplicationMigration, error) {
	if !validNote(note) || (step != "validated" && step != "cutover") {
		return nil, ErrInvalid
	}
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, _ int, m *ApplicationMigration) error {
		now := time.Now().UTC()
		query, from := `UPDATE application_migrations SET status=?,validated_by=?,validated_at=?,validated_note=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`, MigrationDestinationCreated
		to := MigrationValidated
		if step == "cutover" {
			query, from, to = `UPDATE application_migrations SET status=?,confirmed_by=?,confirmed_at=?,cutover_note=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`, MigrationValidated, MigrationCutoverConfirmed
		}
		if m.Status != from {
			return ErrMigrationState
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(query), to, a.ActorID, now, note, now, a.OrganizationID, a.EnvironmentID, m.ID)
		return err
	})
}

// AbandonMigration closes app's open migration. A destination it created stays, with its
// deployments; nothing on either runtime changes.
func (t *tenancyStore) AbandonMigration(ctx context.Context, a TenantAccess, app string) (*ApplicationMigration, error) {
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, _ int, m *ApplicationMigration) error {
		_, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_migrations SET status=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`), MigrationAbandoned, time.Now().UTC(), a.OrganizationID, a.EnvironmentID, m.ID)
		return err
	})
}
