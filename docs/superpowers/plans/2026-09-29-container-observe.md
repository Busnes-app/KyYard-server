# Container Observe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every Docker container in KyYard shows uptime, health, IP and restart policy, has its own page with Overview, Configuration, Logs, Terminal and Activity tabs, and is acted on through icon buttons that never reflow the table.

**Architecture:** The agent's inventory report gains four fields, read from one bounded `ContainerInspect` per running container inside the existing snapshot. The server stores and clamps them like every other inventory field. The web app adds one route and one page; `ContainerControls` becomes an icon toolbar whose Logs and Terminal buttons link to the page instead of opening modals.

**Tech Stack:** Go 1.26 (`internal/agent/protocol`, `internal/runtime/docker`, `internal/store`, `internal/api`), React 19 + TypeScript + Vite (`web/`), vitest, `lucide-react` icons (already a dependency).

**Spec:** `docs/superpowers/specs/2026-09-29-container-management-design.md`, section 1 (Observe).

## Global Constraints

- Protocol major version stays 1; every wire change is additive and the server decodes both the old and the new shape of `networks`.
- Inventory stays bounded: `MaxContainers = 1000`, `MaxSnapshotBytes = 1 << 20`; the new per-container inspect is capped at `maxSnapshotInspects = 200` running containers and `snapshotInspectBudget = 5 * time.Second` total.
- Health vocabulary is exactly `none`, `starting`, `healthy`, `unhealthy` (same as `protocol.ContainerInspection.Health`); the empty string means "not reported" (older agent).
- Uptime is computed in the browser from `started_at`; the Docker `status` text is never parsed.
- Confirmations are unchanged: `window.confirm` for start, stop, restart; removal preview plus typed-name `window.prompt` for remove.
- `secureFetch` for every write. Fixed error texts; never render server text.
- Every icon button has an `aria-label`; the table row's height must not change when actions are shown.
- Web build: `web/dist` is embedded in the binary and CI fails if the committed `dist` does not match source. The last task rebuilds and commits it.
- Every DOX doc named in Task 11 is updated in the same PR.
- Commit messages end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. **An agent older than this change** sends `networks` as `["shop_default"]` and no `started_at`, `health` or `restart_policy`. The server must accept the report unchanged, the list must show `—` for uptime and IP, and no badge for health. Pinned in Task 1 (protocol decode test) and Task 7 (UI test with a string-network container).
2. **A container that stops between the list read and the inspect** returns 404 from `/containers/{id}/json`. The snapshot must still succeed with zero-value fields for that container. Pinned in Task 2.
3. **A host with 200+ running containers** must not stall the snapshot. Inspects stop at the cap or the budget; containers past it report zero-value fields, the snapshot still reports. Pinned in Task 2.
4. **A container removed while its page is open** must show "no longer reported" and disable every action instead of crashing on an undefined row. Pinned in Task 6.
5. **A container ID that is not 64 hex** in the URL (`/containers/..`, `/containers/x%2Fy`) must land on not-found, never in a fetch to a path the operator did not intend. Pinned in Task 4.

---

### Task 1: Protocol fields and dual-shape network decoding

**Files:**
- Modify: `internal/agent/protocol/inventory.go:176-193` (Container), `:270-320` (Clamp)
- Test: `internal/agent/protocol/inventory_test.go`

**Interfaces:**
- Produces:
  ```go
  type NetworkAttachment struct {
      Name string `json:"name"`
      IP   string `json:"ip,omitempty"`  // IPv4 as the runtime reports it, "" when none
      IP6  string `json:"ip6,omitempty"` // global IPv6, "" when none
  }
  func (n *NetworkAttachment) UnmarshalJSON(b []byte) error // accepts "name" or {name, ip, ip6}
  // Container gains:
  StartedAt     time.Time           `json:"started_at,omitzero"` // zero when not running or not read
  Health        string              `json:"health,omitempty"`    // none|starting|healthy|unhealthy, "" not reported
  RestartPolicy string              `json:"restart_policy,omitempty"`
  Networks      []NetworkAttachment `json:"networks"`
  const MaxNetworkAttachments = 32
  ```

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/protocol/inventory_test.go`:

```go
func TestContainerNetworksDecodeBothShapes(t *testing.T) {
	old := []byte(`{"containers":[{"id":"c1","name":"web","networks":["shop_default","bridge"]}]}`)
	var s Snapshot
	if err := UnmarshalSnapshotBounded(old, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Containers[0].Networks) != 2 || s.Containers[0].Networks[0] != (NetworkAttachment{Name: "shop_default"}) {
		t.Fatalf("string networks: %+v", s.Containers[0].Networks)
	}
	if !s.Containers[0].StartedAt.IsZero() || s.Containers[0].Health != "" {
		t.Fatalf("old agent must report no start or health: %+v", s.Containers[0])
	}
	current := []byte(`{"containers":[{"id":"c1","name":"web","started_at":"2026-09-29T10:00:00Z","health":"healthy","restart_policy":"unless-stopped","networks":[{"name":"shop_default","ip":"172.18.0.3","ip6":""}]}]}`)
	s = Snapshot{}
	if err := UnmarshalSnapshotBounded(current, &s); err != nil {
		t.Fatal(err)
	}
	c := s.Containers[0]
	if c.Networks[0] != (NetworkAttachment{Name: "shop_default", IP: "172.18.0.3"}) || c.Health != "healthy" || c.RestartPolicy != "unless-stopped" || c.StartedAt.Year() != 2026 {
		t.Fatalf("attachment networks: %+v", c)
	}
	var n NetworkAttachment
	if err := json.Unmarshal([]byte(`42`), &n); err == nil {
		t.Fatal("a number is neither shape")
	}
}

func TestClampBoundsNewContainerFields(t *testing.T) {
	var nets []NetworkAttachment
	for i := 0; i < MaxNetworkAttachments+3; i++ {
		nets = append(nets, NetworkAttachment{Name: "n", IP: "not an ip"})
	}
	s := Snapshot{Containers: []Container{{ID: "c1", Health: "bogus\x00", RestartPolicy: strings.Repeat("r", 40), Networks: nets}}}
	Clamp(&s)
	c := s.Containers[0]
	if len(c.Networks) != MaxNetworkAttachments || c.Networks[0].IP != "" {
		t.Fatalf("networks not bounded or ip not validated: %d %q", len(c.Networks), c.Networks[0].IP)
	}
	if c.Health != "" || len(c.RestartPolicy) != 32 {
		t.Fatalf("health %q policy %q", c.Health, c.RestartPolicy)
	}
	s = Snapshot{Containers: []Container{{ID: "c1", Health: "unhealthy", Networks: []NetworkAttachment{{Name: "b", IP: "10.0.0.2", IP6: "fd00::2"}}}}}
	Clamp(&s)
	if s.Containers[0].Health != "unhealthy" || s.Containers[0].Networks[0].IP != "10.0.0.2" || s.Containers[0].Networks[0].IP6 != "fd00::2" {
		t.Fatalf("valid values must survive: %+v", s.Containers[0])
	}
	s = Snapshot{Containers: []Container{{ID: "c1"}}}
	Clamp(&s)
	if s.Containers[0].Networks == nil {
		t.Fatal("nil networks must become an empty list")
	}
}
```

Add `"encoding/json"` and `"strings"` to the test file's imports if absent.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/protocol -run 'TestContainerNetworksDecodeBothShapes|TestClampBoundsNewContainerFields' 2>&1 | head -20`
Expected: compile error, `undefined: NetworkAttachment`.

- [ ] **Step 3: Implement the types**

In `internal/agent/protocol/inventory.go`, replace the `Container` struct and add the attachment type directly above it:

```go
// NetworkAttachment is one network a container is joined to. An agent older than the IP
// fields sent a bare name; UnmarshalJSON accepts both so a report never fails on shape.
type NetworkAttachment struct {
	Name string `json:"name"`
	IP   string `json:"ip,omitempty"`  // IPv4 as the runtime reports it, "" when none
	IP6  string `json:"ip6,omitempty"` // global IPv6, "" when none
}

func (n *NetworkAttachment) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &n.Name)
	}
	type plain NetworkAttachment
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*n = NetworkAttachment(p)
	return nil
}

// MaxNetworkAttachments bounds a container's reported networks.
const MaxNetworkAttachments = 32

// HealthStates is the closed vocabulary of Container.Health and ContainerInspection.Health.
var HealthStates = map[string]bool{"none": true, "starting": true, "healthy": true, "unhealthy": true}

type Container struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	ImageID   string            `json:"image_id"`
	State     string            `json:"state"`  // created, running, paused, restarting, exited, dead
	Status    string            `json:"status"` // human text from the runtime, bounded
	CreatedAt time.Time         `json:"created_at"`
	// StartedAt is zero when the container is not running or the agent could not read it in
	// budget; the UI derives uptime from it and never parses Status.
	StartedAt     time.Time           `json:"started_at,omitzero"`
	Health        string              `json:"health,omitempty"` // HealthStates, "" when not reported
	RestartPolicy string              `json:"restart_policy,omitempty"`
	Ports         []Port              `json:"ports"`
	Labels        map[string]string   `json:"labels"`
	Networks      []NetworkAttachment `json:"networks"`
	// Managed is the Compose project label when present; ownership arrives with M6.
	ComposeProject string `json:"compose_project,omitempty"`
	// Mounts is nil when the agent did not report them (older than mounts), empty when there
	// are none; MountsTruncated says entries past MaxMounts were dropped.
	Mounts          []Mount `json:"mounts"`
	MountsTruncated bool    `json:"mounts_truncated,omitempty"`
}
```

In `Clamp`, replace the two `Networks` blocks (`if len(c.Networks) > 32 {...}` with its loop, and `if c.Networks == nil { c.Networks = []string{} }`) with:

