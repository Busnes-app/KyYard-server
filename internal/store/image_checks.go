package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/google/uuid"
)

// DigestResolver is what the check needs from a registry client. The API satisfies it with
// internal/registry; tests use a fake. Head must return once ctx ends: the check waits for it.
type DigestResolver interface {
	Head(ctx context.Context, ref registry.Reference, cred *registry.Credential, allowPrivate bool) (string, error)
}

// ImageCheckDeadline bounds every registry call of one check; the API sizes its write deadline
// from it. A var so a test can shorten it.
var ImageCheckDeadline = 60 * time.Second

const imageCheckConcurrency = 4

// ImageCheck is the cached update verdict for one mapped service.
type ImageCheck struct {
	Service      string    `json:"service"`
	Reference    string    `json:"reference"`
	LocalDigest  string    `json:"local_digest"`
	RemoteDigest string    `json:"remote_digest"`
	Verdict      string    `json:"verdict"`
	Detail       string    `json:"detail"`
	CheckedAt    time.Time `json:"checked_at"`
}

// UpdateCheck is an instance's cached checks; InstanceID is empty when nothing is adopted.
type UpdateCheck struct {
	InstanceID     string       `json:"instance_id"`
	MappingVersion int          `json:"mapping_version"`
	Services       []ImageCheck `json:"services"`
}

