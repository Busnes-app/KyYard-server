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

// imageCheckDeadline bounds every registry call of one check; a var so a test can shorten it.
var imageCheckDeadline = 60 * time.Second

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

type imageCheckWork struct {
	LocalDigest  string
	Ref          registry.Reference
	Cred         *registry.Credential
	AllowPrivate bool
	Row          ImageCheck // pre-filled for pinned, unknown_local and not_configured
	Skip         bool       // no registry call
}

// CheckImageUpdates compares each mapped service's local repository digest with the registry's.
// It reads, resolves with no transaction open, then writes only if the mapping is unchanged.
func (t *tenancyStore) CheckImageUpdates(ctx context.Context, a TenantAccess, app string, resolver DigestResolver, key []byte, privateAllowed bool) (*UpdateCheck, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" || len(key) != 32 || resolver == nil {
		return nil, ErrInvalid
	}
	var work []imageCheckWork
	var instance string
	var version int
	err = t.readTenant(ctx, a, permissions.ApplicationDeploy, func(tx *sql.Tx) error {
		m, err := t.applicationMapping(ctx, tx, a, id.String(), false)
		if err != nil {
			return err
		}
		if m.Version < 1 {
			return ErrMappingRequired
		}
		_, m, spec, snapshot, _, err := t.preflight(ctx, tx, a, id.String(), false, 0)
		if err != nil {
			return err
		}
		instance, version = m.InstanceID, m.Version
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
		for _, s := range spec.Services {
			container, mapped := m.Bindings[s.Name]
			if !mapped {
				continue
			}
			w := imageCheckWork{Row: ImageCheck{Service: s.Name, Reference: s.Image}}
			w.Ref, err = registry.ParseReference(s.Image)
			switch {
			case err != nil:
				w.Skip, w.Row.Verdict, w.Row.Detail = true, "registry_error", "unavailable"
			case w.Ref.Digest != "":
				w.Skip, w.Row.Verdict = true, "pinned"
			default:
				w.LocalDigest = localRepoDigest(images[containers[container].ImageID], w.Ref)
				if w.LocalDigest == "" {
					w.Skip, w.Row.Verdict = true, "unknown_local"
					break
				}
				r, cred, err := t.registryFor(ctx, tx, a.OrganizationID, w.Ref.Host, key)
				switch {
				case errors.Is(err, ErrNotFound) && !anonymous:
					w.Skip, w.Row.Verdict, w.Row.Detail = true, "registry_error", "not_configured"
				case errors.Is(err, ErrNotFound):
				case err != nil:
					return err
				default:
					w.Cred, w.AllowPrivate = cred, r.AllowPrivate && privateAllowed
				}
			}
			w.Row.LocalDigest = w.LocalDigest
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
	err = t.withTenantTargetDetails(ctx, a, permissions.ApplicationDeploy, id.String()+"/updates", &details, func(tx *sql.Tx) error {
		var v int
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT mapping_version FROM application_instances WHERE id=? AND organization_id=? AND environment_id=? AND application_id=?`), instance, a.OrganizationID, a.EnvironmentID, id.String()).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && v != version) {
			return ErrAdoptionChanged
		}
		if err != nil {
			return err
		}
		if err := t.clearImageChecks(ctx, tx, instance); err != nil {
			return err
		}
		for _, w := range work {
			r := w.Row
			if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO image_checks(instance_id,service_name,reference,local_digest,remote_digest,verdict,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`), instance, r.Service, r.Reference, r.LocalDigest, r.RemoteDigest, r.Verdict, r.Detail, now); err != nil {
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

// localRepoDigest returns the host image's digest for exactly the reference's repository, or ""
// when there is none or more than one.
func localRepoDigest(im protocol.Image, ref registry.Reference) string {
	found := ""
	for _, d := range im.Digests {
		name, digest, ok := strings.Cut(d, "@")
		if !ok {
			continue
		}
		parsed, err := registry.ParseReference(name)
		if err != nil || parsed.Host != ref.Host || parsed.Repository != ref.Repository {
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
// at once, and gives up on whatever is left at imageCheckDeadline.
func resolveImageChecks(ctx context.Context, resolver DigestResolver, work []imageCheckWork) {
	ctx, cancel := context.WithTimeout(ctx, imageCheckDeadline)
	defer cancel()
	sem := make(chan struct{}, imageCheckConcurrency)
	var wg sync.WaitGroup
	for i := range work {
		if work[i].Skip {
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
			case digest == w.LocalDigest:
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
