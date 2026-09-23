package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/registry"
	"github.com/google/uuid"
)

const maxRegistryCredentialBytes = 4096

var registryHostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// NormalizeRegistryHost returns host as image references name it: lowercase host[:port], with
// Docker Hub's aliases folded into docker.io. Like ParseReference, it takes only localhost or a
// name with a dot or port as a registry.
func NormalizeRegistryHost(host string) (string, error) {
	host = registry.CanonicalHost(host)
	name, port, hasPort := strings.Cut(host, ":")
	if len(name) > 253 || !registryHostName.MatchString(name) || !(hasPort || name == "localhost" || strings.Contains(name, ".")) {
		return "", ErrInvalid
	}
	if n, err := strconv.Atoi(port); hasPort && (err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port) {
		return "", ErrInvalid
	}
	return host, nil
}

// registryCredentialKey binds ciphertext to its organization and row, so a credential moved to
// another row does not decrypt.
func registryCredentialKey(key []byte, org, id string) []byte {
	label, _ := json.Marshal([]any{"kyyard/registry-credential/v1", org, id})
	return crypto.DeriveKey(key, string(label))
}

func validRegistryInput(in RegistryInput, key []byte) bool {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 64 || protocol.CleanText(name, 64) != name {
		return false
	}
	// Basic auth cannot carry a ':' in the user-id.
	if len(in.Username) > 255 || protocol.CleanText(in.Username, 255) != in.Username || strings.ContainsRune(in.Username, ':') || len(key) != 32 {
		return false
	}
	if c := in.Credential; c != nil && (len(*c) > maxRegistryCredentialBytes || !utf8.ValidString(*c) || strings.ContainsRune(*c, 0)) {
		return false
	}
	return true
}

const registryColumns = `id,organization_id,host,name,username,credential_enc<>'',allow_private,created_by,created_at,updated_at`

func scanRegistry(row interface{ Scan(...any) error }, r *Registry) error {
	return row.Scan(&r.ID, &r.OrganizationID, &r.Host, &r.Name, &r.Username, &r.HasCredential, &r.AllowPrivate, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
}

func (t *tenancyStore) ListRegistries(ctx context.Context, a TenantAccess) ([]Registry, error) {
	result := []Registry{}
	err := t.readTenant(ctx, a, permissions.RegistryRead, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT `+registryColumns+` FROM registries WHERE organization_id=? ORDER BY host`), a.OrganizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Registry
			if err := scanRegistry(rows, &r); err != nil {
				return err
			}
			result = append(result, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// PutRegistry creates or updates the organization's entry for a host. A nil credential keeps
// the stored one; "" clears it. privateAllowed is the operator's KY_REGISTRY_ALLOW_PRIVATE.
func (t *tenancyStore) PutRegistry(ctx context.Context, a TenantAccess, in RegistryInput, key []byte, privateAllowed bool) (*Registry, error) {
	host, err := NormalizeRegistryHost(in.Host)
	if err != nil || !validRegistryInput(in, key) {
		return nil, ErrInvalid
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := Registry{OrganizationID: a.OrganizationID, Host: host, Name: strings.TrimSpace(in.Name), Username: in.Username, AllowPrivate: in.AllowPrivate, CreatedBy: a.ActorID, CreatedAt: now, UpdatedAt: now}
	target, details := "registries", ""
	err = t.run(ctx, a, permissions.RegistryManage, &target, &details, true, func(tx *sql.Tx) error {
		// allow_private is a tenant's switch; only the operator's opt-in makes it available.
		// Checked after the permission, so a refusal is an audited failure.
		if in.AllowPrivate && !privateAllowed {
			return ErrPrivateRegistriesDisabled
		}
		if err := t.lockOrganization(ctx, tx, a.OrganizationID); err != nil {
			return err
		}
		var enc string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT id,credential_enc,created_by,created_at FROM registries WHERE organization_id=? AND host=?`), a.OrganizationID, host).Scan(&r.ID, &enc, &r.CreatedBy, &r.CreatedAt)
		exists := err == nil
		if errors.Is(err, sql.ErrNoRows) {
			r.ID = uuid.NewString()
		} else if err != nil {
			return err
		}
		state := "kept"
		switch {
		case in.Credential == nil:
		case *in.Credential == "":
			enc, state = "", "cleared"
		default:
			if enc, err = crypto.EncryptAESGCM([]byte(*in.Credential), registryCredentialKey(key, a.OrganizationID, r.ID)); err != nil {
				return err
			}
			state = "set"
		}
		r.HasCredential = enc != ""
		details = fmt.Sprintf("host=%s allow_private=%t credential=%s", host, r.AllowPrivate, state)
		if exists {
			_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE registries SET name=?,username=?,credential_enc=?,allow_private=?,updated_at=? WHERE organization_id=? AND id=?`), r.Name, r.Username, enc, r.AllowPrivate, now, a.OrganizationID, r.ID)
		} else {
			_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO registries (id,organization_id,host,name,username,credential_enc,allow_private,created_by,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`), r.ID, r.OrganizationID, host, r.Name, r.Username, enc, r.AllowPrivate, r.CreatedBy, now, now)
		}
		if err == nil {
			target = "registries/" + r.ID
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (t *tenancyStore) DeleteRegistry(ctx context.Context, a TenantAccess, id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return ErrInvalid
	}
	id = parsed.String()
	var details string
	return t.withTenantTargetDetails(ctx, a, permissions.RegistryManage, "registries/"+id, &details, func(tx *sql.Tx) error {
		var host string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT host FROM registries WHERE organization_id=? AND id=?`), a.OrganizationID, id).Scan(&host)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		details = "host=" + host
		result, err := tx.ExecContext(ctx, t.store.rebind(`DELETE FROM registries WHERE organization_id=? AND id=?`), a.OrganizationID, id)
		return tenantChangeResult(result, err)
	})
}

func (t *tenancyStore) ReadRegistryPolicy(ctx context.Context, a TenantAccess) (RegistryPolicy, error) {
	var p RegistryPolicy
	err := t.readTenant(ctx, a, permissions.RegistryRead, func(tx *sql.Tx) error {
		var err error
		p.AnonymousPullEnabled, err = t.anonymousPull(ctx, tx, a.OrganizationID)
		return err
	})
	return p, err
}

func (t *tenancyStore) anonymousPull(ctx context.Context, tx *sql.Tx, org string) (bool, error) {
	var enabled bool
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT anonymous_pull_enabled FROM organizations WHERE id=?`), org).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return enabled, err
}

