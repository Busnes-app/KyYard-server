# Config

## Purpose
Manages environment and file-based configuration loading, defaults, and type conversions for kyyard-server.

## Ownership
Owns environment variable parsing, configuration validation, default fallbacks, and security key generations.

## Local Contracts
- `KY_AGENT_IMAGE` is optional and, when set, must be a digest-pinned reference (`registry/repo@sha256:<64 hex>`); a tag is refused at startup because the image runs with the Docker socket. Empty means enrollment tokens issue without a ready-to-run command.
- `KY_ALLOW_PLAINTEXT_BIND=true` explicitly acknowledges a non-loopback plaintext listen socket; without it HTTP accepts only literal loopback bind addresses. Compose opts in alongside its loopback host publish and explicitly trusts co-resident bridge peers; the image does not. `KY_ENV` and `ServerConfig.Environment` are removed: security follows the transport contract.
- Bare HTTP listens on loopback by default. `KY_APP_URL` must be an origin: HTTP only on localhost/loopback, HTTPS only with an explicit trusted proxy. Cookie Secure follows its scheme in every environment; contradictory overrides fail startup. Default ports and host casing normalize to browser origins. Environment SSO defaults off; UI-configured providers are independent. Legacy `SCIMConfig` remains for library tests; runtime configuration no longer reads SCIM environment variables and no HTTP routes are exposed.
- `DefaultAppName` is `KyYard`, shared by server branding and the default recovery service identity.
- `LoadFromEnv() (*Config, error)` must supply safe, valid defaults for all subsystems.
- Never log plaintext secrets or sensitive tokens.
- `KY_TRUSTED_PROXIES` is a comma-separated list of reverse-proxy IPs or CIDRs, empty by default, parsed once at startup into `[]netip.Prefix`; an unparsable entry fails startup. Only a request whose peer address is in the list may speak for another client through `X-Forwarded-For`. `0.0.0.0/0` and `::/0` are refused at startup; list only the proxy's own address or subnet.
- Startup inspects `DataDir` through its opened descriptor and leaves owner-only modes unchanged. Group/other access is tightened to 0700 with a log naming the path and old/new modes; chmod failures report the path, mode and owning UID. Final symlinks are rejected. Keep ancestor directories trusted. All environments load/create `session.key`, `encryption.key` and `instance.key` using `keyfile.LoadOrCreate`: 32 random bytes encoded as hex, owner-only files, atomic exclusive publication and fsync. Malformed, truncated, symlink, special or permissive key files fail startup without replacement.
- `KY_SESSION_SECRET` and `KY_ENCRYPTION_KEY`, when nonempty, override their files without modifying them; each must encode 32 bytes as hex or base64. Session secrets normalize to lowercase hex for HMAC use. Empty/unset overrides use files. `instance.key` is an Ed25519 seed with no environment override; the agent protocol will define its use. All three keys are excluded from config JSON.
- Ordinary restart preserves keys and database sessions. Capsule restore preserves instance identity but removes authentication grants from its database snapshot; see `internal/backup/AGENTS.md`.

- `KY_BACKUP_DEPOSIT_INTERVAL` is a Go duration (default `24h`), only the default for the schedule the admin screen stores; `0` is off, anything else below `MinDepositInterval` (15m) or negative fails startup. `KY_BACKUP_DIR` (default empty, off) is the sealed local-copy directory and `KY_BACKUP_KEEP` (default 7) how many to retain; below 1 fails startup because the lib refuses it at write time. `KY_BACKUP_ALLOW_PRIVATE_RECOVERY` (default false) admits RFC1918 and CGNAT KyRecovery destinations only.

- Shared-bridge guidance requires certificate-verified PostgreSQL TLS and a host-service/host-network proxy with stable gateway peer trust; never treat recycled container IPs as stable identities. The PostgreSQL overlay requires operator-provided TLS files.

## Verification
- `go test -v ./internal/config/...`
- `go test -v ./internal/auth/ -run TestClientIP` (the helper that consumes the allowlist)

## Child DOX Index
None.
