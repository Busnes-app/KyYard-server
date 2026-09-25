# Applications

## Purpose
Validate imported desired configuration independently of runtime discovery and execution.

## Ownership
Owns the bounded Compose parser; store owns revision persistence and encryption, API owns authenticated transport, and web owns draft presentation.

## Local Contracts
- Parse at most 64 KiB of one YAML document using pinned go.yaml.in/yaml/v3. Enforce 8,192 nodes and depth 16; reject aliases, anchors, tags, duplicate keys and unsupported fields.
- Accept services with image, explicit string environment values, restart, long-form published ports and volumes only. Never read files, URLs or process environment. Interpolation is refused; `$$` decodes to a literal dollar.
- Volumes: service `volumes` is a list of at most 32 entries, short `SOURCE:TARGET[:ro|rw]` (split on `:` at most three ways) or long `type` (`volume`|`bind`), `source`, `target`, boolean `read_only`. A source starting with `/` is a bind; `.`, `~` or a drive-letter (`X:`) source is refused with the "write the absolute path" diagnostic; any other source is a named volume that must be declared. Empty sources or targets, unnormalized bind sources and targets, a `/` target, duplicate targets, anonymous volumes, tmpfs and other long keys are refused, each at its node's line and column. Top-level `volumes` is a mapping of at most 64 names whose value is null, `{}` or `{external: <bool>}`, names checked by `store.ValidDeclaredVolumeName` at the key (`[a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}`, at least two characters for a newly declared name); the store's `ValidVolumeName` (`{0,63}`) remains the backstop for stored revisions saved before that minimum existed. `driver`, `name` and other keys are refused.
- Convert every environment value to a reference plus transient secret map. Never serialize or log source or values. Diagnostics contain only position and fixed reasons, not YAML library errors or input text.
- Import does not adopt resources, dispatch commands or deploy. Unsupported Compose features fail rather than disappear.

## Work Guidance
- The implemented subset and future model are distinguished in `docs/application-schema.md`.

## Verification
- `go test ./internal/applications` covers accepted conversion, deterministic output, unsupported syntax, volume syntax with each refusal's line/column/reason, and diagnostic redaction.

## Child DOX Index
None.
