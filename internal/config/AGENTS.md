# Config

## Purpose
Manages environment and file-based configuration loading, defaults, and type conversions for kyyard-server.

## Ownership
Owns environment variable parsing, configuration validation, default fallbacks, and security key generations.

## Local Contracts
- `DefaultAppName` is `KyYard`, shared by server branding and the default recovery service identity.
- `LoadFromEnv() (*Config, error)` must supply safe, valid defaults for all subsystems.
- Never log plaintext secrets or sensitive tokens.
- `KY_TRUSTED_PROXIES` is a comma-separated list of reverse-proxy IPs or CIDRs, empty by default, parsed once at startup into `[]netip.Prefix`; an unparsable entry fails startup. Only a request whose peer address is in the list may speak for another client through `X-Forwarded-For`. `0.0.0.0/0` and `::/0` are refused at startup; list only the proxy's own address or subnet.
- Startup secures `DataDir` to 0700 using an opened directory descriptor and rejects a final symlink. Keep ancestor directories trusted. All environments load/create `session.key`, `encryption.key` and `instance.key` using `keyfile.LoadOrCreate`: 32 random bytes encoded as hex, owner-only files, atomic exclusive publication and fsync. Malformed, truncated, symlink, special or permissive key files fail startup without replacement.
- `KY_SESSION_SECRET` and `KY_ENCRYPTION_KEY`, when nonempty, override their files without modifying them; each must encode 32 bytes as hex or base64. Session secrets normalize to lowercase hex for HMAC use. Empty/unset overrides use files. `instance.key` is an Ed25519 seed with no environment override; the agent protocol will define its use. All three keys are excluded from config JSON.
- Ordinary restart preserves keys and database sessions. Capsule restore preserves instance identity but removes authentication grants from its database snapshot; see `internal/backup/AGENTS.md`.

- `KY_BACKUP_DEPOSIT_INTERVAL` is a Go duration (default `24h`), only the default for the schedule the admin screen stores; `0` is off, anything else below `MinDepositInterval` (15m) or negative fails startup. `KY_BACKUP_DIR` (default empty, off) is the sealed local-copy directory and `KY_BACKUP_KEEP` (default 7) how many to retain; below 1 fails startup because the lib refuses it at write time. `KY_BACKUP_ALLOW_PRIVATE_RECOVERY` (default false) admits RFC1918 and CGNAT KyRecovery destinations only.

## Verification
- `go test -v ./internal/config/...`
- `go test -v ./internal/auth/ -run TestClientIP` (the helper that consumes the allowlist)

## Child DOX Index
None.
