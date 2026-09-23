# Registries and the registry client (M7a, PR A)

Milestone 7a delivers manual image updates: registries with credentials, update detection, and
a deliberate recreate. It ships as three PRs:

- **PR A (this spec):** per-organization registries with write-only encrypted credentials, the
  audited anonymous-pull opt-in, and `internal/registry`, a server-side client that resolves a
  reference to its current manifest digest. Nothing pulls yet.
- **PR B:** update detection: per-service digest comparison for an adopted instance, cached,
  reported without deploying; UI.
- **PR C:** controlled recreate: a plan may pin a repository digest that is not on the host; apply
  gains a `pull` step (by digest, with the registry credential in the frame) that verifies the
  pulled image ID before the existing replacement; the M7a carried items (closed detail
  vocabulary, agent started-marker, clock-skew refusal, audit correlation IDs).

Decisions recorded 2026-09-23 (Yoshi): per-organization registries with in-frame credentials;
server-side manifest check; recreate as a plan plus a pull step; anonymous pulls refused by
default with an audited per-organization opt-in.

## Data (migration 26, `registries`)

```
registries (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  host TEXT NOT NULL,              -- lowercase registry host[:port], e.g. docker.io, ghcr.io, registry.example:5000
  name TEXT NOT NULL,              -- display name, 1..64
  username TEXT NOT NULL DEFAULT '',
  credential_enc TEXT NOT NULL DEFAULT '',   -- AES-GCM of the password/token, '' when anonymous
  allow_private INTEGER/BOOLEAN NOT NULL DEFAULT false,  -- egress to private/CGNAT addresses
  created_by, created_at, updated_at,
  UNIQUE(organization_id, host)
)
organizations ADD COLUMN anonymous_pull_enabled (default false)
```

- The credential is sealed with `crypto.EncryptAESGCM` under
  `DeriveKey(encryptionKey, json(["kyyard/registry-credential/v1", org, registryID]))`, so
  ciphertext cannot be moved between rows. It is never returned by any route or list; the row
  reports `has_credential`.
- `host` is the registry host as it appears in image references, normalised: lowercase;
  `docker.io` for references with no host, `index.docker.io`, or `registry-1.docker.io`.
- Backup: the table joins the SQLite recovery drill; the credential decrypts after restore
  with the recovered key.

## Reference resolution (`internal/registry`)

`registry.ParseReference(ref string) (Reference, error)` returns `{Host, Repository, Tag,
Digest}`: no host → `docker.io`; a single-component repository on `docker.io` → `library/<name>`;
`Tag` defaults to `latest` when both tag and digest are empty; `ValidImageReference` grammar.

`registry.Client`:

```go
type Credential struct{ Username, Secret string } // Secret held only for the call
type Options struct{ AllowPrivate bool; Timeout time.Duration } // Timeout default 15s
type Resolved struct {
	Digest    string   // "sha256:…" of the manifest or index the tag points at (Docker-Content-Digest)
	MediaType string
	Platforms []string // "os/arch[/variant]" from an index; empty for a single manifest, nil from Head
	Checked   time.Time
}
func New(opts Options) *Client
func (c *Client) Resolve(ctx context.Context, ref Reference, cred *Credential) (Resolved, error)
func (c *Client) Head(ctx context.Context, ref Reference, cred *Credential) (Resolved, error)
```

- `Head` is `Resolve` with `HEAD` on the manifest URL: same auth flow, guard and budget (two
  HEADs plus one token GET). The digest is the `Docker-Content-Digest` header (format checked,
  equal to a requested digest), the media type the `Content-Type`; no body is read, so nothing
  is hash-verified. The header is TLS-authenticated and a pull by digest verifies the content.
  Docker Hub counts a manifest GET as a pull and not a HEAD, so PR B polls with `Head` and calls
  `Resolve` only when the digest changed or platforms are needed.

- HTTPS only. `GET https://<host>/v2/<repo>/manifests/<tag|digest>` with `Accept` for the OCI
  index, OCI manifest, Docker manifest list and Docker manifest v2. The digest is the
  `Docker-Content-Digest` header, verified against the SHA-256 of the body on this GET path
  (mismatch → `ErrDigestMismatch`; an empty body is `ErrUnavailable`).
- Token flow: a 401 with `WWW-Authenticate: Bearer realm=…,service=…,scope=…` triggers one
  `GET <realm>?service=…&scope=…`; with a credential the request carries HTTP basic auth,
  anonymously it carries none; the bearer token (at most 8 KiB, else `ErrUnavailable`) is used
  for one retry. `Basic` challenges are
  answered with the credential directly. The credential goes only to the configured host and to
  the realm that host itself advertised, both HTTPS, never on a redirect.
- Redirects are not followed (`http.ErrUseLastResponse`); a 3xx is `ErrUnavailable`.
- Egress guard: the host must resolve to public unicast addresses; loopback, link-local,
  private and carrier-grade NAT are refused with `ErrPrivateDestination` unless
  `AllowPrivate`; loopback is never allowed. A caller sets `AllowPrivate` only when the row's
  `allow_private` is set, which the store admits only under the operator's
  `KY_REGISTRY_ALLOW_PRIVATE`. Resolution is done once and the dialer is pinned
  to the checked address (no rebinding between check and connect).
