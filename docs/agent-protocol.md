# KyYard agent protocol

**Status:** draft proposal for review before any agent code (M3 PR 07). Every value marked *proposed* is a planning default to confirm or change at review; the handoff is silent on them. Reviewed decisions are recorded in the table at the end.

## 1. Roles and trust

- The **control plane** owns tenancy, authorization, desired state, secrets and audit. It never trusts an agent's claim about which organization or environment it belongs to.
- The **agent** is one binary with runtime adapters (Docker first, Kubernetes later). It initiates an outbound encrypted connection, authenticates with a unique endpoint identity, reports capabilities and inventory, executes authorized versioned commands, and streams logs and terminal traffic. It makes no authorization decisions and stores no tenant policy, users, desired state, registry passwords or enrollment tokens.
- Enrollment is separate from PWA device pairing in model, token type, API namespace and lifecycle.
- Mounting the Docker socket is host-equivalent authority. Enrollment output and the UI say so plainly.

## 2. Transport

**Decided by spike (2026-09-16):** one outbound WebSocket over TLS from the agent to `wss://<control-plane>/api/agent/v1/connect`, carrying length-delimited JSON envelopes (section 4). Chosen over gRPC because it traverses ordinary reverse proxies with default settings, needs no HTTP/2 end to end, and keeps the one-binary deployment. The disposable spike must show: connect through Caddy and nginx with default proxy timeouts, idle survival with heartbeats, reconnect after the proxy restarts, and a 4 MiB frame passing both proxies.

Spike results (`scripts/spikes/websocket-proxy/run.sh`, re-runnable; echo server behind Caddy 2 and nginx 1 on the host network, `github.com/coder/websocket`): Caddy proxies the upgrade with a bare `reverse_proxy` line. nginx needs `proxy_http_version 1.1`, `proxy_set_header Upgrade $http_upgrade` and a `Connection` header driven by the standard `map $http_upgrade $connection_upgrade` block so only handshakes are upgraded (the two-part snippet in `scripts/spikes/websocket-proxy/nginx.conf`, which the README will carry), and keeps its 60-second `proxy_read_timeout` default: with 30-second heartbeats a connection idled for 90 seconds survived both proxies; with a 75-second heartbeat nginx closed it at the first gap, so the heartbeat interval is load-bearing and must stay under 60 seconds. Restarting nginx mid-idle dropped the socket and the client reconnected on its first backoff attempt. A 4 MiB binary frame echoed through both in under 10 ms with no size configuration. The agent will use `github.com/coder/websocket` (no CGO, context-based, no transitive dependencies).

The control plane's TLS story is unchanged from M1: HTTPS terminates at a reverse proxy or the built-in listener; the agent verifies the server certificate against the system trust store, or against a pinned CA supplied at enrollment for private CAs (`--ca-file`). No `--insecure` flag ships.

Connection authentication is challenge-response, not a bearer token: on connect the server sends a 32-byte nonce and its instance public-key fingerprint; the agent checks the fingerprint against the one it pinned at enrollment, then replies with its endpoint ID, protocol version, and an Ed25519 signature over the domain-separated message `"kyyard-agent-auth-v1" || len(endpoint_id) || endpoint_id || len(nonce) || nonce || len(server_host) || server_host || len(version) || version`. Every message the agent key signs uses this shape: the ASCII context string and then each field, every one as a 4-byte big-endian length followed by the raw bytes (endpoint IDs, hosts and versions as UTF-8; tokens, nonces, fingerprints and public keys as their raw 32 bytes). The three messages are `kyyard-agent-auth-v1` (connection), `kyyard-agent-enroll-v1` (enrollment proof) and `kyyard-agent-rotate-v1` (rotation); a signature for one purpose can never verify under another context. The server checks the signature against the identity's current public key and the tenant binding, then binds the socket to that endpoint for its lifetime. State decides what the socket may do: `pending` gets the enrollment channel only (facts and the approval notice); `approved`, `active` and `offline` get the full protocol. A revoked or unknown identity is closed with code `identity_revoked` and no further detail. A second concurrent connection for the same endpoint is refused with `duplicate_connection` and raises an endpoint event, because a cloned identity volume is the most likely cause.

The agent connects only over TLS (`wss://`), except to loopback for development. When the control plane itself listens on HTTP behind a reverse proxy, the proxy terminates TLS and the proxy-to-server hop is the operator's trusted network; the README states this with the HTTPS setup.

## 3. Enrollment and identity

Lifecycle: `pending → approved → active → offline → revoked`. `offline → active` is allowed for an authenticated reconnect. `revoked` is terminal; a revoked host re-enrolls as a new endpoint.

