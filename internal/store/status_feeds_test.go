package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// setEndpointState names the endpoint and puts it in state, as a connection would; endpoint
// names are unique and activeEndpointWith always enrolls "host".
func setEndpointState(t *testing.T, st *SQLStore, id, name, state string) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(`UPDATE endpoints SET name=?, state=?, last_seen_at=? WHERE id=?`), name, state, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), id); err != nil {
		t.Fatal(err)
	}
}

// A feed is shown once, stored as a hash, managed by the organization administrator only and
// audited without its secret; revoking it ends it at once.
func TestStatusFeedLifecycle(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	org := TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID}
	for _, bad := range []string{"", " pad", strings.Repeat("n", 65), "line\nbreak"} {
		if _, err := ts.CreateStatusFeed(ctx, org, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateStatusFeed(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	issue, err := ts.CreateStatusFeed(ctx, org, "uptime monitor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issue.Token, StatusFeedPrefix) || len(issue.Token) != len(StatusFeedPrefix)+64 {
		t.Fatalf("issue: %+v", issue)
	}
	var stored string
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT token_hash FROM status_feeds WHERE id=?`), issue.ID).Scan(&stored); err != nil || strings.Contains(issue.Token, stored) {
		t.Fatalf("token stored in the clear or not at all: %q %v", stored, err)
	}
	if !audited(t, st, AuditStatusFeedCreate, issue.ID, "success", issue.Token) {
		t.Fatal("feed creation was not audited")
	}
	list, err := ts.ListStatusFeeds(ctx, org)
	if err != nil || len(list) != 1 || list[0].ID != issue.ID {
		t.Fatalf("list: %+v %v", list, err)
	}

	if err := st.Users().CreateUser(ctx, &User{ID: "usr_op", Username: "usr_op", Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: "usr_op", Role: "environment_admin", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	other := TenantAccess{ActorID: "usr_op", OrganizationID: a.OrganizationID}
	if _, err := ts.CreateStatusFeed(ctx, other, "mine"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an environment administrator minted a feed: %v", err)
	}
	if err := ts.RevokeStatusFeed(ctx, other, issue.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an environment administrator revoked a feed: %v", err)
	}
	if _, err := ts.ListStatusFeeds(ctx, other); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an environment administrator listed feeds: %v", err)
	}
	if !audited(t, st, AuditStatusFeedRevoke, issue.ID, "denied", issue.Token) {
		t.Fatal("a denied revoke was not audited")
	}

	report, err := ts.ReadStatusFeed(ctx, issue.Token, "10.0.0.9")
	if err != nil || len(report.Items) != 0 || report.OrganizationID != a.OrganizationID {
		t.Fatalf("empty organization: %+v %v", report, err)
	}
	if list, _ := ts.ListStatusFeeds(ctx, org); list[0].LastUsedAt == nil || list[0].LastIP != "10.0.0.9" {
		t.Fatalf("read not stamped: %+v", list[0])
	}
	for _, bad := range []string{"", issue.Token[len(StatusFeedPrefix):], issue.Token + "00", StatusFeedPrefix + strings.Repeat("0", 64)} {
		if _, err := ts.ReadStatusFeed(ctx, bad, ""); !errors.Is(err, ErrForbidden) {
			t.Errorf("ReadStatusFeed(%q) = %v, want ErrForbidden", bad, err)
		}
	}
	if err := ts.RevokeStatusFeed(ctx, org, issue.ID); err != nil {
		t.Fatal(err)
	}
	if !audited(t, st, AuditStatusFeedRevoke, issue.ID, "success", issue.Token) {
		t.Fatal("feed revocation was not audited")
	}
	if _, err := ts.ReadStatusFeed(ctx, issue.Token, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a revoked feed still reads: %v", err)
	}
	if err := ts.RevokeStatusFeed(ctx, org, issue.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	if list, _ := ts.ListStatusFeeds(ctx, org); len(list) != 0 {
		t.Fatalf("revoked feed still listed: %+v", list)
	}
}

func TestStatusFeedLimit(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	org := TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID}
	for i := 0; i < maxStatusFeeds; i++ {
		if _, err := st.Tenancy().CreateStatusFeed(ctx, org, "feed"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Tenancy().CreateStatusFeed(ctx, org, "feed"); !errors.Is(err, ErrStatusFeedLimit) {
		t.Fatalf("past the limit: %v", err)
	}
}

// The feed reports offline endpoints and, on live ones, containers that are unhealthy,
// restarting, dead, or exited against a keep-running restart policy. Stale inventory of an
// offline endpoint is not read, and another organization's feed sees none of it.
func TestStatusFeedReportsWhatIsDown(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	live := activeEndpointWith(t, ts, a, []protocol.Container{
		{ID: "c-ok", Name: "/ok", Image: "nginx", State: "running", Health: "healthy", CreatedAt: created},
		{ID: "c-sick", Name: "/sick", Image: "api", State: "running", Health: "unhealthy", Status: "Up 3 minutes (unhealthy)", CreatedAt: created},
		{ID: "c-loop", Name: "/loop", Image: "worker", State: "restarting", CreatedAt: created},
		{ID: "c-dead", Name: "/dead", Image: "db", State: "dead", CreatedAt: created},
		{ID: "c-gone", Name: "/gone", Image: "cache", State: "exited", RestartPolicy: "unless-stopped", CreatedAt: created},
		{ID: "c-job", Name: "/job", Image: "migrate", State: "exited", RestartPolicy: "no", CreatedAt: created},
	}, nil)
	setEndpointState(t, st, live, "live", "active")
	offline := activeEndpointWith(t, ts, a, []protocol.Container{{ID: "c-stale", Name: "/stale", State: "exited", RestartPolicy: "always"}}, nil)
	setEndpointState(t, st, offline, "gone", "offline")
	// A revoked endpoint is not in service, so its last inventory is not down.
	revoked := activeEndpointWith(t, ts, a, []protocol.Container{{ID: "c-old", Name: "/old", State: "dead"}}, nil)
	setEndpointState(t, st, revoked, "retired", "revoked")

	org := TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID}
	issue, err := ts.CreateStatusFeed(ctx, org, "monitor")
	if err != nil {
		t.Fatal(err)
	}
	report, err := ts.ReadStatusFeed(ctx, issue.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DownItem{}
	for _, it := range report.Items {
		got[it.Condition+":"+it.Subject] = it
	}
	want := []string{"offline:", "unhealthy:sick", "restarting:loop", "dead:dead", "exited:gone"}
	if len(got) != len(want) {
		t.Fatalf("items: %+v", report.Items)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %s in %+v", k, report.Items)
		}
	}
	if it := got["offline:"]; it.EndpointID != offline || it.Kind != DownEndpointOffline || it.At.IsZero() || it.Environment != "Before" {
		t.Errorf("offline item: %+v", it)
	}
	if it := got["unhealthy:sick"]; it.EndpointID != live || it.Kind != DownContainer {
		t.Errorf("unhealthy item: %+v", it)
	}

	// The key is stable across reads, so a reader notifies once per incident.
	again, _ := ts.ReadStatusFeed(ctx, issue.Token, "")
	for i := range again.Items {
		if again.Items[i].Key != report.Items[i].Key {
			t.Fatalf("keys changed between reads: %q %q", again.Items[i].Key, report.Items[i].Key)
		}
	}

}

// A feed token reads its own organization only, both ways, and an organization administrator
// cannot list or revoke another organization's feed.
func TestStatusFeedTenantIsolation(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	orgA := TenantAccess{ActorID: a.ActorID, OrganizationID: a.OrganizationID}
	epA := activeEndpointWith(t, ts, a, []protocol.Container{{ID: "c-a", Name: "/only-in-a", State: "dead"}}, nil)
	setEndpointState(t, st, epA, "host-a", "active")
	orgB := newOrgFor(t, st, "org_b")
	envB := TenantAccess{ActorID: orgB.ActorID, OrganizationID: orgB.OrganizationID}
	if err := ts.CreateEnvironment(ctx, &Environment{ID: "env_b", OrganizationID: orgB.OrganizationID, Name: "B"}); err != nil {
		t.Fatal(err)
	}
	envB.EnvironmentID = "env_b"
	epB := activeEndpointWith(t, ts, envB, []protocol.Container{{ID: "c-b", Name: "/only-in-b", State: "dead"}}, nil)
	setEndpointState(t, st, epB, "host-b", "active")

	feedA, err := ts.CreateStatusFeed(ctx, orgA, "a")
	if err != nil {
		t.Fatal(err)
	}
	feedB, err := ts.CreateStatusFeed(ctx, orgB, "b")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ token, org, want, never string }{{feedA.Token, a.OrganizationID, "only-in-a", "only-in-b"}, {feedB.Token, "org_b", "only-in-b", "only-in-a"}} {
		r, err := ts.ReadStatusFeed(ctx, c.token, "")
		if err != nil || r.OrganizationID != c.org || len(r.Items) != 1 || r.Items[0].Subject != c.want {
			t.Fatalf("feed of %s: %+v %v", c.org, r, err)
		}
		for _, it := range r.Items {
			if it.Subject == c.never {
				t.Fatalf("feed of %s reads another organization: %+v", c.org, it)
			}
		}
	}
	// B's administrator names A's feed in B's organization, and in A's.
	if err := ts.RevokeStatusFeed(ctx, orgB, feedA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked another organization's feed: %v", err)
	}
	if err := ts.RevokeStatusFeed(ctx, TenantAccess{ActorID: orgB.ActorID, OrganizationID: a.OrganizationID}, feedA.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-member revoked a feed: %v", err)
	}
	if _, err := ts.ListStatusFeeds(ctx, TenantAccess{ActorID: orgB.ActorID, OrganizationID: a.OrganizationID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-member listed feeds: %v", err)
	}
	if list, _ := ts.ListStatusFeeds(ctx, orgB); len(list) != 1 || list[0].ID != feedB.ID {
		t.Fatalf("B lists: %+v", list)
	}
	if _, err := ts.ReadStatusFeed(ctx, feedA.Token, ""); err != nil {
		t.Fatalf("A's feed after B's attempts: %v", err)
	}
}

// audited reports an audit row of action on resource with result; no row may carry the secret.
func audited(t *testing.T, st *SQLStore, action, resource, result, token string) bool {
	t.Helper()
	all, _, err := st.Audit().ListAuditRecords(context.Background(), 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range all {
		if strings.Contains(r.Details+r.Resource+r.Action, token[len(StatusFeedPrefix):]) {
			t.Fatalf("the secret reached the audit trail: %+v", r)
		}
		found = found || (r.Action == action && r.Resource == resource && r.Result == result)
	}
	return found
}

// A cluster reports each node that is not ready.
func TestStatusFeedReportsNotReadyNodes(t *testing.T) {
	observed := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(protocol.Snapshot{Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "cp-1", Ready: true}, {Name: "worker-2", Ready: false}}}})
	items := downInSnapshot(DownItem{EndpointID: "ep", Endpoint: "cluster"}, protocol.RuntimeKubernetes, observed, raw)
	if len(items) != 1 || items[0].Subject != "worker-2" || items[0].Kind != DownNodeNotReady || !items[0].At.Equal(observed) {
		t.Fatalf("items: %+v", items)
	}
}

func newOrgFor(t *testing.T, st *SQLStore, id string) TenantAccess {
	t.Helper()
	ctx := context.Background()
	user := "usr_" + id
	if err := st.Users().CreateUser(ctx, &User{ID: user, Username: user, Role: "user", Status: "active", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().CreateOrganizationWithAdmin(ctx, &Organization{ID: id, Name: id}, user); err != nil {
		t.Fatal(err)
	}
	return TenantAccess{ActorID: user, OrganizationID: id}
}