- Bounds: 15 s per request, two requests per resolve (plus the token request), response bodies
  ≤ 4 MiB, header values bounded. Errors are typed: `ErrUnauthorized` (401/403 after the token
  step), `ErrNotFound` (404), `ErrRateLimited` (429), `ErrUnavailable` (other), and never
  include response bodies.
- Tests use an `httptest` fake registry (TLS via `httptest.NewTLSServer` and a client trusting
  its certificate through an `Options.RootCAs` hook used only by tests) covering: anonymous
  index, single manifest, bearer challenge with and without credential, basic challenge, 404,
  429, redirect refused, digest mismatch, private destination refused, credential never sent to
  a redirect or another host. An online test `TestResolveDockerHubAlpine` gated by
  `KY_TEST_REGISTRY_ONLINE=1` resolves `alpine:3.24` and runs in CI's Docker job.

## Store

- Permissions: `registry.read` (all five roles), `registry.manage` (organization admin only).
- `ListRegistries(ctx, a) ([]Registry, error)` (`registry.read`; no credential fields, `has_credential`).
- `PutRegistry(ctx, a, RegistryInput{Host, Name, Username, Credential *string, AllowPrivate}, key, privateAllowed) (*Registry, error)`
  (`registry.manage`; create or update by host; `AllowPrivate` without `privateAllowed`, the
  operator's `KY_REGISTRY_ALLOW_PRIVATE`, is `ErrPrivateRegistriesDisabled` after the permission check, audited, nothing written; `Credential` nil keeps the stored one, `""` clears
  it; audited with target `registries/<id>` and details `host=… allow_private=… credential=set|kept|cleared`).
- `DeleteRegistry(ctx, a, id)` (`registry.manage`, audited).
- `SetAnonymousPull(ctx, a, enabled bool)` (`registry.manage`; audited with `old=… new=…` in
  details) and `ReadRegistryPolicy(ctx, a) (RegistryPolicy{AnonymousPullEnabled bool}, error)` (`registry.read`).
- Internal, for PR B/C: `registryFor(ctx, tx, org, host) (id, username, secret string, allowPrivate bool, found bool, err error)`
  decrypting with the key inside the transaction; and `ResolveRegistryAccess(ctx, a, action, ref, key) (*RegistryAccess, error)`
  that returns `{Registry *Registry, Credential *registry.Credential, Anonymous bool}` or
  `ErrRegistryNotConfigured` when the host has no row and the opt-in is off. `action` is the
  permission of the operation that uses the credential (`application.deploy`, `image.pull`,
  PR B's detection action); the read runs under it, so a denial is audited against it, and
  `registry.read` itself is `ErrForbidden`. A permitted resolution audits nothing; callers audit
  the operation.

## API

- `GET /api/organizations/{organization}/registries` → list.
- `PUT /api/organizations/{organization}/registries` body `{host, name, username, credential?, allow_private}` → 200 row (credential omitted) — create or update.
- `DELETE /api/organizations/{organization}/registries/{registry}` → 204.
- `GET /api/organizations/{organization}/registry-policy` → `{anonymous_pull_enabled, private_registries_enabled}`
  (the second is the operator's `KY_REGISTRY_ALLOW_PRIVATE`, read-only);
  `PUT` same path body `{anonymous_pull_enabled}` only → 204 (`registry.manage`).
- All through `tenantRoute` (CSRF on writes). Errors: 403 without the permission, 400 invalid
  host/name, 403 `private_registries_disabled` for `allow_private` without the operator opt-in,
  409 `registry_in_use` is not needed in PR A.

## UI

`web/src/components/Registries.tsx` on the organization page (beside members): list with host,
name, username, "credential set", allow-private flag; a form (the allow-private checkbox only
when `private_registries_enabled`, otherwise a line naming `KY_REGISTRY_ALLOW_PRIVATE`) to add/update (credential field
never prefilled, write-only, cleared on submit), delete with confirmation, and the anonymous-pull
switch with its explanation ("Off: images from hosts without a registry entry cannot be pulled.
On: anonymous pulls are allowed; audited."). Admin-only controls are hidden for other roles by
the 403 the list route does not raise (everyone can read); the write controls render for every
role and the server refuses non-admins with fixed text, matching the existing panels' pattern.

## Tests

Store (SQLite + PostgreSQL): CRUD, sealing binding (ciphertext moved to another row does not
decrypt), write-only (list never carries the secret), `ResolveRegistryAccess` for configured,
unconfigured with opt-in on/off, docker.io normalisation; permission matrix; audit rows; backup
drill. API: routes, CSRF, 403 for env admin on writes, 200 for reads. Registry client as above.
Web: vitest for list, add with credential, update keeping the credential, delete, policy switch.

## Documents

`docs/application-schema.md` (Registry decision rows → implemented for PR A), `docs/authorization-matrix.md`
(`registry.read`/`registry.manage`/opt-in rows → implemented), `docs/threat-model.md`
(registry credential row: implemented mitigations and the token-realm exception), a new
`internal/registry/AGENTS.md`, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`,
`web/AGENTS.md`, `KyYard-Implementation-Plan.md` section 8.

## Out of scope (PR B and C)

Update detection routes and UI; pulls; credential delivery to the agent; the carried items.