```go
		if !HealthStates[c.Health] {
			c.Health = ""
		}
		c.RestartPolicy = CleanText(c.RestartPolicy, 32)
		if len(c.Networks) > MaxNetworkAttachments {
			c.Networks = c.Networks[:MaxNetworkAttachments]
		}
		for j := range c.Networks {
			n := &c.Networks[j]
			n.Name = CleanText(n.Name, MaxNameBytes)
			if _, err := netip.ParseAddr(n.IP); err != nil {
				n.IP = ""
			}
			if _, err := netip.ParseAddr(n.IP6); err != nil {
				n.IP6 = ""
			}
		}
		if c.Networks == nil {
			c.Networks = []NetworkAttachment{}
		}
```

Add `"net/netip"` to the imports. Search the package for other uses of `Networks` as `[]string` (`grep -rn "Networks\b" internal/agent/protocol/*.go`) and fix any compile errors the same way (a `Shrink` step that drops networks keeps working since it slices the list).

- [ ] **Step 4: Fix the compile fallout in the Docker adapter**

`internal/runtime/docker/docker.go:186-189` appends strings. Change the temporary decoding of `NetworkSettings.Networks` from `map[string]json.RawMessage` to:

```go
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress         string `json:"IPAddress"`
				GlobalIPv6Address string `json:"GlobalIPv6Address"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
```

and the loop to:

```go
		pc.Networks = []protocol.NetworkAttachment{}
		for n, settings := range ct.NetworkSettings.Networks {
			pc.Networks = append(pc.Networks, protocol.NetworkAttachment{Name: n, IP: settings.IPAddress, IP6: settings.GlobalIPv6Address})
		}
		sort.Slice(pc.Networks, func(i, j int) bool { return pc.Networks[i].Name < pc.Networks[j].Name })
```

Update the existing assertion at `internal/runtime/docker/docker_test.go:70` from `snap.Containers[0].Networks[0] != "shop_default"` to `snap.Containers[0].Networks[0].Name != "shop_default"`. Run `go build ./... && go vet ./...` and fix every other `[]string` use of `Networks` the compiler reports (search `internal/store`, `internal/api`, `internal/applications`, `cmd/`).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/agent/protocol ./internal/runtime/docker ./internal/store ./internal/api 2>&1 | tail -20`
Expected: all `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/protocol internal/runtime/docker
git commit -m "protocol: report container start time, health, restart policy and network IPs

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Docker snapshot reads start time and health per running container

**Files:**
- Modify: `internal/runtime/docker/docker.go` (Snapshot, after the containers loop)
- Test: `internal/runtime/docker/docker_test.go`

**Interfaces:**
- Consumes: `protocol.Container.StartedAt`, `.Health`, `.RestartPolicy` from Task 1.
- Produces: private `func (c *Client) enrichRunning(ctx context.Context, containers []protocol.Container)`; constants `maxSnapshotInspects = 200`, `snapshotInspectBudget = 5 * time.Second`.

- [ ] **Step 1: Extend the fake engine and write the failing tests**

In `fakeEngine` (`docker_test.go:16`), add a case before `default`:

```go
		case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
			if id == "c000b" { // the second container vanished between list and inspect
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write([]byte(`{"Id":"` + id + `","State":{"StartedAt":"2026-09-29T09:30:00.123456789Z","Health":{"Status":"healthy","Log":[{"Output":"secret-looking output"}]}},"HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}}}`))
```

Append tests:

```go
func TestSnapshotEnrichesRunningContainers(t *testing.T) {
	srv := fakeEngine(t, 3)
	defer srv.Close()
	snap, err := NewHTTP(srv.URL).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, b := snap.Containers[0], snap.Containers[1]
	if a.StartedAt.IsZero() || a.StartedAt.Nanosecond() != 0 || a.Health != "healthy" || a.RestartPolicy != "unless-stopped" {
		t.Fatalf("first container not enriched (whole seconds): %+v", a)
	}
	if !b.StartedAt.IsZero() || b.Health != "" || b.RestartPolicy != "" {
		t.Fatalf("a container gone at inspect must report zero values, not fail: %+v", b)
	}
	if a.Networks[0].Name != "shop_default" {
		t.Fatalf("networks: %+v", a.Networks)
	}
}

func TestSnapshotInspectsAtMostTheCap(t *testing.T) {
	var inspects atomic.Int32
	srv := fakeEngine(t, maxSnapshotInspects+10)
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json") {
			inspects.Add(1)
		}
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		resp, err := http.Get(srv.URL + r.URL.RequestURI())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer counting.Close()
	defer srv.Close()
	snap, err := NewHTTP(counting.URL).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := inspects.Load(); got != maxSnapshotInspects {
		t.Fatalf("inspected %d, want the cap %d", got, maxSnapshotInspects)
	}
	if !snap.Containers[maxSnapshotInspects+1].StartedAt.IsZero() {
		t.Fatal("a container past the cap must not be enriched")
	}
}
```

Add `"io"`, `"sync/atomic"` to the imports (and `"net/http"`, `"net/http/httptest"` if not already there). If `NewHTTP` is not the constructor name, use the one `TestSnapshotMapsAndBoundsEngineData` uses.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runtime/docker -run 'TestSnapshotEnrichesRunningContainers|TestSnapshotInspectsAtMostTheCap' 2>&1 | head`
Expected: FAIL, `first container not enriched` (or `undefined: maxSnapshotInspects`).

- [ ] **Step 3: Implement the enrichment**

In `docker.go`, after `snap.Containers = append(snap.Containers, pc)` loop ends and before the images read, add `c.enrichRunning(ctx, snap.Containers)`. Add:

```go
// The container list carries no start time or health, so each running container is inspected
// once, bounded in count and time so a large host still reports. A container that cannot be
// read (gone, or past the budget) keeps zero values: the UI shows "—", never a stale guess.
const (
	maxSnapshotInspects   = 200
	snapshotInspectBudget = 5 * time.Second
)

func (c *Client) enrichRunning(parent context.Context, containers []protocol.Container) {
	ctx, cancel := context.WithTimeout(parent, snapshotInspectBudget)
	defer cancel()
	inspected := 0
	for i := range containers {
		if containers[i].State != "running" || inspected >= maxSnapshotInspects || ctx.Err() != nil {
			continue
		}
		inspected++
		var raw struct {
			State struct {
				StartedAt string `json:"StartedAt"`
				// Only the status is read: the health log carries the healthcheck's output.
				Health *struct{ Status string } `json:"Health"`
			} `json:"State"`
			HostConfig struct {
				RestartPolicy struct{ Name string } `json:"RestartPolicy"`
			} `json:"HostConfig"`
		}
		if err := c.get(ctx, "/containers/"+containers[i].ID+"/json", &raw); err != nil {
			continue
		}
		if started, err := time.Parse(time.RFC3339Nano, raw.State.StartedAt); err == nil && started.Year() > 1 {
			containers[i].StartedAt = started.UTC().Truncate(time.Second)
		}
		containers[i].Health = "none"
		if raw.State.Health != nil && protocol.HealthStates[raw.State.Health.Status] {
			containers[i].Health = raw.State.Health.Status
		}
		containers[i].RestartPolicy = raw.HostConfig.RestartPolicy.Name
	}
}
```

