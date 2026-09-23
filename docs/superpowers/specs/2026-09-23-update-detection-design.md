# Update detection (M7a, PR B)

PR B of the M7a split (PR A: registries and the registry client, merged as #58; PR C: the pull
step and the controlled recreate). It answers one question for an adopted application: does
the registry hold a newer image than the host runs, per mapped service. It changes nothing on
the host and mints no plan.

Decisions recorded 2026-09-23 (Yoshi): on-demand check with a cached result, no timer (periodic
polling belongs to M7b policies); the check runs under `application.deploy`, the operation
whose plan the result feeds.

## Data (migration 27, `image_checks`)

```
image_checks (
  instance_id TEXT NOT NULL REFERENCES application_instances(id) ON DELETE CASCADE,
  service_name TEXT NOT NULL,
  reference TEXT NOT NULL,          -- the revision's image reference for the service
  local_digest TEXT NOT NULL DEFAULT '',   -- the host image's repository digest, '' when unknown
  remote_digest TEXT NOT NULL DEFAULT '',  -- the registry's digest for the reference, '' on error
  verdict TEXT NOT NULL,            -- current | update_available | pinned | unknown_local | registry_error
  detail TEXT NOT NULL DEFAULT '',  -- registry_error: not_configured | unauthorized | not_found | rate_limited | private_destination | unavailable
  checked_at DATETIME/TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (instance_id, service_name)
)
```

- One row per mapped service, replaced on every check. Rows for services no longer mapped are
  deleted by the check. The cascade clears rows when the instance is released or the
  application removed; `settleApply` deletes the instance's rows when an apply succeeds, so a
  stale `update_available` never outlives the deployment that took it.
- The table joins the SQLite recovery drill. It holds no secret.

## Store

`CheckImageUpdates(ctx, a, app string, resolver DigestResolver, key []byte, privateAllowed bool) (*UpdateCheck, error)`
under `withTenantTargetDetails(ApplicationDeploy, app+"/updates", details)`. Three phases, so no
network call runs inside a transaction:

1. **Read** (own transaction, `readTenant(ApplicationDeploy)`): the instance, its mapping
   (`mapping_version >= 1`, else `ErrMappingRequired`), the latest revision's services, the
   endpoint's inventory snapshot (`freshInventory`; a stale or missing snapshot is
   `ErrAdoptionChanged`), and for every mapped service: the parsed reference, the host image's
   repository digest, and the registry access for the reference's host (`registryFor` and the
   anonymous-pull setting, with `allow_private = stored && privateAllowed`).
   - The local digest is the one entry of the mapped container's image `RepoDigests` whose
     repository (`host/repository` after `registry.CanonicalHost` and the `library/` default)
     equals the reference's; zero or several entries is `unknown_local`.
   - A reference with a digest is `pinned`; no registry request is made.
   - A host with no registry row and the opt-in off is `registry_error/not_configured`; no
     request is made.
2. **Resolve** (no transaction): `resolver.Head(ctx, ref, cred, allowPrivate)` per remaining
   service, at most 4 concurrently, under a 60 s overall deadline. `DigestResolver` is a
   one-method interface the API satisfies with `internal/registry` (`registry.New(Options{AllowPrivate}).Head`);
   tests use a fake. Credentials live only in this phase's memory. Errors map by type:
   `ErrUnauthorized`→`unauthorized`, `ErrNotFound`→`not_found`, `ErrRateLimited`→`rate_limited`,
   `ErrPrivateDestination`→`private_destination`, anything else→`unavailable`. Error strings
   are never stored.
3. **Write** (own transaction, the audited one): if the mapping version changed since phase 1,
   `ErrAdoptionChanged` (nothing written). Otherwise delete the instance's rows and insert the
   new ones. Audit details `services=N updates=N errors=N`.

`ReadImageChecks(ctx, a, app) ([]ImageCheck, error)` under `readTenant(ApplicationRead)` returns
the cached rows ordered by service name; an application without an instance returns an empty
list.

Verdict: `current` when local and remote digests are equal; `update_available` when both are
known and differ.

```go
type ImageCheck struct {
	Service, Reference, LocalDigest, RemoteDigest, Verdict, Detail string
	CheckedAt time.Time
}
type UpdateCheck struct {
	InstanceID     string
	MappingVersion int
	Services       []ImageCheck
}
```

Errors: `ErrMappingRequired` (new; 409 `mapping_required`), `ErrAdoptionChanged` (409, existing),
`ErrNotFound`, `ErrForbidden`.

## API

- `GET /api/organizations/{organization}/environments/{environment}/applications/{application}/updates`
  → 200 `{instance_id, mapping_version, services: [...]}`; `services` is `[]` when nothing is cached.
- `POST .../updates/check` → 200 same shape after a fresh check (`application.deploy`, CSRF).
  409 `mapping_required`, 409 `adoption_changed`, 409 `check_in_progress` when a check for the
  same application is already running (a per-application in-memory mutex in the server; one
  server process is the supported deployment). The handler passes
  `s.config.Security.EncryptionKey` and `s.config.Registry.AllowPrivate`.

The registry client is constructed per request with `Options{AllowPrivate}` from the service's
row (folded with the operator switch by the store), so one check can mix public and private
hosts without one host's setting leaking to another.

## UI

An **Updates** panel on the application page: per mapped service the reference, a verdict badge
(`Up to date`, `Update available`, `Pinned`, `Unknown on host`, `Registry error`), the short
digests, and the checked-at time; a **Check for updates** button; fixed texts per error detail
("The registry refused the credentials.", "The image was not found in the registry.", "The
registry rate limit was reached; try later.", "The registry is on a private address this
organization may not reach.", "No registry entry for this host and anonymous pulls are off.",
"The registry could not be reached."). No deploy action: PR C adds it. The panel renders only
when the application has an adopted mapping.

## Documents

`docs/application-schema.md` (Update detection: implemented; PR C remains), `docs/threat-model.md`
(the check uses a credential under `application.deploy`; digests only, never bodies, stored),
`KyYard-Implementation-Plan.md` section 8, `internal/store/AGENTS.md`, `internal/api/AGENTS.md`,
`web/AGENTS.md`, `internal/registry/AGENTS.md` (the store consumes `Head` through
`DigestResolver`).

## Tests

Store (SQLite + PostgreSQL) with a fake resolver: each verdict; `pinned` and `not_configured`
make no request; the local-digest rule with zero, one and several repo digests; rows replaced
and unmapped services dropped; cascade on release; rows cleared by a successful apply; mapping
change between phases refused; audit details; permission matrix (developer allowed, operator
refused); no credential in any returned value or audit row; backup drill. API: routes, CSRF,
409 codes, concurrent check refused. Web: badge per verdict, button posts and re-renders, fixed
error texts, hidden without a mapping.

## Out of scope

Pulling, plans that pin a registry digest, periodic checks, per-platform digests (the index
digest is compared, which is what `docker pull <tag>` records in `RepoDigests`).
