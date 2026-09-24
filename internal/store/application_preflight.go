package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"slices"
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
	// Mounts is the definition's volumes resolved to host names. DroppedMounts are the mapped
	// container's mounts a recreate leaves off; DroppedBinds are the binds among them.
	// UnsupportedMounts are the container's mounts the agent refuses to replace.
	Mounts            []protocol.Mount `json:"mounts"`
	DroppedBinds      []protocol.Mount `json:"dropped_binds"`
	DroppedMounts     []protocol.Mount `json:"dropped_mounts"`
	UnsupportedMounts []protocol.Mount `json:"unsupported_mounts"`
}

// anonymousVolume is the name Docker gives a volume nobody named.
var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PreflightApplication reports whether a plan may be minted; it approves nothing. Inventory
// omits host processes and configuration needed to safely replace a container.
// See docs/application-schema.md, Deployment preflight.
func (t *tenancyStore) PreflightApplication(ctx context.Context, a TenantAccess, app string) (*DeploymentPreflight, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	var out *DeploymentPreflight
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, _, _, _, _, err = t.preflight(ctx, tx, a, id.String(), false, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// preflight is the shared diagnostic. lock=true takes the mapping locks for a writer. revision
// picks a saved revision to check, 0 the latest. It also returns the mapping, the parsed spec,
// the parsed snapshot and the revision digest so a writer can build a plan from exactly what it
// checked.
func (t *tenancyStore) preflight(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, lock bool, revision int) (*DeploymentPreflight, *ApplicationMapping, ApplicationSpec, protocol.Snapshot, string, error) {
	m, err := t.applicationMapping(ctx, tx, a, app, lock)
	if err != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
	}
	chosen, args := "a.latest_revision", []any{a.OrganizationID, a.EnvironmentID, app}
	if revision != 0 {
		var n int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, app, revision).Scan(&n); err != nil {
			return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
		}
		if n != 1 {
			return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrNotFound
		}
		chosen, args = "?", []any{revision, a.OrganizationID, a.EnvironmentID, app}
	}
	var raw, specRaw, digest, instance, state string
	var version, head, number int
	var received, observed time.Time
	err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.id,i.mapping_version,a.latest_revision,r.number,r.spec,r.digest,e.state,v.snapshot,v.received_at,v.observed_at FROM applications a JOIN application_instances i ON i.application_id=a.id JOIN application_revisions r ON r.application_id=a.id AND r.number=`+chosen+` JOIN endpoints e ON e.id=i.endpoint_id JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE a.organization_id=? AND a.environment_id=? AND a.id=?`), args...).Scan(&instance, &version, &head, &number, &specRaw, &digest, &state, &raw, &received, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
	}
	if err != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
	}
	if instance != m.InstanceID || version != m.Version || head != m.Preview.Revision {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
	}
	snapshot, current, err := freshInventory(state, raw, received, observed)
	if err != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", err
	}
	var spec ApplicationSpec
	if applicationSpecDigest([]byte(specRaw)) != digest || json.Unmarshal([]byte(specRaw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrRevisionCorrupt
	}
	for _, c := range m.Preview.Containers {
		now, found := current[c.ID]
		if !found || now.ImageID != c.ImageID || now.CreatedAt.UnixMicro() != c.CreatedAt.UnixMicro() || now.ComposeProject != m.Preview.Project {
			return nil, nil, ApplicationSpec{}, protocol.Snapshot{}, "", ErrAdoptionChanged
		}
	}
	out := buildDeploymentPreflight(m, spec, snapshot, number == head)
	out.Revision, out.ReceivedAt = number, received
	return out, m, spec, snapshot, digest, nil
}

// freshInventory parses an endpoint's stored snapshot and indexes its containers by ID. A
// snapshot from an inactive endpoint, received over 3 minutes ago, observed over 5 minutes
// either side of now, or with a partial or duplicated container list is ErrAdoptionChanged.
func freshInventory(state, raw string, received, observed time.Time) (protocol.Snapshot, map[string]protocol.Container, error) {
	var snapshot protocol.Snapshot
	if state != "active" || time.Since(received) > 3*time.Minute || time.Until(received) > time.Minute || time.Since(observed) > 5*time.Minute || time.Until(observed) > 5*time.Minute {
		return snapshot, nil, ErrAdoptionChanged
	}
	if json.Unmarshal([]byte(raw), &snapshot) != nil || len(snapshot.Containers) > protocol.MaxContainers || snapshot.Engine.Version == "" || slices.Contains(snapshot.Truncated, "containers") {
		return snapshot, nil, ErrAdoptionChanged
	}
	current := map[string]protocol.Container{}
	for _, c := range snapshot.Containers {
		if _, found := current[c.ID]; found {
			return snapshot, nil, ErrAdoptionChanged
		}
		current[c.ID] = c
	}
	return snapshot, current, nil
}

// buildDeploymentPreflight checks spec against the mapping. latest says spec is the latest
// revision; a prior one whose services differ from the mapped ones gets one blocker.
func buildDeploymentPreflight(m *ApplicationMapping, spec ApplicationSpec, snapshot protocol.Snapshot, latest bool) *DeploymentPreflight {
	out := &DeploymentPreflight{InstanceID: m.InstanceID, EndpointID: m.Preview.EndpointID, EndpointName: m.Preview.EndpointName, Revision: m.Preview.Revision, MappingVersion: m.Version, Blockers: []string{}, Services: []PreflightService{}}
	if m.Version == 0 || m.MappedRevision != m.Preview.Revision {
		out.Blockers = append(out.Blockers, "mapping_requires_review")
	}
	// Every adopted container is bound, and to a service of the revision being checked. A
	// prior revision whose services are not exactly the mapped ones is revision_services_differ.
	defined := map[string]bool{}
	differ := false
	for _, s := range spec.Services {
		defined[s.Name] = true
		differ = differ || m.Bindings[s.Name] == ""
	}
	stray := false
	for service := range m.Bindings {
		stray = stray || !defined[service]
	}
	if !latest && (stray || differ) {
		out.Blockers = append(out.Blockers, "revision_services_differ")
	}
	if (latest && stray) || len(m.Bindings) != len(m.Preview.Containers) {
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
	containers := make(map[string]AdoptedContainer, len(m.Preview.Containers))
	for _, c := range m.Preview.Containers {
		target := protocol.InspectionTarget{ContainerID: c.ID, ImageID: c.ImageID, CreatedUnix: c.CreatedAt.Unix()}
		if target.Validate() == nil {
			owned[c.ID] = target
		}
		containers[c.ID] = c
	}
	declared := make(map[string]DeclaredVolume, len(spec.Volumes))
	for _, v := range spec.Volumes {
		declared[v.Name] = v
	}
	// An external volume is mounted, never created, so it must already exist.
	volumesComplete := len(snapshot.Volumes) <= protocol.MaxVolumes && !slices.Contains(snapshot.Truncated, "volumes")
	existing := make(map[string]bool, len(snapshot.Volumes))
	for _, v := range snapshot.Volumes {
		existing[v.Name] = true
	}
	desired := map[portKey]map[string]bool{}
	blocked := false
	for _, s := range spec.Services {
		row := PreflightService{Name: s.Name, Reference: s.Image, ContainerID: m.Bindings[s.Name], Blockers: []string{}, Mounts: resolveMounts(m.Preview.Project, declared, s.Volumes), DroppedBinds: []protocol.Mount{}, DroppedMounts: []protocol.Mount{}, UnsupportedMounts: []protocol.Mount{}}
		if slices.ContainsFunc(s.Volumes, func(v ApplicationVolume) bool {
			return v.Kind == "named" && declared[v.Source].External && (!volumesComplete || !existing[v.Source])
		}) {
			row.Blockers = append(row.Blockers, "volume_missing")
		}
		if target, ok := owned[row.ContainerID]; ok {
			row.InspectionTarget = &target
			c := containers[row.ContainerID]
			if c.Mounts == nil || c.MountsTruncated {
				row.Blockers = append(row.Blockers, "mounts_unreported")
			} else {
				if slices.ContainsFunc(row.Mounts, func(want protocol.Mount) bool {
					return want.Kind == protocol.MountBind && !slices.Contains(c.Mounts, want)
				}) {
					row.Blockers = append(row.Blockers, "bind_mount_new")
				}
				// Only an identical mount is kept: a changed read-only flag shows the old mount
				// dropped beside the new one.
				for _, has := range c.Mounts {
					switch {
					case has.Kind == protocol.MountOther || (has.Kind == protocol.MountVolume && anonymousVolume.MatchString(has.Source)):
						row.UnsupportedMounts = append(row.UnsupportedMounts, has)
					case !slices.Contains(row.Mounts, has):
						row.DroppedMounts = append(row.DroppedMounts, has)
						if has.Kind == protocol.MountBind {
							row.DroppedBinds = append(row.DroppedBinds, has)
						}
					}
				}
				if len(row.UnsupportedMounts) > 0 {
					row.Blockers = append(row.Blockers, "mount_unsupported")
				}
			}
		}
		if row.ContainerID == "" {
			if latest {
				row.Blockers = append(row.Blockers, "service_unmapped")
			}
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
				if validSHA256(id) {
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
		blocked = blocked || len(row.Blockers) > 0
	}
	out.Executable = !blocked && len(out.Blockers) == 0
	return out
}

// resolveMounts turns a service's volumes into runtime mounts: a named volume by its host name
// in project, a bind by its path.
func resolveMounts(project string, declared map[string]DeclaredVolume, volumes []ApplicationVolume) []protocol.Mount {
	out := make([]protocol.Mount, 0, len(volumes))
	for _, v := range volumes {
		mount := protocol.Mount{Kind: protocol.MountBind, Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly}
		if v.Kind == "named" {
			mount.Kind, mount.Source = protocol.MountVolume, VolumeHostName(project, declared[v.Source])
		}
		out = append(out, mount)
	}
	return out
}

// validSHA256 accepts a full image ID or digest: sha256: and 64 lowercase hex.
func validSHA256(id string) bool {
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
