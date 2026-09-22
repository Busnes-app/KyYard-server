package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

type DeploymentPreflight struct {
	InstanceID     string             `json:"instance_id"`
	EndpointID     string             `json:"endpoint_id"`
	EndpointName   string             `json:"endpoint_name"`
	Revision       int                `json:"revision"`
	MappingVersion int                `json:"mapping_version"`
	ReceivedAt     time.Time          `json:"received_at"`
	Executable     bool               `json:"executable"`
	Blockers       []string           `json:"blockers"`
	Services       []PreflightService `json:"services"`
}
type PreflightService struct {
	InspectionTarget *protocol.InspectionTarget `json:"inspection_target,omitempty"`
	Name             string                     `json:"name"`
	Reference        string                     `json:"reference"`
	ImageID          string                     `json:"image_id"`
	ContainerID      string                     `json:"container_id"`
	Blockers         []string                   `json:"blockers"`
}

// PreflightApplication is a diagnostic only, never a deploy approval. Inventory
// omits host processes and configuration needed to safely replace a container.
// See docs/application-schema.md, Deployment preflight.
func (t *tenancyStore) PreflightApplication(ctx context.Context, a TenantAccess, app string) (*DeploymentPreflight, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	var out *DeploymentPreflight
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, _, _, _, _, err = t.preflight(ctx, tx, a, id.String(), false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// preflight is the shared diagnostic. lock=true takes the mapping locks for a writer. It also
// returns the mapping, the parsed spec, the parsed snapshot and the revision digest so a writer
// can build a plan from exactly what it checked.
func (t *tenancyStore) preflight(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool) (*DeploymentPreflight, *ApplicationMapping, ApplicationSpec, protocol.Snapshot, string, error) {
	m, err := t.applicationMapping(ctx, tx, a, app, lock)
	if err != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
	}
	var raw, specRaw, digest, instance, state string
	var version, head int
	var received, observed time.Time
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.id,i.mapping_version,a.latest_revision,r.spec,r.digest,e.state,v.snapshot,v.received_at,v.observed_at FROM applications a JOIN application_instances i ON i.application_id=a.id JOIN application_revisions r ON r.application_id=a.id AND r.number=a.latest_revision JOIN endpoints e ON e.id=i.endpoint_id JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE a.organization_id=? AND a.environment_id=? AND a.id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&instance, &version, &head, &specRaw, &digest, &state, &raw, &received, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
	}
	if err != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
	}
	if instance != m.InstanceID || version != m.Version || head != m.Preview.Revision || state != "active" || time.Since(received) > 3*time.Minute || time.Until(received) > time.Minute || time.Since(observed) > 5*time.Minute || time.Until(observed) > 5*time.Minute {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
	}
	var spec ApplicationSpec
	var snapshot protocol.Snapshot
	if applicationSpecDigest([]byte(specRaw)) != digest || json.Unmarshal([]byte(specRaw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrRevisionCorrupt
	}
	if json.Unmarshal([]byte(raw), &snapshot) != nil || len(snapshot.Containers) > protocol.MaxContainers || snapshot.Engine.Version == "" {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
	}
	for _, part := range snapshot.Truncated {
		if part == "containers" {
			return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
		}
	}
	current := map[string]protocol.Container{}
	for _, c := range snapshot.Containers {
		if _, found := current[c.ID]; found {
			return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
		}
		current[c.ID] = c
	}
	for _, c := range m.Preview.Containers {
		now, found := current[c.ID]
		if !found || now.ImageID != c.ImageID || now.CreatedAt.UnixMicro() != c.CreatedAt.UnixMicro() || now.ComposeProject != m.Preview.Project {
			return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
		}
	}
	out := buildDeploymentPreflight(m, spec, snapshot)
	out.ReceivedAt = received
	return out, m, spec, snapshot, digest, nil
}