Check that `c.get` returns an error on a 404 (read its implementation in `docker.go`); if it decodes an empty body instead, compare `raw.State.StartedAt == ""` and skip.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/runtime/docker 2>&1 | tail -5`
Expected: `ok`. Also run `go test -race ./internal/runtime/docker -run TestSnapshotAgainstLocalDocker -v 2>&1 | tail -5` if Docker is available locally; it logs the counts and must pass.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/docker
git commit -m "docker: inspect running containers for start time, health and restart policy

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Command list filtered by container

**Files:**
- Modify: `internal/store/store.go:345`, `internal/store/commands.go:252-280`, `internal/api/endpoint_handlers.go:506-519`, `internal/api/commands_test.go:49`
- Test: `internal/api/commands_test.go`

**Interfaces:**
- Produces: `ListCommands(ctx, access, endpointID string, containerID string, limit int) ([]Command, error)`; HTTP `GET .../commands?container=<id>&limit=<n>`.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/commands_test.go` (reuse that file's fixtures: it already logs in, approves an agent `ag` and posts commands; copy the setup lines from the test around line 40 that produce `ts`, `ctx`, `ag` and a posted command):

```go
func TestListCommandsFiltersByContainer(t *testing.T) {
	ts, ctx, ag := commandFixture(t) // extract from the existing test if no such helper exists
	access := store.TenantAccess{ActorID: "usr_envadmin", OrganizationID: "a"}
	for _, id := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if _, err := ts.CreateCommand(ctx, access, ag.id, protocol.ActionStart, id, "", "", protocol.Expectation{}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := ts.ListCommands(ctx, access, ag.id, "", 0)
	if err != nil || len(all) < 2 {
		t.Fatalf("all: %v %d", err, len(all))
	}
	only, err := ts.ListCommands(ctx, access, ag.id, strings.Repeat("a", 64), 0)
	if err != nil || len(only) != 1 || only[0].ContainerID != strings.Repeat("a", 64) {
		t.Fatalf("filtered: %v %+v", err, only)
	}
}
```

Adapt `CreateCommand`'s real name and signature from `internal/store/commands.go` (read the function that `handleCreateCommand` calls). If the fixture is awkward, put the test in `internal/store` next to the existing command tests instead; the assertion is the same.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api -run TestListCommandsFiltersByContainer 2>&1 | head`
Expected: compile error, too many arguments to `ListCommands`.

- [ ] **Step 3: Implement**

`internal/store/store.go:345`:
```go
	ListCommands(ctx context.Context, access TenantAccess, endpointID, containerID string, limit int) ([]Command, error)
```

`internal/store/commands.go` `ListCommands`: add the parameter and build the query:
```go
		query, args := commandColumns+` WHERE endpoint_id=? ORDER BY created_at DESC LIMIT ?`, []any{endpointID, limit}
		if containerID != "" {
			query, args = commandColumns+` WHERE endpoint_id=? AND container_id=? ORDER BY created_at DESC LIMIT ?`, []any{endpointID, containerID, limit}
		}
		rows, err := tx.QueryContext(ctx, t.store.rebind(query), args...)
```

`internal/api/endpoint_handlers.go` `handleListCommands`:
```go
	container := r.URL.Query().Get("container")
	if container != "" && !protocol.ValidContainerID(container) {
		s.writeError(w, http.StatusBadRequest, "invalid_container")
		return
	}
	rows, err := s.store.Tenancy().ListCommands(r.Context(), a, id, container, limit)
```
Use the package's existing container-ID validator (grep `ValidContainerID\|containerID = regexp` in `internal/agent/protocol`); if none exists, add `var containerID = regexp.MustCompile("^[0-9a-f]{64}$")` and `func ValidContainerID(s string) bool` to `protocol/inventory.go`. Use the handler file's existing error-writing helper name in place of `writeError`.

Update the caller at `commands_test.go:49` to pass `""`.

- [ ] **Step 4: Run the tests**

Run: `go build ./... && go test -race ./internal/store ./internal/api 2>&1 | tail -5`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/api internal/agent/protocol
git commit -m "api: filter an endpoint's command list by container

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Route, path helper and tab query parameter

**Files:**
- Modify: `web/src/router.ts`, `web/src/App.tsx:110-120`
- Test: `web/src/router.test.ts`

**Interfaces:**
- Produces:
  ```ts
  type Route = ... | { name: 'container'; org: string; endpoint: string; container: string }
  export const containerPath = (org: string, endpoint: string, container: string, tab?: string) => string
  export function useSearchParam(name: string): string  // '' when absent; follows navigate() and popstate
  ```
  `navigate(path)` now compares `pathname + search` so a tab change is one history entry.

- [ ] **Step 1: Write the failing tests**

Append to `web/src/router.test.ts`:

```ts
it('routes a container page and refuses IDs that are not 64 hex', () => {
  const id = 'a'.repeat(64);
  expect(matchRoute(`/organizations/a/endpoints/ep_1/containers/${id}`)).toEqual({ name: 'container', org: 'a', endpoint: 'ep_1', container: id });
  for (const bad of ['..', 'x%2Fy', 'A'.repeat(64), 'a'.repeat(63), 'a'.repeat(65), 'g'.repeat(64)]) {
    expect(matchRoute(`/organizations/a/endpoints/ep_1/containers/${bad}`).name).toBe('notfound');
  }
  expect(containerPath('a', 'ep 1', id)).toBe(`/organizations/a/endpoints/ep%201/containers/${id}`);
  expect(containerPath('a', 'ep_1', id, 'logs')).toBe(`/organizations/a/endpoints/ep_1/containers/${id}?tab=logs`);
});

it('reads a search parameter and follows in-app navigation', async () => {
  const { renderHook, act } = await import('@testing-library/react');
  const { useSearchParam, navigate } = await import('./router');
  const hook = renderHook(() => useSearchParam('tab'));
  expect(hook.result.current).toBe('');
  act(() => navigate('/organizations/a/endpoints/ep_1/containers/' + 'a'.repeat(64) + '?tab=logs'));
  expect(hook.result.current).toBe('logs');
  const entries = window.history.length;
  act(() => navigate(window.location.pathname + '?tab=logs'));
  expect(window.history.length).toBe(entries);
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npx vitest run src/router.test.ts 2>&1 | tail -15`
Expected: FAIL (`containerPath` is not exported; container route is `notfound`).

- [ ] **Step 3: Implement**

In `web/src/router.ts`:

```ts
export type Route =
  | { name: 'dashboard' | 'endpoints' | 'backup' | 'settings' | 'notfound' }
  | { name: 'organization' | 'members' | 'audit'; org: string }
  | { name: 'environment'; org: string; env: string }
  | { name: 'endpoint'; org: string; endpoint: string }
  | { name: 'container'; org: string; endpoint: string; container: string };

// A Docker container ID as the inventory reports it: lowercase 64 hex, nothing else.
const containerID = /^[0-9a-f]{64}$/;
```

In `matchRoute`, after the `endpoints` line:
```ts
    if (parts.length === 6 && parts[2] === 'endpoints' && segment.test(parts[3]) && parts[4] === 'containers' && containerID.test(parts[5])) return { name: 'container', org, endpoint: parts[3], container: parts[5] };
```

Change `navigate`:
```ts
export function navigate(path: string): void {
  if (path !== window.location.pathname + window.location.search) window.history.pushState(null, '', path);
  window.dispatchEvent(new Event(NAV_EVENT));
}
```

Add:
```ts
export function useSearchParam(name: string): string {
  const read = () => new URLSearchParams(window.location.search).get(name) ?? '';
  const [value, setValue] = useState(read);
  useEffect(() => {
    const update = () => setValue(read());
    window.addEventListener('popstate', update);
    window.addEventListener(NAV_EVENT, update);
    return () => { window.removeEventListener('popstate', update); window.removeEventListener(NAV_EVENT, update); };
  }, [name]);
  return value;
}

export const containerPath = (org: string, endpoint: string, container: string, tab?: string) => endpointPath(org, endpoint) + `/containers/${container}` + (tab ? `?tab=${tab}` : '');
```

In `App.tsx` `Screen`, add after the `endpoint` case (the page is created in Task 6; add a placeholder import now so the build stays green):
```tsx
    case 'container': return <ContainerPage key={`${route.org}/${route.endpoint}/${route.container}`} org={route.org} endpoint={route.endpoint} container={route.container} />;
```
and create a minimal `web/src/pages/ContainerPage.tsx`:
```tsx
import React from 'react';
export const ContainerPage: React.FC<{ org: string; endpoint: string; container: string }> = ({ container }) => <div className="ky-page"><h1>{container.slice(0, 12)}</h1></div>;
```

- [ ] **Step 4: Run the tests and typecheck**

Run: `cd web && npx vitest run src/router.test.ts && npx tsc -b 2>&1 | tail -5`
Expected: PASS, no type errors.

- [ ] **Step 5: Commit**

```bash
git add web/src/router.ts web/src/router.test.ts web/src/App.tsx web/src/pages/ContainerPage.tsx
git commit -m "web: container route, path helper and tab query parameter

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Shared container presentation helpers

**Files:**
- Create: `web/src/components/containerFacts.ts`
- Modify: `web/src/tenant.ts:11` (Container type)
- Test: `web/src/components/containerFacts.test.ts`

**Interfaces:**
- Produces:
  ```ts
  // tenant.ts
  export interface NetworkAttachment { name: string; ip?: string; ip6?: string }
  export interface Container { ...existing; started_at?: string; health?: string; restart_policy?: string; networks: (NetworkAttachment | string)[] }
  // containerFacts.ts
  export const attachments = (c: Pick<Container, 'networks'>): NetworkAttachment[]  // normalises strings
  export const primaryIP = (c: Pick<Container, 'networks'>): string                  // first non-empty ip, else ''
  export const uptime = (startedAt: string | undefined, now: number): string         // '' when not running/unknown; '3d 4h', '2h 15m', '45s'
  export const healthBadge = (health: string | undefined): { text: string; className: string } | null
  export const stateBadge = (state: string): string                                   // badge class used in every table
  export const ago = (iso: string, now?: number) => string                            // moved from EndpointPage
  export const bytes = (n: number) => string                                          // moved from EndpointPage
  ```

- [ ] **Step 1: Write the failing tests**

`web/src/components/containerFacts.test.ts`:
```ts
import { expect, it } from 'vitest';
import { attachments, healthBadge, primaryIP, uptime } from './containerFacts';

it('normalises old string networks and picks the first IP', () => {
  expect(attachments({ networks: ['bridge'] })).toEqual([{ name: 'bridge' }]);
  expect(primaryIP({ networks: ['bridge'] })).toBe('');
  expect(primaryIP({ networks: [{ name: 'a' }, { name: 'b', ip: '172.18.0.3' }, { name: 'c', ip: '10.0.0.9' }] })).toBe('172.18.0.3');
});

it('formats uptime from started_at and says nothing when unknown', () => {
  const now = Date.parse('2026-09-29T12:00:00Z');
  expect(uptime(undefined, now)).toBe('');
  expect(uptime('', now)).toBe('');
  expect(uptime('0001-01-01T00:00:00Z', now)).toBe('');
  expect(uptime('2026-09-29T11:59:15Z', now)).toBe('45s');
  expect(uptime('2026-09-29T09:45:00Z', now)).toBe('2h 15m');
  expect(uptime('2026-09-26T08:00:00Z', now)).toBe('3d 4h');
  expect(uptime('2026-09-29T12:00:30Z', now)).toBe('0s');
});

it('maps health to a badge and hides unknown health', () => {
  expect(healthBadge(undefined)).toBeNull();
  expect(healthBadge('')).toBeNull();
  expect(healthBadge('none')).toBeNull();
  expect(healthBadge('healthy')).toEqual({ text: 'healthy', className: 'badge-success' });
  expect(healthBadge('unhealthy')).toEqual({ text: 'unhealthy', className: 'badge-danger' });
  expect(healthBadge('starting')).toEqual({ text: 'starting', className: 'badge-accent' });
  expect(healthBadge('weird')).toBeNull();
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/components/containerFacts.test.ts 2>&1 | tail -5`
Expected: FAIL, cannot resolve `./containerFacts`.

- [ ] **Step 3: Implement**

`web/src/tenant.ts` line 11 becomes:
```ts
export interface NetworkAttachment { name: string; ip?: string; ip6?: string }
// networks holds bare names from an agent older than the IP fields; containerFacts.attachments normalises.
export interface Container { id: string; name: string; image: string; image_id: string; state: string; status: string; created_at: string; started_at?: string; health?: string; restart_policy?: string; ports: Port[]; labels: Record<string, string>; networks: (NetworkAttachment | string)[]; compose_project?: string; mounts?: { kind: string; source: string; target: string; read_only: boolean }[]; mounts_truncated?: boolean }
```

`web/src/components/containerFacts.ts`:
```ts
import type { Container, NetworkAttachment } from '../tenant';

export const attachments = (c: Pick<Container, 'networks'>): NetworkAttachment[] => c.networks.map((n) => typeof n === 'string' ? { name: n } : n);
export const primaryIP = (c: Pick<Container, 'networks'>): string => attachments(c).find((n) => n.ip)?.ip ?? '';

// A zero time (year 1) is the agent saying "unknown"; it is never an uptime.
export const uptime = (startedAt: string | undefined, now: number): string => {
  if (!startedAt) return '';
  const started = Date.parse(startedAt);
  if (!Number.isFinite(started) || new Date(started).getUTCFullYear() < 1970) return '';
  const s = Math.max(0, Math.floor((now - started) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60), h = Math.floor(m / 60), d = Math.floor(h / 24);
  if (h < 1) return `${m}m`;
  if (d < 1) return `${h}h ${m % 60}m`;
  return `${d}d ${h % 24}h`;
};

const healthClasses: Record<string, string> = { healthy: 'badge-success', unhealthy: 'badge-danger', starting: 'badge-accent' };
export const healthBadge = (health: string | undefined) => health && healthClasses[health] ? { text: health, className: healthClasses[health] } : null;

export const stateBadge = (state: string) => `badge ${state === 'running' ? 'badge-success' : state === 'exited' || state === 'dead' ? 'badge-danger' : 'badge-secondary'}`;

export const bytes = (n: number) => n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GiB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(0)} MiB` : `${n} B`;
export const ago = (iso: string, now = Date.now()) => { const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000)); return s < 90 ? `${s}s ago` : s < 5400 ? `${Math.round(s / 60)}m ago` : `${Math.round(s / 3600)}h ago`; };
```

Remove the local `bytes` and `ago` from `EndpointPage.tsx` and import them from `../components/containerFacts`.

- [ ] **Step 4: Run tests and typecheck**

Run: `cd web && npx vitest run src/components/containerFacts.test.ts src/pages/EndpointPage.test.tsx && npx tsc -b 2>&1 | tail -5`
Expected: PASS, no type errors. Fix any test file that builds a `Container` literal without `networks` typed correctly (they use `networks: []`, which still type-checks).

- [ ] **Step 5: Commit**

```bash
git add web/src/tenant.ts web/src/components/containerFacts.ts web/src/components/containerFacts.test.ts web/src/pages/EndpointPage.tsx
git commit -m "web: shared container facts (uptime, IP, health badge)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Container page

**Files:**
- Modify: `web/src/pages/ContainerPage.tsx` (replace the Task 4 placeholder)
- Modify: `web/src/components/ApplicationInspection.tsx` (export `parseInspection` and `Inspection` if not already exported)
- Test: `web/src/pages/ContainerPage.test.tsx`

**Interfaces:**
- Consumes: `useTenantResource`, `canExec`, `Endpoint`, `Inventory`, `Container` (`tenant.ts`); `ContainerLogs`, `ContainerTerminal` (existing); `useSearchParam`, `containerPath`, `endpointPath`, `navigate` (Task 4); `containerFacts` (Task 5); `GET .../commands?container=` (Task 3); `GET .../containers/{c}/inspection`; `GET .../containers/{c}/rollups?hours=24`.
- Produces: `export const ContainerPage: React.FC<{ org: string; endpoint: string; container: string }>`; the page renders `<ContainerControls ... onStatus={setStatus} />` (prop added in Task 7; until then pass nothing).

- [ ] **Step 1: Write the failing tests**

`web/src/pages/ContainerPage.test.tsx`:
```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ContainerPage } from './ContainerPage';
vi.mock('../components/ContainerTerminal', () => ({ ContainerTerminal: () => <p>terminal stub</p> }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); window.history.replaceState(null, '', '/'); });

const id = 'c'.repeat(64);
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const endpoint = { id: 'ep_1', environment_id: 'env-a', name: 'host-1', runtime: 'docker', state: 'active', facts: {}, fingerprint: '', capabilities: ['container.inspect'], alerts: [], created_at: '' };
const container = (over: object = {}) => ({ id, name: 'web', image: 'nginx:1', image_id: 'sha256:1', state: 'running', status: 'Up', created_at: '2026-09-29T08:00:00Z', started_at: '2026-09-29T09:00:00Z', health: 'healthy', restart_policy: 'always', ports: [{ host: 8080, container: 80, protocol: 'tcp' }], labels: { tier: 'web' }, networks: [{ name: 'bridge', ip: '172.17.0.2' }], mounts: [{ kind: 'volume', source: 'data', target: '/data', read_only: false }], ...over });
function stub(role = 'organization_admin', containers: object[] = [container()]) {
  const now = '2026-09-29T10:00:00Z';
  const fetcher = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/organizations') return json([{ id: 'a', name: 'Team', role }]);
    if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 1, observed_at: now, received_at: now, snapshot: { generation: 1, observed_at: now, engine: { runtime: 'docker', version: '29', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers, images: [], networks: [], volumes: [] } });
    if (url.includes('/commands')) return json([{ id: 'cmd1', action: 'container.restart', outcome: 'succeeded', container_id: id, created_at: now }]);
    if (url.endsWith('/rollups?hours=24')) return json([{ container_id: id, hour: now, samples: 60, cpu_avg: 1.5, cpu_max: 3, memory_avg: 1048576, memory_max: 2097152, rx_bytes: 10, tx_bytes: 20, pids_max: 4, restart_count: 0 }]);
    if (url.endsWith('/inspection')) return json({ target: { container_id: id, image_id: 'sha256:1', created_unix: 1 }, observed_at: now, state: 'running', health: 'healthy', restart_count: 0, image_platform: { os: 'linux', architecture: 'amd64' }, restart_policy: 'always', restart_retries: 0, ports: [], mounts: { bind: 0, volume: 1, tmpfs: 0, other: 0, read_only: 0 }, network_mode: 'bridge', network_count: 1, privileged: false, read_only_rootfs: false, auto_remove: false, configuration_verified: true, unsupported: [] });
    return json(endpoint);
  });
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}

