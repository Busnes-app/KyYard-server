package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestUpdatePolicyRoutes(t *testing.T) {
	h := newPlanHost(t, policyCaps, "web")
	ctx := context.Background()
	ts := h.st.Tenancy()
	envAdmin := loginAs(t, h.s, h.st, "envadmin", "user")
	viewer := loginAs(t, h.s, h.st, "viewer", "user")
	for user, role := range map[string]store.TenantRole{"usr_envadmin": store.RoleEnvironmentAdmin, "usr_viewer": store.RoleReadOnly} {
		if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: user, Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	other := h.secondAdmin(t)
	url := strings.TrimSuffix(h.deployments, "deployments") + "update-policy"
	send := func(cookie *http.Cookie, method, path, body string, csrf bool, status int) string {
		t.Helper()
		w := tenantRequest(h.s, cookie, method, path, body, csrf)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), status)
		}
		return w.Body.String()
	}
	code := func(body, want string) {
		t.Helper()
		var e struct{ Code string }
		if json.Unmarshal([]byte(body), &e) != nil || e.Code != want {
			t.Fatalf("want code %s: %s", want, body)
		}
	}
	send(h.admin, "GET", url, "", false, 404)
	body := `{"mode":"apply","timezone":"Europe/Paris","weekdays":[5,1,3],"start_minute":120,"end_minute":240}`
	send(envAdmin, "PUT", url, body, true, 403)
	send(h.admin, "PUT", url, body, false, 403) // no CSRF token
	raw := send(h.admin, "PUT", url, body, true, 201)
	var created store.UpdatePolicy
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatal(err)
	}
	// next_occurrence is RFC3339 in UTC: it parses back into time.UTC, not a fixed offset.
	if created.Mode != "apply" || created.Timezone != "Europe/Paris" || len(created.Weekdays) != 3 || created.Weekdays[0] != 1 || created.StartMinute != 120 || created.EndMinute != 240 || created.Status != "active" || created.CreatedBy != "usr_planner" || created.NextOccurrence == nil || created.NextOccurrence.Location() != time.UTC {
		t.Fatalf("created: %s", raw)
	}
	for _, tc := range []struct{ code, body string }{
		{"invalid_mode", `{"mode":"auto","timezone":"UTC","weekdays":[1],"start_minute":0,"end_minute":60}`},
		{"invalid_timezone", `{"mode":"apply","timezone":"Mars/Olympus_Mons","weekdays":[1],"start_minute":0,"end_minute":60}`},
		{"invalid_timezone", `{"mode":"apply","timezone":"Local","weekdays":[1],"start_minute":0,"end_minute":60}`},
		{"invalid_weekdays", `{"mode":"apply","timezone":"UTC","weekdays":[],"start_minute":0,"end_minute":60}`},
		{"invalid_weekdays", `{"mode":"apply","timezone":"UTC","weekdays":[7],"start_minute":0,"end_minute":60}`},
		{"invalid_window", `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":600,"end_minute":610}`},
		{"invalid_window", `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":1380,"end_minute":60}`},
		{"invalid_window", `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":0,"end_minute":1441}`},
	} {
		code(send(h.admin, "PUT", url, tc.body, true, 400), tc.code)
	}
	send(h.admin, "PUT", url, `{"mode":"apply","timezone":"UTC","weekdays":[1],"start_minute":0,"end_minute":60,"created_by":"usr_viewer"}`, true, 400)
	// Another administrator edits: the policy now acts as them. Tomorrow's weekday only, so no
	// window before the edit can be recorded as missed at the tick below.
	daily := fmt.Sprintf(`{"mode":"plan_only","timezone":"UTC","weekdays":[%d],"start_minute":600,"end_minute":660}`, int(tomorrow().Weekday()))
	var edited store.UpdatePolicy
	if err := json.Unmarshal([]byte(send(other, "PUT", url, daily, true, 200)), &edited); err != nil || edited.ID != created.ID || edited.CreatedBy != "usr_other" || edited.Mode != "plan_only" {
		t.Fatalf("edited: %+v %v", edited, err)
	}
	// Every member reads; runs are bounded.
	var view struct {
		store.UpdatePolicy
		Runs []store.PolicyRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(send(viewer, "GET", url, "", false, 200)), &view); err != nil || view.ID != created.ID || view.Runs == nil || len(view.Runs) != 0 {
		t.Fatalf("read: %+v %v", view, err)
	}
	if got := send(viewer, "GET", url+"/runs?limit=100", "", false, 200); got != "[]\n" && got != "[]" {
		t.Fatalf("runs: %q", got)
	}
	for _, q := range []string{"0", "101", "x"} {
		send(viewer, "GET", url+"/runs?limit="+q, "", false, 400)
	}
	// Resume: 409 while active; a pause (the editor lost application.deploy) then resumes.
	code(send(other, "POST", url+"/resume", "", true, 409), "policy_not_paused")
	if err := ts.SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_other", Role: store.RoleOperator, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	h.tick(tomorrow().Add(10*time.Hour + 30*time.Minute))
	if runs := h.runs(t, "usr_planner"); len(runs) != 1 || runs[0].Outcome != store.RunPaused {
		t.Fatalf("runs: %+v", runs)
	}
	var runs []store.PolicyRun
	if err := json.Unmarshal([]byte(send(viewer, "GET", url+"/runs", "", false, 200)), &runs); err != nil || len(runs) != 1 {
		t.Fatalf("runs route: %+v %v", runs, err)
	}
	send(envAdmin, "POST", url+"/resume", "", true, 403)
	var resumed store.UpdatePolicy
	if err := json.Unmarshal([]byte(send(h.admin, "POST", url+"/resume", "", true, 200)), &resumed); err != nil || resumed.Status != "active" || resumed.ConsecutiveFailures != 0 || resumed.PausedReason != "" {
		t.Fatalf("resumed: %+v %v", resumed, err)
	}
	// Delete: administrators only; it takes the runs; a second delete is 404.
	send(viewer, "DELETE", url, "", true, 403)
	send(h.admin, "DELETE", url, "", true, 204)
	send(h.admin, "GET", url, "", false, 404)
	send(h.admin, "DELETE", url, "", true, 404)
	if runs := h.runs(t, "usr_planner"); len(runs) != 0 {
		t.Fatalf("runs outlived their policy: %+v", runs)
	}
	// Every write is audited on the policy.
	saves := 0
	for _, r := range policyAuditRows(t, h, h.admin) {
		if r.Action == "application.policy" && r.Result == "success" && strings.HasPrefix(r.Resource, h.appID()+"/policies/"+created.ID) {
			saves++
		}
	}
	if saves != 4 { // create, edit, resume, delete
		t.Fatalf("policy write rows: %d", saves)
	}
}
