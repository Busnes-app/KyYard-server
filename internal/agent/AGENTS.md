# Agent

## Purpose
The KyYard agent side of the fleet: `protocol` (wire helpers shared with the control plane) and `client` (durable identity, enrollment, connection loop). The binary is `cmd/agent`.

## Ownership
Owns signed-message preimages, frame types, the agent's identity file, enrollment against `/api/agent/v1/enroll`, and the outbound WebSocket session against `/api/agent/v1/connect`. The control plane side (handshake, registry, state transitions) is owned by `internal/api` and `internal/store`.

## Local Contracts
- `protocol.Preimage` length-prefixes the context and every field with 4 bytes; the three contexts are enrollment, connection and rotation and no signature verifies across them. `AuthPreimage` binds endpoint ID, nonce, the host the agent dialed and the protocol version.
- Frames are JSON `Envelope`s with `v: 1`; the connection lifecycle uses `challenge`, `auth`, `hello`, `heartbeat`, `inventory`, `enrollment.approved` and `error`. Close reasons are the constants in `messages.go` and are the agent's only signal for terminal versus retryable failures.
- Identity lives in `identity.json` under a 0700 directory as a 0600 file written atomically and durably (the temp file and the directory are fsynced before the rename returns, so a rotation offer is never announced for a key a power loss could erase): endpoint ID, Ed25519 private key, the pinned instance fingerprint and the server origin. The enrollment token is read from stdin once and never written anywhere.
- `checkServerOrigin` gates both enrollment and connection: https anywhere, http only to loopback, no credentials/query/fragment; `Enroll` refuses before generating a key or sending anything. `Enroll` checks the server echoed our fingerprint before saving; a challenge whose instance fingerprint differs from the pinned one is `ErrInstanceChanged` and stops the loop.
- Rotation (`--rotate-every`, default 30 days, 0 disables; `RotatedAt` is set at enrollment so the first offer lands one interval later): at the start of a session past the interval the agent mints a key and persists it as pending; if that write fails nothing is sent and the session errors, because a key held only in memory would strand the agent once acknowledged. Then it sends `identity.rotate` signed by the current key and keeps authenticating with the current key. An offer unacknowledged for seven days (`PendingSince`) is dropped so a fresh key can be offered. `identity.rotated` from the server, or a connect refused with `key_retired` while a pending key exists, promotes the pending key and reconnects after a one-second pause (so the server has freed the endpoint's slot); `key_retired` with no pending key is terminal like revocation. A rotation the server declines (`rotation_pending`, `rotation_blocked`, `rotation_refused`) drops the pending key.
- `Auth` names the fingerprint of the signing key, so the server can say `key_retired` or `key_pending_review` instead of a bare refusal.
- `Run` reconnects with 1 s → 60 s exponential backoff and ±20 % jitter, heartbeats at the interval the server states (capped below 60 s so nginx defaults hold), sends one inventory snapshot per session with a generation that rises across restarts (`Identity.Generation`, persisted through `Options.IdentityDir`, seeded from the wall clock when higher), bounds the handshake to 15 s and every later read to two heartbeat intervals so a silent path triggers a reconnect, reconnects on `enrollment.approved`, and stops on `identity_revoked`/`enrollment_rejected` or an incompatible version. Cancelling the context closes the socket with `agent_shutdown`.

## Verification
- `go test ./internal/agent/...` (`client_test.go` runs the whole path against a real server: enroll, pending, approval notice, active through inventory, revocation stops the loop, token never persisted).
- `scripts/smoke-test.sh` runs the built `kyyard-agent` against the built server.

## Child DOX Index
None.
