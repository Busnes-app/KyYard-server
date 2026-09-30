# Deferred Minors Cleanup Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close the small findings deferred during PRs #86, #87 and #88 (container observe, direct edit, Kubernetes parity) that are still present on master, in one reviewable PR, without behaviour changes beyond the ones each item names.

**Architecture:** Batched same-shape edits per area. Each task lists its items verbatim from the consolidation survey (against master 46fd239; line numbers are approximate, locate by content). Every behavioural item gets a test; pure test items add the test. No restructuring, no renames beyond the items, no new abstractions.

**Spec:** `docs/superpowers/specs/2026-09-29-container-management-design.md`; items below are polish on the merged implementation.

## Global Constraints

- Security defaults never weaken: refusals become stricter or stay; nothing that today refuses may start accepting.
- Fixed texts only in the web; every control labelled; no new dependencies.
- Web caps mirror the Go protocol constants; when the item says "align", the Go constant wins.
- Every change keeps `make ci` green; `web/dist` is rebuilt once, in the last task.
- Commit trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- If an item turns out to be already fixed or to require a design, skip it and say so in the report; do not improvise a design.

## Review Focus

1. No refusal relaxed (protocol UID, seccomp, sysctl, env parsing).
2. Docker run/rollback changes (`deploy.go`) keep the rollback invariant: the old container ends running under its name, or a `rollback_failed` step says otherwise.
3. Web cap alignment does not reject configurations the server accepts.

---

### Task 1: Protocol and Kubernetes runtime

**Files:** `internal/agent/protocol/kubernetes.go`, `kubernetes_workloads.go`, `deployment_explicit_test.go`, `inventory.go`; `internal/runtime/kubernetes/workloads.go`, `exec.go`, `exec_test.go`, `manifest/manifest_test.go`; `docs/agent-protocol.md` where a cap or check changes.

- `protocol/kubernetes.go:217`: `Pod.UID` is only length-clamped. Set `p.UID = ""` unless `deploymentUUID.MatchString(p.UID)`.
- `protocol/kubernetes.go:391-394`: a log `PodTarget` accepts `UID`. Add `|| r.Pod.UID != ""` to the refusal.
- `protocol/kubernetes_workloads.go:282-287`: the deadline range is checked twice (`issued()` already bounds it). Keep only `!a.Deadline.After(now)`.
- `protocol/deployment_explicit_test.go:121`: the negative table only asserts `(err == nil) != tc.ok`. Assert the field named in the error.
- `protocol/deployment_explicit_test.go:14`: add one marshal→unmarshal→DeepEqual round-trip for a populated `ExplicitService`.
- `protocol/inventory.go:185`: `MaxNetworkAttachments = 32` but the spec says 16. Change the constant to 16 and update the spec/doc line that states it if they disagree.
- `runtime/kubernetes/workloads.go:193`: restart annotation uses `time.RFC3339`; use `time.RFC3339Nano`. Same line: bind the restart patch to `metadata.resourceVersion` the way scale does at `:198`.
- `runtime/kubernetes/exec.go:161-162`: sysctl allowlist adds `net.ipv4.tcp_rmem` and `net.ipv4.tcp_wmem` (Kubernetes 1.32 safe set).
- `runtime/kubernetes/exec.go:178-182`: refuse legacy seccomp annotations `seccomp.security.alpha.kubernetes.io/pod` and `container.seccomp.security.alpha.kubernetes.io/*` set to `unconfined`.
- `runtime/kubernetes/exec_test.go:213`: replace the `time.Sleep(20 * time.Millisecond)` with a ready channel.
- `runtime/kubernetes/manifest/manifest_test.go:223`: use `slices.Equal(r.Resources, []string{"selfsubjectaccessreviews"})`.
- Commit `protocol, kubernetes: close deferred minors`.

### Task 2: Docker runtime

**Files:** `internal/runtime/docker/deploy.go`, `configuration.go`, `docker.go`, `stats.go` (+tests), `docs/agent-protocol.md` if a code is added.

- `deploy.go:739`: drop the redundant `status == http.StatusNotModified` term.
- `deploy.go:716-717`: `unhealthy` during the start watch counts as success. Make `Health.Status == "unhealthy"` return `fail("exited_early")` (document under the code if the doc lists triggers).
- `configuration.go:386`: env without `=` becomes `NAME=`. Skip the entry when `!ok` (or flag it as unsupported, whichever the surrounding code does for unreadable settings).
- `deploy.go:651` with `:666-676`: a run's start cleanup also fires on `identity_unreadable` and `identity_unverified`, removing a container that did start. Set `failed` only for start/watch failures.
- `deploy.go:597-603`: a run's `bind_missing` check sits in the create step after pull/image/volume steps. Move it before the pull.
- `deploy.go:768-771`: `undo` ignores `unpark`'s result. Append a `rollback_failed` step when it fails.
- `deploy.go:567` and `:736`: a paused old container comes back running after rollback. Record `State.Paused` at the precondition and re-pause on rollback.
- `deploy.go:417-426`: the explicit precondition does not detect truncation (`list_truncated:*`). Deny when an inspected list exceeds its protocol cap.
- `configuration.go:292-298`: `list()` caps the entry count but not entry bytes (`MaxListEntryBytes`). Cut and flag oversized entries so the read does not fail closed.
- `docker.go:341` and `stats.go:~105`: each running container is inspected twice per cycle. Share one inspect.
- `docker.go:327`: past 200 running containers the same first 200 always get facts. Rotate the start offset each cycle.
- Tests: fake-client unit tests for each; real-Docker tests only if `KY_TEST_DOCKER_INSPECTION_IMAGE` is set locally (report which ran).
- Commit `docker: close deferred minors`.

