package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestApplicationReplacementBundles(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := make([]byte, 32)
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1", Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "token"}}}}}
	app, err := ts.ImportApplication(ctx, a, "shop", spec, map[string]string{"token": "old-secret"}, key)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"token": "new-secret"}, key); err != nil || n != 2 {
		t.Fatalf("replace: %d %v", n, err)
	}
	for n, want := range map[int]string{1: "old-secret", 2: "new-secret"} {
		values, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, n, key)
		if err != nil || values["token"] != want {
			t.Fatalf("revision %d: %v", n, err)
		}
	}
	var first string
	if err = st.db.QueryRowContext(ctx, st.rebind(`SELECT secrets_enc FROM application_revisions WHERE application_id=? AND number=1`), app.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first, "old-secret") {
		t.Fatal("plaintext stored")
	}
	if _, err = st.db.ExecContext(ctx, st.rebind(`UPDATE application_revisions SET secrets_enc=? WHERE application_id=? AND number=2`), first, app.ID); err != nil {
		t.Fatal(err)
	}
	if values, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, 2, key); !errors.Is(err, ErrRevisionCorrupt) || values != nil {
		t.Fatal("cross-revision ciphertext accepted")
	}
	// Invalid bundles/keys must roll back head and history together.
	for _, values := range []map[string]string{nil, {"wrong": "secret"}, {"token": strings.Repeat("x", 16385)}} {
		if _, err = ts.ReplaceApplicationRevision(ctx, a, app.ID, 2, spec, values, key); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid values: %v", err)
		}
	}
	if _, err = ts.ReplaceApplicationRevision(ctx, a, app.ID, 2, spec, map[string]string{"token": "x"}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, map[string]string{"token": "x"}, key); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("stale edit accepted")
	}
	// Complete replacement can intentionally remove all environment values.
	spec.Services[0].Environment = nil
	if n, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 2, spec, nil, key); err != nil || n != 3 {
		t.Fatalf("empty replacement: %d %v", n, err)
	}
	values, err := ts.ResolveApplicationSecrets(ctx, a, app.ID, 3, key)
	if err != nil || len(values) != 0 {
		t.Fatal("removed values carried forward")
	}
	if _, err = st.db.ExecContext(ctx, `DROP TABLE audit_records`); err != nil {
		t.Fatal(err)
	}
	if _, err = ts.ReplaceApplicationRevision(ctx, a, app.ID, 3, spec, nil, key); err == nil {
		t.Fatal("saved without audit")
	}
	var head, count int
	if err = st.db.QueryRowContext(ctx, st.rebind(`SELECT latest_revision,(SELECT COUNT(*) FROM application_revisions WHERE application_id=applications.id) FROM applications WHERE id=?`), app.ID).Scan(&head, &count); err != nil || head != 3 || count != 3 {
		t.Fatalf("partial save: %d %d %v", head, count, err)
	}
}
func TestApplicationReplacementConcurrentWriters(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	key := make([]byte, 32)
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "nginx:1"}}}
	app, err := ts.ImportApplication(ctx, a, "shop", spec, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, spec, nil, key)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrRevisionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("winners %d conflicts %d", success, conflict)
	}
	if _, err = ts.ResolveApplicationSecrets(ctx, a, app.ID, 2, key); err != nil {
		t.Fatal(err)
	}
}