it('shows overview facts with live uptime, IP, health and links back to the host', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date('2026-09-29T10:00:00Z'));
  stub();
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByRole('heading', { level: 1, name: /web/ })).toBeTruthy();
  const facts = screen.getByRole('region', { name: 'Overview' });
  expect(facts.textContent).toContain('1h 0m');
  expect(facts.textContent).toContain('172.17.0.2');
  expect(facts.textContent).toContain('always');
  expect(facts.textContent).toContain('tier=web');
  expect(facts.textContent).toContain('/data');
  expect(screen.getByText('healthy')).toBeTruthy();
  await act(async () => { vi.advanceTimersByTime(60_000); });
  expect(facts.textContent).toContain('1h 1m');
  expect(screen.getByRole('link', { name: 'host-1' }).getAttribute('href')).toBe('/organizations/a/endpoints/ep_1');
});

it('opens the tab named in the URL and puts tab changes in the URL', async () => {
  stub();
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=logs`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByRole('button', { name: 'Load logs' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Activity' }));
  expect(window.location.search).toBe('?tab=activity');
  expect(await screen.findByText(/container.restart/)).toBeTruthy();
});

it.each([['organization_admin', true], ['operator', false]])('offers the Terminal tab to %s: %s', async (role, want) => {
  stub(role);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  await screen.findByRole('heading', { level: 1, name: /web/ });
  await act(async () => {});
  expect(screen.queryByRole('button', { name: 'Terminal' }) !== null).toBe(want);
});

it('says when the container is no longer reported and disables actions', async () => {
  stub('organization_admin', []);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/no longer reported/)).toBeTruthy();
  expect(screen.queryByRole('button', { name: /Restart web/ })).toBeNull();
  expect(screen.getByRole('link', { name: 'host-1' })).toBeTruthy();
});

it('shows the redacted inspection under Configuration', async () => {
  stub();
  window.history.replaceState(null, '', `/organizations/a/endpoints/ep_1/containers/${id}?tab=configuration`);
  render(<ContainerPage org="a" endpoint="ep_1" container={id} />);
  expect(await screen.findByText(/Network mode/)).toBeTruthy();
  expect(screen.getByRole('region', { name: 'Configuration' }).textContent).toContain('bridge');
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/pages/ContainerPage.test.tsx 2>&1 | tail -15`
Expected: FAIL (no heading "web", no region "Overview").

- [ ] **Step 3: Implement the page**

Check `ApplicationInspection.tsx` exports `parseInspection` and the `Inspection` type; if they are module-private, add `export` to both (no other change).

`web/src/pages/ContainerPage.tsx`:
```tsx
import React, { lazy, Suspense, useEffect, useState } from 'react';
import { Box } from 'lucide-react';
import { Link } from '../components/Link';
import { EmptyNotice, StateNotice } from '../components/StateNotice';
import { ContainerControls, ContainerLogs } from '../components/ContainerControls';
import { displayName } from '../components/Endpoints';
import { ContainerPorts } from '../components/ContainerPorts';
import { ago, attachments, bytes, healthBadge, stateBadge, uptime } from '../components/containerFacts';
import { parseInspection, type Inspection } from '../components/ApplicationInspection';
import { containerPath, endpointPath, navigate, useSearchParam } from '../router';
import { canExec, useTenantResource, type Container, type Endpoint, type Inventory, type MemberOrganization } from '../tenant';
const ContainerTerminal = lazy(() => import('../components/ContainerTerminal').then((m) => ({ default: m.ContainerTerminal })));

const TABS = ['overview', 'configuration', 'logs', 'terminal', 'activity'] as const;
type Tab = typeof TABS[number];
interface Command { id: string; action: string; outcome: string; detail?: string; created_at: string }
interface Rollup { hour: string; samples: number; cpu_avg: number; cpu_max: number; memory_avg: number; memory_max: number; rx_bytes: number; tx_bytes: number; pids_max: number; restart_count: number }

// Ticks once a second while mounted so uptime is live; the value is the clock, not the row.
function useNow() {
  const [now, setNow] = useState(Date.now());
  useEffect(() => { const t = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(t); }, []);
  return now;
}

export const ContainerPage: React.FC<{ org: string; endpoint: string; container: string }> = ({ org, endpoint, container }) => {
  const base = `/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(endpoint)}`;
  const details = useTenantResource<Endpoint>(base);
  const inventory = useTenantResource<Inventory>(`${base}/inventory`);
  const organizations = useTenantResource<MemberOrganization[]>('/api/organizations');
  const role = (organizations.data ?? []).find((o) => o.id === org)?.role;
  const exec = canExec(role);
  const requested = useSearchParam('tab');
  const tab: Tab = (TABS as readonly string[]).includes(requested) ? requested as Tab : 'overview';
  const [status, setStatus] = useState('');
  useEffect(() => {
    if (inventory.state === 'denied') return;
    const t = window.setInterval(() => { if (!document.hidden) inventory.reload(); }, 30_000);
    return () => window.clearInterval(t);
  }, [inventory.state, inventory.reload]);
  const e = details.data;
  const c = inventory.data?.snapshot.containers.find((row) => row.id === container) ?? null;
  const active = e?.state === 'active';
  const scope = `Host ${displayName(e?.name ?? endpoint)} · Endpoint ${endpoint}`;
  const tabs = TABS.filter((t) => t !== 'terminal' || exec);
  return <div className="ky-page ky-container-page">
    <nav aria-label="Breadcrumb" className="ky-subnav"><Link to="/endpoints">Endpoints</Link><span>/</span><Link to={endpointPath(org, endpoint)}>{e?.name ?? endpoint}</Link></nav>
    <div className="ky-page-heading">
      <h1 style={{ fontSize: 24 }}><Box size={24} style={{ color: 'var(--accent)' }} /><span>{c ? displayName(c.name) : container.slice(0, 12)}</span>
        {c && <span className={stateBadge(c.state)} title={c.status}>{displayName(c.state)}</span>}
        {c && healthBadge(c.health) && <span className={`badge ${healthBadge(c.health)!.className}`}>{healthBadge(c.health)!.text}</span>}
      </h1>
      {c && <ContainerControls key={c.id} base={base} container={c} active={active} scope={scope} onRefresh={inventory.reload} canExec={exec} onStatus={setStatus} />}
    </div>
    {c && <p style={{ color: 'var(--ink)' }} title={c.image}>{displayName(c.image)}</p>}
    {status && <p role="status">{status}</p>}
    <StateNotice state={details.state} onRetry={details.reload} />
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {inventory.state === 'ready' && !c && <EmptyNotice>This container is no longer reported by <Link to={endpointPath(org, endpoint)}>{e?.name ?? endpoint}</Link>. It may have been removed or renamed.</EmptyNotice>}
    {c && <>
      <nav aria-label="Container sections" className="ky-resource-tabs">
        {tabs.map((t) => <button type="button" key={t} aria-pressed={tab === t} onClick={() => navigate(containerPath(org, endpoint, container, t === 'overview' ? undefined : t))}>{t[0].toUpperCase() + t.slice(1)}</button>)}
      </nav>
      {tab === 'overview' && <Overview base={base} container={c} received={inventory.data?.received_at ?? ''} />}
      {tab === 'configuration' && <Configuration base={base} container={c} capable={(e?.capabilities ?? []).includes('container.inspect')} />}
      {tab === 'logs' && <section className="panel" aria-label="Logs"><ContainerLogs key={c.id} url={`${base}/containers/${encodeURIComponent(c.id)}/logs`} name={c.name} /></section>}
      {tab === 'terminal' && exec && <section className="panel" aria-label="Terminal">{c.state === 'running' && active ? <Suspense fallback={<p role="status">Loading terminal…</p>}><ContainerTerminal key={`${base}/${c.id}/${c.image_id}`} base={base} container={c} scope={scope} /></Suspense> : <EmptyNotice>The terminal needs a running container on an active host.</EmptyNotice>}</section>}
      {tab === 'activity' && <Activity base={base} container={c.id} />}
    </>}
  </div>;
};

function Overview({ base, container: c, received }: { base: string; container: Container; received: string }) {
  const now = useNow();
  const rollups = useTenantResource<Rollup[]>(`${base}/containers/${encodeURIComponent(c.id)}/rollups?hours=24`);
  const up = uptime(c.started_at, now);
  const nets = attachments(c);
  const latest = Array.isArray(rollups.data) && rollups.data.length ? rollups.data[rollups.data.length - 1] : null;
  return <section className="panel" aria-label="Overview">
    <dl className="ky-facts">
      <dt>State</dt><dd>{displayName(c.state)}{c.status ? ` · ${displayName(c.status)}` : ''}</dd>
      <dt>Uptime</dt><dd>{up || '—'}</dd>
      <dt>Started</dt><dd>{up ? new Date(c.started_at!).toLocaleString() : '—'}</dd>
      <dt>Created</dt><dd>{c.created_at ? new Date(c.created_at).toLocaleString() : '—'}</dd>
      <dt>Image</dt><dd><span title={c.image}>{displayName(c.image)}</span><br /><small className="font-mono">{c.image_id}</small></dd>
      <dt>Restart policy</dt><dd>{c.restart_policy ? displayName(c.restart_policy) : '—'}</dd>
      <dt>Networks</dt><dd>{nets.length ? <ul className="ky-list">{nets.map((n) => <li key={n.name}>{displayName(n.name)}{n.ip ? ` · ${n.ip}` : ''}{n.ip6 ? ` · ${n.ip6}` : ''}</li>)}</ul> : '—'}</dd>
      <dt>Ports</dt><dd><ContainerPorts ports={c.ports} /></dd>
      <dt>Mounts</dt><dd>{c.mounts?.length ? <ul className="ky-list">{c.mounts.map((m) => <li key={m.target}>{displayName(m.kind)} {displayName(m.source)} → {displayName(m.target)}{m.read_only ? ' (read-only)' : ''}</li>)}</ul> : '—'}{c.mounts_truncated ? ' (list truncated)' : ''}</dd>
      <dt>Labels</dt><dd>{Object.keys(c.labels).length ? <ul className="ky-list">{Object.entries(c.labels).map(([k, v]) => <li key={k} style={{ overflowWrap: 'anywhere' }}>{displayName(k)}={displayName(v)}</li>)}</ul> : '—'}</dd>
      {c.compose_project && <><dt>Compose project</dt><dd>{displayName(c.compose_project)}</dd></>}
      <dt>Container ID</dt><dd className="font-mono" style={{ fontSize: 11 }}>{c.id}</dd>
      <dt>Reported</dt><dd>{received ? ago(received, now) : '—'}</dd>
    </dl>
    <h2 style={{ fontSize: 16, marginTop: 16 }}>Last 24 hours</h2>
    <StateNotice state={rollups.state} onRetry={rollups.reload} />
    {rollups.state === 'ready' && (latest ? <dl className="ky-facts">
      <dt>CPU</dt><dd>avg {latest.cpu_avg.toFixed(1)}% · peak {latest.cpu_max.toFixed(1)}%</dd>
      <dt>Memory</dt><dd>avg {bytes(latest.memory_avg)} · peak {bytes(latest.memory_max)}</dd>
      <dt>Network</dt><dd>rx {bytes(latest.rx_bytes)} · tx {bytes(latest.tx_bytes)}</dd>
      <dt>Processes</dt><dd>peak {latest.pids_max}</dd>
      <dt>Restarts</dt><dd>{latest.restart_count}</dd>
    </dl> : <EmptyNotice>No usage samples yet.</EmptyNotice>)}
  </section>;
}

function Configuration({ base, container: c, capable }: { base: string; container: Container; capable: boolean }) {
  const [result, setResult] = useState<{ kind: 'loading' } | { kind: 'ready'; data: Inspection } | { kind: 'error'; text: string }>({ kind: 'loading' });
  useEffect(() => {
    if (!capable) return;
    const controller = new AbortController();
    (async () => {
      try {
        const r = await fetch(`${base}/containers/${encodeURIComponent(c.id)}/inspection`, { signal: controller.signal, cache: 'no-store' });
        if (controller.signal.aborted) return;
        if (!r.ok) { setResult({ kind: 'error', text: r.status === 403 ? 'You do not have permission to inspect this container.' : r.status === 429 ? 'Inspection capacity reached. Try again later.' : 'Inspection is unavailable right now.' }); return; }
        const data = parseInspection(await r.json(), { container_id: c.id, image_id: c.image_id, created_unix: Math.floor(Date.parse(c.created_at) / 1000) });
        if (!controller.signal.aborted) setResult(data ? { kind: 'ready', data } : { kind: 'error', text: 'The inspection did not match this container.' });
      } catch { if (!controller.signal.aborted) setResult({ kind: 'error', text: 'The inspection connection was lost.' }); }
    })();
    return () => controller.abort();
  }, [base, c.id, c.image_id, c.created_at, capable]);
  return <section className="panel" aria-label="Configuration">
    <p>Read-only observation of the running container. Environment values, commands and paths are not shown here.</p>
    {!capable && <EmptyNotice>Upgrade the host agent to enable live inspection.</EmptyNotice>}
    {capable && result.kind === 'loading' && <p role="status">Inspecting container…</p>}
    {result.kind === 'error' && <p role="status">{result.text}</p>}
    {result.kind === 'ready' && <dl className="ky-facts">
      <dt>Restart policy</dt><dd>{displayName(result.data.restart_policy)}{result.data.restart_retries ? ` (${result.data.restart_retries} retries)` : ''}</dd>
      <dt>Network mode</dt><dd>{displayName(result.data.network_mode)} · {result.data.network_count} networks</dd>
      <dt>Mounts</dt><dd>{result.data.mounts.volume} volumes · {result.data.mounts.bind} binds · {result.data.mounts.tmpfs} tmpfs · {result.data.mounts.read_only} read-only</dd>
      <dt>Platform</dt><dd>{result.data.image_platform.os}/{result.data.image_platform.architecture}</dd>
      <dt>Flags</dt><dd>{[result.data.privileged && 'privileged', result.data.read_only_rootfs && 'read-only root', result.data.auto_remove && 'auto-remove'].filter(Boolean).join(', ') || 'none'}</dd>
      <dt>Observed</dt><dd>{new Date(result.data.observed_at).toLocaleString()}</dd>
    </dl>}
  </section>;
}

function Activity({ base, container }: { base: string; container: string }) {
  const commands = useTenantResource<Command[]>(`${base}/commands?container=${encodeURIComponent(container)}&limit=50`);
  return <section className="panel" aria-label="Activity">
    <div className="panel-header"><h2>Recent activity</h2><button className="btn-secondary" onClick={commands.reload}>Refresh</button></div>
    <StateNotice state={commands.state} onRetry={commands.reload} />
    {commands.state === 'ready' && Array.isArray(commands.data) && (commands.data.length ? <ul className="ky-list">{commands.data.map((k) => <li key={k.id}>{new Date(k.created_at).toLocaleString()} · {k.action} · {k.outcome || 'pending'}{k.detail ? ` — ${k.detail}` : ''}</li>)}</ul> : <EmptyNotice>No commands have been sent to this container.</EmptyNotice>)}
  </section>;
}
```

Check the exact field names `parseInspection` expects (`web/src/components/ApplicationInspection.tsx:49-81`) and adjust the `Inspection` property names used above to match. `ContainerControls` does not yet accept `onStatus`; add the optional prop in Task 7 and, for now, omit it from the JSX so the build passes (the test for status placement lives in Task 7).

- [ ] **Step 4: Run the tests and typecheck**

Run: `cd web && npx vitest run src/pages/ContainerPage.test.tsx && npx tsc -b 2>&1 | tail -10`
Expected: PASS (the "Terminal" test may need the Task 7 button; if `ContainerControls` still renders the old disclosure, the test still passes because the tab bar has a "Terminal" button).

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/ContainerPage.tsx web/src/pages/ContainerPage.test.tsx web/src/components/ApplicationInspection.tsx
git commit -m "web: container page with overview, configuration, logs, terminal and activity tabs

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Icon action toolbar replacing the disclosure and the modals

**Files:**
- Modify: `web/src/components/ContainerControls.tsx:1-84` (the `ContainerControls` component; `ContainerLogs` stays), `web/src/styles/theme.css:419-427, 432, 469-470`
- Test: `web/src/components/ContainerControls.test.tsx`, `web/src/pages/EndpointPage.test.tsx`

**Interfaces:**
- Produces:
  ```tsx
  export function ContainerControls(props: { base: string; container: Container; active: boolean; scope: string; onRefresh: () => void; canExec: boolean; org: string; endpoint: string; onStatus?: (text: string) => void }): JSX.Element
  ```
  Renders `<div className="ky-container-actions" role="group" aria-label={`Actions for ${name}`}>` with icon buttons labelled `Start <name>`, `Stop <name>`, `Restart <name>`, `Remove <name>` and links labelled `Logs for <name>`, `Terminal for <name>` (links to `containerPath(org, endpoint, id, 'logs' | 'terminal')`). When `onStatus` is given, messages go there and nothing is rendered inline; otherwise a `<p role="status">` follows the group as today.

- [ ] **Step 1: Rewrite the failing tests**

Replace the body of `web/src/components/ContainerControls.test.tsx` tests that click `Actions` with toolbar interactions. Keep the log-bounding test. New file content for the toolbar tests:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ContainerControls, ContainerLogs } from './ContainerControls';
import type { Container } from '../tenant';
const id = 'a'.repeat(64);
const container: Container = { id, name: 'web', image: 'nginx:1', image_id: 'sha256:123', state: 'running', status: 'Up', ports: [], networks: [], labels: {}, created_at: '' };
const props = { base: '/api/org/endpoint', container, active: true, scope: 'Team / Production / Host', onRefresh: () => {}, org: 'org', endpoint: 'endpoint' };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it('dispatches the observed ID and state with CSRF and never automatically retries an unknown result', async () => {
  document.cookie = 'ky_csrf=test-token';
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  const fetcher = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(JSON.stringify({ id: 'cmd', action: 'container.restart', outcome: 'unknown' }), { status: 202 }));
  vi.stubGlobal('fetch', fetcher);
  render(<ContainerControls {...props} canExec={false} />);
  fireEvent.click(screen.getByRole('button', { name: 'Restart web' }));
  expect(await screen.findByText('container.restart: unknown')).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(1);
  const call = fetcher.mock.calls[0];
  expect(call?.[0]).toBe('/api/org/endpoint/commands');
  expect(JSON.parse(String(call?.[1]?.body))).toEqual({ action: 'container.restart', container: id, confirm: '', expects: { state: 'running', image_digest: 'sha256:123' } });
  expect(new Headers(call?.[1]?.headers).get('X-CSRF-Token')).toBe('test-token');
  expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('Team / Production / Host'));
});

