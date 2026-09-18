package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

const MaxApplicationValuesBytes = 64 * 1024

// ImportApplication atomically saves a validated spec and its complete encrypted
// environment values. It never reads external files or changes runtime ownership.
func (t *tenancyStore) ImportApplication(ctx context.Context, a TenantAccess, name string, spec ApplicationSpec, values map[string]string, key []byte) (*Application, error) {
	if values == nil {
		values = map[string]string{}
	}
	return t.createApplication(ctx, a, name, spec, values, key)
}

func validateApplicationValues(spec ApplicationSpec, values map[string]string) error {
	refs := map[string]bool{}
	for _, service := range spec.Services {
		for _, ref := range service.Environment {
			refs[ref.SecretRef] = true
		}
	}
	if len(refs) != len(values) {
		return ErrInvalid
	}
	total := 0
	for ref, value := range values {
		total += len(ref) + len(value)
		if !refs[ref] || len(value) > 16*1024 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || total > MaxApplicationValuesBytes {
			return ErrInvalid
		}
	}
	return nil
}

// Bind ciphertext to scope, immutable revision and spec digest. Moving ciphertext
// to a different record or swapping its spec cannot decrypt under the derived key.
func applicationValuesKey(key []byte, a TenantAccess, id string, number int, digest string) []byte {
	scope, _ := json.Marshal([]any{"kyyard/application-values/v1", a.OrganizationID, a.EnvironmentID, id, number, digest})
	return crypto.DeriveKey(key, string(scope))
}
func (t *tenancyStore) sealApplicationValues(ctx context.Context, tx *sql.Tx, a TenantAccess, id string, number int, spec ApplicationSpec, digest string, values map[string]string, key []byte) error {
	if len(key) != 32 {
		return ErrInvalid
	}
	if err := validateApplicationValues(spec, values); err != nil {
		return err
	}
	raw, err := json.Marshal(values)
	if err != nil || len(raw) > MaxApplicationValuesBytes {
		return ErrInvalid
	}
	encrypted, err := crypto.EncryptAESGCM(raw, applicationValuesKey(key, a, id, number, digest))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE application_revisions SET secrets_enc=? WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), encrypted, a.OrganizationID, a.EnvironmentID, id, number)
	return err
}

// ResolveApplicationSecrets is an internal, explicitly secret-revealing operation,
// not an ordinary revision read or HTTP route. Commit its scoped audit before
// returning plaintext; failure/revocation returns no values. Deployment authority
// will need its own operation when deployment is implemented.
func (t *tenancyStore) ResolveApplicationSecrets(ctx context.Context, a TenantAccess, id string, number int, key []byte) (map[string]string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrInvalid
	}
	id = parsed.String()
	var values map[string]string
	err = t.withTenantTarget(ctx, a, permissions.SecretReveal, id+"/revisions/"+strconv.Itoa(number), func(tx *sql.Tx) error {
		if a.EnvironmentID == "" || number < 1 || number > MaxApplicationRevisions || len(key) != 32 {
			return ErrInvalid
		}
		var raw, digest, encrypted string
		err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest,secrets_enc FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, id, number).Scan(&raw, &digest, &encrypted)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(applicationSpecDigest([]byte(raw))), []byte(digest)) != 1 {
			return ErrRevisionCorrupt
		}
		plaintext, err := crypto.DecryptAESGCM(encrypted, applicationValuesKey(key, a, id, number, digest))
		if err != nil {
			return ErrRevisionCorrupt
		}
		if len(plaintext) > MaxApplicationValuesBytes || json.Unmarshal(plaintext, &values) != nil {
			return ErrRevisionCorrupt
		}
		var spec ApplicationSpec
		if json.Unmarshal([]byte(raw), &spec) != nil || validateApplicationValues(spec, values) != nil {
			return ErrRevisionCorrupt
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

// ReplaceApplicationRevision saves a full replacement definition and value bundle.
// Nothing is copied from older secrets or inferred from runtime observations.
func (t *tenancyStore) ReplaceApplicationRevision(ctx context.Context, a TenantAccess, id string, expected int, spec ApplicationSpec, values map[string]string, key []byte) (int, error) {
	if values == nil {
		values = map[string]string{}
	}
	return t.appendApplicationRevision(ctx, a, id, expected, spec, values, key)
}
