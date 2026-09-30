# Explicit Unknown Outcomes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close the follow-ups recorded in PR #90: an explicit recreate or run whose rename, stop or start call ends `unknown` or `timed_out` must leave the host in the state the rollback invariant promises, or say `rollback_failed`; workload apply/run blockers come from a typed field error; the Applications removal test stops flaking.

**Architecture:** In `internal/runtime/docker/deploy.go` every restore already runs under `restoring(ctx)` (`context.WithoutCancel` plus `rollbackBudget`). The three gaps are all "the daemon may or may not have done it": the fix is one inspect under the restoring context before acting, then the existing restore primitives. `protocol.FieldError` gains a message prefix so workload validation can return it with its text unchanged, and the store stops parsing error text. The web test waits for the counter instead of reading it synchronously.

**Spec:** `docs/superpowers/specs/2026-09-29-container-management-design.md` section 2 (direct edit, rollback); `docs/agent-protocol.md` Docker explicit frames.

## Global Constraints

- Rollback invariant (`internal/runtime/AGENTS.md` line 12, `docs/agent-protocol.md` 177-181): after a failed explicit recreate the old container is running (paused again if the recheck read it paused) under its name, or a `rollback` step `rollback_failed` follows; a run never removes a container whose start succeeded.
- Never remove a container whose state is unknown: remove only after an inspect under `restoring(ctx)` proves it is not running; if the inspect fails, leave it and report.
- Outcome vocabulary unchanged: step outcomes `succeeded|failed|denied|timed_out|unknown|skipped`; `unknown` = parent cancelled with no answer; the only new step emissions are `rollback` `rollback_failed` where a restore was attempted and failed.
- Wire and text unchanged for `FieldError`: `"invalid container configuration: <field>"` and `"invalid workload configuration: <field>"` byte for byte; every existing `spec_invalid:<field>` blocker in the store and API tests keeps its value.
- No new fake-engine framework: extend `fakeDeployEngine` with the minimal hooks the tests need (a delay or `onCall` hook per endpoint, same shape as `stopDelay`/`onStart`).
- `make ci` green; commit trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. A run whose start call ends `unknown` or `timed_out` with the container actually running is left running and named; one whose container is not running is removed; an inspect failure leaves it and reports `rollback_failed`. Task 1.
2. A recreate whose stop ends `unknown` or `timed_out` after the daemon did stop the old container ends with the old container started again (and re-paused if it was paused); one where the daemon did not stop it ends with it running untouched. Task 1.
3. A recreate whose rename ends `unknown` or `timed_out` after the daemon did rename ends with the old container back under its name; nothing is created or stopped. Task 1.
4. `errors.As` replaces every `strings.Cut(err.Error(), ": ")` on validation errors in the store; blockers identical before and after. Task 2.

---

### Task 1: Docker explicit unknown outcomes

**Files:** `internal/runtime/docker/deploy.go`, `deploy_test.go`; `internal/runtime/AGENTS.md` (line 12 bullet); `docs/agent-protocol.md` (179-181); `internal/agent/protocol/deployment.go:46` comment if the rollback meaning widens.

- **Run start (`replace` ~592-599, `start` ~733-772, `discard` ~871-876).** When `failed && !up`, before `discard`: if the start step's outcome is `unknown` or `timed_out`, inspect `created` under `restoring(ctx)`; `State.Running` true → leave it (no removal; the result keeps the start step's outcome and code); false → `discard`; inspect error → leave it and `rollbackFailed`. A failed `discard` on a run (create failure via `undo`, or this start branch) appends `rollback_failed`: make `undo` return the discard result for runs instead of `run(...) || ...`.
- **Stop (`~625-645`, `undo` ~865-868).** Record `wasRunning` at the recheck (`state.State.Running`, beside `paused` at ~621). In the stop step's deferred undo, when the outcome is `unknown` or `timed_out` and `wasRunning`: after `unpark`, inspect the old container under `restoring(ctx)`; if not running, POST `/start` and, if `paused`, `/pause` (reuse the tail of `rollback`, ~815-831, as a helper `revive` rather than duplicating it); any failure → `rollback_failed`. A `failed` stop (daemon answered) keeps today's undo.
- **Rename (`~612-630`).** Give the rename step a deferred undo: when its outcome is `unknown` or `timed_out`, inspect the old container by ID under `restoring(ctx)`; if `Name` is the parked name, `unpark`; failure → `rollback_failed`. On any non-success rename, later steps stay `skipped` as today.
- **Tests (fake engine):** add `renameDelay`/`startDelay` hooks (or a generic `delay map[string]time.Duration` keyed by endpoint suffix) and `inspectNewState`/`inspectOldState` knobs. Cases: run start `timed_out` with running container → no DELETE, no rollback step; run start `timed_out` with exited container → DELETE; run start `timed_out` with inspect 500 → no DELETE, `rollback=failed:rollback_failed`; run create failure with discard 500 → `rollback_failed`; stop `timed_out` old actually stopped → `/start` (and `/pause` when paused) after unpark; stop `timed_out` old still running → no `/start`; stop `unknown` (parent cancelled) same; rename `timed_out` daemon renamed → rename-back call; rename `timed_out` daemon did not → no rename-back; each with the exact `f.steps()` string. Run the real-Docker regressions with `KY_TEST_DOCKER_INSPECTION_IMAGE=alpine:3.24 KY_TEST_DOCKER_DEPLOY_IMAGE=alpine:3.24` and report.
- Commit `docker: settle unknown rename, stop and start outcomes`.

### Task 2: Typed workload field error

**Files:** `internal/agent/protocol/configuration.go:149-154`, `kubernetes_workloads.go:135-137` (+tests), `internal/store/commands_workload.go:300-302`, `commands_recreate.go:134` if it parses text (+tests), `internal/api/recreate_handlers.go:231-236` unchanged.

- `FieldError` gains the message: `type FieldError struct{ Field, Message string }` with `Error()` = `Message + ": " + Field`; `configErr` sets `"invalid container configuration"`; `workloadErr` returns `&FieldError{Field: field, Message: "invalid workload configuration"}`. If anything uses `errors.Is(err, errWorkload)`, keep `errWorkload` and add an `Is` method; otherwise delete it. Text pinned by tests before and after.
- `createWorkloadFrame`: replace `strings.Cut(err.Error(), ": ")` with `errors.As`; a non-`FieldError` (the four plain `WorkloadApply.Validate` errors) keeps the `frame` fallback so blockers do not change. Pin `spec_invalid:replicas` on the line-174 apply test instead of the prefix check.
- Commit `protocol, store: workload blockers from a typed field error`.

### Task 3: Deflake and CI

**Files:** `web/src/components/Applications.test.tsx:100-113`.

- Replace the synchronous `expect(instanceReads).toBeGreaterThan(before)` with `await waitFor(() => expect(instanceReads).toBeGreaterThan(before))` (real timers; `waitFor` from testing-library). Run the file 5 times.
- `make ci`; no `web/dist` change expected (test only); commit `web: wait for the instance reload in the removal test`.

## Rulings

- An inspect failure during a restore is treated as "state unknown": the container is left in place and `rollback_failed` says so. Leaving a stray container is recoverable by an operator; removing a running one is not.
- The four plain `WorkloadApply.Validate` errors keep the `spec_invalid:frame` blocker; naming them is a wire change with no consumer.