### Task 3: Store, API and agent client

**Files:** `internal/store/commands_recreate.go`, `internal/api/inspection.go`, `configuration_handlers.go`, `recreate_handlers.go` (+tests), `internal/agent/client/configuration_test.go`.

- `store/commands_recreate.go:481`: the per-id deadline sweep `UPDATE ... WHERE id=?` gets `AND outcome=''`.
- `api/inspection.go:275` and `:292`: compute the `Noun` capitalisation once.
- `api/configuration_handlers.go:65` and `recreate_handlers.go:76`: `ContainerManaged` is checked once before the round trip; re-check after the answer.
- `api/recreate_handlers.go:229-231`: `specInvalid` parses the field name from the error text; use a typed field error from the protocol package.
- `api/configuration_test.go`: add tests for a 504 and a target-changed 409 on the configuration route.
- `agent/client/configuration_test.go`: add a `configuration.cancel` test and an oversize-downgrade test.
- Commit `store, api, agent: close deferred minors`.

### Task 4: Web

**Files:** under `web/src` as named (+tests); `web/AGENTS.md` only if a contract changes.

- `components/workloadConfiguration.ts:13`: `workloadUnsupportedLabel` falls back to `unsupportedLabel`; allow only `list_truncated:*` in the fallback.
- `workloadConfiguration.ts:98`: `diffWorkload` ignores `image_id`; add `'image_id'` to the field list.
- `workloadConfiguration.ts:61-63`: kind-specific checks: DaemonSet has no `replicas`; `strategy` must be valid for the kind (mirror the protocol's per-kind strategies).
- `components/WorkloadControls.tsx:36`: after a failed poll `busy` stays true; add `&& !error`.
- `components/PodControls.tsx:21`: a dotted owner name returns `null`, hiding Delete; gate only the two links on `dnsLabel.test(pod.owner_name)`.
- `pages/WorkloadPage.tsx:129`: PodPicker falls back to `pods[0]` silently; show a fixed notice when `initial` is not found.
- `components/configurationGroups/environment.tsx:20`: render "Reveal all" only if `literals.length > 0`; keep a fixed label with `aria-pressed` instead of changing the label.
- `configurationGroups/fields.tsx:24`: add an `inputMode` prop and pass `numeric` for replicas and retries. `fields.tsx:42`: after a row is removed, focus the next row or the Add button.
- `components/ContainerConfigurationForm.tsx:127-128`: negative CPUs, memory and `restart_retries` must fail client validation (`pids_limit` −1 stays allowed).
- `components/containerConfiguration.ts:75`: align the parser caps (labels 32 and the others) with the Go constants in `internal/agent/protocol`.
- `pages/ContainerPage.tsx:115-116`: branch on `organizations.state === 'error'` instead of the administrator text. `:119`: `pending={!!command && command.outcome !== 'failed'}` so a successful run cannot be repeated by one click. `:132-137`: add a 401 text to `READ_ERRORS`. `:64-67`: call `details.reload()` in the 30 s interval.
- `components/WorkloadControls.tsx:20` / `workloadTexts.ts:50`: test `commandLine`'s `timed_out` text.
- `components/WorkloadConfigurationForm.test.tsx:149-150`: the `env_from` test must use the wire form `secret/db-credentials`; rename the test and fix the fixture.
- `pages/ContainerPage.test.tsx`: page tests for `READ_ERRORS` 403, 429, 501, 504.
- `components/ContainerConfigurationForm.tsx:36`: export `buildSpec` and test the draft→spec direction.
- Commit `web: close deferred minors`.

### Task 5: Scripts, docs, dist, CI

**Files:** `scripts/smoke-test.sh`, `docs/ACCEPTANCE.md`, `web/dist`.

- `scripts/smoke-test.sh:311-316`: guard the post-run block with `if [ -n "$RUN_ID" ]`.
- `scripts/smoke-test.sh:300`: remove only `$SMOKE_CID` (or filter by a smoke label) instead of `docker rm -f ky-smoke-run` by name.
- `docs/ACCEPTANCE.md:25-26`: the audit known-gap line says run and recreate results are audited.
- `cd web && npm run build`; `make ci`; commit `scripts, docs: close deferred minors; rebuild web dist`.

## Left out (with reasons)

- `commands_recreate.go:262-275` stopped containers as port conflicts: conservative and intended.
- `api/server.go:58` `inspections` registry rename: churn without behaviour change.
- `ContainerPage.tsx:184` Overview application link: a feature, not a minor.
- Abandoned-command second edit, host network by ID prefix, writable bind beneath `/etc`, real-API strategic merge: unproven or policy/design, not cleanup.
