# Suite

## Purpose
Client for suite runner protocol v1: the contract in KyQuickStart
`docs/superpowers/specs/2026-10-16-suite-runner-design.md` ("Runner protocol v1", "KyYard"). The
runner is `kyquickstart serve` beside KyYard; KyYard asks it for status, catalog, plan and
operations and never holds target credentials.

## Ownership
Owns the wire types, the Unix-socket transport and response validation. `internal/api` owns
authorization, re-authentication, the `suite_operations` record and audit; `internal/config`
reads `KY_SUITE_RUNNER_SOCKET` and refuses a bad path through `CheckSocketPath`.

## Local Contracts
- HTTP/1.1 over the socket, no keep-alive, so every request dials. Each dial rechecks that the
  socket's directory has trusted ancestry and is private to the current user
  (`keyfile.RequireTrustedAncestry`, `keyfile.RequirePrivateDirectory`), as `applysetup`
  checks its own socket; a failure is `ErrUnsafeSocket`.
- Requests are at most `MaxRequestBytes` (64 KiB), answers at most 1 MiB, each exchange 10 s.
- Answers decode with unknown fields refused and no trailing data (`ErrInvalidResponse`). A 404
  is `ErrNotFound` whatever its body; any other unexpected status must carry
  `{"error":{"code","message"}}` and becomes `*Error` with the status.
- `Request` always sends `apps` as a list and `target` only when set. `Start` requires the 202
  to echo the ID; `Operation` requires the asked ID and a kind and state from `Kinds`/`States`.
- A transport failure is `ErrUnreachable`: the runner may or may not have seen the request.

## Work Guidance
- Change these types only with the spec; the runner side is built against the same document.

## Verification
- `go test -race ./internal/suite` (fake runner on a temp Unix socket: path checks, the private
  directory recheck, strict decoding, refusals, missing operations, the exact request bodies).

## Child DOX Index
None.
