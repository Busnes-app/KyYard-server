# gridlock-server Re-fork Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** gridlock-server becomes the migrated scaffold plus gridlock's product code, pinned to `ky-primitives v0.4.0`, with its compat workflow green again.

**Architecture:** gridlock was forked from `ky_server_base` and then both drifted: gridlock adopted `ky-primitives v0.3.0` for capsule and Shamir, the scaffold hardened the KyRecovery client and moved to the middleware-wrapped route style. Rather than port the scaffold's migration into gridlock hunk by hunk, this plan regenerates gridlock from the migrated scaffold with the scaffold's own `scripts/ky-init.sh`, overlays the packages that exist only in gridlock, and reconciles the short list of shared files gridlock changed for product reasons. Shared code: scaffold wins. Product code: gridlock wins. The result replaces gridlock's working tree on a branch, so the diff against gridlock's `master` is reviewable and revertible.

**Tech Stack:** Go 1.26.6, `ky-primitives v0.4.0`, `github.com/crewjam/saml` + `github.com/russellhaering/goxmldsig` (gridlock-only), SQLite and Postgres.

**Spec:** `/home/yoshi/busness.app/ky-primitives/docs/superpowers/specs/2026-09-02-suite-migration-design.md` Phase 3, "Re-fork gridlock" (lines 306–309). Prerequisite plan: `2026-09-03-scaffold-adopts-ky-primitives.md` in this directory, **merged to `ky_server_base` master** before Task 1 here starts.

## Global Constraints

- Do not start until `ky_server_base` master contains the scaffold adoption plan's Task 8 commit. Check: `grep -q ky-primitives /home/yoshi/busness.app/ky_server_base/go.mod`.
- gridlock's `master` is never rewritten. All work lands on branch `refork/scaffold-v0.4.0`; the review is `git diff master...refork/scaffold-v0.4.0`.
- **Nothing is in the wild** for gridlock. Migration versions may be renumbered and stored formats (password hashes, recovery codes, TOTP secrets) may change without a data migration.
- Shared files take the scaffold's version. A gridlock-side change survives only if it is product behaviour (tickets, approvals, alerts, SAML login, step-up MFA, branding) and is re-applied on top of the scaffold's file.
- The whole tree stays `gofmt`-clean, `go vet`-clean, `go test -race ./...` green, and `go mod tidy` a no-op, because gridlock's `ci.yml` gates on all four.
- Superseded: `2026-09-02-gridlock-server-migration.md` in this directory. Its Task 1 (module rename) is done; its Tasks 2–3 (adopt Shamir, adopt capsule) are replaced by this plan. Do not execute it.

## Decisions this plan takes

| Decision | Choice | Why |
|---|---|---|
| Re-fork mechanism | `ky_server_base/scripts/ky-init.sh gridlock-server <tmp>` then overlay | The script exists for exactly this and now derives the module path. Using it exercises the scaffold adoption plan's Task 8 change. |
| SCIM | **Scaffold's `elimity-com/scim` implementation wins.** gridlock's hand-rolled `internal/scim/{handler.go,types.go}` is not carried; it stays reachable at gridlock commit `503b9cb` for the parent plan's Task 6, which wants a stdlib SCIM in the scaffold anyway. | Two incompatible SCIM stacks cannot be merged; the rule is scaffold wins. **Flagged for Yoshi:** if gridlock's SCIM has behaviour a customer depends on, reverse this and carry gridlock's, dropping `elimity` from the fork's `go.mod`. |
| Route authorisation style | Scaffold's wrapper middleware (`requireAdmin(h)`, `requireAuthenticated(h)`) everywhere, including gridlock's ticket, approval and alert routes | gridlock registered those routes bare and checked inside some handlers with a bool-returning helper. Wrapping at registration is the scaffold's style and closes the gap for free. |
| Migrations | Scaffold's v1–v3, then gridlock's ticket tables as v4 and `saml_requests` as v5 | Numbering is free while nothing is deployed; scaffold's `totp_last_counter` must keep its scaffold number so future scaffold migrations line up. |
| KyRecovery client | Scaffold's, with SSRF validation, redirect cap and bounded reads | gridlock's is the older, unhardened copy. |
| Compat and CI workflows | Scaffold's `ci.yml` and `ky-primitives-compat.yml` | The scaffold's `ci.yml` already carries the `security` and `docker` jobs gridlock added; the compat workflow now lives in both. |
| Deletions of gridlock tests | `internal/backup/recovery_kit_test.go` deleted; `internal/backup/drill_test.go` rewritten against `RunRestoreDrill`'s new signature or deleted if the scaffold's `capsule_test.go` covers the same cases | The kit is gone; the drill signature changed. |

