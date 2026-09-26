# Hygiene after Kubernetes health validation (#79)

Closes the minors the task and whole-branch reviews of #79 deferred. No new feature, no schema
change; each behaviour change carries a test. Decisions (controller, delegated approval,
2026-09-26) are recorded inline.

## 1. Prune keeps what an open validation may still roll back to

`Prune` (`internal/store/samples.go`, the `deployments` clause) keeps the newest succeeded apply
per revision and any deployment whose own validation is `rollbackOpen`. It does not keep the
apply a pending rollback would return to: once update C settles at the same revision as its
predecessor B, B is no longer "newest for its revision", and if B is older than
`DeploymentHistoryRetention` it can be deleted during C's validation window, so C's rollback
reads `no_prior_identity`.

Decision: while any validation on an instance is `rollbackOpen`, no succeeded apply of that
instance is pruned. One `NOT EXISTS` over `deployments x JOIN deployment_validations v` on
`x.instance_id = d.instance_id AND rollbackOpen`. Over-protecting a few rows for the minutes a
window lasts costs nothing; picking exactly the predecessor would duplicate `clusterRollback`'s
ordering in SQL for no gain. Test: an instance with applies A (old, revision 1), B (old, same
revision as C), C (validation in `observing`, automated); `Prune` past the retention deletes A
and keeps B; once C's validation is `done` with `rollback_outcome` set, B is pruned. Both dialects.

## 2. Cluster rows skip loads they discard

`PendingValidations` calls `boundServices` and `latestInventory` for every row, then forces
`Presence = unknown` for `Kind: Deployment` services. Decision: when `p.Kubernetes`, skip both
loads. Test: a cluster row is returned with `unknown` presences and no inventory row present
(today `latestInventory` tolerates absence, so the test pins that the result is identical with
and without an inventory).

## 3. The no-registry-call claim is asserted

`fakeDigests` (`internal/api/image_check_handlers_test.go`) gains a mutex-guarded call count and
`calls()` accessor; `newClusterValidation` keeps its resolver on the harness.
`TestClusterValidationRollsBackToThePriorDigests` asserts the count is unchanged across the
rollback plan.

## 4. The registry-row gate on a pinned cluster rollback is tested

Behaviour is unchanged and intended: `registryFor` runs before the digest pin shortcut, so a
pinned rollback whose host has no registry row and anonymous pull off is refused
`registry_not_configured`, the rollback deployment fails and the policy pauses with the blocker
text. Store test by analogy with `TestPlanDeploymentPinsARegistryDigest`: a cluster
`PlanRequest{PinImages}` with the host's registry row deleted and anonymous pull off is blocked
`registry_not_configured`; API test in `kubernetes_validation_test.go`: the same arrangement
before the rollback tick leaves the validation `rollback_outcome` failed/ineligible as the code
records it and the policy `paused` with a reason naming `registry_not_configured`. The test
asserts the code's existing outcome fields; it must not change them.

## 5. Web

`HealthWarning` (`web/src/components/ApplicationPolicy.tsx`) branches `kubernetes` else Docker.
Decision: make the Docker branch explicit (`runtime === 'docker'`) and render nothing for an
unknown runtime, with a test. Old cluster validation rows closed before #79 carry the Docker
detail; the web keys on the server's detail string and has no runtime: accepted as historical,
no change.

## Documents

`internal/store/AGENTS.md` (Prune rule, PendingValidations cluster rows), `internal/api/AGENTS.md`
(the rollback tests), `docs/retention-policy.md` if it states the deployments prune rule,
`web/AGENTS.md` if it describes the warning.

## Out of scope

Everything else on the #79 ledger's parked list: duplicate pod UIDs, baseline size, HPA
Deployments, the residual plan/apply window in cluster rollback.