1. An organization or environment administrator requests an enrollment token for one organization, one environment and one runtime type. The server stores only the SHA-256 of the 32-byte token, with a *proposed* 15-minute expiry, and shows it once together with a generated `docker run -i` command that mounts a persistent identity volume. The token is piped to the agent on stdin, never placed in `-e` or a command argument, so `docker inspect` and the process list never carry it; the shell history of the pasting operator does, which the single use and 15-minute life bound.
2. On first start the agent generates an Ed25519 keypair in the identity volume (0600) and calls `POST /api/agent/v1/enroll` with the token, the public key, a proof of possession (signature over `"kyyard-agent-enroll-v1" || len(token) || token`, token as its raw 32 bytes), the endpoint name it proposes, and bounded enrollment facts: hostname, OS, runtime version, CPU count, memory. The agent pins the server's instance public-key fingerprint from the enrollment response in its identity volume. The server consumes the token atomically (`UPDATE ... WHERE token_hash=? AND consumed_at IS NULL AND expires_at>now`), creates the endpoint as `pending` bound to the token's organization, environment and runtime, and records the key fingerprint. Endpoint names are unique within an organization by a database constraint; a clash fails enrollment with `name_taken`. Exactly one caller wins under concurrency; the token is then useless.
3. An organization administrator reviews the fingerprint and facts in the UI and approves or rejects. Approval binds the reviewed fingerprint to the issued identity and moves it to `approved`; the server sends `enrollment.approved` on the pending socket and the agent reconnects with a full handshake. The first accepted inventory snapshot moves it to `active`. A pending agent may keep the socket open and refresh the facts but receives no commands and no secrets. Rejection and expiry (*proposed* 24 hours pending) close the socket with `enrollment_rejected`.
4. Rotation: the agent may present a new public key with a signature by the current key over `"kyyard-agent-rotate-v1" || len(old_fingerprint) || old_fingerprint || len(new_public_key) || new_public_key` (`identity.rotate`, raw 32-byte fields). The server records the new key as `pending_review`; it does not authenticate until an operator with `endpoint.enroll` acknowledges its fingerprint in the UI. Until then only the approved key authenticates, which the legitimate agent still holds. At most one key may be `pending_review` per endpoint: a further rotation request while one is pending is refused with `rotation_pending` and raises an event; an unacknowledged pending key expires after *proposed* 7 days. Acknowledgement retires every key other than the acknowledged one. Every rotation raises a high-severity endpoint event that the endpoint list surfaces until acknowledged or expired, with the approved and pending fingerprints shown side by side. Rotation is refused for an endpoint that has recorded a `duplicate_connection` event until an operator clears it. A rotation therefore never changes which key the fleet trusts without a human: a copied identity volume cannot lock the real agent out, cannot authenticate with a key it minted, and its attempt is the alert.
5. Revocation is a server-side state change that closes the live socket (which ends its streams) in the same request, rejects the next challenge, and needs no control-plane restart. Restore from a control-plane backup marks every non-revoked identity `offline` and every in-flight command `unknown`; revoked identities stay revoked because the state travels in the capsule. Agents reconnect with their existing keys; if the restored instance's key was rotated (see `threat-model.md`), the pinned fingerprint mismatches and the agent stops with `instance_changed` until an operator re-enrolls it.

Tables: `endpoints`, `endpoint_agents` (identity, public keys with validity windows, state, fingerprint, last seen), `agent_enrollment_tokens`, `endpoint_capabilities`. All carry `organization_id` and `environment_id`; foreign keys enforce same-organization references.

## 4. Envelope and versioning

Every frame is one JSON object:

```json
{"v":1,"type":"command","id":"01J…","request_id":"24f4…","org":"org_x","env":"env_y","endpoint":"ep_z",
 "deadline":"2026-09-16T14:00:30Z","payload":{…}}
```

