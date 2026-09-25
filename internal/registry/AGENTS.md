# Registry client

## Purpose
Parses image references and resolves a tag or digest to the registry's manifest digest and platforms.

## Ownership
Owns `Reference`, `ParseReference`, `CanonicalHost`, `Credential`, `Client`, `Options`, `Resolved`, the egress guard (`egress.go`) and the typed errors. Must not import `internal/store`; the store imports this package.

## Local Contracts
- `ParseReference`: no host is `docker.io`; a one-component Docker Hub repository is `library/<name>`; no tag or digest is `latest`; a tag beside a digest and a bare image ID are `ErrInvalidReference`.
- `Head` is `Resolve` with `HEAD` on the manifest URL (one shared `manifest` function): same guard, auth flow and budget (two HEADs, one token GET). It reads no body; the digest is `Docker-Content-Digest` (format-checked, equal to a requested digest), the media type the `Content-Type`, `Platforms` nil. Docker Hub counts a manifest GET as a pull and not a HEAD, so the update check uses `Head` alone; `Resolve` is for when platforms are needed.
- The store calls `Head` through `store.DigestResolver` (`CheckImageUpdates`), passing the credential and `allowPrivate` per call; the API adapter (`registryResolver`) builds one `Client` per call with `Options.AllowPrivate` equal to that call's `allowPrivate`.
- `Options.AllowPrivate` is set by the caller only from `RegistryAccess.Registry.AllowPrivate`, which the store has already combined with the operator's `KY_REGISTRY_ALLOW_PRIVATE` on that use.
- `Resolve` is HTTPS only; `docker.io` is served from `registry-1.docker.io`. At most two manifest GETs (anonymous, then one retry) and one token GET per call. Redirects are never followed; any 3xx is `ErrUnavailable`. `Resolve` parses the URL it builds and requires exactly the reference's host and manifest path, with no userinfo, query, fragment or dot segments (`ErrInvalidReference`), because `Reference` fields are public.
- Proxy environment variables are ignored so every connection passes the egress guard.
- Challenges: `Bearer` fetches a token from the advertised realm, which must be HTTPS without userinfo; the credential goes there as basic auth, anonymous otherwise. `Basic` is answered with the credential. The credential reaches only the configured host and that realm; the bearer token (at most 8 KiB, else `ErrUnavailable`) only the configured host and lives in a local variable.
- Egress: each host is resolved once per `Resolve` or `Head` call (`Options.DialAddr` or the system resolver, bounded by `Options.Timeout`), the first address `checkAddr` admits is pinned, and the transport dials only pinned addresses, keeping the original host for SNI and `Host`. `checkAddr` unmaps IPv4-mapped and NAT64 (`64:ff9b::/96`) addresses, always refuses loopback, unspecified, multicast, link-local and local-use NAT64 (`64:ff9b:1::/48`), and refuses private and CGNAT (`100.64.0.0/10`) unless `AllowPrivate` (`ErrPrivateDestination`).
- Bounds: `Options.Timeout` per request (15s default) and the caller's context; bodies ≤ 4 MiB, `WWW-Authenticate` ≤ 4 KiB, response headers ≤ 64 KiB. `Docker-Content-Digest` is required, must match `^sha256:[0-9a-f]{64}$` and must equal a requested digest (`ErrDigestMismatch`); on the GET path a 200 also needs a non-empty body whose `sha256` equals it. Platforms come from an index's `manifests[].platform` as `os/arch[/variant]`, deduplicated, skipping empty, malformed and `unknown` entries, at most 64. The media type is the `Content-Type` value, or else the body's `mediaType` only when it is one of the four accepted manifest types.
- Errors: `ErrUnauthorized` (401/403), `ErrNotFound` (404), `ErrRateLimited` (429), `ErrUnavailable` (other statuses, transport, 3xx), `ErrDigestMismatch`, `ErrPrivateDestination`, `ErrInvalidReference`. Error strings carry host and status only, never bodies, tokens or credentials. When a call fails and the caller's context is done, the error wraps `ctx.Err()` and none of these, even if a late response arrived.
- `Options.RootCAs`, `Options.DialAddr` and the unexported `Client.allowLoopback` exist for tests only.

## Work Guidance
- Log nothing that holds a credential or token.

## Verification
- `go test -race ./internal/registry/`: TLS fake registries (`httptest.NewTLSServer`, `*.example.com` pinned to loopback) with recording servers that show where each `Authorization` header went. Removing any of these checks fails a test: HTTPS realm, realm userinfo, realm-host egress, one lookup per host, refusal to dial an unchecked host, URL host/path/dot segments, platform cap, media-type fallback, local-use NAT64, redirects, the 4 MiB cap, body hash, requested digest, the 8 KiB token cap, and `Head` sending HEAD (the fake registry answers HEAD with headers only), and the caller's context winning over a late malformed answer (`TestCallerContextWinsOverALateAnswer`). The URL userinfo check and the empty-body check duplicate the host check and the JSON parse; their outcomes are tested, not their removal.
- `TestResolveDockerHubAlpine` resolves `alpine:3.24` on Docker Hub when `KY_TEST_REGISTRY_ONLINE=1`; CI's real-Docker step sets it.