---

## Reference: what is gridlock-only

Measured 2026-09-03 with `diff -rq` against `ky_server_base` at `8cc00c7`. Re-measure in Task 2; if the list has changed, the new list wins.

**Packages and files with no scaffold counterpart (carry over verbatim):**
- `internal/tickets/service.go`, `internal/tickets/service_test.go`
- `internal/store/tickets.go`
- `internal/api/ticket_handlers.go`, `internal/api/tickets_api_test.go`
- `internal/auth/mfa_transaction.go`, `internal/auth/mfa_transaction_test.go`
- `internal/sso/saml_test.go`
- `.github/FUNDING.yml`, `README.md`

**Shared files gridlock changed for product reasons (reconcile, scaffold base + gridlock hunks):**
- `internal/config/config.go` — `KY_APP_NAME` default `"Gridlock ITSM"`, SQLite file `gridlock.db`, Postgres db `gridlock`.
- `internal/store/store.go` — `Tickets() TicketStore` accessor and the `TicketStore` interface.
- `internal/store/factory.go`, `internal/store/sqlstore.go` — wiring for the ticket store.
- `internal/store/migrations/migrations.go` — ticket tables; `saml_requests`.
- `internal/api/server.go` — routes `/api/v1/tickets`, `/api/v1/tickets/`, `/api/v1/approvals/pending`, `/api/v1/approvals/`, `/api/v1/alerts/stream`, `/api/v1/alerts/summary`, `/api/sso/saml/login`, `/saml/acs`; ticket service construction.
- `internal/sso/saml.go`, `internal/sso/sso.go` — SAML login and ACS (scaffold has only metadata).
- `cmd/server/main.go` — version string and any gridlock-only subcommand.
- `Dockerfile`, `docker-compose.yml`, `Makefile`, `.gitignore` — branding and binary name only.
- `go.mod` — add `github.com/crewjam/saml v0.5.1`, `github.com/russellhaering/goxmldsig v1.6.0`.

**gridlock files that do not survive:**
- `internal/scim/handler.go`, `internal/scim/types.go`, `internal/scim/scim_test.go` (decision above).
- `internal/backup/{capsule.go,shamir.go,recovery_kit.go,client.go,drill.go}` and their tests — scaffold's versions.
- `internal/api/backup_handlers.go` — scaffold's.
- `internal/sso/kysignon.go`, `oidc.go` — scaffold's, unless the Task 2 diff shows a product hunk.
- Every per-package `AGENTS.md` — scaffold's, then Task 5 adds one line for tickets.

---

### Task 1: Generate the fresh tree

**Files:**
- Create: `/tmp/gridlock-refork/` (scratch; never committed)

- [ ] **Step 1: Preconditions**

```bash
cd /home/yoshi/busness.app/ky_server_base && git status --short --branch | head -1 && grep -n ky-primitives go.mod
cd /home/yoshi/busness.app/gridlock-server && git status --short --branch | head -1
```

Expected: scaffold on `master`, clean, with `github.com/Busness-app/ky-primitives v0.4.0`; gridlock on `master`, clean. If either is dirty or the require is missing, stop.

- [ ] **Step 2: Run the scaffold's init script**

