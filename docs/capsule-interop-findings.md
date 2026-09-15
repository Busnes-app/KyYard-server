# Capsule interoperability across the suite

**Verdict: there is no single capsule format to converge on. There are two incompatible
on-disk containers that both use the `.kycap` extension, and two further in-memory-only
implementations that never persist anything.**

Measured 2026-09-02 against the merged state of every repo. Every claim below was
executed, not read off the source.

The plan this investigation was meant to serve —
[capsule format interop](superpowers/plans/2026-09-02-capsule-format-interop.md) — is
built on the premise that the divergence is a base64 alphabet mismatch inside one
container shape. That premise is wrong, and acting on it would have produced a shared
package that cannot read the only capsules the suite actually stores.

## What was compared

Four implementations, not three:

| Repo | Package | Persists a capsule? | Container |
|---|---|---|---|
| `kysignon-server` | `internal/backup` | **yes** | JSON, `{"format":"kycap/1",…}` |
| `kyrecovery-server` | `internal/capsule` | **yes** | tar of `manifest.json`, `nonce.bin`, `payload.enc` |
| `ky_server_base` | `internal/backup` | no | in-memory `Capsule` struct only |
| `gridlock-server` | `internal/backup` | no | in-memory `Capsule` struct only |

The earlier survey counted three because it looked only at files named `capsule.go`.
`kyrecovery-server` names its package `capsule` and its entry points `Pack`/`Unpack`, so
a filename-based sweep misses it entirely — while it is one of only two implementations
whose output ever reaches a disk.

## The two containers cannot read each other

Real capsules generated from each implementation, then cross-parsed:

```
kysignon.ParseCapsule(kyrecovery.kycap)
  -> not a readable .kycap container: invalid character 'm' looking for beginning of value

kyrecovery.Unpack(kysignon.kycap, key)
  -> error reading capsule archive: archive/tar: invalid tar header
```

The `'m'` is the first byte of `manifest.json`, the tar member name. Both fail at the
container layer, before any key or ciphertext is examined, so no amount of encoding
detection reaches the problem.

```
kysignon.kycap    598 bytes   JSON, base64 ciphertext string, nonce prefixed to ciphertext
kyrecovery.kycap 4096 bytes   tar, raw binary payload, nonce in a separate member
```

Failing at the container is the good case: it is loud and immediate. But it means a
shared `Open` must dispatch on container type first, and the two branches share almost
nothing.

## ky_server_base and gridlock-server have no backups on disk to orphan

This is the finding that most changes the plan's shape.

In both repos `Ciphertext` appears in exactly three places — the struct field, the
assignment in `CreateCapsule`, and the read in `ExtractCapsule`:

```
internal/backup/capsule.go:52   Ciphertext []byte `json:"ciphertext"`
internal/backup/capsule.go:133  Ciphertext: []byte(ciphertextHex),
internal/backup/capsule.go:143  crypto.DecryptAESGCM(string(capsule.Ciphertext), …)
```

Nothing marshals the `Capsule`. The `json:` tags on it are vestigial. The two callers
build a capsule and immediately consume it:

- `handleRunRestoreDrill` passes it to `RunRestoreDrill`, whose `DrillResult` carries
  `Passed`, `DurationMS`, `Checks` and `ErrorMessage` — no capsule, no ciphertext.
- `handleExportRecoveryKit` passes it to `GenerateRecoveryKitHTML`, which embeds the
  shares and manifest fields and **not** the ciphertext (`internal/backup/recovery_kit.go`,
  88 lines; `capsule.Ciphertext` is never referenced).

So the recovery kit these two products hand an operator contains the shares that unlock a
payload it does not contain. Whether that is intended is a separate question worth asking,
but for this plan the consequence is narrow and firm: **no capsule written by
`ky_server_base` or `gridlock-server` has ever reached a disk.** There is nothing to keep
compatible.

Their durable path runs through KyRecovery instead. `internal/backup/client.go` pushes
`PushBackupFile{Path, DataBase64, Mode}` — raw files, not a capsule — to
`/api/backup/push`, and KyRecovery packs them with its own tar implementation.

## The base64 divergence is real but not load-bearing

The encoding fork the plan describes does exist, at the call site:

| Repo | Call | Ciphertext encoding |
|---|---|---|
| `ky_server_base` | `crypto.EncryptAESGCM(tarBytes, hex.EncodeToString(key))` | `base64.RawURLEncoding` |
| `gridlock-server` | `crypto.EncryptAESGCM(tarBytes, hex.EncodeToString(key))` | `base64.RawURLEncoding` |
| `kysignon-server` | `crypto.EncryptAESGCM(key, tarBytes)` | `base64.StdEncoding` |

Note the argument order is also swapped — same name, same arity, both parameters
assignable, so a mixed-up call compiles. That is the more dangerous half of this table.

But since neither `RawURLEncoding` producer persists anything, **the only capsules on
disk in the entire suite are `StdEncoding` (kysignon) and raw binary (kyrecovery)**. The
tolerant decoder is still worth having — it is four lines, it costs nothing, and it keeps
a future shared package honest about what the code writes — but it protects a
hypothetical, not an installed base. It should not be described as preventing data loss.

## Corrections to the plan

1. **"Three implementations" is four.** `kyrecovery-server/internal/capsule` was missed
   and it is one of the two that matter.
2. **"A migration that standardises on either encoding orphans the other camp's backups"
   is false.** The `RawURLEncoding` camp has no backups. Nothing is orphaned.
3. **Task 1's fixture instruction cannot be followed as written.** `ky_server_base` and
   `gridlock-server` have no serialisation function; producing a `.kycap` from them means
   inventing a container that has never existed, and a fixture of an invented format
   proves nothing about compatibility.
4. **`BackupFile` is `{Path string, Data []byte, Mode int64}`**, not `{Path, Content, Mode}`.
5. **`Ciphertext` is `[]byte` holding the ASCII of a base64 string**, so `encoding/json`
   base64s it a second time on the way out. Any shared type should hold it as a `string`,
   as kysignon's `capsuleFile` already does.
6. **The real question is which container wins**, and that is a product decision about
   KyRecovery's tar format versus KySignOn's JSON format, not something to settle inside
   a shared-package refactor.

## Reproducing this

Fixtures are generated by temporary tests placed in each repo and removed afterwards;
neither repo is left modified.

```bash
mkdir -p /tmp/capsule-interop

# kysignon: CreateCapsule + SerializeCapsule -> .kycap
# kyrecovery: capsule.Pack(PackOptions{...}) -> PackResult.CapsuleBytes

tar -tvf /tmp/capsule-interop/kyrecovery.kycap
head -c 300 /tmp/capsule-interop/kysignon.kycap
```

Cross-parsing is a temporary test in each package calling the other's fixture through
`ParseCapsule` and `Unpack` respectively. Both fail at the container layer, as quoted above.

## What should happen next

Not "build the shared capsule package". The prior question is which on-disk container the
suite standardises on, given that both live formats hold real recovery data:

- **KyRecovery's tar** streams, so it packs a directory without holding the payload in
  memory (`internal/capsule/stream.go`, 449 lines). That matters for large backups.
- **KySignOn's JSON** is versioned (`kycap/1`) and rejects unknown formats, so it can
  evolve deliberately. It also has the stronger extraction hardening — `safeRelPath`,
  `prepareTargetDir`, `countingReader`, `O_EXCL|O_NOFOLLOW`.

A shared package can read both and write one. Which one it writes is the decision that
gates everything downstream, and it is not mine to make.