it('renders every action as a labelled icon control and adds no expandable content', () => {
  render(<ContainerControls {...props} canExec />);
  const group = screen.getByRole('group', { name: 'Actions for web' });
  for (const name of ['Stop web', 'Restart web', 'Remove web']) expect(screen.getByRole('button', { name })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Logs for web' }).getAttribute('href')).toBe(`/organizations/org/endpoints/endpoint/containers/${id}?tab=logs`);
  expect(screen.getByRole('link', { name: 'Terminal for web' }).getAttribute('href')).toBe(`/organizations/org/endpoints/endpoint/containers/${id}?tab=terminal`);
  expect(screen.queryByRole('button', { name: 'Start web' })).toBeNull();
  expect(group.querySelector('details, dialog')).toBeNull();
  expect(screen.queryByText('Actions')).toBeNull();
});

it('shows Start for a stopped container and disables Remove while running', () => {
  render(<ContainerControls {...props} container={{ ...container, state: 'exited' }} canExec={false} />);
  expect(screen.getByRole('button', { name: 'Start web' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Stop web' })).toBeNull();
  expect((screen.getByRole('button', { name: 'Remove web' }) as HTMLButtonElement).disabled).toBe(false);
  cleanup();
  render(<ContainerControls {...props} canExec={false} />);
  expect((screen.getByRole('button', { name: 'Remove web' }) as HTMLButtonElement).disabled).toBe(true);
});

it.each([false, true])('offers the Terminal link only when the role may exec (%s)', (canExec) => {
  render(<ContainerControls {...props} canExec={canExec} />);
  expect(screen.queryByRole('link', { name: 'Terminal for web' }) !== null).toBe(canExec);
  expect(screen.getByRole('link', { name: 'Logs for web' })).toBeTruthy();
});

it('reports status through onStatus instead of inline when given', async () => {
  vi.spyOn(window, 'confirm').mockReturnValue(true);
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ id: 'cmd', action: 'container.stop', outcome: 'succeeded' }), { status: 200 })));
  const onStatus = vi.fn();
  render(<ContainerControls {...props} canExec={false} onStatus={onStatus} />);
  fireEvent.click(screen.getByRole('button', { name: 'Stop web' }));
  await waitFor(() => expect(onStatus).toHaveBeenCalledWith('container.stop: succeeded'));
  expect(screen.queryByRole('status')).toBeNull();
});