- `v` is the protocol version. On connect both sides exchange `hello` with the highest version they support and the capability list; the server picks the highest common version or closes with `incompatible_version` naming the supported range. Within a major version, unknown fields are ignored and new message types are refused with `unsupported_type`, never crash.
- `id` is a ULID minted by the sender and is the dedupe key. Commands also carry `request_id` (the server's audit correlation ID, so agent logs join the audit trail), `org`, `env`, `endpoint`, `deadline`, the expected capability, and where the operation is destructive an `expects` block naming the resource identity and state the actor saw (for example container ID plus image digest). The actor is not sent to the agent; it is recorded server-side only. The agent refuses a command whose tenant or endpoint binding does not match its own, and refuses one whose deadline has passed.
- Types: `hello`, `heartbeat`, `inventory` (snapshot with a monotonically increasing `generation`), `event`, `metrics`, `command`, `result`, `stream_open`, `stream_data`, `stream_close`, `identity.rotate`, `error`.

## 5. Commands, results and unknown outcomes

- The agent persists a dedupe record `{id, outcome, result}` (result bounded to *proposed* 64 KiB) for every command it starts, in the identity volume, and returns the stored result for a repeated `id` after a restart. Records are pruned after *proposed* 24 hours or 10,000 entries.
- Every command ends in exactly one of `succeeded`, `failed`, `denied`, `timed_out` or `unknown`. The server records `unknown` when the socket drops after dispatch and before a result. `unknown` is never silently retried: the server first requests a fresh inventory snapshot and compares it with `expects`, then either resolves the outcome or presents it to the operator.
- Retryable commands (reads, inventory, non-destructive operations) are idempotent by construction and may be re-dispatched with the same `id`. Destructive commands (remove container, delete volume, prune, redeploy) are dispatched once; a retry is a new command with new preconditions that the operator confirms.
- Docker has no universal resource version; preconditions are operation-specific (container ID, image digest, volume name plus creation time) and checked by the agent immediately before acting.

## 6. Inventory, events, metrics

- Inventory is a full snapshot with a `generation` that only increases per endpoint. The server applies a snapshot only if its generation is higher than the stored one, so reordered or duplicate reports cannot roll state back, and a partial snapshot is rejected rather than treated as deletions. Each resource carries `observed_at` from the agent clock and the server records `received_at`; the UI shows staleness from `received_at` and flags clock skew over *proposed* 5 minutes.
- Reachability (socket up, heartbeat fresh) and freshness (last accepted generation age) are separate fields. Heartbeat every *proposed* 30 seconds; three missed heartbeats mark the endpoint `offline`.
- Metrics are bounded samples (CPU, memory, network, restarts) every *proposed* 60 seconds; the agent drops samples rather than queueing beyond 5 minutes while disconnected. Retention is in `retention-policy.md`.

## 7. Streams

- Log follow and exec are streams multiplexed on the same socket, opened by the server only after authorization and bound to a short-lived stream grant (*proposed* 60 seconds to open, then the stream's own timeouts).
- Limits (*proposed*): 16 concurrent streams per endpoint, 8 per user; 1 MiB buffered per stream with backpressure to the runtime, oldest data dropped with an explicit `gap` marker rather than blocking the socket; exec idle timeout 15 minutes, absolute 8 hours, resize supported, browser disconnect closes the stream within one heartbeat.
- Revocation of the identity, removal or disabling of the membership, or loss of the session closes every open stream with `revoked` in the same request that commits the change; the test bound is one second.

## 8. Secrets

Registry credentials and Compose secrets are delivered only inside the command that needs them, over the TLS connection above, held in memory, and scrubbed after the command ends. Where a runtime insists on a file (Docker's registry auth for a pull), the agent writes it 0600 under a tmpfs it owns and removes it when the operation ends and again on every start, so nothing survives a crash. The agent never echoes secrets into results, events or logs.

## 9. Reconnect and upgrade

- Backoff *proposed* 1 s doubling to 60 s with ±20 % jitter; a `hello` after reconnect resyncs capabilities and triggers a full inventory snapshot.
- Compatibility policy: the control plane supports the current and previous protocol major version; an agent older than that is refused with a message naming the minimum version and is shown in the UI as `incompatible`. Agent upgrades are operator-driven in 0.1.
- Graceful shutdown: the agent finishes in-flight non-destructive commands within the deadline, marks the rest `unknown` in its dedupe store, closes streams with `agent_shutdown`, and exits.

## 10. Test cases to implement with the code

1. Two concurrent enrollments with one token: exactly one `pending` endpoint, one `token_consumed` failure.
2. Expired token, replayed token, token for another organization or runtime, mismatched key or bad proof of possession: all refused, none creates an endpoint.
3. Pending endpoint receives no command and no secret; rejection and expiry close it.
4. Command with wrong `org`/`env`/`endpoint` or expired deadline: agent refuses with `binding_mismatch`/`deadline_passed`.
5. Old, duplicate and partial inventory generations never overwrite newer state.
6. Socket drop after dispatch yields `unknown`; reconciliation resolves it from a fresh snapshot; a destructive `unknown` is never auto-retried.
7. Rotation: a `pending_review` key is refused at connect with `key_pending_review` until acknowledged while the approved key still authenticates; a second rotation while one is pending is refused with `rotation_pending` and recorded; after acknowledgement the approved key and any earlier pending key no longer authenticate; an unacknowledged key expires; rotation is refused after a `duplicate_connection` event.
8. Revocation closes open log and exec streams within one second and the next connect fails.
12. Handshake replay: a captured connection signature is refused on a new nonce; an enrollment proof is refused as a connection signature (domain separation).
13. Second concurrent connection for one endpoint is refused and recorded.
9. Incompatible version refused with a named range; unknown message type answered with `unsupported_type`.
10. Reverse-proxy reconnect through Caddy and nginx defaults; proxy restart triggers backoff and full resync.
11. Restore from capsule: all identities `offline`, in-flight commands `unknown`, agents reconnect without re-enrollment.

## Decisions

| Decision | Proposed | Status |
|---|---|---|
| Transport | Outbound WebSocket over TLS, JSON envelopes, `coder/websocket` | decided: spike passed on Caddy and nginx (section 2) |
| Connection auth | Ed25519 challenge-response per connection with domain-separated messages, no bearer tokens; duplicate connections refused | proposed |
| Identity | Ed25519 keypair in an agent volume, fingerprint reviewed at approval; a rotated key authenticates only after an operator acknowledges it, one pending key at a time | implemented (rotation slice) |
| Token | 32 random bytes, SHA-256 stored, single use, 15 min | proposed |
| Heartbeat / offline | 30 s / 3 missed; must stay under nginx's 60 s default read timeout | implemented (PR 09) |
| Unknown outcome | Reconcile from inventory before any retry; destructive never auto-retried | required by handoff |
| Compatibility | Current and previous major version | proposed |