func (t *tenancyStore) ReadImageChecks(ctx context.Context, a TenantAccess, app string) (*UpdateCheck, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return nil, ErrInvalid
	}
	out := &UpdateCheck{Services: []ImageCheck{}}
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,mapping_version FROM application_instances WHERE organization_id=? AND environment_id=? AND application_id=?`), a.OrganizationID, a.EnvironmentID, id.String()).Scan(&out.InstanceID, &out.MappingVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT service_name,reference,local_digest,remote_digest,verdict,detail,checked_at FROM image_checks WHERE instance_id=? ORDER BY service_name`), out.InstanceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c ImageCheck
			if err := rows.Scan(&c.Service, &c.Reference, &c.LocalDigest, &c.RemoteDigest, &c.Verdict, &c.Detail, &c.CheckedAt); err != nil {
				return err
			}
			out.Services = append(out.Services, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *tenancyStore) clearImageChecks(ctx context.Context, tx *sql.Tx, instance string) error {
	_, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM image_checks WHERE instance_id=?`), instance)
	return err
}

// CheckImageUpdateAccess admits a check before the API takes its per-application slot, so a
// caller without application.deploy can neither see nor hold it. CheckImageUpdates re-authorizes.
func (t *tenancyStore) CheckImageUpdateAccess(ctx context.Context, a TenantAccess, app string) error {
	return t.checkApplication(ctx, a, permissions.ApplicationDeploy, app, "/updates")
}

// CheckApplicationAccess authorizes action on an application in scope, audited on the
// application only when denied.
func (t *tenancyStore) CheckApplicationAccess(ctx context.Context, a TenantAccess, action permissions.Action, app string) error {
	return t.checkApplication(ctx, a, action, app, "")
}

// checkApplication authorizes action on app, auditing a denial on app+suffix.
func (t *tenancyStore) checkApplication(ctx context.Context, a TenantAccess, action permissions.Action, app, suffix string) error {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return ErrInvalid
	}
	target := id.String() + suffix
	return t.run(ctx, a, action, &target, nil, false, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT 1 FROM applications WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, id.String()).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
}

type imageCheckWork struct {
	Ref          registry.Reference
	Cred         *registry.Credential
	AllowPrivate bool
	Row          ImageCheck // a verdict set in phase 1 means no registry call
}

// CheckImageUpdates compares each mapped service's local repository digest with the registry's.
// It reads, resolves with no transaction open, then writes only if imageCheckState is unchanged.
func (t *tenancyStore) CheckImageUpdates(ctx context.Context, a TenantAccess, app string, resolver DigestResolver, key []byte, privateAllowed bool) (*UpdateCheck, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 || resolver == nil {
		return nil, ErrInvalid
	}
	var work []imageCheckWork
	var instance, state string
	var version int
	target := id.String() + "/updates"
	// A read: no lock and no success row, but a denial or failure audits the check's target.
	err = t.run(ctx, a, permissions.ApplicationDeploy, &target, nil, false, func(tx *sql.Tx) error {
		m, err := t.applicationMapping(ctx, tx, a, id.String(), false)
		if err != nil {
			return err
		}
		if m.Version < 1 {
			return ErrMappingRequired
		}
		_, m, spec, snapshot, _, err := t.preflight(ctx, tx, a, id.String(), false, 0, nil)
		if err != nil {
			return err
		}
		// preflight verified m against the latest revision and the live inventory: the rows
		// describe exactly this state.
		instance, version = m.InstanceID, m.Version
		state = imageCheckStateOf(m.Version, m.Preview.Revision, m.Preview.Containers)
		images := map[string]protocol.Image{}
		for _, im := range snapshot.Images {
			images[im.ID] = im
		}
		containers := map[string]protocol.Container{}
		for _, c := range snapshot.Containers {
			containers[c.ID] = c
		}
		anonymous, err := t.anonymousPull(ctx, tx, a.OrganizationID)
		if err != nil {
			return err
		}
		kube := m.Runtime == protocol.RuntimeKubernetes
		objects := protocol.KubernetesNames(m.Preview.Project, spec.serviceNames())
		for _, s := range spec.Services {
			container, mapped := m.Bindings[s.Name]
			if !mapped && !kube {
				continue
			}
			w := imageCheckWork{Row: ImageCheck{Service: s.Name, Reference: s.Image}}
			w.Ref, err = registry.ParseReference(s.Image)
			switch {
			case err != nil:
				w.Row.Verdict, w.Row.Detail = "registry_error", "unavailable"
			case w.Ref.Digest != "":
				w.Row.Verdict = "pinned"
			default:
				if kube {
					w.Row.LocalDigest = workloadDigest(snapshot, m, objects[s.Name], w.Ref)
				} else {
					w.Row.LocalDigest = localRepoDigest(images[containers[container].ImageID], w.Ref)
				}
				if w.Row.LocalDigest == "" {
					w.Row.Verdict = "unknown_local"
					break
				}
				r, cred, err := t.registryFor(ctx, tx, a.OrganizationID, w.Ref.Host, key)
				switch {
				case errors.Is(err, ErrNotFound) && !anonymous:
					w.Row.Verdict, w.Row.Detail = "registry_error", "not_configured"
				case errors.Is(err, ErrNotFound):
				case err != nil:
					return err
				default:
					w.Cred, w.AllowPrivate = cred, r.AllowPrivate && privateAllowed
				}
			}
			work = append(work, w)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	resolveImageChecks(ctx, resolver, work)
	now := time.Now().UTC().Truncate(time.Microsecond)
	updates, failures := 0, 0
	for i := range work {
		work[i].Row.CheckedAt = now
		switch work[i].Row.Verdict {
		case "update_available":
			updates++
		case "registry_error":
			failures++
		}
	}
	details := fmt.Sprintf("services=%d updates=%d errors=%d", len(work), updates, failures)
	out := &UpdateCheck{InstanceID: instance, MappingVersion: version, Services: []ImageCheck{}}
	// Uncancelled, so a check the client abandoned mid-registry still audits a failure. It
	// writes no rows: a half-finished check must not replace good ones with unavailable.
	wctx := context.WithoutCancel(ctx)
	err = t.withTenantTargetDetails(wctx, a, permissions.ApplicationDeploy, target, &details, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Settle, remap and revision append write the application row; locking it first
		// serializes them with the comparison.
		if err := t.lockApplication(wctx, tx, a, id.String()); err != nil {
			return err
		}
		current, err := t.imageCheckState(wctx, tx, a, id.String(), instance)
		if err != nil {
			return err
		}
		if current != state {
			return ErrAdoptionChanged
		}
		if err := t.clearImageChecks(wctx, tx, instance); err != nil {
			return err
		}
		for _, w := range work {
			r := w.Row
			if _, err := tx.ExecContext(wctx, t.store.rebind(`INSERT INTO image_checks(instance_id,service_name,reference,local_digest,remote_digest,verdict,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`), instance, r.Service, r.Reference, r.LocalDigest, r.RemoteDigest, r.Verdict, r.Detail, now); err != nil {
				return err
			}
			out.Services = append(out.Services, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// imageCheckState is what the rows describe: the mapping version, the latest revision and the
// instance's container and image IDs. An apply or a revision landing mid-check changes it. A
// released instance is ErrAdoptionChanged.
func (t *tenancyStore) imageCheckState(ctx context.Context, tx *sql.Tx, a TenantAccess, app, instance string) (string, error) {
	var version, revision int
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.mapping_version,a.latest_revision FROM application_instances i JOIN applications a ON a.id=i.application_id WHERE i.id=? AND i.organization_id=? AND i.environment_id=? AND i.application_id=?`), instance, a.OrganizationID, a.EnvironmentID, app).Scan(&version, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAdoptionChanged
	}
	if err != nil {
		return "", err
	}
	rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT container_id,image_id FROM application_resources WHERE instance_id=? ORDER BY container_id`), instance)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var containers []AdoptedContainer
	for rows.Next() {
		var c AdoptedContainer
		if err := rows.Scan(&c.ID, &c.ImageID); err != nil {
			return "", err
		}
		containers = append(containers, c)
	}
	return imageCheckStateOf(version, revision, containers), rows.Err()
}

// imageCheckStateOf formats the state; containers are in container ID order.
func imageCheckStateOf(version, revision int, containers []AdoptedContainer) string {
	state := fmt.Sprintf("%d %d", version, revision)
	for _, c := range containers {
		state += " " + c.ID + "=" + c.ImageID
	}
	return state
}

// workloadDigest is the digest a Kubernetes instance's Deployment runs for ref: its one image,
// pinned as host/repository@digest to exactly ref's repository. "" when the Deployment is not
// reported, is not the instance's, or runs anything else.
func workloadDigest(snapshot protocol.Snapshot, m *ApplicationMapping, name string, ref registry.Reference) string {
	if snapshot.Kubernetes == nil {
		return ""
	}
	for _, w := range snapshot.Kubernetes.Workloads {
		if w.Kind != protocol.KindDeployment || w.Namespace != m.Namespace || w.Name != name || w.Instance != m.InstanceID || len(w.Images) != 1 {
			continue
		}
		repo, digest, ok := strings.Cut(w.Images[0], "@")
		parsed, err := registry.ParseReference(repo)
		if ok && err == nil && parsed.Host == ref.Host && parsed.Repository == ref.Repository && validSHA256(digest) {
			return digest
		}
	}
	return ""
}

// localRepoDigest returns the host image's digest for exactly the reference's repository, or ""
// when there is none, more than one, or it is not a sha256 digest.
func localRepoDigest(im protocol.Image, ref registry.Reference) string {
	found := ""
	for _, d := range im.Digests {
		name, digest, ok := strings.Cut(d, "@")
		if !ok {
			continue
		}
		parsed, err := registry.ParseReference(name)
		if err != nil || parsed.Host != ref.Host || parsed.Repository != ref.Repository || !validSHA256(digest) {
			continue
		}
		if found != "" {
			return ""
		}
		found = digest
	}
	return found
}

// resolveImageChecks asks the registry for every unskipped service, at most imageCheckConcurrency
// at once, and gives up on whatever is left at ImageCheckDeadline.
func resolveImageChecks(ctx context.Context, resolver DigestResolver, work []imageCheckWork) {
	ctx, cancel := context.WithTimeout(ctx, ImageCheckDeadline)
	defer cancel()
	sem := make(chan struct{}, imageCheckConcurrency)
	var wg sync.WaitGroup
	for i := range work {
		if work[i].Row.Verdict != "" {
			continue
		}
		wg.Add(1)
		go func(w *imageCheckWork) {
			defer wg.Done()
			cred := w.Cred
			w.Cred = nil
			w.Row.Verdict, w.Row.Detail = "registry_error", "unavailable"
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			digest, err := resolver.Head(ctx, w.Ref, cred, w.AllowPrivate)
			switch {
			case err != nil:
				w.Row.Detail = registryErrorDetail(err)
			case digest == w.Row.LocalDigest:
				w.Row.Verdict, w.Row.Detail, w.Row.RemoteDigest = "current", "", digest
			default:
				w.Row.Verdict, w.Row.Detail, w.Row.RemoteDigest = "update_available", "", digest
			}
		}(&work[i])
	}
	wg.Wait()
}

func registryErrorDetail(err error) string {
	switch {
	case errors.Is(err, registry.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, registry.ErrNotFound):
		return "not_found"
	case errors.Is(err, registry.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, registry.ErrPrivateDestination):
		return "private_destination"
	}
	return "unavailable"
}
