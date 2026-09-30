# Restore Budget Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close the follow-ups left by PR #91: every explicit undo runs under one restoring budget instead of one per call; a run's unanswered `create` is settled; the plan poll test waits on a condition instead of a count.

**Architecture:** `restoring(ctx)` (`internal/runtime/docker/deploy.go` ~855) currently makes a fresh 70 s context inside each of `discard`, `unpark`, `containerNow`, `revive` and the stop wait, so an undo can chain five budgets. Each undo entry point (`undo`, `rollback`, `settleRun`, `unparkIfParked`, `reviveIfStopped`) will build one restoring context and pass it down; the helpers take a context and no longer call `restoring`. The stop wait keeps its own `stopWait` bound nested inside that context. The unanswered create on a run is settled the same way as the unanswered start: look the container up by name under the restoring context, remove it only when it exists with status `created`, report `rollback_failed` when the lookup fails. The web test fakes only timers, not promise resolution, so the poll advances deterministically.

**Spec:** `docs/superpowers/specs/2026-09-29-container-management-design.md` section 2; `docs/agent-protocol.md` Docker explicit frames.

## Global Constraints

- Rollback invariant unchanged (`internal/runtime/AGENTS.md` line 12): old container running, re-paused if it was paused, under its name, or `rollback` `rollback_failed`; a run never removes a container whose start succeeded; a container is removed only after a read proves it never started.
- Budget: one `rollbackBudget` (70 s) per undo entry point plus `stopWait` (30 s) for the wait; the docs state the new worst case (recompute it from the constants and the recheck guard's reserve, and say what the recheck guard reserves).
- Outcome and step vocabulary unchanged; exact call and step strings asserted in tests; no sleeps for synchronisation.
- Web deflake keeps every assertion; fake timers only for `setTimeout`/`setInterval` and their clears (`vi.useFakeTimers({ toFake: [...] })`) so `fetch`/`Response` promises resolve on the real microtask queue; the first test in the file drops its three fixed advances for the same advance-until-condition loop.
- `make ci` green; commit trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. No restore helper builds its own restoring context any more; every undo entry point builds exactly one; the existing fake-engine tests (call and step strings) still pass unchanged except where the brief names a change. Task 1.
2. A run's unanswered `create`: a container found by name with status `created` is removed; one absent leaves no step; a lookup failure is `rollback_failed`; a recreate's unanswered create keeps today's `undo` (unpark 409 already reports it). Task 1.
3. The web tests assert the same facts as before and pass 5 of 5 runs. Task 2.

---

### Task 1: One restoring context per undo, and the unanswered create

**Files:** `internal/runtime/docker/deploy.go`, `deploy_test.go`, `docker.go` if `restoring` moves; `internal/runtime/AGENTS.md` line-12 bullet; `docs/agent-protocol.md` explicit-frames paragraph.

- Change `discard`, `unpark`, `containerNow` (or `current`), `revive` and `Client.waitStopped`'s caller to take the context they are given; make `undo`, `rollback`, `settleRun`, `unparkIfParked` and `reviveIfStopped` each open one `restoring(ctx)` and pass it down; the wait nests `context.WithTimeout(rctx, stopWait)` inside it. A budget that runs out mid-undo is a failed restore (`rollback_failed`) unless the wait's own bound is what expired (still running, no step).
- Unanswered create on a run (`replace` run branch; `create` step): when the create outcome is `unknown` or `timed_out` and `created == ""`, under the restoring context `GET /containers/<name>/json`: 404 → nothing; status `created` → `DELETE ?force=1` (a failed delete is `rollback_failed`); any other status → leave it, no step; lookup error → `rollback_failed`. Fake-engine hook: a create delay (headers held) like `renameDelay`; cases: timed out and absent, timed out and created (DELETE), cancelled and created, lookup 500, delete 500.
- Tests: assert that a hung `discard` (delay past the budget) makes the whole undo end `rollback_failed` within one `rollbackBudget` (use a shortened budget knob in `export_test.go` as the wait test does), not five.
- Docs: replace the "each call with its own 70 s budget … about 165 s" sentence in both files with the new rule and worst case.
- Commit `docker: one restore budget per undo; settle a run's unanswered create`.

### Task 2: Condition-based plan poll test

**Files:** `web/src/components/ApplicationDeploymentPlan.test.tsx` (the two polling tests around lines 95-135).

- `vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })` in both tests; replace the first test's three fixed 5000 ms advances and the second test's 60-iteration loop with the same loop: advance 5000 ms until the body matches `/succeeded/i`, at most 20 iterations, then the existing assertions unchanged (including "polling stopped on a terminal state" and `onChanged` counts). Remove the now-stale comment about act() and the second advance. Run the file 5 times and the whole web suite once.
- Commit `web: poll the plan test on a condition with timer-only fakes`.

### Task 3: CI

- `make ci`; no `web/dist` change expected.

## Rulings

- A recreate's unanswered create is not looked up: `undo`'s `unpark` returns 409 when the daemon created the container, which already reports `rollback_failed`.
