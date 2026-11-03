package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/crypto"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
)

const (
	// StatusFeedPrefix marks a status feed token, so it can never parse as a kyPulse token
	// (64 bare hex characters) or an agent token, and a secret scanner knows it for one.
	StatusFeedPrefix = "kyy_feed_"
	// maxStatusFeeds bounds an organization's live feeds.
	maxStatusFeeds = 20
	// maxFeedEndpoints and maxFeedItems bound one feed read: each endpoint may cost a snapshot
	// decode of up to protocol.MaxSnapshotBytes.
	maxFeedEndpoints = 200
	maxFeedItems     = 200
)

// Audit actions of status feed management; a feed read writes no audit row.
const (
	AuditStatusFeedCreate = "status_feed.create"
	AuditStatusFeedRevoke = "status_feed.revoke"
)

// ErrStatusFeedLimit refuses a feed past maxStatusFeeds live ones.
var ErrStatusFeedLimit = errors.New("too many status feeds")

// ErrStatusFeedThrottled is a valid feed read that the caller's allow func refused.
var ErrStatusFeedThrottled = errors.New("status feed read throttled")

// StatusFeed is an organization's down-monitoring feed. The token is never stored:
// token_hash is its SHA-256.
type StatusFeed struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	Name           string     `json:"name"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	LastIP         string     `json:"last_ip,omitempty"`
}

// StatusFeedIssue is a created feed: the token, once.
type StatusFeedIssue struct {
	StatusFeed
	Token string `json:"token"`
}

// Kinds of DownItem.
const (
	DownEndpointOffline = "endpoint_offline"
	DownContainer       = "container"
	DownNodeNotReady    = "node_not_ready"
)

// DownItem is one thing a status feed reports as down. Key is stable for as long as the same
// thing stays down in the same way, so a reader notifies once per incident. ReadStatusFeed
// returns it opaque: a SHA-256 of the feed ID and the incident, so it carries no endpoint or
// container ID or time, and two feeds key the same incident differently.
type DownItem struct {
	Kind        string
	Key         string
	Environment string
	Endpoint    string
	EndpointID  string
	// Subject is the container or node name; empty for an offline endpoint.
	Subject string
	// Condition is offline, not_ready, unhealthy, restarting, dead or exited.
	Condition string
	// At is when KyYard last heard from the endpoint (offline) or last observed the condition.
	At time.Time
}

// StatusReport is what a status feed shows: everything down in one organization now.
type StatusReport struct {
	OrganizationID string
	Organization   string
	Items          []DownItem
	// Truncated says the organization has more endpoints or down items than one read covers.
	Truncated bool
}

func validFeedName(name string) bool {
	return name != "" && len([]rune(name)) <= 64 && strings.TrimSpace(name) == name && validTenantName(name)
}

// CreateStatusFeed mints a status feed for a.OrganizationID. Only its SHA-256 is stored.
func (t *tenancyStore) CreateStatusFeed(ctx context.Context, a TenantAccess, name string) (*StatusFeedIssue, error) {
	if !validFeedName(name) {
		return nil, ErrInvalid
	}
	secret := make([]byte, serviceTokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	issue := &StatusFeedIssue{StatusFeed: StatusFeed{ID: "feed_" + crypto.RandomHex(12), OrganizationID: a.OrganizationID, Name: name, CreatedBy: a.Principal(), CreatedAt: now}, Token: StatusFeedPrefix + hex.EncodeToString(secret)}
	target := issue.ID
	err := t.runAs(ctx, a, permissions.StatusFeedsManage, AuditStatusFeedCreate, &target, nil, true, func(tx *sql.Tx) error {
		var live int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM status_feeds WHERE organization_id=? AND revoked_at IS NULL`), a.OrganizationID).Scan(&live); err != nil {
			return err
		}
		if live >= maxStatusFeeds {
			return ErrStatusFeedLimit
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO status_feeds (id,organization_id,name,token_hash,created_by,created_at) VALUES (?,?,?,?,?,?)`), issue.ID, a.OrganizationID, name, crypto.SHA256Hex(secret), issue.CreatedBy, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return issue, nil
}

// ListStatusFeeds lists the organization's live feeds, newest first. A revoked feed leaves the
// list; its creation and revocation stay in the audit trail.
func (t *tenancyStore) ListStatusFeeds(ctx context.Context, a TenantAccess) ([]StatusFeed, error) {
	out := []StatusFeed{}
	err := t.readTenant(ctx, a, permissions.StatusFeedsManage, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, t.store.rebind(`SELECT id,organization_id,name,created_by,created_at,last_used_at,last_ip FROM status_feeds WHERE organization_id=? AND revoked_at IS NULL ORDER BY created_at DESC, id LIMIT 200`), a.OrganizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f StatusFeed
			var lastUsed sql.NullTime
			if err := rows.Scan(&f.ID, &f.OrganizationID, &f.Name, &f.CreatedBy, &f.CreatedAt, &lastUsed, &f.LastIP); err != nil {
				return err
			}
			if lastUsed.Valid {
				f.LastUsedAt = &lastUsed.Time
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeStatusFeed ends a feed at once. Another organization's feed is ErrNotFound.
func (t *tenancyStore) RevokeStatusFeed(ctx context.Context, a TenantAccess, id string) error {
	now := time.Now().UTC()
	return t.runAs(ctx, a, permissions.StatusFeedsManage, AuditStatusFeedRevoke, &id, nil, true, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE status_feeds SET revoked_at=?, revoked_by=? WHERE id=? AND organization_id=? AND revoked_at IS NULL`), now, a.Principal(), id, a.OrganizationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// ReadStatusFeed resolves a feed token and reports what is down in its organization. The token