it('bounds the browser log display and aborts its request on unmount', async () => {
  const fetcher = vi.fn(async () => new Response('x'.repeat(300000)));
  vi.stubGlobal('fetch', fetcher);
  const view = render(<ContainerLogs url="/api/logs" name="web" />);
  fireEvent.click(screen.getByRole('button', { name: 'Load logs' }));
  await waitFor(() => expect(view.container.querySelector('pre')?.textContent?.length).toBe(262144));
  expect(screen.getByRole('status').textContent).toContain('last 256 KiB');
  view.unmount();
});
```

Keep any other existing test in that file that exercises removal preview or `ContainerLogs` follow, re-targeting `screen.getByText('Actions')` clicks to nothing and `{ name: 'Remove' }` to `{ name: 'Remove web' }`.

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/components/ContainerControls.test.tsx 2>&1 | tail -15`
Expected: FAIL (no role group, no `Restart web`).

- [ ] **Step 3: Implement the toolbar**

Replace the `ContainerControls` function in `ContainerControls.tsx` (keep its command polling and `act` logic verbatim; only the state for `showLogs`/`showTerminal`, the dialog refs and the JSX change):

```tsx
import { useEffect, useRef, useState } from 'react';
import { Play, RotateCw, ScrollText, Square, Terminal, Trash2 } from 'lucide-react';
import { Link } from './Link';
import { containerPath } from '../router';
import { secureFetch } from '../api';
import type { Container } from '../tenant';

interface Command { id: string; action: string; outcome: string; detail?: string }
export function ContainerControls({ base, container, active, scope, onRefresh, canExec, org, endpoint, onStatus }: { base: string; container: Container; active: boolean; scope: string; onRefresh: () => void; canExec: boolean; org: string; endpoint: string; onStatus?: (text: string) => void }) {
  const [busy, setBusy] = useState(false);
  const [inline, setInline] = useState('');
  const [command, setCommand] = useState<Command | null>(null);
  const setMessage = (text: string) => { if (onStatus) onStatus(text); else setInline(text); };
  // ... existing alive ref, command polling effect and act() unchanged, but every setMessage call is the helper above ...
  const disabled = busy || !active || (command !== null && !command.outcome);
  const name = container.name;
  const icon = 16;
  return <>
    <div className="ky-container-actions" role="group" aria-label={`Actions for ${name}`}>
      {container.state !== 'running' && <button type="button" className="btn-secondary ky-icon-button" aria-label={`Start ${name}`} title="Start" disabled={disabled} onClick={() => void act('start')}><Play size={icon} aria-hidden /></button>}
      {container.state === 'running' && <>
        <button type="button" className="btn-secondary ky-icon-button" aria-label={`Stop ${name}`} title="Stop" disabled={disabled} onClick={() => void act('stop')}><Square size={icon} aria-hidden /></button>
        <button type="button" className="btn-secondary ky-icon-button" aria-label={`Restart ${name}`} title="Restart" disabled={disabled} onClick={() => void act('restart')}><RotateCw size={icon} aria-hidden /></button>
      </>}
      <Link className="btn btn-secondary ky-icon-button" aria-label={`Logs for ${name}`} title="Logs" to={containerPath(org, endpoint, container.id, 'logs')}><ScrollText size={icon} aria-hidden /></Link>
      {canExec && <Link className="btn btn-secondary ky-icon-button" aria-label={`Terminal for ${name}`} title="Terminal" to={containerPath(org, endpoint, container.id, 'terminal')}><Terminal size={icon} aria-hidden /></Link>}
      <button type="button" className="btn-secondary ky-icon-button" aria-label={`Remove ${name}`} title="Remove" disabled={disabled || ['running', 'paused', 'restarting'].includes(container.state)} onClick={() => void act('remove')}><Trash2 size={icon} aria-hidden /></button>
    </div>
    {!onStatus && inline && <p role="status">{inline}</p>}
  </>;
}
```