func (t *tenancyStore) SetAnonymousPull(ctx context.Context, a TenantAccess, enabled bool) error {
	var details string
	return t.withTenantTargetDetails(ctx, a, permissions.RegistryManage, "registry-policy", &details, func(tx *sql.Tx) error {
		if err := t.lockOrganization(ctx, tx, a.OrganizationID); err != nil {
			return err
		}
		old, err := t.anonymousPull(ctx, tx, a.OrganizationID)
		if err != nil {
			return err
		}
		details = fmt.Sprintf("old=%t new=%t", old, enabled)
		result, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE organizations SET anonymous_pull_enabled=? WHERE id=?`), enabled, a.OrganizationID)
		return tenantChangeResult(result, err)
	})
}

func (t *tenancyStore) ResolveRegistryAccess(ctx context.Context, a TenantAccess, action permissions.Action, ref string, key []byte) (*RegistryAccess, error) {
	// The credential is unlocked only by an operation that uses it, never by a read permission.
	if action != permissions.ImagePull && action != permissions.ApplicationDeploy {
		return nil, ErrForbidden
	}
	parsed, err := registry.ParseReference(ref)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalid
	}
	var access RegistryAccess
	notConfigured := false
	// Not configured is a result, not a failure: returned outside the op so it audits nothing.
	err = t.readTenant(ctx, a, action, func(tx *sql.Tx) error {
		r, credential, err := t.registryFor(ctx, tx, a.OrganizationID, parsed.Host, key)
		if errors.Is(err, ErrNotFound) {
			enabled, err := t.anonymousPull(ctx, tx, a.OrganizationID)
			if err != nil {
				return err
			}
			notConfigured, access.Anonymous = !enabled, enabled
			return nil
		}
		access.Registry, access.Credential = r, credential
		return err
	})
	if err != nil {
		return nil, err
	}
	if notConfigured {
		return nil, ErrRegistryNotConfigured
	}
	return &access, nil
}

// registryFor reads and decrypts the organization's entry for host inside the caller's
// transaction; the caller owns permission and audit. A stored credential that does not decrypt
// is ErrInvalid.
func (t *tenancyStore) registryFor(ctx context.Context, tx *sql.Tx, org, host string, key []byte) (*Registry, *registry.Credential, error) {
	var r Registry
	var enc string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT `+registryColumns+`,credential_enc FROM registries WHERE organization_id=? AND host=?`), org, host).Scan(&r.ID, &r.OrganizationID, &r.Host, &r.Name, &r.Username, &r.HasCredential, &r.AllowPrivate, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil || enc == "" {
		return &r, nil, err
	}
	secret, err := crypto.DecryptAESGCM(enc, registryCredentialKey(key, org, r.ID))
	if err != nil {
		return nil, nil, ErrInvalid
	}
	return &r, &registry.Credential{Username: r.Username, Secret: string(secret)}, nil
}