// is the whole authorization: it reads one organization's down list and nothing else. A
// malformed, non-canonical (any upper-case hex), unknown or revoked token is ErrForbidden
// alike. A found feed is then passed to allow, when set, before anything is stamped or
// decoded; a refusal is ErrStatusFeedThrottled. last_used_at and last_ip are stamped at most
// once per lastUsedEvery; reads are not audited.
func (t *tenancyStore) ReadStatusFeed(ctx context.Context, token, ip string, allow func(feedID string) bool) (*StatusReport, error) {
	secret, ok := strings.CutPrefix(token, StatusFeedPrefix)
	raw, err := hex.DecodeString(secret)
	// hex.DecodeString takes either case; only the form CreateStatusFeed issued opens a feed, so
	// case variants are not extra names for it.
	if !ok || err != nil || len(raw) != serviceTokenBytes || hex.EncodeToString(raw) != secret {
		return nil, ErrForbidden
	}
	var id, org, orgName string
	var lastUsed sql.NullTime
	err = t.store.db.QueryRowContext(ctx, t.store.rebind(`SELECT f.id,f.organization_id,o.name,f.last_used_at FROM status_feeds f JOIN organizations o ON o.id=f.organization_id WHERE f.token_hash=? AND f.revoked_at IS NULL`), crypto.SHA256Hex(raw)).Scan(&id, &org, &orgName, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	if allow != nil && !allow(id) {
		return nil, ErrStatusFeedThrottled
	}
	now := time.Now().UTC()
	if !lastUsed.Valid || now.Sub(lastUsed.Time) > lastUsedEvery {
		if _, err := t.store.db.ExecContext(ctx, t.store.rebind(`UPDATE status_feeds SET last_used_at=?, last_ip=? WHERE id=? AND revoked_at IS NULL`), now, ip, id); err != nil {
			return nil, err
		}
	}
	report := &StatusReport{OrganizationID: org, Organization: orgName, Items: []DownItem{}}
	// Only endpoints that have connected are monitored: pending, approved-never-connected,
	// expired and revoked endpoints are not down, they are not in service.
	rows, err := t.store.db.QueryContext(ctx, t.store.rebind(`SELECT e.id,e.name,e.runtime,e.state,e.last_seen_at,COALESCE(v.name,''),i.observed_at,i.snapshot FROM endpoints e LEFT JOIN environments v ON v.id=e.environment_id AND v.organization_id=e.organization_id LEFT JOIN endpoint_inventory i ON i.endpoint_id=e.id WHERE e.organization_id=? AND e.state IN ('active','offline') ORDER BY e.name,e.id LIMIT ?`), org, maxFeedEndpoints+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		if seen++; seen > maxFeedEndpoints {
			report.Truncated = true
			break
		}
		var e DownItem
		var runtime, state string
		var lastSeen, observed sql.NullTime
		var snapshot sql.NullString
		if err := rows.Scan(&e.EndpointID, &e.Endpoint, &runtime, &state, &lastSeen, &e.Environment, &observed, &snapshot); err != nil {
			return nil, err
		}
		if state == "offline" {
			e.Kind, e.Condition, e.Key = DownEndpointOffline, "offline", DownEndpointOffline+":"+e.EndpointID
			if lastSeen.Valid {
				e.At = lastSeen.Time
				e.Key += ":" + lastSeen.Time.UTC().Format(time.RFC3339)
			}
			report.Items = append(report.Items, e)
			continue
		}
		// An offline endpoint's inventory is stale, so only a live one's is read.
		if snapshot.Valid && observed.Valid {
			report.Items = append(report.Items, downInSnapshot(e, runtime, observed.Time, []byte(snapshot.String))...)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(report.Items) > maxFeedItems {
		report.Items, report.Truncated = report.Items[:maxFeedItems], true
	}
	for i := range report.Items {
		report.Items[i].Key = crypto.SHA256Hex([]byte(id + "\x00" + report.Items[i].Key))
	}
	return report, nil
}

// downInSnapshot lists a live endpoint's down containers (Docker) or not-ready nodes
// (Kubernetes). A container is down when its healthcheck reports unhealthy, it is restarting
// or dead, or it exited although its restart policy says it should be running.
func downInSnapshot(endpoint DownItem, runtime string, observed time.Time, raw []byte) []DownItem {
	var s struct {
		Containers []protocol.Container `json:"containers"`
		Kubernetes *struct {
			Nodes []protocol.Node `json:"nodes"`
		} `json:"kubernetes"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	var out []DownItem
	if runtime == protocol.RuntimeKubernetes {
		if s.Kubernetes == nil {
			return nil
		}
		for _, n := range s.Kubernetes.Nodes {
			if n.Ready {
				continue
			}
			item := endpoint
			item.Kind, item.Condition, item.Subject, item.At = DownNodeNotReady, "not_ready", protocol.CleanText(n.Name, 253), observed
			item.Key = DownNodeNotReady + ":" + endpoint.EndpointID + ":" + item.Subject
			out = append(out, item)
		}
		return out
	}
	for _, c := range s.Containers {
		condition := containerDown(c)
		if condition == "" {
			continue
		}
		item := endpoint
		item.Kind, item.Condition, item.At = DownContainer, condition, observed
		// Only the name: an image reference can name a registry by address, and status text
		// is whatever the agent sent.
		item.Subject = protocol.CleanText(strings.TrimPrefix(c.Name, "/"), 255)
		// CreatedAt, not StartedAt: a restarting container starts again and again, and each
		// start must not read as a new incident. A recreated container is a new one.
		item.Key = DownContainer + ":" + condition + ":" + endpoint.EndpointID + ":" + protocol.CleanText(c.ID, 128) + ":" + c.CreatedAt.UTC().Format(time.RFC3339)
		out = append(out, item)
	}
	return out
}

// containerDown is the reason c counts as down, "" when it does not.
func containerDown(c protocol.Container) string {
	switch {
	case c.Health == "unhealthy":
		return "unhealthy"
	case c.State == "restarting", c.State == "dead":
		return c.State
	case c.State == "exited" && (c.RestartPolicy == "always" || c.RestartPolicy == "unless-stopped"):
		return "exited"
	}
	return ""
}