func buildDeploymentPreflight(m *ApplicationMapping, spec ApplicationSpec, snapshot protocol.Snapshot) *DeploymentPreflight {
	out := &DeploymentPreflight{InstanceID: m.InstanceID, EndpointID: m.Preview.EndpointID, EndpointName: m.Preview.EndpointName, Revision: m.Preview.Revision, MappingVersion: m.Version, Blockers: []string{"runtime_verification_required"}, Services: []PreflightService{}}
	if m.Version == 0 || m.MappedRevision != m.Preview.Revision {
		out.Blockers = append(out.Blockers, "mapping_requires_review")
	}
	if len(m.Bindings) != len(m.Preview.Containers) {
		out.Blockers = append(out.Blockers, "unassigned_adopted_containers")
	}
	imagesComplete := len(snapshot.Images) <= protocol.MaxImages
	for _, part := range snapshot.Truncated {
		if part == "images" {
			imagesComplete = false
		}
	}
	if !imagesComplete {
		out.Blockers = append(out.Blockers, "image_inventory_incomplete")
	}
	images := map[string]map[string]bool{}
	if imagesComplete {
		for _, im := range snapshot.Images {
			refs := append([]string{im.ID}, im.Tags...)
			refs = append(refs, im.Digests...)
			for _, ref := range refs {
				if images[ref] == nil {
					images[ref] = map[string]bool{}
				}
				images[ref][im.ID] = true
			}
		}
	}
	// Indexed by protocol and published port: no services x containers x ports scan.
	occupied := map[portKey]map[string]bool{}
	assigned := map[string]bool{}
	for _, id := range m.Bindings {
		assigned[id] = true
	}
	for _, c := range snapshot.Containers {
		if assigned[c.ID] {
			continue
		}
		for _, p := range c.Ports {
			if p.Host > 0 {
				addBinding(occupied, portKey{p.Host, p.Protocol}, p.HostIP)
			}
		}
	}
	owned := make(map[string]protocol.InspectionTarget, len(m.Preview.Containers))
	for _, c := range m.Preview.Containers {
		target := protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
		if target.Validate() == nil {
			owned[c.ID] = target
		}
	}
	desired := map[portKey]map[string]bool{}
	for _, s := range spec.Services {
		row := PreflightService{Name: s.Name, Reference: s.Image, ContainerID: m.Bindings[s.Name], Blockers: []string{}}
		if target, ok := owned[row.ContainerID]; ok {
			row.InspectionTarget = &target
		}
		if row.ContainerID == "" {
			row.Blockers = append(row.Blockers, "service_unmapped")
		} else if row.InspectionTarget == nil {
			row.Blockers = append(row.Blockers, "replacement_identity_invalid")
		}
		_, tag := protocol.SplitImageReference(s.Image)
		switch {
		case tag == "":
			row.Blockers = append(row.Blockers, "explicit_image_reference_required")
		case !imagesComplete:
			row.Blockers = append(row.Blockers, "image_inventory_incomplete")
		case len(images[s.Image]) == 0:
			row.Blockers = append(row.Blockers, "image_not_reported")
		case len(images[s.Image]) != 1:
			row.Blockers = append(row.Blockers, "image_reference_ambiguous")
		default:
			for id := range images[s.Image] {
				if fullImageID(id) {
					row.ImageID = id
				} else {
					row.Blockers = append(row.Blockers, "image_identity_invalid")
				}
			}
		}
		external, duplicate := false, false
		for _, p := range s.Ports {
			k := portKey{p.Published, p.Protocol}
			external = external || overlapsBinding(occupied, k, p.HostIP)
			duplicate = duplicate || overlapsBinding(desired, k, p.HostIP)
			addBinding(desired, k, p.HostIP)
		}
		if external {
			row.Blockers = append(row.Blockers, "reported_port_overlap")
		}
		if duplicate {
			row.Blockers = append(row.Blockers, "desired_port_overlap")
		}
		out.Services = append(out.Services, row)
	}
	return out
}
func fullImageID(id string) bool {
	if !strings.HasPrefix(id, "sha256:") {
		return false
	}
	raw := strings.TrimPrefix(id, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == raw
}

type portKey struct {
	port     int
	protocol string
}

func bindingAddress(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.IsUnspecified() {
		return "*"
	}
	return a.Unmap().String()
}
func addBinding(index map[portKey]map[string]bool, key portKey, ip string) {
	if index[key] == nil {
		index[key] = map[string]bool{}
	}
	index[key][bindingAddress(ip)] = true
}
func overlapsBinding(index map[portKey]map[string]bool, key portKey, ip string) bool {
	set := index[key]
	addr := bindingAddress(ip)
	return set["*"] || set[addr] || (addr == "*" && len(set) > 0)
}
