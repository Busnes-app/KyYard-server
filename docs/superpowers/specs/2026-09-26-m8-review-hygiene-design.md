# M8 review hygiene (deferred minors of PRs 20–22)

This slice closes the minors the task and whole-branch reviews of feat/k8s-enrollment (#73),
feat/k8s-reconcile (#75) and feat/migration-analysis (#76) deferred. Each item below is a
requirement: a behaviour change or test with a Verify step, no new features, no schema change.
Items that depend on #76 (the migration and claims items) are included only where they touch
files master already has; anything else waits for #76 to merge and is marked so.

Decisions recorded 2026-09-25 (Yoshi): hygiene PR after PR 22, branched off master; approvals
delegated to the controller for the night.

## Runtime / agent (internal/runtime/kubernetes)
- Rollout wait: one failed `Get` ends the wait (`deploy.go` rollout); keep the last good read
  and poll on until the deadline. NotFound on `Update` should fall back to `Create`.
- `refusedWrite` labels any 403 on a write `admission_denied`; a Role missing a verb (partial
  or older manifest) reads as a quota/policy refusal. Distinguish with a SelfSubjectAccessReview
  for the failing verb, or name both causes in the sentence.
- A claim stuck `Pending` with no provisioner surfaces only as a generic rollout failure; add
  the claim's `Pending` phase/events reason to the `rollout_timeout` detail vocabulary.
- Snapshot: `ServerVersion` fetched by both `Facts` and `engine`; fetch once.
- `Remove`: the claim `List` cap now counts emitted steps; add a test for the cap.
- Test gaps: claim `Get` non-NotFound error; quota 403 on claim create; claim `List` failure in
  `Remove`; forbidden StorageClass list landing in `Truncated`; conflict→foreign `name_taken`;
  `AlreadyExists` race on create; parent cancel during rollout; idempotent re-apply.

## Store (internal/store)
- Cluster `settleRemoval`: retained-claim steps are informational (ruling); consider recording
  the retained claim names on the deployment row for the UI.
- Cross-project object-name collision (`shop`+`web-api` vs `shop-web`+`api`) is refused only
  at apply (`name_taken`); the plan could detect it when two instances share a namespace.
- Adding a service whose slug matches an existing one (`a-b` beside `a_b`) renames the
  existing objects; the old Deployment keeps running with a shared selector. Plan-time check.
- `decorate` parses the whole stored snapshot on every endpoint read for `cluster_health`;
  narrow the decode.
- Migration: abandon then recreate hits `application_name_taken` until the old destination is
  removed (documented in PR 22); consider suffixing the destination name.
- Migration: destination name overflow (>255) returns generic `ErrInvalid`; name a code.
- Migration: StorageClasses come from a snapshot of any age; add a freshness gate.
- Env value loop duplicated between `buildDeploymentFrame` and `kubernetesFrame`.

## API (internal/api)
- Runtime gate ordering: `dockerOnly`/`runtimeGate` runs before the action's own permission on
  commands, removal preview, adoption and mapping, so a viewer gets 409 not 403 and no denial
  audit row. Exec and inspection were fixed in PR 20; align the rest.
- Cluster plan still calls `inspectForPlan`; add an explicit `if !kube` guard.
- Shape-and-capability selection duplicated in apply and removal handlers.
- Mapping PUT reads the instance even when unused.
- Migration: inspection budget spent before cheap state refusals (reanalyze on a non-analyzed
  row; second POST while open; invalid choices); check state first.
- Migration: `sourceMigration` reads under `application.read`, so env admins get 404 before 403
  on choices/analyze; read under `application.migrate` for mutations.
- Test gaps: viewer on cluster routes; offline cluster at apply; removal before any apply;
  pending-endpoint `capability_mismatch` (added in PR 20 fix wave; keep); a migration row in
  the runtime gate matrix; inspection budget on the migration route; canary check on audit rows.

## Web
- `healthBadge` duplicated in `Endpoints.tsx` and `KubernetesCluster.tsx`.
- Toggle buttons lack `aria-expanded`.
- No client-side namespace format validation at enrollment.
- The `pod_security` sentence does not show which case applied (missing/privileged/invalid).
- Failed cluster rows still show Docker wording (`.kyyard-prev`) in the generic `failed` text.
- Edge-state tests: zero nodes, empty containers, vanished namespace filter.

## Docs
- `clusterDisclosure` first sentence now says "cluster-wide"; keep the disclosure and the
  threat model in step whenever the ClusterRole changes (add a test pinning the blast-radius
  wording).
- README: cluster inter-service addressing (`<project>-<service>`, published ports only) is
  undocumented (PR 21 M5).
- README upgrade order for a cluster agent exists; add the server-before-agent rule to the
  Kubernetes section header.
