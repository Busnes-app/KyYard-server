# KyYard retention policy

**Status:** draft proposal for review (M3 PR 07). Every number is *proposed* and must be confirmed by the SQLite fixture soak before it is frozen (M4 gate). Unlimited retention never ships; a limit the soak rejects is lowered, not removed.

## Principles

- The control-plane database holds bounded, low-cardinality state. Container log bodies, terminal contents and raw high-frequency metrics do not live in it.
- Every bounded store has a time limit, a byte or row limit, and a query bound; whichever trips first wins.
- Cleanup is a periodic job on the control plane (*proposed* every minute) that deletes in bounded batches (*proposed* 5,000 rows per store per pass, about 7 million rows a day) so a large backlog never holds the writer lock; the soak verifies deletion keeps ahead of insertion at the capacity targets.
- The UI distinguishes missing data from zero, shows gaps explicitly, and shows the retention window on every history view.

## Limits (proposed)

| Store | Time | Size / cardinality | Query bound | UI indication |
|---|---|---|---|---|
| Audit records | 365 days for distinct events | denials are budgeted per actor: each member may produce *proposed* 100 denial rows per hour; beyond that, further denials increment a counter on that actor's existing row for the same action instead of inserting, and one `denial_rate_exceeded` row records the episode. Denials against targets that do not resolve inside the organization collapse into one row per `(actor, action, result)` with the target omitted. Per organization *proposed* 1,000,000 rows or 512 MiB, with an operator alert at 80 %; at the ceiling that organization's mutations are refused with `audit_budget_exhausted` (reads continue), except `organization.members.manage` and `platform.tenant.assume`, so an administrator can still remove the offending member; other organizations and platform operations are unaffected | 200 per page | retention window, row count and budget share on the audit screen |
| Exec session metadata | 365 days (with audit) | permission check plus open/close metadata per session (implemented); no argv or contents; an open without inspected exit remains unknown | 200 per page | listed under audit |
| Deployment history and events | deployments older than 90 days pruned unless they produced the current or previous revision; 90 days after the application is removed everything goes | 50 revisions per application (oldest non-current pruned), 500 events per deployment | 100 events per page | "older revisions pruned" note |
| Endpoint events (state changes, errors) | 7 days | 50,000 rows per endpoint | 200 per page | gap markers between reconnects |
| Command records | settled: 7 days; unsettled: kept until an operator resolves them | one row per dispatched command | – | an outcome nobody knows is never pruned away |
| Metrics samples (CPU, memory, network, restarts) | 6 hours at 60 s (time limit, per-container cadence cap, query bound, per-sample restart counts, the 95 % disk-budget stop and hourly roll-ups implemented); hourly roll-ups for 7 days | per container: 360 raw samples, 168 roll-ups (about 1.3 million rows at the capacity targets) | 6 h raw or 7 d roll-up per request | "no data" versus "0" rendered differently |
| Inventory snapshots | current generation only; previous kept until the next accepted one | one per endpoint | n/a | observed-at age and offline state |
| Container logs | never stored; streamed on demand (implemented) | per request 10,000 lines or 4 MiB, whichever first, counted over every line read rather than only the ones a filter matched; follow buffer 1 MiB per stream with explicit gap markers; 4 streams per endpoint and 1 per reader; a following stream ends after an hour, with its own response deadline so the server's write timeout cannot sever it unannounced | 10,000 lines | truncation and gap markers inline, marked as the control plane speaking rather than the container |
| Agent dedupe records (on the agent) | 24 hours | 10,000 entries | n/a | n/a |
| Agent queued metrics while disconnected | 5 minutes | dropped beyond | n/a | gap in the chart |
| Enrollment tokens | 15 minutes unconsumed; consumed tokens deleted after 24 hours | n/a | n/a | expired shown once, then gone |
| Sessions, MFA challenges, device pairings | inherited from the base | | | |