Delete the `lazy`/`Suspense` import of `ContainerTerminal` from this file, the `logDialog`/`terminalDialog` refs and effects, and both `<dialog>` blocks. `ContainerLogs` stays exported and unchanged.

CSS in `theme.css`: replace lines 419-423 and 427 with
```css
.ky-container-actions { display: inline-flex; flex-wrap: wrap; gap: 4px; }
.ky-icon-button { min-height: 34px; min-width: 34px; padding: 6px; display: inline-flex; align-items: center; justify-content: center; }
```
Remove `.ky-log-dialog` (line 432) only if no other file uses it (`grep -rn ky-log-dialog web/src`); `KubernetesCluster.tsx` and `ApplicationInspection.tsx` use it, so keep it. In the 760px media block replace lines 469-470 with `.ky-icon-button { min-height: 40px; min-width: 40px; }`.

Update callers: `EndpointPage.tsx:111` and `Dashboard.tsx` `HostContainers` pass `org={org} endpoint={endpoint}` (Dashboard has both in scope; in `EndpointPage` they are the props). `ContainerPage.tsx` passes `org`, `endpoint` and `onStatus={setStatus}`. In `EndpointPage.tsx` add `const [status, setStatus] = useState('')` and render `{status && <p role="status">{status}</p>}` directly above the containers `ResourceTable`, passing `onStatus={setStatus}` to each row's controls. Do the same in `Dashboard.tsx` `HostContainers`.

Update `EndpointPage.test.tsx`: the three places that click `findByLabelText('Actions for web')` then a `Terminal`/`Logs` button (lines 135, 222) now assert links instead. Rewrite the polling test at line 130 so it asserts that a container added ahead does not remount the row's controls (`rowKey` still keys rows) by checking `screen.getByRole('group', { name: 'Actions for web' })` is the same element before and after the poll. Rewrite `terminalOffered` to return `screen.queryByRole('link', { name: 'Terminal for web' }) !== null`. Line 277's `expect(screen.queryByText('Actions')).toBeNull()` stays valid. Remove `showModal` stubs that only served the dialogs.

- [ ] **Step 4: Run the web suite and typecheck**

Run: `cd web && npx vitest run && npx tsc -b 2>&1 | tail -10`
Expected: all PASS, no type errors.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/ContainerControls.tsx web/src/components/ContainerControls.test.tsx web/src/pages/EndpointPage.tsx web/src/pages/EndpointPage.test.tsx web/src/pages/Dashboard.tsx web/src/pages/ContainerPage.tsx web/src/styles/theme.css
git commit -m "web: icon action toolbar; logs and terminal open on the container page

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: List columns and container links

**Files:**
- Modify: `web/src/pages/EndpointPage.tsx:111` (containers `ResourceTable`), `web/src/pages/Dashboard.tsx` (`HostContainers` table)
- Test: `web/src/pages/EndpointPage.test.tsx`, `web/src/pages/Dashboard.test.tsx`

**Interfaces:**
- Consumes: `containerFacts` (Task 5), `containerPath` (Task 4).

- [ ] **Step 1: Write the failing tests**

