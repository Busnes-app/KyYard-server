# Applications

## Purpose
Validate imported desired configuration independently of runtime discovery and execution.

## Ownership
Owns the bounded Compose parser; store owns revision persistence and encryption, API owns authenticated transport, and web owns draft presentation.

## Local Contracts
- Parse at most 64 KiB of one YAML document using pinned go.yaml.in/yaml/v3. Enforce 8,192 nodes and depth 16; reject aliases, anchors, tags, duplicate keys and unsupported fields.
- Accept services with image, explicit string environment values, restart and long-form published ports only. Never read files, URLs or process environment. Interpolation is refused; `$$` decodes to a literal dollar.
- Convert every environment value to a reference plus transient secret map. Never serialize or log source or values. Diagnostics contain only position and fixed reasons, not YAML library errors or input text.
- Import does not adopt resources, dispatch commands or deploy. Unsupported Compose features fail rather than disappear.

## Work Guidance
- The implemented subset and future model are distinguished in `docs/application-schema.md`.

## Verification
- `go test ./internal/applications` covers accepted conversion, deterministic output, unsupported syntax and diagnostic redaction.

## Child DOX Index
None.
