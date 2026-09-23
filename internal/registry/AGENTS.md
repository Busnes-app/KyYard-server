# Registry client

## Purpose
Parses image references and resolves a tag or digest to the registry's manifest digest and platforms.

## Ownership
Owns `Reference`, `ParseReference`, `CanonicalHost`, `Credential`, `Client`, `Options`, `Resolved`, the egress guard (`egress.go`) and the typed errors. Must not import `internal/store`; the store imports this package.

## Local Contracts
- `ParseReference`: no host is `docker.io`; a one-component Docker Hub repository is `library/<name>`; no tag or digest is `latest`; a tag beside a digest and a bare image ID are `ErrInvalidReference`.
- `Resolve` is HTTPS only; `docker.io` is served from `registry-1.docker.io`. At most two manifest GETs (anonymous, then one retry) and one token GET per call. Redirects are never followed; any 3xx is `ErrUnavailable`. No proxy.
- Challenges: `Bearer` fetches a token from the advertised realm, which must be HTTPS without userinfo; the credential goes there as basic auth, anonymous otherwise. `Basic` is answered with the credential. The credential reaches only the configured host and that realm; the bearer token only the configured host and lives in a local variable.
- Egress: each host is resolved once (`Options.DialAddr` or the system resolver), the first address `checkAddr` admits is pinned, and the transport dials only pinned addresses, keeping the original host for SNI and `Host`. `checkAddr` unmaps IPv4-mapped and NAT64 (`64:ff9b::/96`) addresses, always refuses loopback, unspecified, multicast and link-local, and refuses private and CGNAT (`100.64.0.0/10`) unless `AllowPrivate` (`ErrPrivateDestination`).
- Bounds: `Options.Timeout` per request (15s default) and the caller's context; bodies ≤ 4 MiB, `WWW-Authenticate` ≤ 4 KiB, response headers ≤ 64 KiB. `Docker-Content-Digest` is required, must match `^sha256:[0-9a-f]{64}$`, must equal `sha256(body)`, and must equal a requested digest (`ErrDigestMismatch`). Platforms come from an index's `manifests[].platform` as `os/arch[/variant]`, deduplicated, skipping empty, malformed and `unknown` entries, at most 64.
- Errors: `ErrUnauthorized` (401/403), `ErrNotFound` (404), `ErrRateLimited` (429), `ErrUnavailable` (other statuses, transport, 3xx), `ErrDigestMismatch`, `ErrPrivateDestination`, `ErrInvalidReference`. Error strings carry host and status only, never bodies, tokens or credentials.
- `Options.RootCAs`, `Options.DialAddr` and the unexported `Client.allowLoopback` exist for tests only.

## Work Guidance
- Log nothing that holds a credential or token.

## Verification
- `go test -race ./internal/registry/`: TLS fake registries (`httptest.NewTLSServer`, `*.example.com` pinned to loopback) cover every contract above, including where each `Authorization` header went.
- `TestResolveDockerHubAlpine` resolves `alpine:3.24` on Docker Hub when `KY_TEST_REGISTRY_ONLINE=1`; CI's real-Docker step sets it.
