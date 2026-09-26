# M8 hygiene follow-up (items that depended on #76)

This slice closes the review minors the hygiene plan deferred until PR 22 (#76, migration
analysis and persistent volume claims) merged. It stacks on `chore/m8-review-hygiene` (#77)
because it edits the same runtime and store files; it is retargeted to `master` once #77 merges.
No new features, no schema change; every behaviour change carries a test.

Decisions recorded 2026-09-26 (Yoshi): this follow-up is the next slice after #77; approvals
delegated to the controller.

## Runtime (`internal/runtime/kubernetes`)

- A claim stuck `Pending` (no provisioner, no default class) is named in the `rollout_timeout`
  detail: the rollout wait reads each planned claim's phase once the Deployment is not
  progressing and adds `claim=<Phase>` (closed reason words, `Pending` or `Lost`) to the detail
  list, at most one entry per claim within the existing three-pair cap. The web sentence for
  `rollout_timeout` names the claim when present ("a volume claim is still Pending: no
  StorageClass provisioned it").
- `Remove`'s claim `List` cap: a test that more than `MaxDeploymentVolumes` labelled claims emit
  exactly the cap and a log line, no error.
- Tests: claim `Get` failing with a non-NotFound error → the existing failure path
  (`runtime_status`), never `name_taken`; quota 403 on claim create → `admission_denied`;
  claim `List` failing in `Remove` → the run fails with `runtime_status`, deletes nothing; a
  forbidden StorageClass list → `storage_classes` in `Truncated` and an otherwise complete
  snapshot.

## Store (`internal/store`)

- Cluster removal records the claims the agent reported retained on the deployment row's result
  (the step list already carries them; `Deployment.RetainedClaims []string` is derived on read
  from the `volume`/`skipped`/`retained` steps, no schema change) and the application page lists
  them under "Kept on the cluster".
- Migration destination name: the destination is named `<source> on <endpoint>` and, when that
  name is taken, `<source> on <endpoint> (2)`, `(3)`, up to 9, so abandon-then-recreate succeeds
  without deleting the old destination; the tenth attempt is `application_name_taken`.
- Migration destination name overflow: a source or endpoint name that makes the destination
  exceed 255 characters is refused with the closed code `destination_name_too_long` (409) and
  a web sentence naming the limit.
- Migration StorageClasses freshness: analysis and choices read the destination endpoint's
  inventory only if it is fresher than `InventoryStale` (the existing freshness rule the plan
  uses); a stale inventory is 409 `inventory_stale` (existing code) with the existing sentence.

## API (`internal/api`)

- Migration state is checked before inspections are spent: `POST …/migration` while one is open
  answers `migration_open` before inspecting; `POST …/analyze` and `PUT …/choices` on a row that
  is not `analyzed` answer `migration_state` before inspecting; invalid choices are refused
  before re-analysis.
- `sourceMigration` for the mutating routes reads under `application.migrate`, so an
  environment administrator gets 403 (and a denied audit row) before any 404 or 409.
- Tests: a migration row in `TestRuntimeGateMatrix`; the inspection budget on the migration
  route (analysis past the per-actor budget yields `inspection_unavailable`, not an extra
  inspection); the secret canary asserted absent from migration audit rows.

## Documents

`docs/application-schema.md` (retained claims on removal; destination naming and the two
codes), `docs/agent-protocol.md` (`claim=` reason words), `docs/authorization-matrix.md`
(mutating migration routes read under `application.migrate`), README (retained claims are
listed; destination naming), AGENTS.md chain, `web/src` code tables and the vocabulary fixtures.

## Out of scope

Everything else in the hygiene backlog's "deferred" lines that the hygiene review already
declined; any behaviour change to Docker paths; schema changes.
