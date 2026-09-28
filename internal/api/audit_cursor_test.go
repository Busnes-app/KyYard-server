package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestAuditCursorContract(t *testing.T) {
	s, st, cookie, csrf, org := tenantAdminFixture(t)
	ctx := context.Background()
	bearer := pairAndClaim(t, s, cookie, csrf, org)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, env := range []string{"one", "two"} {
		must(st.Tenancy().CreateEnvironment(ctx, &store.Environment{ID: env, OrganizationID: org, Name: env}))
	}
	must(st.Tenancy().CreateOrganization(ctx, &store.Organization{ID: "foreign", Name: "foreign"}))
	must(st.Tenancy().CreateEnvironment(ctx, &store.Environment{ID: "foreign-env", OrganizationID: "foreign", Name: "foreign"}))
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 405; i++ {
		env := "one"
		if i%2 == 1 {
			env = "two"
		}
		must(st.Audit().LogAudit(ctx, &store.AuditRecord{Action: "fixture", Resource: fmt.Sprint(i), Scope: "organization", OrganizationID: org, EnvironmentID: env, CreatedAt: at}))
		must(st.Audit().LogAudit(ctx, &store.AuditRecord{Action: "foreign", Scope: "organization", OrganizationID: "foreign", CreatedAt: at}))
	}
	get := func(path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w
	}
	base := "/api/organizations/" + org + "/audit"
	for _, q := range []string{"after_id=-1", "after_id=", "after_id=no", "after_id=9223372036854775808", "after_id=1&after_id=2", "after_id=0&offset=0", "after_id=0&offset=", "after_id=0&limit=1&limit=2", "after_id=0&limit=0", "after_id=0&limit=201", "offset=0&offset=1", "limit=1&limit=2"} {
		get(base+"?"+q, 400)
	}
	type page struct {
		Items []store.AuditRecord `json:"items"`
		Next  int64               `json:"next_after_id"`
	}
	var cursor int64
	count := 0
	for {
		var p page
		must(json.Unmarshal(get(fmt.Sprintf("%s?after_id=%d&limit=200", base, cursor), 200).Body.Bytes(), &p))
		if p.Items == nil || len(p.Items) > 200 {
			t.Fatalf("invalid items: %+v", p)
		}
		for _, row := range p.Items {
			if row.ID <= cursor || row.OrganizationID != org {
				t.Fatalf("cursor/scope: %+v", row)
			}
			cursor = row.ID
			if row.Action == "fixture" {
				count++
			}
		}
		if p.Next != cursor {
			t.Fatalf("cursor advanced outside page: %+v", p)
		}
		if len(p.Items) == 0 {
			break
		}
	}
	if count != 405 {
		t.Fatalf("got %d fixture rows", count)
	}
	var p page
	must(json.Unmarshal(get(base+"?after_id=9223372036854775807", 200).Body.Bytes(), &p))
	if p.Next != 9223372036854775807 || len(p.Items) != 0 {
		t.Fatalf("empty cursor: %+v", p)
	}
	must(json.Unmarshal(get("/api/organizations/"+org+"/environments/one/audit?after_id=0&limit=200", 200).Body.Bytes(), &p))
	if len(p.Items) != 200 {
		t.Fatalf("environment page: %d", len(p.Items))
	}
	for _, row := range p.Items {
		if row.EnvironmentID != "one" {
			t.Fatal("environment leak")
		}
	}
	get("/api/organizations/foreign/audit?after_id=0", 403)
	get("/api/organizations/"+org+"/environments/foreign-env/audit?after_id=0", 404)
	var legacy []store.AuditRecord
	must(json.Unmarshal(get(base+"?offset=0&limit=2", 200).Body.Bytes(), &legacy))
	if len(legacy) != 2 || legacy[0].ID <= legacy[1].ID {
		t.Fatalf("legacy order: %+v", legacy)
	}
	viewer := loginAs(t, s, st, "cursorviewer", "user")
	must(st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: org, UserID: "usr_cursorviewer", Role: store.RoleReadOnly, Status: "active"}))
	if w := tenantRequest(s, viewer, "GET", base+"?after_id=0", "", false); w.Code != 403 {
		t.Fatalf("viewer: %d", w.Code)
	}
	if w := tenantRequest(s, cookie, "GET", base+"?after_id=0&limit=1", "", false); w.Code != 200 {
		t.Fatalf("admin: %d", w.Code)
	}
	tok, err := st.Tenancy().LookupServiceToken(ctx, bearer)
	must(err)
	must(st.Tenancy().RevokeServiceToken(ctx, store.TenantAccess{ActorID: "usr_orgadmin", OrganizationID: org}, tok.ID))
	get(base+"?after_id=0", 401)
}