```bash
rm -rf /tmp/gridlock-refork
/home/yoshi/busness.app/ky_server_base/scripts/ky-init.sh gridlock-server /tmp/gridlock-refork
```

Expected: the script rsyncs, rewrites `github.com/Busness-app/ky_server_base` → `github.com/Busness-app/gridlock-server` in `*.go`, `go.mod`, `*.md`, renames the container and app name, and finishes `go mod tidy` without network errors. If `go mod tidy` fails to fetch `ky-primitives`, the scaffold plan's Task 8 claim (public module, no `GOPRIVATE`) is wrong; stop and report rather than adding credentials.

- [ ] **Step 3: Prove the fresh tree builds and passes**

```bash
cd /tmp/gridlock-refork && head -1 go.mod && go build ./... && go test -count=1 ./... 2>&1 | tail -15
```

Expected: `module github.com/Busness-app/gridlock-server`; all packages `ok`. This is the scaffold's own test suite under gridlock's name, which is the baseline everything below is added to.

- [ ] **Step 4: Record**

No commit; the scratch tree is not a repo. Note the scaffold commit used: `git -C /home/yoshi/busness.app/ky_server_base rev-parse --short HEAD`. It goes in the final commit message.

---

### Task 2: Overlay gridlock-only files

**Files:**
- Copy from `/home/yoshi/busness.app/gridlock-server` into `/tmp/gridlock-refork`: the "no scaffold counterpart" list above.

- [ ] **Step 1: Re-measure the divergence**

```bash
diff -rq --exclude=.git --exclude=web --exclude=node_modules --exclude=docs --exclude=coverage.out --exclude=gridlock --exclude=ky_server_base \
  /home/yoshi/busness.app/ky_server_base /home/yoshi/busness.app/gridlock-server | sort > /tmp/gridlock-refork-divergence.txt
grep -c . /tmp/gridlock-refork-divergence.txt
grep "^Only in /home/yoshi/busness.app/gridlock-server" /tmp/gridlock-refork-divergence.txt
```

Compare the "Only in gridlock" lines to the reference list. Anything new is either product code (add it to the copy below) or an artifact (ignore it). Write the resolution for each new line into the commit message of Task 5.

- [ ] **Step 2: Copy the product packages**

```bash
G=/home/yoshi/busness.app/gridlock-server; T=/tmp/gridlock-refork
mkdir -p $T/internal/tickets
cp $G/internal/tickets/service.go $G/internal/tickets/service_test.go $T/internal/tickets/
cp $G/internal/store/tickets.go $T/internal/store/
cp $G/internal/api/ticket_handlers.go $G/internal/api/tickets_api_test.go $T/internal/api/
cp $G/internal/auth/mfa_transaction.go $G/internal/auth/mfa_transaction_test.go $T/internal/auth/
cp $G/internal/sso/saml_test.go $T/internal/sso/
cp $G/.github/FUNDING.yml $T/.github/
cp $G/README.md $T/README.md
```

- [ ] **Step 3: Fix module paths in the copied files**