Disk budget: the operator sees the database size and each store's share on the settings screen. At *proposed* 80 % of a configured budget (default 2 GiB for SQLite) the control plane halves metric retention and warns; at 95 % it stops accepting metrics and events (commands and audit continue) and shows the reason.

## Overload behaviour

- Log streams: bounded buffers with backpressure toward the runtime; when a browser reads slowly the server drops data and inserts a gap marker rather than growing memory.
- Exec streams (implemented): eight-frame server/agent queues, 32 KiB decoded chunks; overflow cancels rather than presenting a corrupted continuous terminal. Four per endpoint, one per actor/endpoint, 32 per organization and 128 server-wide. Terminal attempts are limited to ten per actor/minute before audited authorization, including denials. Live authorization reads run every 250 ms with a 500 ms deadline rather than per input frame. Ordinary terminal cleanup uses best-effort cancellation through the control queue; revocation uses the bounded direct cancellation path and fails closed. Browser pending writes/socket backlog cap at 256 KiB, scrollback at 1,000 lines. Input idle expires at 15 minutes, absolute lifetime at eight hours. Audit retention follows the existing audit store; numeric retention remains subject to the soak gate.
- Inventory: an endpoint reporting faster than *proposed* once per 10 seconds is rate-limited at the agent; the server rejects out-of-order generations without writing.
- Metrics: samples beyond the per-container cap are dropped oldest-first before insert.
- Audit: never silently dropped; if the database refuses the audit write the operation fails (existing contract). The per-organization ceiling above turns that fail-closed rule into an organization-local outage rather than an instance-wide one, the per-actor denial budget keeps a single member from filling it at request rate regardless of how many distinct targets they name, and membership management stays writable so the member can be removed.

## Capacity targets to fix before the M4 soak (proposed)

| Target | Value |
|---|---|
| Endpoints per instance | 25 |
| Containers per endpoint | 100 |
| Concurrent streams | 16 per endpoint, 8 per user, 64 per instance |
| Retention disk budget | 2 GiB SQLite default |

The fixture is `cmd/soak` ([docs/soak.md](soak.md)), which asserts these bounds and names what it cannot yet cover. The soak runs it at these targets for 24 hours on SQLite, including sustained denied mutations from a read-only member against freshly generated target identifiers for the whole run while another organization keeps writing (asserting rows stay under the ceiling, each distinct-target denial adds no row past the per-actor budget, and an administrator can still remove that member at exhaustion), verifies the database stays within budget, cleanup keeps up, p95 read latency on list screens stays under 500 ms, and memory stays bounded with slow log clients attached. Numbers that fail are lowered before freeze.

## Decisions

| Decision | Proposed | Status |
|---|---|---|
| Log bodies in the database | never | required by handoff |
| Terminal contents | never recorded | required by handoff |
| Audit retention | 365 days, per-actor denial budget, per-organization ceiling with organization-local refusal that keeps membership management open | proposed |
| Metrics | 6 h raw at 60 s, 7 d hourly (1 d under disk-budget pressure) | implemented; soak-gated |
| Disk budget behaviour | degrade metrics, then stop telemetry, never audit | implemented: `KY_RETENTION_DISK_BUDGET` (default 2 GiB, 0 disables; negative or absurd values refuse startup) measured every prune pass over the telemetry relations. The level rises only while usage is over budget **and still growing**, and any prune pass clears it. Release cannot wait for the reading to fall: a delete returns pages to the table rather than the file, a plain PostgreSQL vacuum never shrinks an index, and the reading also counts data retention cannot touch (audit, which is never refused and not yet pruned, and inventory, where a report replaces a row). Once a pass has taken everything retention is owed, holding the level would refuse telemetry for the rest of the server's life for a reason no dropped metric could fix. At the ceiling this throttles rather than stops, and the condition is logged every fifteen minutes while it lasts, because it needs an operator: a larger budget, more disk, or shorter retention. At 95 % metrics are refused with `error retention_pressure` and the raw window closes to one hour; at 100 % inventory is refused too. Heartbeats, endpoint state and audit are never refused |