Append to `EndpointPage.test.tsx` (reuse that file's `json`, `endpoint` and organization stubs):

```tsx
it('lists uptime, IP and health and links each container to its page', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-29T10:00:00Z'));
  const now = '2026-09-29T09:59:00Z';
  const id = 'd'.repeat(64);
  const containers = [
    { id, name: 'web', image: 'nginx:1', image_id: 'i', state: 'running', status: 'Up', created_at: '', started_at: '2026-09-29T07:30:00Z', health: 'unhealthy', ports: [], labels: {}, networks: [{ name: 'bridge', ip: '172.17.0.5' }] },
    { id: 'e'.repeat(64), name: 'old', image: 'redis:7', image_id: 'j', state: 'running', status: 'Up', created_at: '', ports: [], labels: {}, networks: ['bridge'] },
  ];
  const fetcher = vi.fn(async (input: RequestInfo | URL) => { const url = String(input); if (url === '/api/organizations') return json([{ id: 'a', name: 'Team', role: 'operator' }]); if (url.endsWith('/inventory')) return json({ endpoint_id: 'ep_1', state: 'active', generation: 1, observed_at: now, received_at: now, snapshot: { generation: 1, observed_at: now, engine: { runtime: 'docker', version: '29', api_version: '1.55', os: 'linux', arch: 'x86_64', kernel: '7', cpus: 1, memory_bytes: 1, hostname: 'h' }, containers, images: [], networks: [], volumes: [] } }); return url.endsWith('/samples') || url.includes('/commands') || url.endsWith('/applications') ? json([]) : json(endpoint); });
  vi.stubGlobal('fetch', fetcher);
  render(<EndpointPage org="a" endpoint="ep_1" />);
  const table = await screen.findByRole('table');
  const head = within(table).getAllByRole('columnheader').map((h) => h.textContent);
  expect(head).toEqual(['Container', 'Status', 'Uptime', 'IP', 'Usage', 'Actions']);
  const [web, old] = within(table).getAllByRole('row').slice(1);
  expect(within(web).getByRole('link', { name: 'web' }).getAttribute('href')).toBe(`/organizations/a/endpoints/ep_1/containers/${id}`);
  expect(web.textContent).toContain('2h 30m');
  expect(web.textContent).toContain('172.17.0.5');
  expect(within(web).getByText('unhealthy')).toBeTruthy();
  expect(within(old).getByRole('cell', { name: /Uptime/ }).textContent?.replace('Uptime', '').trim()).toBe('—');
  expect(within(old).getByRole('cell', { name: /IP/ }).textContent?.replace('IP', '').trim()).toBe('—');
});
```

If `getByRole('cell', { name })` does not resolve through `data-label`, assert on `old.querySelectorAll('td')[2].textContent` and `[3]` instead.

Append to `Dashboard.test.tsx` a test in that file's style asserting the home table's headers are `['Container', 'Status', 'Uptime', 'IP', 'Ports', 'Actions']` and that the container name is a link to its page.

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/pages/EndpointPage.test.tsx src/pages/Dashboard.test.tsx 2>&1 | tail -15`
Expected: FAIL on the header list.

- [ ] **Step 3: Implement**

`EndpointPage.tsx`: add `const now = useNow()` (move `useNow` from `ContainerPage.tsx` into `containerFacts.ts` as an exported hook, importing `useEffect`/`useState` there). Replace the containers `ResourceTable` `head` and `render`:

```tsx
head={['Container', 'Status', 'Uptime', 'IP', 'Usage', 'Actions']} render={(c) => [
  <div className="ky-resource-name"><strong><Link to={containerPath(org, endpoint, c.id)}>{displayName(c.name)}</Link></strong><span title={c.image}>{displayName(c.image)}</span><ContainerPorts ports={c.ports} />{c.compose_project && <small>{displayName(c.compose_project)}</small>}</div>,
  <span style={{ display: 'inline-flex', gap: 4, flexWrap: 'wrap' }}><span className={stateBadge(c.state)} title={c.status}>{displayName(c.state)}</span>{healthBadge(c.health) && <span className={`badge ${healthBadge(c.health)!.className}`}>{healthBadge(c.health)!.text}</span>}</span>,
  uptime(c.started_at, now) || '—',
  <span title={attachments(c).map((n) => `${n.name}: ${n.ip || '—'}`).join('\n')}>{primaryIP(c) || '—'}</span>,
  usage(c),
  <ContainerControls key={c.id} base={base} container={c} active={e?.state === 'active'} scope={...} onRefresh={commands.reload} canExec={exec} org={org} endpoint={endpoint} onStatus={setStatus} />,
]}
```

`Dashboard.tsx` `HostContainers`: the same columns without Usage, headers `['Container', 'Status', 'Uptime', 'IP', 'Ports', 'Actions']`, name wrapped in `<Link to={containerPath(org, endpoint, c.id)}>`.

- [ ] **Step 4: Run the suite**

Run: `cd web && npx vitest run && npx tsc -b 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src
git commit -m "web: uptime, IP and health columns; container names link to their page

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Pod uptime on the cluster view

**Files:**
- Modify: `web/src/components/KubernetesCluster.tsx:46-53` (Pods table)
- Test: `web/src/components/KubernetesCluster.test.tsx`

- [ ] **Step 1: Write the failing test**

Append, in that file's style (it builds a `KubernetesInventory` and renders `KubernetesCluster`):
```tsx
it('shows pod uptime from started_at and a dash for the zero time', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-29T10:00:00Z'));
  const inventory: KubernetesInventory = { ...empty, namespaces: ['shop'], pods: [
    { namespace: 'shop', name: 'web-1', phase: 'Running', node: 'n1', owner_kind: 'ReplicaSet', owner_name: 'web', started_at: '2026-09-29T09:00:00Z', containers: [] },
    { namespace: 'shop', name: 'web-2', phase: 'Pending', node: '', owner_kind: '', owner_name: '', started_at: '0001-01-01T00:00:00Z', containers: [] },
  ] };
  render(<KubernetesCluster org="a" base="/api/x" endpoint={endpoint} inventory={inventory} instances={[]} admin={false} onChanged={() => {}} />);
  const rows = within(screen.getByRole('table', { name: /Pods/ })).getAllByRole('row').slice(1);
  expect(rows[0].textContent).toContain('1h 0m');
  expect(rows[1].querySelectorAll('td')[2].textContent).toBe('—');
  vi.useRealTimers();
});
```
Adapt the `endpoint` fixture and table lookup to what the file already uses (the `ResourceTable` title is `Pods`; if `getByRole('table', { name })` fails, find the section by heading text and query within it).

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npx vitest run src/components/KubernetesCluster.test.tsx 2>&1 | tail -10`
Expected: FAIL.

- [ ] **Step 3: Implement**

In the Pods `ResourceTable`, `head` becomes `['Pod', 'Phase', 'Uptime', 'Node', 'Restarts', 'Containers']` and the render array inserts `uptime(p.started_at, now) || '—'` after the phase, with `const now = useNow()` at the top of the component and imports from `./containerFacts`.

- [ ] **Step 4: Run the suite**

Run: `cd web && npx vitest run src/components/KubernetesCluster.test.tsx src/pages/EndpointPage.test.tsx 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/KubernetesCluster.tsx web/src/components/KubernetesCluster.test.tsx
git commit -m "web: pod uptime on the cluster view

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: Smoke test and real-Docker regression

**Files:**
- Modify: `scripts/smoke-test.sh` (near line 96 and after line 249), `internal/runtime/docker/inspection_integration_test.go` (or wherever `TestInspectionRealDocker` lives; `grep -rn "func TestInspectionRealDocker" internal`)

- [ ] **Step 1: Add the smoke assertions**

After line 96 (`SPA fallback for unknown route`):
```bash
check "SPA serves a container page URL" "$(status "$BASE/organizations/org_initial/endpoints/ep_x/containers/$(printf 'a%.0s' $(seq 64))")" "200"
```
After line 249 where `INV` is captured, add:
```bash
  contains "inventory carries network attachments" "$INV" '"networks":['
  # The local agent inspects running containers: every running one has started_at and health.
  RUNNING_WITHOUT_START="$(printf '%s' "$INV" | python3 -c 'import json,sys; s=json.load(sys.stdin)["snapshot"]; print(sum(1 for c in s["containers"] if c["state"]=="running" and (not c.get("started_at") or not c.get("health"))))')"
  check "running containers report started_at and health" "$RUNNING_WITHOUT_START" "0"
```
Confirm the smoke script already has a running container at that point (it approves the local agent and reads inventory); if the inventory can legitimately have zero running containers there, keep only the first `contains` check.

- [ ] **Step 2: Extend the real-Docker test**

In `TestInspectionRealDocker` (or `TestSnapshotAgainstLocalDocker`, whichever creates a fixture container), after the snapshot is taken, assert for the fixture container: `StartedAt` is non-zero and within the last hour, `Health` is `"none"` (the fixture has no healthcheck), `RestartPolicy` equals what the fixture was created with (`"no"` if unset), and `Networks[0].IP` parses with `netip.ParseAddr`.

- [ ] **Step 3: Run**

Run: `make ci 2>&1 | tail -30` (builds, lints, runs Go and web tests, smoke). If Docker is available: `go test -race ./internal/runtime/docker -run 'RealDocker|LocalDocker' -v 2>&1 | tail -20`.
Expected: everything green. Paste the tail of the output in the commit body if anything was skipped.

- [ ] **Step 4: Commit**

```bash
git add scripts/smoke-test.sh internal/runtime/docker
git commit -m "test: smoke and real-Docker checks for container start time, health and IPs

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 11: Documentation, DOX pass and embedded web build

**Files:**
- Modify: `docs/agent-protocol.md` (section 6, after the mounts bullet), `web/AGENTS.md` (lines 20, 25-26, 29 and the Home line at 32), `internal/runtime/AGENTS.md` (line 28-30), `internal/api/AGENTS.md` (line 11, the `GET .../commands` sentence), `internal/agent/AGENTS.md` (line 31), `docs/ACCEPTANCE.md` (Known gaps), `docs/authorization-matrix.md:78` (`container.read` row), `web/dist/*`

- [ ] **Step 1: Protocol doc**

Add to `docs/agent-protocol.md` section 6, after the mounts bullet:
```
- Each inventory container carries `started_at` (RFC 3339, whole seconds, absent when not running or when the agent could not read it in budget), `health` (`none`, `starting`, `healthy`, `unhealthy`; absent from an agent older than the field), `restart_policy` (the daemon's policy name) and `networks` as `[{name, ip, ip6}]`. A Docker agent reads these with one `ContainerInspect` per running container, at most 200 per report and 5 seconds in total; containers past either bound report none of them. The server also accepts `networks` as a list of names from an older agent. Health status only: the health log is never read.
```

- [ ] **Step 2: web/AGENTS.md**

- Line 20: replace "Container rows use a compact native Actions disclosure." with "Container rows render `ContainerControls` as an icon group (`role=group`, `aria-label` "Actions for <name>", every control labelled `<Action> <name>`); start, stop, restart and remove are buttons with the confirmations below, Logs and Terminal are links to the container page's tab. Command results go to one `role=status` line above the table (`onStatus`), never into the row."
- Line 25: append "Container names link to `/organizations/{org}/endpoints/{endpoint}/containers/{id}` (`ContainerPage.tsx`: Overview with live uptime from `started_at`, per-network IPs, mounts, labels and the last 24-hour roll-up; Configuration from the redacted inspection when the endpoint has `container.inspect`; Logs; Terminal for `canExec`; Activity from `GET .../commands?container=`). The tab lives in `?tab=` (`useSearchParam`) so reloads and links land on it. A container missing from a fresh inventory shows "no longer reported" and no controls. Lists show Status (state plus health badge), Uptime and IP; an older agent's report shows `—`."
- Line 26: replace "Logs open in a native modal dialog; Escape/close/unmount releases the reader." with "Logs render inline on the container page's Logs tab (pod logs on the cluster view still use a dialog); unmount and tab changes release the reader."
- Line 29: replace "It opens a native modal dialog (focus trap, Escape/close disconnect) with an explicitly confirmed terminal" with "It renders inline on the container page's Terminal tab with an explicitly confirmed terminal".
- Line 32 (Home): replace "direct container controls" with "the same icon action group" and add "Uptime and IP columns".

- [ ] **Step 3: Other DOX docs**

- `internal/runtime/AGENTS.md` line 28: append "then `enrichRunning`: one `GET /containers/<id>/json` per running container (`maxSnapshotInspects` 200, `snapshotInspectBudget` 5 s) for `StartedAt` (whole seconds), `Health` (status only) and `RestartPolicy`; a failed or skipped read leaves the zero values. Network IPs come from the list call's `NetworkSettings`."
- `internal/api/AGENTS.md` line 11: change "`GET .../commands` and `GET .../commands/{command}` are `endpoint.read`." to "`GET .../commands` (optional `container=<64 hex>`, 400 otherwise) and `GET .../commands/{command}` are `endpoint.read`."
- `internal/agent/AGENTS.md` line 31: append "The snapshot's per-container fields are listed in docs/agent-protocol.md section 6."
- `docs/authorization-matrix.md:78`: change the Secret column of `container.read` from "redacted env/labels" to "redacted env; labels, network names and IPs are reported".
- `docs/ACCEPTANCE.md`: in the Known gaps list, remove any entry about missing per-container detail, uptime or modal actions if present; add a step "Open a container from the list; confirm uptime ticks, the IP matches `docker inspect`, and Logs and Terminal tabs work" to the container section.

- [ ] **Step 4: Rebuild the embedded web bundle**

Run: `cd web && npm ci && npm run build && cd .. && git status --short web/dist | head`
Expected: changed files under `web/dist`. Then `go build ./... && go test ./internal/api -run TestSPA 2>&1 | tail -3` (or whichever test asserts the embedded bundle; `grep -rn "dist" internal/api/*_test.go | head -3`).

- [ ] **Step 5: Full CI locally**

Run: `make ci 2>&1 | tail -20`
Expected: green. Fix anything red before committing.

- [ ] **Step 6: Commit**

```bash
git add docs web/AGENTS.md internal/runtime/AGENTS.md internal/api/AGENTS.md internal/agent/AGENTS.md web/dist
git commit -m "docs: container page, icon actions and inventory fields; rebuild web dist

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Self-review notes

- Spec 1.1 (protocol fields, dual decode, bounded inspect): Tasks 1, 2. Spec 1.2 (route, page, tabs in URL, polling, disappeared state): Tasks 4, 6. Spec 1.3 (icon toolbar, links not modals, status line, mobile hit target): Task 7. Spec 1.4 (columns, links, Dashboard): Task 8. Pod uptime: Task 9. Spec 1.5 tests: Tasks 1, 2, 4, 6, 7, 10. Docs: Task 11.
- `onStatus` is introduced in Task 7; Task 6 must omit it until then, as its Step 3 says.
- `useNow` is defined in Task 6 and moved to `containerFacts.ts` in Task 8; Task 9 imports it from there.
- Review Focus 1 through 5 map to Tasks 1 and 8, 2, 2, 6, 4.