The copied files already import `github.com/Busness-app/gridlock-server/...` (gridlock's module), so nothing changes. Confirm:

```bash
grep -rl "ky_server_base" $T --include='*.go' --include='*.md' --include='*.yml' --include='go.mod' | grep -v '^/tmp/gridlock-refork/docs'
```

Expected: no output. (`docs/` is excluded because plan documents legitimately name the scaffold.)

- [ ] **Step 4: Build to see what the overlay needs**

```bash
cd $T && go build ./... 2>&1 | head -30
```

Expected: errors, all of one of three kinds: `undefined: store.TicketStore` (Task 3), missing `crewjam/saml` imports in `saml_test.go` (Task 3), and `s.requireAdmin` used as a bool function in `ticket_handlers.go` (Task 3). Any *other* error means a gridlock-only file depends on a shared-file change not in the reference list; add that file to Task 3's list.

---

### Task 3: Reconcile the shared files

**Files:**
- Modify in `/tmp/gridlock-refork`: `internal/config/config.go`, `internal/store/store.go`, `internal/store/factory.go`, `internal/store/sqlstore.go`, `internal/store/migrations/migrations.go`, `internal/api/server.go`, `internal/api/ticket_handlers.go`, `internal/sso/saml.go`, `internal/sso/sso.go`, `cmd/server/main.go`, `go.mod`, `Dockerfile`, `docker-compose.yml`, `Makefile`, `.gitignore`

For every file in this list the procedure is the same, so it is written once and the per-file notes follow.

**Procedure per file:**
1. `diff -u /home/yoshi/busness.app/ky_server_base/<f> /home/yoshi/busness.app/gridlock-server/<f>` — this is what gridlock changed relative to the *old* scaffold.
2. For each hunk decide: product behaviour (keep) or shared-code drift (drop). The per-file notes say which hunks are expected.
3. Apply the kept hunks by hand to `/tmp/gridlock-refork/<f>`, which is the *new* scaffold's file under gridlock's module path.
4. `go build ./...` after each file.

- [ ] **Step 1: `internal/config/config.go`**

Keep: the three defaults — `getEnv("KY_APP_NAME", "Gridlock ITSM")`, `gridlock.db` in the SQLite DSN, `gridlock` as the Postgres database name. Drop: everything else (the scaffold's keyfile-backed encryption key and `[]byte` type must stay).

- [ ] **Step 2: `internal/store/store.go`, `factory.go`, `sqlstore.go`**

Keep: `Tickets() TicketStore` on the `Store` interface, the `TicketStore` interface, and whatever `factory.go`/`sqlstore.go` hunks construct and return the ticket store (`internal/store/tickets.go` was copied whole in Task 2 and expects them). Drop: any hunk touching users, sessions, settings — the scaffold's `SpendTOTPCounter` and column lists win.

- [ ] **Step 3: `internal/store/migrations/migrations.go`**

Keep the scaffold's `registry` entries 1–3 untouched. Append gridlock's ticket-table migration as `Version: 4` and its `saml_replay_protection` (`saml_requests` table) as `Version: 5`, copying the SQLite and Postgres strings from gridlock's file. Then:

```bash
cd /tmp/gridlock-refork && go test -count=1 ./internal/store/...
```

Expected: pass, including the scaffold's `TestSpendTOTPCounterRefusesReplay` and gridlock's ticket store tests (which live in `tickets.go`'s test file if one was copied; if gridlock's store tests are inside `store_test.go`, port the ticket cases into the scaffold's `store_test.go` rather than replacing it).

- [ ] **Step 4: `internal/api/server.go` and `ticket_handlers.go`**

Keep: construction of the ticket service in `NewServer` and the eight product routes. Register them in the scaffold's style:

```go
	// Gridlock ITSM
	s.mux.HandleFunc("/api/v1/tickets", s.requireAuthenticated(s.handleTickets))
	s.mux.HandleFunc("/api/v1/tickets/", s.requireAuthenticated(s.handleTicketByID))
	s.mux.HandleFunc("/api/v1/approvals/pending", s.requireAuthenticated(s.handlePendingApprovals))
	s.mux.HandleFunc("/api/v1/approvals/", s.requireAuthenticated(s.handleApproval))
	s.mux.HandleFunc("/api/v1/alerts/stream", s.requireAuthenticated(s.handleAlertStream))
	s.mux.HandleFunc("/api/v1/alerts/summary", s.requireAuthenticated(s.handleAlertSummary))

	// SAML
	s.mux.HandleFunc("/api/sso/saml/login", s.handleSAMLLogin)
	s.mux.HandleFunc("/saml/acs", s.handleSAMLACS)
```

Use the handler names as they appear in the copied `ticket_handlers.go` and gridlock's `saml.go`; the ones above are the shape, not a guarantee of the names. If a ticket handler performed an **admin** check inside itself with gridlock's bool helper (`if !s.requireAdmin(w, r) { return }`), wrap that route with `s.requireAdmin(...)` instead and delete the in-handler check. Then delete gridlock's bool-returning `requireAdmin` if the copied file defined it; the scaffold's middleware of the same name is what remains.

Drop: gridlock's variants of the auth, backup, device, settings and SSO handler registrations.

- [ ] **Step 5: `internal/sso/saml.go`, `sso.go`, `go.mod`**

Keep: the SAML service-provider construction, login redirect and ACS handler from gridlock's `saml.go`, and whatever `sso.go` hunk exposes them. Drop: any hunk that changes the OIDC or KySignOn paths.

```bash
cd /tmp/gridlock-refork && go get github.com/crewjam/saml@v0.5.1 github.com/russellhaering/goxmldsig@v1.6.0 && go mod tidy
```

- [ ] **Step 6: `cmd/server/main.go`**

Keep: the `version` string (`gridlock-server v...`) and any gridlock-only subcommand. Drop: everything else; the scaffold's `backup-drill`, `export-capsule` and `restore` win.

- [ ] **Step 7: `Dockerfile`, `docker-compose.yml`, `Makefile`, `.gitignore`**

Keep only branding: binary name, container name, `KY_APP_NAME`, ignored binary path. Drop structural changes; if gridlock's Dockerfile pinned a different Go image, the scaffold's `golang:1.26.6-alpine` wins.

- [ ] **Step 8: Gate**

```bash
cd /tmp/gridlock-refork && gofmt -l . ; go vet ./... && go test -race -count=1 ./... 2>&1 | tail -25 && go mod tidy && git diff --no-index --stat go.mod go.mod
```

Expected: gofmt silent, vet clean, every package `ok`. The `git diff --no-index` line is a no-op sanity check that tidy changed nothing (there is no repo here yet; compare `go.mod` to a copy taken before tidy if you want a real check: `cp go.mod /tmp/gomod.before && go mod tidy && diff /tmp/gomod.before go.mod`).

---

### Task 4: Drill test and package docs

**Files:**
- Modify or delete in `/tmp/gridlock-refork`: `internal/backup/drill_test.go` (gridlock's, if copied — it should not have been; confirm it is absent)
- Modify: `internal/api/AGENTS.md`, `internal/store/AGENTS.md` (one line each for tickets), `AGENTS.md` (package list)

- [ ] **Step 1: Confirm no stale backup tests were carried**

```bash
ls /tmp/gridlock-refork/internal/backup/
```

Expected: the scaffold's set only — `capsule.go`, `capsule_test.go`, `client.go`, `drill.go`, `recoverykey.go`, `recoverykey_test.go`, `AGENTS.md`, plus whatever the scaffold plan left. No `shamir*`, no `recovery_kit*`, no `drill_test.go` from gridlock.

- [ ] **Step 2: Docs**

Add to `internal/api/AGENTS.md` and `internal/store/AGENTS.md` a line each naming the ticket handlers and ticket store, in the files' existing style. Add `internal/tickets` to the package index in the root `AGENTS.md`. Nothing else in the docs changes.

---

### Task 5: Replace gridlock's working tree on a branch

**Files:**
- Everything under `/home/yoshi/busness.app/gridlock-server` except `.git/` and `web/node_modules/`

- [ ] **Step 1: Branch**

```bash
cd /home/yoshi/busness.app/gridlock-server && git switch -c refork/scaffold-v0.4.0
```

- [ ] **Step 2: Swap the tree**

```bash
rsync -a --delete --exclude='.git' --exclude='web/node_modules' --exclude='gridlock' /tmp/gridlock-refork/ /home/yoshi/busness.app/gridlock-server/
git -C /home/yoshi/busness.app/gridlock-server status --short | wc -l
```

`--delete` is what removes `internal/scim/types.go`, `internal/backup/shamir.go` and the rest of the "does not survive" list. The `gridlock` exclusion keeps the ignored 25 MB binary from being rsynced or deleted; it is not tracked.

- [ ] **Step 3: Review the diff before committing**

```bash
git -C /home/yoshi/busness.app/gridlock-server diff --stat | tail -5
git -C /home/yoshi/busness.app/gridlock-server diff -- internal/scim/ | head -40
git -C /home/yoshi/busness.app/gridlock-server status --short | grep '^ D'
```

Every deleted path must be on the "does not survive" list or in the resolution notes from Task 2 Step 1. If a deletion is not accounted for, restore it (`git checkout -- <path>`) and decide.

- [ ] **Step 4: Web assets**

```bash
cd /home/yoshi/busness.app/gridlock-server/web && npm ci && npm run build && cd .. && git status --short web/dist | head -3
```

gridlock's `ci.yml` builds the frontend; the scaffold plan changed `Backup.tsx` and rebuilt `dist`. If gridlock's `web/` has product pages (tickets UI), they arrived via the scaffold's `web/` overwrite being **wrong** — check `git status --short web/src` for deletions and restore gridlock's `web/src` product pages before building. Record what was restored.

- [ ] **Step 5: Gate, the same commands `ci.yml` runs**

```bash
cd /home/yoshi/busness.app/gridlock-server
gofmt -l $(git ls-files '*.go'); go vet ./... && go test -race -count=1 ./... && go build -o /tmp/gridlock-server ./cmd/server
go mod tidy && git diff --exit-code go.mod go.sum
```

- [ ] **Step 6: Run the compat check locally**

```bash
cd /home/yoshi/busness.app/gridlock-server && cp go.mod /tmp/gomod.keep && cp go.sum /tmp/gosum.keep
go mod edit -replace github.com/Busness-app/ky-primitives=/home/yoshi/busness.app/ky-primitives && go build ./... && go test -count=1 ./... | tail -3
cp /tmp/gomod.keep go.mod && cp /tmp/gosum.keep go.sum && git diff --exit-code go.mod go.sum
```

Expected: green against the library's local master, then `go.mod`/`go.sum` restored byte for byte. This is what the scheduled workflow will do overnight.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "refactor!: re-fork gridlock from ky_server_base <scaffold-sha> on ky-primitives v0.4.0

Scaffold wins for shared code; gridlock keeps tickets, approvals, alerts, step-up MFA,
SAML login/ACS and branding. gridlock's hand-rolled SCIM is dropped for the scaffold's
(reachable at 503b9cb). Migrations renumbered: ticket tables v4, saml_requests v5.
Resolutions for divergence not in the plan: <list or 'none'>."
```

Replace `<scaffold-sha>` with the value recorded in Task 1 Step 4, and fill the resolutions line.

- [ ] **Step 8: Push and open the PR**

Only if Yoshi has asked for a PR in this session. Otherwise stop here and report the branch name and `git diff --stat master` summary.

```bash
git push -u origin refork/scaffold-v0.4.0
gh pr create --title "Re-fork gridlock from the migrated scaffold (ky-primitives v0.4.0)" --body-file - <<'EOF'
...
EOF
```

---

## Self-review

- Spec coverage: Phase 3 "Re-fork gridlock against the migrated scaffold" — Tasks 1–5. The three files the spec calls byte-identical (`internal/crypto/crypto.go`, `internal/auth/totp.go`, `internal/auth/recovery.go`) are now *changed* by the scaffold plan, so they are not reconciled: the scaffold's new versions simply arrive with the fresh tree. The compat workflow is red on gridlock master today and green on this branch, which is the exit criterion the consumers-drifted memory names.
- Open decision, flagged rather than taken silently: SCIM (scaffold's elimity vs gridlock's stdlib). Default is scaffold's; reversing it is a one-task change (carry `internal/scim/{handler.go,types.go,scim_test.go}` from `503b9cb`, drop the scaffold's `internal/scim/{handler.go,protocol.go}` and `elimity` from `go.mod`).
- Names: `refork/scaffold-v0.4.0`, `/tmp/gridlock-refork`, migration versions 4 and 5, handler names in Task 3 Step 4 are illustrative and must be read off the copied files.
