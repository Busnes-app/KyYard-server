# Migration Analysis and Preview (M8 PR 22) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An administrator analyzes a Docker-adopted application against a Kubernetes cluster, sees every service classified on every axis with a fixed code and sentence, chooses a StorageClass and size per named volume, and gets a linked destination application on the cluster that plans and applies through PR 21's path with PersistentVolumeClaims, while the source keeps running until the operator confirms cutover.

**Architecture:** The protocol gains claims and claim mounts on a cluster frame, the `claim_immutable` step code, the retained-claim removal step and the inventory's StorageClasses (Task 1). The store adds `KubernetesExtension` on the spec, migration 35 and the migration lifecycle, including destination creation that copies the source's value bundle (Task 2). The pure `internal/migration` package analyzes a source into a report (Task 3). The Kubernetes preflight, plan and frame turn chosen named volumes into claims and mounts (Task 4). The agent renders, creates and retains claims and reports StorageClasses (Task 5), under an extended RBAC manifest proven on a real cluster (Task 6). The API orchestrates analysis over the plan-time inspection primitive (Task 7). The web shows the Migration card and the new sentences (Task 8). Documents and the gate close (Task 9).

**Tech Stack:** Go 1.26 (module `go 1.26.6`), `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go` v0.37.1 (already required; no module is added, agent only), SQLite + PostgreSQL 17, the coder/websocket agent protocol (fake agent sockets in tests), React 19 + Vitest.

**Spec:** `docs/superpowers/specs/2026-09-25-migration-analysis-design.md`

## Global Constraints

- Work in the worktree `/home/yoshi/busness.app/KyYard-Server/.worktrees/migration` (branch `feat/migration-analysis`, on top of `master` 50095a8 and the committed spec). Every command below runs from its root.
- Commit trailer, exactly: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Gate each commit on the previous command's exit (`&&`).
- `gofmt -w` every edited Go file and check `gofmt -l cmd internal` prints nothing before each commit. Never put two single quotes or two backticks in a row in a Go comment: gofmt turns them into curly quotes. `make tidy-check lint` runs after `git add` (it diffs the working tree against the index) and before `git commit`. `make ci` must pass before the branch is pushed (Task 9). Never run `make build-web` while `make ci` runs: both run `npm ci` in `web/`.
- `web/dist` is embedded and committed: Task 8 rebuilds it with `make build-web` and commits it with `web/tsconfig.tsbuildinfo`.
- DOX: the task that changes a directory's contract updates that directory's `AGENTS.md` in the same commit. `internal/migration/AGENTS.md` is new (Task 3) and is indexed in the root `AGENTS.md` Child DOX Index in the same commit.
- The server links no client-go: `go list -deps ./cmd/server | grep -c k8s.io` prints `0`, and `go list -deps ./internal/migration | grep -c k8s.io` prints `0`. `internal/migration` imports `store` types and `protocol`, never SQL or an SDK; the store never imports `internal/migration`.
- Every store change is tested on SQLite and PostgreSQL. PostgreSQL runs use the local container `kyyard-access-pg`; read the password into a variable, never print it: `PGPASS=$(docker inspect kyyard-access-pg | jq -r '.[0].Config.Env[]' | sed -n 's/^POSTGRES_PASSWORD=//p') && KY_TEST_POSTGRES_DSN="postgres://postgres:$PGPASS@127.0.0.1:15440/kyyard?sslmode=disable" go test ...`. Below this prefix is written `PG=… go test`.
- Every new SQL statement carries `organization_id` and `environment_id` predicates like its neighbours.
- Real-cluster tests skip unless `KY_TEST_KUBECONFIG` is set; with it set, `KY_TEST_DEPLOY_IMAGE` must name a digest-pinned image that keeps running (for example `registry.k8s.io/pause@sha256:<digest>`).
- Migration: **35**, `application_migrations` (the latest registered is 34, `kubernetes_namespaces`).
- Every new closed code has a sentence in a web code table, and the web tables are checked against the Go vocabulary through generated JSON fixtures: `web/src/protocol-codes.json` (step and unsupported codes, `internal/agent/protocol`, Task 1) and `web/src/migration-codes.json` (analyzer, checklist and assumption codes, `internal/migration`, Task 3). `KY_UPDATE_FIXTURES=1 go test <package> -run TestWebVocabularyFixture` regenerates a fixture; the Go test fails while a fixture is stale, and the web tests (Task 8) fail while a table misses a fixture code.
- Spec values, exactly: statuses `analyzed`, `destination_created`, `validated`, `cutover_confirmed`, `abandoned`; `MaxMigrationReportBytes` 65536; `KubernetesExtension{Volumes map[string]KubernetesVolume}` with `storage_class` (DNS-1123 subdomain, `""` the cluster default), `size` (1Mi..16Ti), `access_mode` (`ReadWriteOnce` only); classes `supported`, `operator_choice_required`, `blocked`; axes `storage`, `networking`, `ports`, `secrets`, `probes`, `resources`, `scheduling`, `flags`; action `application.migrate`, audited on `<application>/migration/<id>`; routes under `.../applications/{application}/migration`: `POST` (start), `GET`, `PUT .../choices`, `POST .../analyze`, `POST .../destination`, `POST .../validated`, `POST .../cutover`, `DELETE`; errors 409 `runtime_unsupported`, 400 `namespace_unknown`, 409 `migration_open`, 400 `storage_class_unknown`, `size_invalid`, `volume_unknown`, 409 `application_name_taken`; destination name `<name> on <endpoint>`; claim name `<project>-<volume>` through `KubernetesNames`; caps 16 claims per request, 8 mounts per service, 100 StorageClasses; step code `claim_immutable` (detail `Kind/name`); unsupported code `k8s_volume_shared`; `k8s_volume` detail `choice_required`; Role rule `persistentvolumeclaims` `get, list, create`; ClusterRole rule `storage.k8s.io` `storageclasses` `get, list`; notes 1..500 printable characters.

Plan decisions where the code or the spec's silence forced a choice (each is reported to the reviewer; none changes a spec value):

1. **Who runs the analyzer.** `internal/migration` imports store types, so the store cannot call it. The API gathers the inputs (`store.ReadMigrationSource`, the plan-time inspections), runs `migration.Analyze`, and hands the store a `store.MigrationAnalysis{Revision, Report, Ready}`; the store persists it with the migration under one authorized, audited transaction and trusts `Ready` (a value produced inside the boundary). The store itself validates everything it owns: runtimes, the namespace grant, one open migration, choices against the analyzed revision's named volumes and the destination inventory's StorageClasses.
2. **Extra columns.** The spec's table has no place for the analyzed revision, the readiness verdict or the first confirmation's actor and note. Migration 35 adds `source_revision`, `ready`, `validated_by`, `validated_at`, `validated_note` and `cutover_note` beside the spec's columns. Destination creation refuses 409 `migration_stale` when the source's latest revision is no longer the analyzed one.
3. **Destination application FK.** A composite `ON DELETE SET NULL` would null the tenant columns too, so `destination_application_id` references `applications(id)` alone; the source is the composite tenant FK (`ON DELETE CASCADE`: a discarded source takes its migration history) and so is the destination endpoint (`ON DELETE RESTRICT`).
4. **`application.migrate` holders.** The spec says "the roles that hold `application.adopt` (organization administrators)"; environment administrators also hold `application.adopt`. The concrete parenthetical wins: organization administrators only, because destination creation copies the source's secret values, which only they may reveal.
5. **Extra refusals.** 409 `migration_not_ready` (destination before `Ready`), 409 `migration_state` (an action the status does not allow), 409 `migration_stale` (decision 2). Each has a sentence in the web.
6. **Source removal while open.** `RemoveApplication` on a source with an open migration is 409 `migration_open`: the decision "the source is preserved until the operator confirms" is enforced, not only displayed. After `cutover_confirmed` (or `abandoned`) the source removes as before.
7. **`GET .../migration` on a destination.** It answers for a source with its open migration and for a destination with the migration that created it (any status), with `role` `source` or `destination`, so the destination page shows "Migration destination of <source>" before its first apply.
8. **Analyzer input shape.** `Input.Containers` is keyed by service (`map[string]protocol.Container`) like `Inspections`, and `Input` carries the source `Project`; the destination names come from `Destination.Project`. Both are needed for the copy recipe and claim names and cannot be recovered from a bare list.
9. **Default axis codes.** "Every service on every axis" needs a code where nothing applies: `storage_supported`, `networking_supported`, `probes_supported`, `resources_supported`, `scheduling_supported`, `flags_supported` (supported), following the spec's `secrets_supported`. `port_unpublished` is the ports axis with no published port.
10. **Unlisted inspection codes.** `network` (on a network other than the project's) counts as `networks_multiple`; every other inspection code the spec does not place (`tmpfs`, `dns`, `init`, `extra_hosts`, ...) is `flag_blocked` with the code as detail, so no reported configuration is read as support.
11. **`read_only_rootfs`.** The spec calls it supported and "rendered as `readOnlyRootFilesystem`", but a `compose.v1` definition cannot carry it and `render` renders no such field; the destination would silently run writable. It is reported `operator_choice_required`, code `read_only_rootfs`, with that sentence. Rendering it is a later slice.
12. **Healthcheck detection.** There is no `healthcheck` unsupported code; `healthcheck_dropped` fires on `image_config` or on an inspected health other than `none`.
13. **`k8s_volume` detail.** `PreflightService` and `BlockedService` gain `details` (`{"k8s_volume": "choice_required"}`) because `unsupported` is a code list with nowhere to put a parameter. A bind anywhere in the service keeps `k8s_volume` without detail.
14. **Retained claims on removal.** `DeploymentResult.Validate` forbids a detail on a skipped step, and a claim is not a service. A kept claim is reported as a `volume` step, outcome `skipped`, detail `retained`, under the service that mounts it (its `kyyard.busnes.app/service` label); `Validate` allows exactly that shape and the cluster `settleRemoval` accepts `volume` steps.
15. **Where `claim_immutable` is found.** At the precondition, from the claim already read, so the run stops before any write (not only before the Deployment). A claim created with the cluster default (`storage_class` `""`) accepts whatever class the cluster assigned; size and access mode must match exactly.
16. **Inspections.** Analysis reuses `planInspections` as-is: the grant names the requesting administrator (a wire actor must be one the authority re-check can follow), under the plan's `inspection:<actor>` budget of 30 a minute. "`system-migration`" in the spec names no actor the code has; nothing is added for it.
17. **Choices and re-analysis in one write.** `PUT .../choices` and `POST .../analyze` gather inputs, analyze with the choices, then call one store method (`AnalyzeMigration`) that validates the choices and stores choices and report together only while the migration is `analyzed`.
18. **Size grammar.** A size is a whole number of `Mi`, `Gi` or `Ti` (`protocol.StorageSizeBytes`), 1Mi..16Ti; the protocol cannot parse full Kubernetes quantities without apimachinery, and one grammar on both sides means the claim KyYard writes is the claim it compares.
19. **Default StorageClass.** `storage_class` `""` is accepted only while the destination inventory reports a default class; otherwise the claim would stay Pending forever, so the choice is 400 `storage_class_unknown`.
20. **A fresh source inventory.** Analysis reads the mapped containers through the mapping, which requires the source endpoint active with a fresh inventory (else 409 `adoption_changed`, as a plan answers); `inspection_unavailable` covers an agent that is connected but does not answer the inspection in the plan's budget.
21. **Error-code sentences.** The API's new error codes are string literals in `tenantError` with no Go vocabulary to generate a fixture from; the web test lists the new ones and requires a `MIGRATION_ERRORS` sentence for each. The analyzer's, checklist, assumption, step and unsupported codes are fixture-checked both ways.
22. **Copy recipe.** `copy_volume` lists only named volumes one service mounts that exist on the source host (the endpoint's volume list), one `docker run ... | kubectl exec -i deploy/<name> -- tar` command per volume with the target shell-quoted; a destination Deployment name comes from `KubernetesNames(<destination project>, services)`.

## Review Focus

1. The operator applies the destination twice, or re-applies after a restart: a claim created with the cluster default must not become `claim_immutable` because the cluster filled in `standard`. Pinned in Task 5 (`TestDeployClaimDefaultClassIsStable`).
2. Someone else (a Helm chart, a hand-made PVC) already owns `<project>-<volume>` in the namespace: the apply stops at the precondition, names the claim, writes nothing. Pinned in Task 5 (`TestDeployClaimPreconditions`).
3. The operator removes the destination application: its claim and the data in it stay, and the removal says so. Pinned in Task 5 (`TestRemoveRetainsClaims`), Task 4 (`TestRemoveKubernetesRetainsClaims`: the store settles the retained step and nothing else new), Task 8 (the retained sentence) and the real-cluster test (Task 6).
4. The operator removes the source before confirming cutover: refused with `migration_open` inside the removal transaction, so no frame is built. Pinned in Task 2 (`TestMigrationKeepsTheSource`, which also proves the removal goes through once the migration is abandoned).
5. The source definition changes after analysis (a new revision saved): destination creation refuses `migration_stale` rather than copying a definition nobody analyzed, and the copied secret bundle is the analyzed revision's. Pinned in Task 2 (`TestMigrationDestination`, `TestMigrationLifecycle`).

## File map

| Path | Task | Responsibility |
|---|---|---|
| `internal/agent/protocol/kubernetes_claims.go` (new), `kubernetes_deploy.go`, `deployment.go`, `inspection.go`, `kubernetes.go`, `vocabulary_test.go` (new), `web/src/protocol-codes.json` (new, generated) | 1 | Claims and mounts, sizes, StorageClasses, `claim_immutable`, retained step, `k8s_volume_shared`, web fixture |
| `internal/store/application_spec.go`, `applications.go`, `application_migration.go` (new), `migrations/migrations.go`, `store.go`, `application_apply.go`, `application_deployment.go`, `internal/permissions/permissions.go` | 2 | `KubernetesExtension`, migration 35, the migration lifecycle, `migration_id`, `application.migrate` |
| `internal/migration/migration.go` (new), `checklist.go` (new), `AGENTS.md` (new), `web/src/migration-codes.json` (new, generated) | 3 | The pure analyzer, report, checklist, web fixture |
| `internal/store/application_preflight.go`, `application_deployment.go`, `application_apply.go` | 4 | Claims in the Kubernetes preflight, plan, frame and removal settle |
| `internal/runtime/kubernetes/render/render.go`, `deploy.go`, `remove.go`, `kubernetes.go` | 5 | PVC objects, claim precondition, create, retain, StorageClasses in the snapshot |
| `internal/runtime/kubernetes/manifest/manifest.go`, `cluster_test.go` | 6 | Claim and StorageClass RBAC, the real-cluster claim |
| `internal/api/migration_handlers.go` (new), `server.go`, `tenant_handlers.go` | 7 | Routes, analysis orchestration, error codes |
| `web/src/components/ApplicationMigration.tsx` (new), `Applications.tsx`, `ApplicationDeploymentPlan.tsx`, `ApplicationPreflight.tsx`, `ApplicationInspection.tsx`, `KubernetesMapping.tsx`, `web/src/tenant.ts` | 8 | Migration card, destination link, code tables, build |
| `README.md`, `docs/*.md`, `KyYard-Implementation-Plan.md`, `KyYard-Engineering-Handoff.md` | 9 | Operator and protocol documents, status, gate |

---
### Task 1: Protocol — claims, claim mounts, StorageClasses and the new codes

**Files:**
- Create: `internal/agent/protocol/kubernetes_claims.go`
- Create: `internal/agent/protocol/kubernetes_claims_test.go`
- Create: `internal/agent/protocol/vocabulary_test.go`
- Create (generated): `web/src/protocol-codes.json`
- Modify: `internal/agent/protocol/kubernetes_deploy.go` (`KubernetesTarget.Claims`, `validateKubernetes`, `kubeObject`)
- Modify: `internal/agent/protocol/deployment.go` (`DeploymentService.Volumes`, `claim_immutable`, the retained step, Docker and removal refusals)
- Modify: `internal/agent/protocol/inspection.go` (`k8s_volume_shared`)
- Modify: `internal/agent/protocol/kubernetes.go` (`KubernetesInventory.StorageClasses`)
- Modify tests: `internal/agent/protocol/deployment_test.go`, `inspection_test.go` (vocabulary counts), `internal/store/kubernetes_deployment_test.go` (`KubernetesTarget` holds a slice now, so it is compared with `reflect.DeepEqual`)
- Docs: `internal/agent/AGENTS.md`

**Interfaces:**
- Consumes: `goodKubernetesDeployment`, `goodDeployment`, `testApp`, `testInstance`, `testDigest` (existing test helpers), `cleanAbsolute`, `ValidDNSLabel`, `ValidDNSSubdomain`, `decodeField`, `Clamp`, `UnmarshalSnapshotBounded` (existing, package `protocol`).
- Produces (package `protocol`):
  ```go
  const MaxKubernetesClaims, MaxKubernetesMounts, MaxStorageClasses = 16, 8, 100
  const AccessReadWriteOnce = "ReadWriteOnce"
  const DetailRetained = "retained"
  type KubernetesClaim struct{ Name, StorageClass, Size, AccessMode string } // json name, storage_class, size, access_mode
  type KubernetesMount struct{ Claim, MountPath string; ReadOnly bool }      // json claim, mount_path, read_only,omitempty
  type StorageClass struct{ Name string; Default bool }                      // json name, default
  func StorageSizeBytes(s string) (int64, bool)
  func ValidStorageClass(s string) bool
  // KubernetesTarget gains  Claims []KubernetesClaim `json:"claims,omitempty"` (a removal carries none)
  // DeploymentService gains Volumes []KubernetesMount `json:"volumes,omitempty"` (Kubernetes only)
  // KubernetesInventory gains StorageClasses []StorageClass `json:"storage_classes"` (never nil after Clamp)
  // Step code claim_immutable (detail empty or Kind/name; kubeObject kinds gain PersistentVolumeClaim).
  // A skipped volume step may carry detail DetailRetained and no code.
  // UnsupportedCodes ends with k8s_volume_shared (35 codes; MaxUnsupported stays 40).
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/protocol/kubernetes_claims_test.go`:

```go
package protocol

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// claimDeployment is goodKubernetesDeployment with web mounting claim shop-data.
func claimDeployment(now time.Time) DeploymentRequest {
	r := goodKubernetesDeployment(now)
	r.Kubernetes.Claims = []KubernetesClaim{{Name: "shop-data", StorageClass: "standard", Size: "10Gi", AccessMode: AccessReadWriteOnce}}
	r.Services[0].Volumes = []KubernetesMount{{Claim: "shop-data", MountPath: "/var/lib/data"}}
	return r
}

// A cluster frame's claims are each mounted by exactly one service at a clean path, sized in
// Mi, Gi or Ti up to 16Ti, ReadWriteOnce, in a named or the default StorageClass; a Docker frame
// carries no claim mount and a removal no claim.
func TestKubernetesClaimsValidation(t *testing.T) {
	now := time.Now()
	if err := claimDeployment(now).ValidateFor(RuntimeKubernetes, now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"default class": func(r *DeploymentRequest) { r.Kubernetes.Claims[0].StorageClass = "" },
		"same claim, two paths": func(r *DeploymentRequest) {
			r.Services[0].Volumes = append(r.Services[0].Volumes, KubernetesMount{Claim: "shop-data", MountPath: "/backup", ReadOnly: true})
		},
		"16Ti": func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "16Ti" },
		"1Mi":  func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "1Mi" },
	} {
		r := claimDeployment(now)
		mutate(&r)
		if err := r.Validate(now); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*DeploymentRequest){
		"unmounted claim": func(r *DeploymentRequest) { r.Services[0].Volumes = nil },
		"unknown claim":   func(r *DeploymentRequest) { r.Services[0].Volumes[0].Claim = "shop-logs" },
		"two services": func(r *DeploymentRequest) {
			r.Services[1].Volumes = []KubernetesMount{{Claim: "shop-data", MountPath: "/data"}}
		},
		"duplicate claim": func(r *DeploymentRequest) { r.Kubernetes.Claims = append(r.Kubernetes.Claims, r.Kubernetes.Claims[0]) },
		"bad claim name": func(r *DeploymentRequest) {
			r.Kubernetes.Claims[0].Name = "Shop_Data"
			r.Services[0].Volumes[0].Claim = "Shop_Data"
		},
		"bad class":       func(r *DeploymentRequest) { r.Kubernetes.Claims[0].StorageClass = "Fast SSD" },
		"read write many": func(r *DeploymentRequest) { r.Kubernetes.Claims[0].AccessMode = "ReadWriteMany" },
		"no access mode":  func(r *DeploymentRequest) { r.Kubernetes.Claims[0].AccessMode = "" },
		"decimal size":    func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "1.5Gi" },
		"bare bytes":      func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "1073741824" },
		"SI suffix":       func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "10G" },
		"zero":            func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "0Gi" },
		"over 16Ti":       func(r *DeploymentRequest) { r.Kubernetes.Claims[0].Size = "17Ti" },
		"relative path":   func(r *DeploymentRequest) { r.Services[0].Volumes[0].MountPath = "data" },
		"root path":       func(r *DeploymentRequest) { r.Services[0].Volumes[0].MountPath = "/" },
		"unclean path":    func(r *DeploymentRequest) { r.Services[0].Volumes[0].MountPath = "/var/../data" },
		"path twice": func(r *DeploymentRequest) {
			r.Services[0].Volumes = append(r.Services[0].Volumes, r.Services[0].Volumes[0])
		},
		"too many mounts": func(r *DeploymentRequest) { r.Services[0].Volumes = manyMounts(MaxKubernetesMounts + 1) },
		"too many claims": func(r *DeploymentRequest) { r.Kubernetes.Claims = manyClaims(MaxKubernetesClaims + 1) },
		"docker mount beside": func(r *DeploymentRequest) {
			r.Services[0].Mounts = []Mount{{Kind: MountVolume, Source: "shop_data", Target: "/x"}}
		},
	} {
		r := claimDeployment(now)
		mutate(&r)
		if r.Validate(now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	docker := goodDeployment(now)
	docker.Services[0].Volumes = []KubernetesMount{{Claim: "shop-data", MountPath: "/data"}}
	if docker.Validate(now) == nil {
		t.Error("a Docker frame carried a claim mount")
	}
	removal := RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(5 * time.Minute),
		Kubernetes: &KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testDigest, Claims: claimDeployment(now).Kubernetes.Claims}, Services: []string{"web"}}
	if removal.Validate(now) == nil {
		t.Error("a removal carried claims")
	}
}

func manyMounts(n int) []KubernetesMount {
	out := []KubernetesMount{}
	for i := range n {
		out = append(out, KubernetesMount{Claim: "shop-data", MountPath: "/m" + strings.Repeat("x", i+1)})
	}
	return out
}

func manyClaims(n int) []KubernetesClaim {
	out := []KubernetesClaim{}
	for i := range n {
		out = append(out, KubernetesClaim{Name: "c" + strings.Repeat("x", i+1), Size: "1Gi", AccessMode: AccessReadWriteOnce})
	}
	return out
}

func TestStorageSizeBytes(t *testing.T) {
	for in, want := range map[string]int64{"1Mi": 1 << 20, "512Mi": 512 << 20, "10Gi": 10 << 30, "16Ti": 16 << 40, "16384Gi": 16 << 40} {
		if got, ok := StorageSizeBytes(in); !ok || got != want {
			t.Errorf("%s: %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0Mi", "01Gi", "1Ki", "1G", "1gi", "16385Gi", "1000000Mi", "-1Gi", " 1Gi", "1Gi "} {
		if _, ok := StorageSizeBytes(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

// claim_immutable names the claim; a removal's kept claim is a skipped volume step with detail
// retained and nothing else carries that detail.
func TestClaimStepCodes(t *testing.T) {
	for _, tc := range []struct {
		step DeploymentStep
		ok   bool
	}{
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "claim_immutable", Detail: "PersistentVolumeClaim/shop-data"}, true},
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "claim_immutable", Detail: ""}, true},
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "claim_immutable", Detail: "10Gi"}, false},
		{DeploymentStep{Service: "db", Step: StepPrecondition, Outcome: OutcomeDenied, Code: "name_taken", Detail: "PersistentVolumeClaim/shop-data"}, true},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSkipped, Detail: DetailRetained}, true},
		{DeploymentStep{Service: "db", Step: StepRemove, Outcome: OutcomeSkipped, Detail: DetailRetained}, false},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSucceeded, Detail: DetailRetained}, false},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSkipped, Detail: "kept"}, false},
		{DeploymentStep{Service: "db", Step: StepVolume, Outcome: OutcomeSkipped, Code: "claim_immutable", Detail: DetailRetained}, false},
	} {
		outcome, code := OutcomeSucceeded, ""
		if tc.step.Outcome == OutcomeDenied {
			outcome, code = OutcomeDenied, ResultStepFailed
		}
		r := DeploymentResult{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", Outcome: outcome, Code: code, Services: []DeploymentIdentity{}, Steps: []DeploymentStep{tc.step}}
		if err := r.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.step, err)
		}
	}
}

// Storage classes decode bounded, are cleaned, never nil, and are cut first among nothing: a
// long list is named storage_classes.
func TestStorageClassesInventory(t *testing.T) {
	classes := make([]string, 0, MaxStorageClasses+5)
	for i := range MaxStorageClasses + 5 {
		classes = append(classes, `{"name":"c`+strings.Repeat("x", i%3)+`","default":`+map[bool]string{true: "true", false: "false"}[i == 0]+`}`)
	}
	var s Snapshot
	if err := UnmarshalSnapshotBounded([]byte(`{"generation":1,"kubernetes":{"storage_classes":[`+strings.Join(classes, ",")+`]}}`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Kubernetes.StorageClasses) != MaxStorageClasses || !s.Kubernetes.StorageClasses[0].Default || !slices.Contains(s.Truncated, "storage_classes") {
		t.Fatalf("decoded %d %v", len(s.Kubernetes.StorageClasses), s.Truncated)
	}
	dirty := &Snapshot{Kubernetes: &KubernetesInventory{StorageClasses: []StorageClass{{Name: "fast\n‮ssd"}}}}
	Clamp(dirty)
	if dirty.Kubernetes.StorageClasses[0].Name != "fastssd" {
		t.Fatalf("clamp %q", dirty.Kubernetes.StorageClasses[0].Name)
	}
	empty := &Snapshot{Kubernetes: &KubernetesInventory{}}
	Clamp(empty)
	if empty.Kubernetes.StorageClasses == nil {
		t.Fatal("a nil storage class list")
	}
}
```

Create `internal/agent/protocol/vocabulary_test.go`:

```go
package protocol

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The web's step-code and unsupported-code tables are checked against this vocabulary through
// a generated fixture. KY_UPDATE_FIXTURES=1 rewrites it.
func TestWebVocabularyFixture(t *testing.T) {
	want, err := json.MarshalIndent(map[string][]string{"step_codes": slices.Sorted(maps.Keys(stepCodes)), "unsupported_codes": UnsupportedCodes}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("..", "..", "..", "web", "src", "protocol-codes.json")
	if os.Getenv("KY_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is stale (%v): run KY_UPDATE_FIXTURES=1 go test ./internal/agent/protocol -run TestWebVocabularyFixture", path, err)
	}
}
```

In `internal/agent/protocol/deployment_test.go`, `TestDeploymentStepCodeDetails`, replace

```go
	if len(stepCodes) != 35 || len(resultCodes) != 8 {
```

with

```go
	if len(stepCodes) != 36 || len(resultCodes) != 8 {
```

In `internal/agent/protocol/inspection_test.go`, `TestInspectionUnsupportedVocabulary`, replace

```go
	if len(UnsupportedCodes) != 34 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" || UnsupportedCodes[33] != "k8s_namespace" {
```

with

```go
	if len(UnsupportedCodes) != 35 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" || UnsupportedCodes[34] != "k8s_volume_shared" {
```

In `internal/store/kubernetes_deployment_test.go`, add `"reflect"` to the imports (after `"errors"`) and, in `TestKubernetesPlanAndFrame`, replace

```go
*req.Kubernetes != (protocol.KubernetesTarget{Namespace: "shop", ApplicationID: app.ID, InstanceID: m.InstanceID, SpecDigest: d.SpecDigest})
```

with

```go
!reflect.DeepEqual(*req.Kubernetes, protocol.KubernetesTarget{Namespace: "shop", ApplicationID: app.ID, InstanceID: m.InstanceID, SpecDigest: d.SpecDigest})
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/protocol/`
Expected: FAIL to compile: `undefined: KubernetesClaim`, `undefined: KubernetesMount`, `undefined: AccessReadWriteOnce`, `undefined: StorageSizeBytes`, `undefined: DetailRetained`, `undefined: StorageClass`.

- [ ] **Step 3: Implement**

Create `internal/agent/protocol/kubernetes_claims.go`:

```go
package protocol

import (
	"errors"
	"regexp"
	"strconv"
)

// Claims a cluster deployment mounts: a PersistentVolumeClaim per chosen named volume, created
// once and never updated or deleted by the agent. See docs/agent-protocol.md, Claims.
const (
	MaxKubernetesClaims = 16 // per request
	MaxKubernetesMounts = 8  // per service
	MaxStorageClasses   = 100
	AccessReadWriteOnce = "ReadWriteOnce"
	// DetailRetained is the detail of a removal's skipped volume step: the claim was kept.
	DetailRetained = "retained"
)

// KubernetesClaim is one PersistentVolumeClaim of the instance: its name, the StorageClass
// ("" is the cluster default), the requested size and the access mode.
type KubernetesClaim struct {
	Name         string `json:"name"`
	StorageClass string `json:"storage_class"`
	Size         string `json:"size"`
	AccessMode   string `json:"access_mode"`
}

// KubernetesMount mounts a claim the request names at MountPath.
type KubernetesMount struct {
	Claim     string `json:"claim"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// StorageClass is a cluster StorageClass as the inventory reports it; Default is the
// is-default-class annotation.
type StorageClass struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

var storageSize = regexp.MustCompile(`^([1-9][0-9]{0,5})(Mi|Gi|Ti)$`)

// StorageSizeBytes reads a claim size: a whole number of Mi, Gi or Ti from 1Mi to 16Ti, the one
// quantity form KyYard writes. ok is false for anything else.
func StorageSizeBytes(s string) (int64, bool) {
	m := storageSize.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	shift := map[string]uint{"Mi": 20, "Gi": 30, "Ti": 40}[m[2]]
	bytes := n << shift
	return bytes, bytes <= 16<<40
}

// ValidStorageClass is "" (the cluster default) or a DNS-1123 subdomain.
func ValidStorageClass(s string) bool { return s == "" || ValidDNSSubdomain(s) }

func (c KubernetesClaim) valid() bool {
	_, size := StorageSizeBytes(c.Size)
	return ValidDNSLabel(c.Name) && ValidStorageClass(c.StorageClass) && size && c.AccessMode == AccessReadWriteOnce
}

// validClaims holds a cluster frame's claims to its services' mounts: at most
// MaxKubernetesClaims distinct claims, each mounted by exactly one service (ReadWriteOnce cannot
// serve two pods), at most MaxKubernetesMounts per service at distinct clean paths.
func (r DeploymentRequest) validClaims() error {
	claims := r.Kubernetes.Claims
	if len(claims) > MaxKubernetesClaims {
		return errors.New("too many claims")
	}
	mountedBy := map[string]string{}
	for _, c := range claims {
		if !c.valid() {
			return errors.New("invalid claim")
		}
		if _, dup := mountedBy[c.Name]; dup {
			return errors.New("duplicate claim")
		}
		mountedBy[c.Name] = ""
	}
	for _, s := range r.Services {
		if len(s.Volumes) > MaxKubernetesMounts {
			return errors.New("too many claim mounts")
		}
		paths := map[string]bool{}
		for _, m := range s.Volumes {
			by, known := mountedBy[m.Claim]
			if !known || (by != "" && by != s.Name) || !cleanAbsolute(m.MountPath) || paths[m.MountPath] {
				return errors.New("invalid claim mount")
			}
			mountedBy[m.Claim], paths[m.MountPath] = s.Name, true
		}
	}
	for _, by := range mountedBy {
		if by == "" {
			return errors.New("a claim no service mounts")
		}
	}
	return nil
}
```

In `internal/agent/protocol/kubernetes_deploy.go`, replace

```go
	InstanceID    string `json:"instance_id"`
	SpecDigest    string `json:"spec_digest"`
}
```

with

```go
	InstanceID    string `json:"instance_id"`
	SpecDigest    string `json:"spec_digest"`
	// Claims are the PersistentVolumeClaims an apply ensures; a removal carries none.
	Claims []KubernetesClaim `json:"claims,omitempty"`
}
```

replace

```go
// validateKubernetes is Validate for a cluster frame: no Docker field, every image pulled by
// digest (the kubelet pulls, so no tag moves and no credential travels), and secret keys named
// among the environment's.
```

with

```go
// validateKubernetes is Validate for a cluster frame: no Docker field, every image pulled by
// digest (the kubelet pulls, so no tag moves and no credential travels), secret keys named
// among the environment's, and claims each mounted by one service.
```

replace the `return nil` that ends `validateKubernetes` (after the `invalid secret keys` loop)

```go
				return errors.New("invalid secret keys")
			}
		}
	}
	return nil
}
```

with

```go
				return errors.New("invalid secret keys")
			}
		}
	}
	return r.validClaims()
}
```

and replace

```go
	// kubeObject is a name_taken, conflict or admission_denied detail: the object's kind and name.
	kubeObject = regexp.MustCompile(`^(Deployment|Service|ConfigMap|Secret)/[a-z0-9]([-a-z0-9.]{0,241}[a-z0-9])?$`)
```

with

```go
	// kubeObject is a name_taken, conflict, admission_denied or claim_immutable detail: the
	// object's kind and name.
	kubeObject = regexp.MustCompile(`^(Deployment|Service|ConfigMap|Secret|PersistentVolumeClaim)/[a-z0-9]([-a-z0-9.]{0,241}[a-z0-9])?$`)
```

In `internal/agent/protocol/deployment.go`, replace

```go
	"pod_security": detailPodSecurity, "admission_denied": detailObject,
}
```

with

```go
	"pod_security": detailPodSecurity, "admission_denied": detailObject, "claim_immutable": detailObject,
}
```

replace

```go
	SecretKeys []string `json:"secret_keys,omitempty"`
}
```

with

```go
	SecretKeys []string `json:"secret_keys,omitempty"`
	// Volumes mount the request's claims; Kubernetes only.
	Volumes []KubernetesMount `json:"volumes,omitempty"`
}
```

in `Validate` replace

```go
!deploymentRestart[s.Restart] || len(s.SecretKeys) > 0 {
```

with

```go
!deploymentRestart[s.Restart] || len(s.SecretKeys) > 0 || len(s.Volumes) > 0 {
```

in `DeploymentResult.Validate` replace

```go
	for _, s := range r.Steps {
		quiet := s.Outcome == OutcomeSucceeded || s.Outcome == OutcomeSkipped
		if !deploymentService.MatchString(s.Service) || !deploymentSteps[s.Step] || !(resultOutcomes[s.Outcome] || s.Outcome == OutcomeSkipped) || len(s.Detail) > MaxDeploymentStepDetailBytes {
			return errors.New("invalid deployment step")
		}
		if (quiet && (s.Code != "" || s.Detail != "")) || (!quiet && !validStepCode(s.Code, s.Detail)) {
```

with

```go
	for _, s := range r.Steps {
		quiet := s.Outcome == OutcomeSucceeded || s.Outcome == OutcomeSkipped
		// A cluster removal reports each claim it kept as a skipped volume step, detail retained.
		retained := s.Outcome == OutcomeSkipped && s.Step == StepVolume && s.Code == "" && s.Detail == DetailRetained
		if !deploymentService.MatchString(s.Service) || !deploymentSteps[s.Step] || !(resultOutcomes[s.Outcome] || s.Outcome == OutcomeSkipped) || len(s.Detail) > MaxDeploymentStepDetailBytes {
			return errors.New("invalid deployment step")
		}
		if (quiet && !retained && (s.Code != "" || s.Detail != "")) || (!quiet && !validStepCode(s.Code, s.Detail)) {
```

and in `RemovalRequest.Validate` replace

```go
		if r.Kubernetes.Validate() != nil || len(r.Containers) > 0 || len(r.Services) == 0 || len(r.Services) > MaxRemovalTargets {
```

with

```go
		if r.Kubernetes.Validate() != nil || len(r.Kubernetes.Claims) > 0 || len(r.Containers) > 0 || len(r.Services) == 0 || len(r.Services) > MaxRemovalTargets {
```

In `internal/agent/protocol/inspection.go`, append `"k8s_volume_shared"` to `UnsupportedCodes`: replace

```go
"k8s_name", "k8s_namespace"}
```

with

```go
"k8s_name", "k8s_namespace", "k8s_volume_shared"}
```

In `internal/agent/protocol/kubernetes.go`, replace

```go
	Services   []Service  `json:"services"`
	Claims     []Claim    `json:"claims"`
}
```

with

```go
	Services   []Service  `json:"services"`
	Claims     []Claim    `json:"claims"`
	// StorageClasses are what a migration's volume choices pick from.
	StorageClasses []StorageClass `json:"storage_classes"`
}
```

in `decodeKubernetes` replace

```go
		decodeField(fields, "claims", MaxClaims, &k.Claims, &cut),
	)
```

with

```go
		decodeField(fields, "claims", MaxClaims, &k.Claims, &cut),
		decodeField(fields, "storage_classes", MaxStorageClasses, &k.StorageClasses, &cut),
	)
```

in `clampKubernetes` replace

```go
	if k.Nodes == nil {
		k.Nodes = []Node{}
	}
```

with

```go
	if len(k.StorageClasses) > MaxStorageClasses {
		k.StorageClasses, truncated["storage_classes"] = k.StorageClasses[:MaxStorageClasses], true
	}
	for i := range k.StorageClasses {
		k.StorageClasses[i].Name = name(k.StorageClasses[i].Name)
	}
	if k.StorageClasses == nil {
		k.StorageClasses = []StorageClass{}
	}
	if k.Nodes == nil {
		k.Nodes = []Node{}
	}
```

and in `shrinkKubernetes` replace

```go
		{"claims", len(k.Claims), func() { k.Claims = k.Claims[:len(k.Claims)*3/4] }},
```

with

```go
		{"claims", len(k.Claims), func() { k.Claims = k.Claims[:len(k.Claims)*3/4] }},
		{"storage_classes", len(k.StorageClasses), func() { k.StorageClasses = k.StorageClasses[:len(k.StorageClasses)*3/4] }},
```

Generate the fixture: `KY_UPDATE_FIXTURES=1 go test ./internal/agent/protocol -run TestWebVocabularyFixture`. It writes `web/src/protocol-codes.json`: `step_codes` (the 36 step codes, sorted) and `unsupported_codes` (the 35 codes in `UnsupportedCodes` order).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w internal/agent/protocol && go vet ./... && go test -race -count=1 ./internal/agent/protocol/`
Expected: PASS, the existing deployment, removal, inspection and inventory tests included. `go vet ./...` proves every caller still compiles.

- [ ] **Step 5: DOX and commit**

In `internal/agent/AGENTS.md`, `## Local Contracts`, append this bullet after the one beginning `- Cluster frames:`:

```markdown
- Claims: a cluster `KubernetesTarget.Claims` lists at most `MaxKubernetesClaims` (16) distinct `KubernetesClaim`s (name a DNS-1123 label, `storage_class` `""` for the cluster default or a DNS-1123 subdomain, `size` a whole number of `Mi`/`Gi`/`Ti` from 1Mi to 16Ti read by `StorageSizeBytes`, `access_mode` `ReadWriteOnce`), and each is mounted by exactly one service through `DeploymentService.Volumes` (`KubernetesMount`: claim, clean absolute `mount_path` distinct per service, `read_only`; at most `MaxKubernetesMounts`, 8, per service). A Docker service carries no claim mount and a removal target no claim. `claim_immutable` (detail empty or `PersistentVolumeClaim/<name>`) is a step code, and a skipped `volume` step with detail `retained` and no code is how a cluster removal reports a claim it kept. `UnsupportedCodes` ends with `k8s_volume_shared`. `KubernetesInventory.StorageClasses` (`{name, default}`, at most `MaxStorageClasses`, 100, cut named `storage_classes`) is never nil after `Clamp`. `TestWebVocabularyFixture` writes the step and unsupported codes to `web/src/protocol-codes.json` (`KY_UPDATE_FIXTURES=1`) and fails while it is stale.
```

```bash
git add internal/agent/protocol internal/agent/AGENTS.md internal/store/kubernetes_deployment_test.go web/src/protocol-codes.json && make tidy-check lint && git commit -m "feat(protocol): claims, claim mounts, StorageClasses and claim_immutable" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 2: Store — spec extension, migration 35 and the migration lifecycle

**Files:**
- Create: `internal/store/application_migration.go`
- Create: `internal/store/application_migration_test.go`
- Modify: `internal/store/application_spec.go` (`KubernetesExtension`, `KubernetesVolume`, `namedVolumes`, validation)
- Modify: `internal/store/applications.go` (`insertApplication` factored out of `createApplication`)
- Modify: `internal/store/migrations/migrations.go` (migration 35)
- Modify: `internal/store/store.go` (`TenancyStore` methods)
- Modify: `internal/store/application_apply.go` (`migration_id` on apply, `RemoveApplication` refuses an open migration's source)
- Modify: `internal/store/application_deployment.go` (`Deployment.MigrationID`, `selectDeployments`, `scanDeployment`)
- Modify: `internal/permissions/permissions.go` (`ApplicationMigrate`)
- Modify tests: `internal/store/application_spec_test.go`, `internal/store/tenancy_test.go` (the upgrade replay drops and replays the new table), `internal/permissions/permissions_test.go`
- Docs: `internal/store/AGENTS.md`, `internal/permissions/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.StorageSizeBytes`, `protocol.ValidStorageClass`, `protocol.AccessReadWriteOnce`, `protocol.MaxKubernetesClaims`, `protocol.StorageClass`, `KubernetesInventory.StorageClasses` (Task 1); `applicationMapping`, `freshInventory`, `resolveApplicationValues`, `sealApplicationValues`, `insertApplicationRevision`, `decodeNamespaces`, `KubernetesProject`, `t.run`, `readTenant`, `withTenantTarget` (existing); test helpers `tenantAtomicStore`, `activeEndpointWith`, `putAdoptionSnapshot`, `mappingRequest`, `activeCluster`, `clusterGeneration`, `kubePlanRequest`, `kubeIdentity`, `planRequest`, `fakeResolver`, `digestOf`, `twoServiceSpec`, `imageCheckKey` (existing, package `store`).
- Produces (package `store`):
  ```go
  type KubernetesExtension struct{ Volumes map[string]KubernetesVolume }        // json volumes
  type KubernetesVolume struct{ StorageClass, Size, AccessMode string }         // json storage_class, size, access_mode
  func (v KubernetesVolume) Valid() bool
  // ApplicationSpec gains Kubernetes *KubernetesExtension `json:"kubernetes,omitempty"`
  func (spec ApplicationSpec) namedVolumes() []string
  const MigrationAnalyzed, MigrationDestinationCreated, MigrationValidated, MigrationCutoverConfirmed, MigrationAbandoned = "analyzed", "destination_created", "validated", "cutover_confirmed", "abandoned"
  const MaxMigrationReportBytes, MaxMigrationNoteRunes = 65536, 500
  var ErrMigrationOpen, ErrMigrationNotReady, ErrMigrationState, ErrMigrationStale, ErrStorageClassUnknown, ErrSizeInvalid, ErrVolumeUnknown, ErrApplicationNameTaken error
  type ApplicationMigration struct{ ID, ApplicationID, ApplicationName, DestinationApplicationID, DestinationApplicationName, DestinationEndpointID, Namespace, Status string; SourceRevision int; Ready bool; Report json.RawMessage; Choices KubernetesExtension; CreatedBy string; CreatedAt, UpdatedAt time.Time; ValidatedBy string; ValidatedAt *time.Time; ValidatedNote, ConfirmedBy string; ConfirmedAt *time.Time; CutoverNote, Role string }
  type MigrationStart struct{ DestinationEndpointID, Namespace string } // json destination_endpoint_id, namespace
  type MigrationAnalysis struct{ Revision int; Report []byte; Ready bool }
  type MigrationSource struct{ ApplicationName string; Revision int; Spec ApplicationSpec; Project, EndpointID string; Containers map[string]protocol.Container; Volumes []protocol.Volume; Destination MigrationDestination }
  type MigrationDestination struct{ EndpointID string; Namespaces []string; StorageClasses []protocol.StorageClass; Name, Project string }
  // TenancyStore gains:
  ReadMigrationSource(ctx, access, applicationID, endpointID string) (*MigrationSource, error)
  CreateMigration(ctx, access, applicationID string, start MigrationStart, analysis MigrationAnalysis) (*ApplicationMigration, error)
  ReadMigration(ctx, access, applicationID string) (*ApplicationMigration, error)
  AnalyzeMigration(ctx, access, applicationID string, choices KubernetesExtension, analysis MigrationAnalysis) (*ApplicationMigration, error)
  CreateMigrationDestination(ctx, access, applicationID string, key []byte) (*ApplicationMigration, error)
  ConfirmMigration(ctx, access, applicationID, step, note string) (*ApplicationMigration, error) // step "validated" | "cutover"
  AbandonMigration(ctx, access, applicationID string) (*ApplicationMigration, error)
  // Deployment gains MigrationID string `json:"migration_id,omitempty"`.
  // RemoveApplication returns ErrMigrationOpen for the source of an open migration.
  const openMigration = `status NOT IN ('cutover_confirmed','abandoned')` // SQL predicate, used by Task 4 too
  ```
- Produces (package `permissions`): `ApplicationMigrate Action = "application.migrate"`, organization administrators only.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/application_migration_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func migrationSpec() ApplicationSpec {
	return ApplicationSpec{Kind: "compose.v1", Volumes: []DeclaredVolume{{Name: "data"}}, Services: []ApplicationService{
		{Name: "db", Image: "ghcr.io/org/db:1", Restart: "always", Volumes: []ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}}, Environment: map[string]ApplicationSecretRef{"PASSWORD": {SecretRef: "db.PASSWORD"}}},
		{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Ports: []ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}},
	}}
}

// migrationFixture imports spec as "shop" with values, adopts and maps one container per service
// on a Docker endpoint, and enrolls a cluster granting namespace shop whose inventory reports the
// StorageClasses standard (the default) and fast.
func migrationFixture(t *testing.T, spec ApplicationSpec, values map[string]string) (*SQLStore, TenantAccess, *Application, string) {
	t.Helper()
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	ts := st.Tenancy()
	app, err := ts.ImportApplication(ctx, a, "shop", spec, values, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Volumes: []protocol.Volume{{Name: "shop_data"}}}
	bindings := map[string]string{}
	for i, s := range spec.Services {
		c := protocol.Container{ID: strings.Repeat("a", 63) + string("a0123456789"[i]), Name: "shop-" + s.Name, ImageID: "sha256:" + strings.Repeat("b", 63) + string("b0123456789"[i]), CreatedAt: time.Now().UTC().Add(-time.Hour), ComposeProject: "shop", Mounts: []protocol.Mount{}, Networks: []string{"shop_default"}}
		snapshot.Containers = append(snapshot.Containers, c)
		snapshot.Images = append(snapshot.Images, protocol.Image{ID: c.ImageID, Tags: []string{s.Image}})
		bindings[s.Name] = c.ID
	}
	endpoint := activeEndpointWith(t, ts, a, snapshot.Containers, nil)
	putAdoptionSnapshot(t, st, endpoint, snapshot)
	p, err := ts.PreviewApplicationAdoption(ctx, a, app.ID, endpoint, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.AdoptApplication(ctx, a, app.ID, AdoptionRequest{EndpointID: endpoint, Project: "shop", Digest: p.Digest, Confirm: "shop"}); err != nil {
		t.Fatal(err)
	}
	m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := mappingRequest(m)
	r.Bindings = bindings
	if err := ts.SetApplicationMapping(ctx, a, app.ID, r); err != nil {
		t.Fatal(err)
	}
	cluster := activeCluster(t, ts, a, []string{"shop"}, nil)
	putStorageClasses(t, ts, cluster, []protocol.StorageClass{{Name: "standard", Default: true}, {Name: "fast"}})
	return st, a, app, cluster
}

// putStorageClasses reports a fresh cluster snapshot with classes and no workload.
func putStorageClasses(t *testing.T, ts TenancyStore, endpoint string, classes []protocol.StorageClass) {
	t.Helper()
	snap := protocol.Snapshot{Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.31.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: []protocol.Workload{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}, StorageClasses: classes}}
	raw, _ := json.Marshal(snap)
	generation := uint64(time.Now().Unix()) - 1000 + clusterGeneration.Add(1)
	if ok, err := ts.AcceptInventory(context.Background(), endpoint, generation, time.Now().UTC(), raw); err != nil || !ok {
		t.Fatalf("cluster inventory: %v %v", ok, err)
	}
}

func analysis(revision int, ready bool) MigrationAnalysis {
	return MigrationAnalysis{Revision: revision, Report: []byte(`{"version":1}`), Ready: ready}
}

var dataChoice = KubernetesExtension{Volumes: map[string]KubernetesVolume{"data": {StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}}}

// A migration starts only from a Docker source to a granted namespace of a cluster, once per
// source; the source reads its inputs; choices are held to its named volumes and the cluster's
// classes while analyzed; the destination copies the analyzed revision with the choices and its
// values, mapped to the namespace; two confirmations with notes close it, and every write is
// audited on the source and the destination.
func TestMigrationLifecycle(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "migration-secret-canary"})
	ctx := context.Background()
	ts := st.Tenancy()
	start := MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}

	src, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if src.Revision != 1 || src.Project != "shop" || len(src.Containers) != 2 || src.Containers["db"].Name != "shop-db" || src.Destination.Project != KubernetesProject(src.Destination.Name) || !strings.HasPrefix(src.Destination.Name, "shop on cluster-") || len(src.Destination.StorageClasses) != 2 || !slices.Equal(src.Destination.Namespaces, []string{"shop"}) || len(src.Volumes) != 1 {
		t.Fatalf("source %+v", src)
	}
	if _, err := ts.ReadMigrationSource(ctx, a, app.ID, src.EndpointID); !errors.Is(err, ErrRuntimeUnsupported) {
		t.Fatalf("a Docker destination: %v", err)
	}
	for name, tc := range map[string]struct {
		start MigrationStart
		an    MigrationAnalysis
		want  error
	}{
		"ungranted namespace": {MigrationStart{DestinationEndpointID: cluster, Namespace: "billing"}, analysis(1, false), ErrNamespaceUnknown},
		"docker destination":  {MigrationStart{DestinationEndpointID: src.EndpointID, Namespace: "shop"}, analysis(1, false), ErrRuntimeUnsupported},
		"stale analysis":      {start, analysis(2, false), ErrMigrationStale},
		"empty report":        {start, MigrationAnalysis{Revision: 1, Report: nil}, ErrInvalid},
		"oversized report":    {start, MigrationAnalysis{Revision: 1, Report: []byte(`"` + strings.Repeat("x", MaxMigrationReportBytes) + `"`)}, ErrInvalid},
	} {
		if _, err := ts.CreateMigration(ctx, a, app.ID, tc.start, tc.an); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	m, err := ts.CreateMigration(ctx, a, app.ID, start, analysis(1, false))
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != MigrationAnalyzed || m.Role != "source" || m.ApplicationName != "shop" || m.Ready || m.SourceRevision != 1 || len(m.Choices.Volumes) != 0 || string(m.Report) != `{"version":1}` {
		t.Fatalf("created %+v", m)
	}
	if _, err := ts.CreateMigration(ctx, a, app.ID, start, analysis(1, false)); !errors.Is(err, ErrMigrationOpen) {
		t.Fatalf("a second open migration: %v", err)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrMigrationNotReady) {
		t.Fatalf("destination before ready: %v", err)
	}
	for name, tc := range map[string]struct {
		v    KubernetesVolume
		vol  string
		want error
	}{
		"unknown volume":   {dataChoice.Volumes["data"], "logs", ErrVolumeUnknown},
		"bad size":         {KubernetesVolume{StorageClass: "fast", Size: "10G", AccessMode: protocol.AccessReadWriteOnce}, "data", ErrSizeInvalid},
		"unreported class": {KubernetesVolume{StorageClass: "gold", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}, "data", ErrStorageClassUnknown},
		"read write many":  {KubernetesVolume{StorageClass: "fast", Size: "10Gi", AccessMode: "ReadWriteMany"}, "data", ErrInvalid},
		"class grammar":    {KubernetesVolume{StorageClass: "Fast!", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}, "data", ErrStorageClassUnknown},
	} {
		if _, err := ts.AnalyzeMigration(ctx, a, app.ID, KubernetesExtension{Volumes: map[string]KubernetesVolume{tc.vol: tc.v}}, analysis(1, true)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	defaulted := KubernetesExtension{Volumes: map[string]KubernetesVolume{"data": {Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}}}
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, defaulted, analysis(1, true)); err != nil {
		t.Fatalf("the default class while one is reported: %v", err)
	}
	putStorageClasses(t, ts, cluster, []protocol.StorageClass{{Name: "fast"}})
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, defaulted, analysis(1, true)); !errors.Is(err, ErrStorageClassUnknown) {
		t.Fatalf("the default class with none reported: %v", err)
	}
	m, err = ts.AnalyzeMigration(ctx, a, app.ID, dataChoice, MigrationAnalysis{Revision: 1, Report: []byte(`{"version":1,"ready":true}`), Ready: true})
	if err != nil || !m.Ready || m.Choices.Volumes["data"] != dataChoice.Volumes["data"] || string(m.Report) != `{"version":1,"ready":true}` {
		t.Fatalf("analyzed %+v %v", m, err)
	}

	m, err = ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != MigrationDestinationCreated || m.DestinationApplicationID == "" || m.DestinationApplicationName != src.Destination.Name {
		t.Fatalf("destination %+v", m)
	}
	rev, err := ts.ReadApplicationRevision(ctx, a, m.DestinationApplicationID, 1)
	if err != nil || rev.Spec.Kubernetes == nil || rev.Spec.Kubernetes.Volumes["data"] != dataChoice.Volumes["data"] || len(rev.Spec.Services) != 2 || rev.Spec.Services[0].Environment["PASSWORD"].SecretRef != "db.PASSWORD" {
		t.Fatalf("destination revision %+v %v", rev, err)
	}
	if source, _ := ts.ReadApplicationRevision(ctx, a, app.ID, 1); source.Spec.Kubernetes != nil {
		t.Fatal("the source definition changed")
	}
	values, err := ts.ResolveApplicationSecrets(ctx, a, m.DestinationApplicationID, 1, imageCheckKey)
	if err != nil || values["db.PASSWORD"] != "migration-secret-canary" {
		t.Fatalf("copied values %v %v", values, err)
	}
	mapped, err := ts.ReadApplicationMapping(ctx, a, m.DestinationApplicationID)
	if err != nil || mapped.Namespace != "shop" || mapped.Runtime != protocol.RuntimeKubernetes || mapped.Preview.Project != src.Destination.Project || mapped.Version != 1 || mapped.MappedRevision != 1 {
		t.Fatalf("destination mapping %+v %v", mapped, err)
	}
	if dest, err := ts.ReadMigration(ctx, a, m.DestinationApplicationID); err != nil || dest.ID != m.ID || dest.Role != "destination" || dest.ApplicationID != app.ID {
		t.Fatalf("read as the destination %+v %v", dest, err)
	}
	if _, err := ts.AnalyzeMigration(ctx, a, app.ID, dataChoice, analysis(1, true)); !errors.Is(err, ErrMigrationState) {
		t.Fatalf("choices after the destination: %v", err)
	}
	if _, err := ts.ConfirmMigration(ctx, a, app.ID, "cutover", "switched"); !errors.Is(err, ErrMigrationState) {
		t.Fatalf("cutover before validation: %v", err)
	}
	for _, note := range []string{"", "   ", "two\nlines", strings.Repeat("n", MaxMigrationNoteRunes+1), "bidi ‮"} {
		if _, err := ts.ConfirmMigration(ctx, a, app.ID, "validated", note); !errors.Is(err, ErrInvalid) {
			t.Errorf("note %q: %v", note, err)
		}
	}
	if _, err := ts.ConfirmMigration(ctx, a, app.ID, "abandoned", "no"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown step: %v", err)
	}
	if m, err = ts.ConfirmMigration(ctx, a, app.ID, "validated", "Checked the orders page on the cluster: ✓"); err != nil || m.Status != MigrationValidated || m.ValidatedBy != "actor" || m.ValidatedAt == nil {
		t.Fatalf("validated %+v %v", m, err)
	}
	if m, err = ts.ConfirmMigration(ctx, a, app.ID, "cutover", "DNS moved to the ingress"); err != nil || m.Status != MigrationCutoverConfirmed || m.ConfirmedBy != "actor" || m.CutoverNote != "DNS moved to the ingress" {
		t.Fatalf("cutover %+v %v", m, err)
	}
	if _, err := ts.ReadMigration(ctx, a, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a closed migration read as the source: %v", err)
	}
	records, _, err := st.Audit().ListAuditRecords(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{app.ID + "/migration/" + m.ID, m.DestinationApplicationID + "/migration/" + m.ID} {
		if !slices.ContainsFunc(records, func(r *AuditRecord) bool {
			return r.Action == "application.migrate" && r.Resource == resource && r.Result == "success"
		}) {
			t.Errorf("no application.migrate row on %s", resource)
		}
	}
	for _, r := range records {
		if strings.Contains(r.Details, "canary") || strings.Contains(r.Resource, "canary") {
			t.Fatalf("a secret reached the audit trail: %+v", r)
		}
	}
}

// Only an organization administrator migrates; a reader may read the migration.
func TestMigrationAuthorization(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, false)); err != nil {
		t.Fatal(err)
	}
	for _, role := range []TenantRole{RoleEnvironmentAdmin, RoleDeveloper} {
		if err := st.Tenancy().SetMembership(ctx, &OrganizationMembership{OrganizationID: a.OrganizationID, UserID: "actor", Role: role, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if _, err := ts.AbandonMigration(ctx, a, app.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s abandoned: %v", role, err)
		}
		if _, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s read the source: %v", role, err)
		}
		if m, err := ts.ReadMigration(ctx, a, app.ID); err != nil || m.Status != MigrationAnalyzed {
			t.Errorf("%s read: %+v %v", role, m, err)
		}
	}
}

// The destination refuses a name already taken and a source whose definition moved on since the
// analysis; nothing is created either way.
func TestMigrationDestination(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
	src, err := ts.ReadMigrationSource(ctx, a, app.ID, cluster)
	if err != nil {
		t.Fatal(err)
	}
	squatter, err := ts.ImportApplication(ctx, a, src.Destination.Name, twoServiceSpec(), map[string]string{"web.TOKEN": "x"}, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrApplicationNameTaken) {
		t.Fatalf("a taken name: %v", err)
	}
	if err := ts.DiscardApplication(ctx, a, squatter.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReplaceApplicationRevision(ctx, a, app.ID, 1, migrationSpec(), map[string]string{"db.PASSWORD": "changed"}, imageCheckKey); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey); !errors.Is(err, ErrMigrationStale) {
		t.Fatalf("a stale analysis: %v", err)
	}
	apps, err := ts.ListApplications(ctx, a, 0, 100)
	if err != nil || len(apps) != 1 {
		t.Fatalf("applications %+v %v", apps, err)
	}
}

// The source of an open migration cannot be removed; abandoning keeps a created destination and
// lets the removal through.
func TestMigrationKeepsTheSource(t *testing.T) {
	st, a, app, cluster := migrationFixture(t, migrationSpec(), map[string]string{"db.PASSWORD": "p"})
	ctx := context.Background()
	ts := st.Tenancy()
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
	m, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := ts.ListApplicationInstances(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	source := instances[slices.IndexFunc(instances, func(i ApplicationInstance) bool { return i.ApplicationID == app.ID })]
	if _, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: source.ID, Confirm: "shop"}); !errors.Is(err, ErrMigrationOpen) {
		t.Fatalf("removing a migrating source: %v", err)
	}
	abandoned, err := ts.AbandonMigration(ctx, a, app.ID)
	if err != nil || abandoned.Status != MigrationAbandoned || abandoned.DestinationApplicationID != m.DestinationApplicationID {
		t.Fatalf("abandoned %+v %v", abandoned, err)
	}
	if _, err := ts.ReadApplicationMapping(ctx, a, m.DestinationApplicationID); err != nil {
		t.Fatalf("the destination went with the migration: %v", err)
	}
	if _, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: source.ID, Confirm: "shop"}); err != nil {
		t.Fatalf("removing the source after abandoning: %v", err)
	}
	if _, err := ts.AbandonMigration(ctx, a, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abandoning twice: %v", err)
	}
}

// Every apply of an open migration's destination records the migration; the source's applies
// never do, and a closed migration stops recording.
func TestMigrationIDOnDestinationApplies(t *testing.T) {
	spec := ApplicationSpec{Kind: "compose.v1", Services: []ApplicationService{{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Environment: map[string]ApplicationSecretRef{"TOKEN": {SecretRef: "web.TOKEN"}}}}}
	st, a, app, cluster := migrationFixture(t, spec, map[string]string{"web.TOKEN": "t"})
	ctx := context.Background()
	ts := st.Tenancy()
	if err := ts.SetAnonymousPull(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.CreateMigration(ctx, a, app.ID, MigrationStart{DestinationEndpointID: cluster, Namespace: "shop"}, analysis(1, true)); err != nil {
		t.Fatal(err)
	}
	m, err := ts.CreateMigrationDestination(ctx, a, app.ID, imageCheckKey)
	if err != nil {
		t.Fatal(err)
	}
	dest := m.DestinationApplicationID
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/web:1": {digest: digestOf("b")}}}
	apply := func() *Deployment {
		t.Helper()
		mapped, err := ts.ReadApplicationMapping(ctx, a, dest)
		if err != nil {
			t.Fatal(err)
		}
		d, err := ts.PlanDeployment(ctx, a, dest, kubePlanRequest(mapped), resolver, imageCheckKey, false)
		if err != nil {
			t.Fatal(err)
		}
		applied, _, err := ts.ApplyDeployment(ctx, a, dest, d.ID, mapped.Preview.Project, imageCheckKey, protocol.MaxDeploymentRequestBytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.SettleDeployment(ctx, cluster, protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{kubeIdentity(d.Plan.Services[0])}}); err != nil {
			t.Fatal(err)
		}
		return applied
	}
	first := apply()
	if first.MigrationID != m.ID {
		t.Fatalf("apply of the destination: %q", first.MigrationID)
	}
	if read, err := ts.ReadDeployment(ctx, a, dest, first.ID); err != nil || read.MigrationID != m.ID {
		t.Fatalf("read back %+v %v", read, err)
	}
	if list, err := ts.ListDeployments(ctx, a, dest); err != nil || list[0].MigrationID != m.ID {
		t.Fatalf("listed %+v %v", list, err)
	}
	source, err := ts.ReadApplicationMapping(ctx, a, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ts.PlanDeployment(ctx, a, app.ID, planRequest(source), nil, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if applied, _, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop", imageCheckKey, protocol.MaxDeploymentRequestBytes); err != nil || applied.MigrationID != "" {
		t.Fatalf("apply of the source %+v %v", applied, err)
	}
	if _, err := ts.AbandonMigration(ctx, a, app.ID); err != nil {
		t.Fatal(err)
	}
	if later := apply(); later.MigrationID != "" {
		t.Fatalf("an apply after the migration closed: %q", later.MigrationID)
	}
}
```

Append to `internal/store/application_spec_test.go`:

```go
// A kubernetes extension names declared volumes only, each with a valid class, size and access
// mode; an empty one is refused, and the digest covers it.
func TestApplicationSpecKubernetesExtension(t *testing.T) {
	good := func() store.ApplicationSpec {
		spec := withVolumes()
		spec.Kubernetes = &store.KubernetesExtension{Volumes: map[string]store.KubernetesVolume{
			"db":     {StorageClass: "fast.ssd", Size: "20Gi", AccessMode: "ReadWriteOnce"},
			"shared": {Size: "1Ti", AccessMode: "ReadWriteOnce"},
		}}
		return spec
	}
	if err := store.ValidateApplicationSpec(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*store.ApplicationSpec){
		"empty":      func(s *store.ApplicationSpec) { s.Kubernetes.Volumes = nil },
		"undeclared": func(s *store.ApplicationSpec) { s.Kubernetes.Volumes["cache"] = s.Kubernetes.Volumes["db"] },
		"bad size": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{StorageClass: "fast", Size: "20GB", AccessMode: "ReadWriteOnce"}
		},
		"too large": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{Size: "17Ti", AccessMode: "ReadWriteOnce"}
		},
		"bad class": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{StorageClass: "Fast", Size: "1Gi", AccessMode: "ReadWriteOnce"}
		},
		"read write many": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{Size: "1Gi", AccessMode: "ReadWriteMany"}
		},
		"17 volumes": func(s *store.ApplicationSpec) {
			for i := range 15 {
				name := fmt.Sprint("v", i)
				s.Volumes = append(s.Volumes, store.DeclaredVolume{Name: name})
				s.Kubernetes.Volumes[name] = store.KubernetesVolume{Size: "1Gi", AccessMode: "ReadWriteOnce"}
			}
		},
	} {
		spec := good()
		mutate(&spec)
		if err := store.ValidateApplicationSpec(spec); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	plain, extended := withVolumes(), good()
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(extended)
	if strings.Contains(string(a), "kubernetes") || !strings.Contains(string(b), `"kubernetes":{"volumes":{"db":{"storage_class":"fast.ssd","size":"20Gi","access_mode":"ReadWriteOnce"}`) {
		t.Fatalf("encoded %s / %s", a, b)
	}
}
```

Append to `internal/permissions/permissions_test.go`:

```go
// A migration copies the source's secret values into the destination it creates, so only the
// organization administrator, who may reveal them, migrates (docs/authorization-matrix.md).
func TestMigrateIsOrganizationAdminOnly(t *testing.T) {
	for _, role := range []string{"organization_admin", "environment_admin", "operator", "developer", "read_only", "admin", "unknown", ""} {
		if Allows(role, ApplicationMigrate) != (role == "organization_admin") || PlatformAllows(role, ApplicationMigrate) {
			t.Errorf("%q application.migrate", role)
		}
	}
	if ApplicationMigrate != "application.migrate" {
		t.Fatal("the audit identifier changed")
	}
}
```

In `internal/store/tenancy_test.go`, `TestTenancyUpgradeAndReopen`, the replay drops every table that references `applications` and replays its migration: replace

```go
"DROP TABLE deployments", "DROP TABLE policy_runs",
```

with

```go
"DROP TABLE deployments", "DROP TABLE application_migrations", "DROP TABLE policy_runs",
```

and replace

```go
31,32,33,34)"}
```

with

```go
31,32,33,34,35)"}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ ./internal/permissions/`
Expected: FAIL to compile: `undefined: KubernetesExtension`, `undefined: MigrationAnalysis`, `undefined: ErrMigrationOpen`, `ts.ReadMigrationSource undefined`, `undefined: ApplicationMigrate`.

- [ ] **Step 3: Implement the spec extension**

In `internal/store/application_spec.go`, add `"slices"` to the imports (between `"regexp"` and `"strings"`), then replace

```go
type ApplicationSpec struct {
	Kind     string               `json:"kind"`
	Services []ApplicationService `json:"services"`
	Volumes  []DeclaredVolume     `json:"volumes,omitempty"`
}
```

with

```go
type ApplicationSpec struct {
	Kind     string               `json:"kind"`
	Services []ApplicationService `json:"services"`
	Volumes  []DeclaredVolume     `json:"volumes,omitempty"`
	// Kubernetes holds what only a cluster needs: a migration's destination revision carries
	// its storage choices. The Compose importer never sets it.
	Kubernetes *KubernetesExtension `json:"kubernetes,omitempty"`
}

// KubernetesExtension carries a StorageClass and size per named volume, keyed by its declared
// name. A chosen volume becomes a PersistentVolumeClaim on a cluster.
type KubernetesExtension struct {
	Volumes map[string]KubernetesVolume `json:"volumes"`
}

// KubernetesVolume is one volume's claim: StorageClass "" is the cluster default, Size a whole
// number of Mi, Gi or Ti from 1Mi to 16Ti, AccessMode ReadWriteOnce.
type KubernetesVolume struct {
	StorageClass string `json:"storage_class"`
	Size         string `json:"size"`
	AccessMode   string `json:"access_mode"`
}

// Valid checks one choice's grammar; the destination inventory decides whether its class exists.
func (v KubernetesVolume) Valid() bool {
	_, size := protocol.StorageSizeBytes(v.Size)
	return protocol.ValidStorageClass(v.StorageClass) && size && v.AccessMode == protocol.AccessReadWriteOnce
}

```

replace

```go
// serviceNames lists the spec's services in order.
```

with

```go
// namedVolumes lists the declared volumes some service mounts by name, in declared order: the
// volumes a migration asks a storage choice for.
func (spec ApplicationSpec) namedVolumes() []string {
	out := []string{}
	for _, v := range spec.Volumes {
		if slices.ContainsFunc(spec.Services, func(s ApplicationService) bool {
			return slices.ContainsFunc(s.Volumes, func(m ApplicationVolume) bool { return m.Kind == "named" && m.Source == v.Name })
		}) {
			out = append(out, v.Name)
		}
	}
	return out
}

// serviceNames lists the spec's services in order.
```

and in `encodeApplicationSpec` replace

```go
		for name, ref := range service.Environment {
			if !applicationEnvName.MatchString(name) || !applicationSecretName.MatchString(ref.SecretRef) {
				return nil, "", ErrInvalid
			}
		}
	}
	raw, err := json.Marshal(spec)
```

with

```go
		for name, ref := range service.Environment {
			if !applicationEnvName.MatchString(name) || !applicationSecretName.MatchString(ref.SecretRef) {
				return nil, "", ErrInvalid
			}
		}
	}
	if k := spec.Kubernetes; k != nil {
		if len(k.Volumes) == 0 || len(k.Volumes) > protocol.MaxKubernetesClaims {
			return nil, "", ErrInvalid
		}
		for name, v := range k.Volumes {
			if !declared[name] || !v.Valid() {
				return nil, "", ErrInvalid
			}
		}
	}
	raw, err := json.Marshal(spec)
```

- [ ] **Step 4: Implement migration 35 and the permission**

In `internal/store/migrations/migrations.go`, replace

```go
	{Version: 34, Name: "kubernetes_namespaces", SQLite: kubernetesNamespaces, Postgres: kubernetesNamespaces},
}
```

with

```go
	{Version: 34, Name: "kubernetes_namespaces", SQLite: kubernetesNamespaces, Postgres: kubernetesNamespaces},
	{Version: 35, Name: "application_migrations", SQLite: applicationMigrations, Postgres: strings.ReplaceAll(applicationMigrations, "DATETIME", "TIMESTAMPTZ")},
}
```

and insert before `// Latest returns the highest registered migration version`:

```go
// applicationMigrations records a migration from a Docker source to a destination application on
// a cluster: at most one open per source, the analyzed revision and report, the storage choices,
// and who confirmed validation and cutover. The destination is referenced by ID alone, so its
// deletion clears the link and leaves the tenant columns; every deployment of an open
// migration's destination names it.
const applicationMigrations = `CREATE TABLE application_migrations (
 id TEXT PRIMARY KEY,
 organization_id TEXT NOT NULL,
 environment_id TEXT NOT NULL,
 application_id TEXT NOT NULL,
 destination_application_id TEXT REFERENCES applications(id) ON DELETE SET NULL,
 destination_endpoint_id TEXT NOT NULL,
 namespace TEXT NOT NULL CHECK(length(namespace) BETWEEN 1 AND 63),
 status TEXT NOT NULL CHECK(status IN ('analyzed','destination_created','validated','cutover_confirmed','abandoned')),
 source_revision INTEGER NOT NULL CHECK(source_revision BETWEEN 1 AND 100),
 ready INTEGER NOT NULL CHECK(ready IN (0,1)),
 report TEXT NOT NULL CHECK(length(report) BETWEEN 2 AND 65536),
 choices TEXT NOT NULL DEFAULT '{}' CHECK(length(choices)<=16384),
 created_by TEXT NOT NULL CHECK(length(created_by) BETWEEN 1 AND 255),
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL,
 validated_by TEXT NOT NULL DEFAULT '' CHECK(length(validated_by)<=255),
 validated_at DATETIME,
 validated_note TEXT NOT NULL DEFAULT '' CHECK(length(validated_note)<=500),
 confirmed_by TEXT NOT NULL DEFAULT '' CHECK(length(confirmed_by)<=255),
 confirmed_at DATETIME,
 cutover_note TEXT NOT NULL DEFAULT '' CHECK(length(cutover_note)<=500),
 FOREIGN KEY(organization_id,environment_id,application_id) REFERENCES applications(organization_id,environment_id,id) ON DELETE CASCADE,
 FOREIGN KEY(organization_id,environment_id,destination_endpoint_id) REFERENCES endpoints(organization_id,environment_id,id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX application_migrations_open ON application_migrations(application_id) WHERE status NOT IN ('cutover_confirmed','abandoned');
CREATE INDEX idx_application_migrations_destination ON application_migrations(destination_application_id);
ALTER TABLE deployments ADD COLUMN migration_id TEXT REFERENCES application_migrations(id) ON DELETE SET NULL;
`

```

In `internal/permissions/permissions.go`, replace

```go
	ApplicationPolicy Action = "application.policy"
)
```

with

```go
	ApplicationPolicy Action = "application.policy"
	// ApplicationMigrate analyzes an application for a cluster, records its storage choices and
	// creates the destination application, copying the source's secret values into it. Only the
	// organization administrator, who may reveal those values anyway, holds it.
	ApplicationMigrate Action = "application.migrate"
)
```

and in the `organization_admin` case replace `ApplicationDeploy, ApplicationPolicy, ContainerExec,` with `ApplicationDeploy, ApplicationPolicy, ApplicationMigrate, ContainerExec,`.

- [ ] **Step 5: Implement the lifecycle**

In `internal/store/applications.go`, replace the body of `createApplication` from `err := t.withTenantTarget(ctx, a, permissions.ApplicationImport, app.ID, func(tx *sql.Tx) error {` to the end of the function with:

```go
	err := t.withTenantTarget(ctx, a, permissions.ApplicationImport, app.ID, func(tx *sql.Tx) error {
		return t.insertApplication(ctx, tx, a, app, spec, values, key)
	})
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// insertApplication writes app with revision 1 = spec and, when values is non-nil, its sealed
// values, inside the caller's authorized transaction, under the organization's quota.
func (t *tenancyStore) insertApplication(ctx context.Context, tx *sql.Tx, a TenantAccess, app Application, spec ApplicationSpec, values map[string]string, key []byte) error {
	if a.EnvironmentID == "" || !validTenantName(app.Name) {
		return ErrInvalid
	}
	raw, digest, err := encodeApplicationSpec(spec)
	if err != nil {
		return err
	}
	// Serialize the organization-wide quota across distinct administrators.
	if _, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE organizations SET name=name WHERE id=?`), a.OrganizationID); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM applications WHERE organization_id=?`), a.OrganizationID).Scan(&count); err != nil {
		return err
	}
	if count >= MaxApplicationsPerOrganization {
		return ErrApplicationLimit
	}
	_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO applications(id,organization_id,environment_id,name,latest_revision,created_by,created_at) VALUES(?,?,?,?,?,?,?)`), app.ID, app.OrganizationID, app.EnvironmentID, app.Name, app.LatestRevision, app.CreatedBy, app.CreatedAt)
	if err != nil {
		return err
	}
	if err := t.insertApplicationRevision(ctx, tx, a, app.ID, 1, raw, digest, app.CreatedAt); err != nil {
		return err
	}
	if values != nil {
		return t.sealApplicationValues(ctx, tx, a, app.ID, 1, spec, digest, values, key)
	}
	return nil
}
```

Create `internal/store/application_migration.go`:

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/google/uuid"
)

// Migration statuses. A migration is open until cutover_confirmed or abandoned, and a source
// has at most one open migration. See docs/application-schema.md, Migration.
const (
	MigrationAnalyzed           = "analyzed"
	MigrationDestinationCreated = "destination_created"
	MigrationValidated          = "validated"
	MigrationCutoverConfirmed   = "cutover_confirmed"
	MigrationAbandoned          = "abandoned"
	MaxMigrationReportBytes     = 65536
	MaxMigrationNoteRunes       = 500
)

var (
	ErrMigrationOpen        = errors.New("the application has an open migration")
	ErrMigrationNotReady    = errors.New("the migration report is not ready")
	ErrMigrationState       = errors.New("the migration's status does not allow this")
	ErrMigrationStale       = errors.New("the source definition changed since the analysis")
	ErrStorageClassUnknown  = errors.New("the destination reports no such StorageClass")
	ErrSizeInvalid          = errors.New("invalid claim size")
	ErrVolumeUnknown        = errors.New("the source mounts no such named volume")
	ErrApplicationNameTaken = errors.New("the destination's name is taken")
)

// ApplicationMigration is one migration of a Docker source to a destination application on a
// cluster. Report is the analyzer's JSON, stored as produced. Role says which end the
// application it was read for is.
type ApplicationMigration struct {
	ID                         string              `json:"id"`
	ApplicationID              string              `json:"application_id"`
	ApplicationName            string              `json:"application_name"`
	DestinationApplicationID   string              `json:"destination_application_id,omitempty"`
	DestinationApplicationName string              `json:"destination_application_name,omitempty"`
	DestinationEndpointID      string              `json:"destination_endpoint_id"`
	Namespace                  string              `json:"namespace"`
	Status                     string              `json:"status"`
	SourceRevision             int                 `json:"source_revision"`
	Ready                      bool                `json:"ready"`
	Report                     json.RawMessage     `json:"report"`
	Choices                    KubernetesExtension `json:"choices"`
	CreatedBy                  string              `json:"created_by"`
	CreatedAt                  time.Time           `json:"created_at"`
	UpdatedAt                  time.Time           `json:"updated_at"`
	ValidatedBy                string              `json:"validated_by,omitempty"`
	ValidatedAt                *time.Time          `json:"validated_at,omitempty"`
	ValidatedNote              string              `json:"validated_note,omitempty"`
	ConfirmedBy                string              `json:"confirmed_by,omitempty"`
	ConfirmedAt                *time.Time          `json:"confirmed_at,omitempty"`
	CutoverNote                string              `json:"cutover_note,omitempty"`
	Role                       string              `json:"role"`
}

// MigrationStart names the cluster and namespace a migration targets.
type MigrationStart struct {
	DestinationEndpointID string `json:"destination_endpoint_id"`
	Namespace             string `json:"namespace"`
}

// MigrationAnalysis is what the analyzer produced for revision Revision of the source.
type MigrationAnalysis struct {
	Revision int
	Report   []byte
	Ready    bool
}

// MigrationSource is everything the analyzer reads: the source's latest revision, its project
// and mapped containers by service from the endpoint's fresh inventory, the endpoint's volumes,
// and the destination.
type MigrationSource struct {
	ApplicationName string
	Revision        int
	Spec            ApplicationSpec
	Project         string
	EndpointID      string
	Containers      map[string]protocol.Container
	Volumes         []protocol.Volume
	Destination     MigrationDestination
}

// MigrationDestination is the cluster side: its namespaces and StorageClasses, and the name and
// project the destination application takes.
type MigrationDestination struct {
	EndpointID     string
	Namespaces     []string
	StorageClasses []protocol.StorageClass
	Name           string
	Project        string
}

const openMigration = `status NOT IN ('cutover_confirmed','abandoned')`

const selectMigration = `SELECT m.id,m.application_id,s.name,COALESCE(m.destination_application_id,''),COALESCE(d.name,''),m.destination_endpoint_id,m.namespace,m.status,m.source_revision,m.ready,m.report,m.choices,m.created_by,m.created_at,m.updated_at,m.validated_by,m.validated_at,m.validated_note,m.confirmed_by,m.confirmed_at,m.cutover_note FROM application_migrations m JOIN applications s ON s.id=m.application_id LEFT JOIN applications d ON d.id=m.destination_application_id `

func scanMigration(row interface{ Scan(...any) error }) (*ApplicationMigration, error) {
	var m ApplicationMigration
	var report, choices string
	var ready int
	var validated, confirmed sql.NullTime
	if err := row.Scan(&m.ID, &m.ApplicationID, &m.ApplicationName, &m.DestinationApplicationID, &m.DestinationApplicationName, &m.DestinationEndpointID, &m.Namespace, &m.Status, &m.SourceRevision, &ready, &report, &choices, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.ValidatedBy, &validated, &m.ValidatedNote, &m.ConfirmedBy, &confirmed, &m.CutoverNote); err != nil {
		return nil, err
	}
	if !json.Valid([]byte(report)) || json.Unmarshal([]byte(choices), &m.Choices) != nil {
		return nil, ErrRevisionCorrupt
	}
	if m.Choices.Volumes == nil {
		m.Choices.Volumes = map[string]KubernetesVolume{}
	}
	m.Ready, m.Report = ready == 1, json.RawMessage(report)
	if validated.Valid {
		m.ValidatedAt = &validated.Time
	}
	if confirmed.Valid {
		m.ConfirmedAt = &confirmed.Time
	}
	return &m, nil
}

// migrationApp parses an application ID and refuses an access without an environment.
func migrationApp(a TenantAccess, app string) (string, error) {
	id, err := uuid.Parse(app)
	if err != nil || a.EnvironmentID == "" {
		return "", ErrInvalid
	}
	return id.String(), nil
}

// lockSource takes the source application's row lock, the order every deployment writer takes,
// and returns its latest revision.
func (t *tenancyStore) lockSource(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) (int, error) {
	lock := ""
	if t.store.driver == "postgres" {
		lock = " FOR UPDATE"
	}
	var head int
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT latest_revision FROM applications WHERE organization_id=? AND environment_id=? AND id=?`+lock), a.OrganizationID, a.EnvironmentID, app).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return head, err
}

// dockerSource refuses a source that is not adopted (ErrMappingRequired) or not on Docker.
func (t *tenancyStore) dockerSource(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) error {
	var namespace, runtime string
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT i.namespace,e.runtime FROM application_instances i JOIN endpoints e ON e.id=i.endpoint_id WHERE i.organization_id=? AND i.environment_id=? AND i.application_id=?`), a.OrganizationID, a.EnvironmentID, app).Scan(&namespace, &runtime)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrMappingRequired
	case err != nil:
		return err
	case namespace != "" || runtime != protocol.RuntimeDocker:
		return ErrRuntimeUnsupported
	}
	return nil
}

// migrationDestination reads a Kubernetes endpoint's namespaces and reported StorageClasses and
// the name the destination of app would take on it.
func (t *tenancyStore) migrationDestination(ctx context.Context, tx *sql.Tx, a TenantAccess, sourceName, endpoint string) (MigrationDestination, error) {
	d := MigrationDestination{EndpointID: endpoint, StorageClasses: []protocol.StorageClass{}}
	var runtime, name, namespaces string
	var snapshot sql.NullString
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.runtime,e.name,e.deploy_namespaces,v.snapshot FROM endpoints e LEFT JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE e.organization_id=? AND e.environment_id=? AND e.id=?`), a.OrganizationID, a.EnvironmentID, endpoint).Scan(&runtime, &name, &namespaces, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if runtime != protocol.RuntimeKubernetes {
		return d, ErrRuntimeUnsupported
	}
	d.Namespaces = decodeNamespaces(namespaces)
	var s protocol.Snapshot
	if snapshot.Valid && json.Unmarshal([]byte(snapshot.String), &s) == nil && s.Kubernetes != nil && s.Kubernetes.StorageClasses != nil {
		d.StorageClasses = s.Kubernetes.StorageClasses
	}
	d.Name = sourceName + " on " + name
	d.Project = KubernetesProject(d.Name)
	return d, nil
}

// revisionSpec reads and verifies one revision's spec.
func (t *tenancyStore) revisionSpec(ctx context.Context, tx *sql.Tx, a TenantAccess, app string, number int) (ApplicationSpec, error) {
	var raw, digest string
	var spec ApplicationSpec
	err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT spec,digest FROM application_revisions WHERE organization_id=? AND environment_id=? AND application_id=? AND number=?`), a.OrganizationID, a.EnvironmentID, app, number).Scan(&raw, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return spec, ErrNotFound
	}
	if err != nil {
		return spec, err
	}
	if applicationSpecDigest([]byte(raw)) != digest || json.Unmarshal([]byte(raw), &spec) != nil || ValidateApplicationSpec(spec) != nil {
		return spec, ErrRevisionCorrupt
	}
	return spec, nil
}

// ReadMigrationSource gathers the analyzer's input for app against the cluster endpoint, under
// application.migrate: the source must be adopted on a Docker endpoint whose inventory is fresh.
func (t *tenancyStore) ReadMigrationSource(ctx context.Context, a TenantAccess, app, endpoint string) (*MigrationSource, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	var out *MigrationSource
	err = t.readTenant(ctx, a, permissions.ApplicationMigrate, func(tx *sql.Tx) error {
		if err := t.dockerSource(ctx, tx, a, app); err != nil {
			return err
		}
		m, err := t.applicationMapping(ctx, tx, a, app, false)
		if err != nil {
			return err
		}
		spec, err := t.revisionSpec(ctx, tx, a, app, m.Preview.Revision)
		if err != nil {
			return err
		}
		var state, raw string
		var received, observed time.Time
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT e.state,v.snapshot,v.received_at,v.observed_at FROM endpoints e JOIN endpoint_inventory v ON v.endpoint_id=e.id WHERE e.organization_id=? AND e.environment_id=? AND e.id=?`), a.OrganizationID, a.EnvironmentID, m.Preview.EndpointID).Scan(&state, &raw, &received, &observed); err != nil {
			return err
		}
		snapshot, current, err := freshInventory(state, raw, received, observed)
		if err != nil {
			return err
		}
		dest, err := t.migrationDestination(ctx, tx, a, m.Preview.ApplicationName, endpoint)
		if err != nil {
			return err
		}
		out = &MigrationSource{ApplicationName: m.Preview.ApplicationName, Revision: m.Preview.Revision, Spec: spec, Project: m.Preview.Project, EndpointID: m.Preview.EndpointID, Containers: map[string]protocol.Container{}, Volumes: snapshot.Volumes, Destination: dest}
		if out.Volumes == nil {
			out.Volumes = []protocol.Volume{}
		}
		for service, id := range m.Bindings {
			out.Containers[service] = current[id]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkAnalysis bounds a report before it is stored.
func checkAnalysis(an MigrationAnalysis) error {
	if an.Revision < 1 || an.Revision > MaxApplicationRevisions || len(an.Report) < 2 || len(an.Report) > MaxMigrationReportBytes || !json.Valid(an.Report) {
		return ErrInvalid
	}
	return nil
}

// CreateMigration starts app's migration to a namespace of a cluster with its first analysis.
// The source must be adopted on Docker, the namespace one the cluster's manifest grants, and the
// source may have no other open migration.
func (t *tenancyStore) CreateMigration(ctx context.Context, a TenantAccess, app string, start MigrationStart, an MigrationAnalysis) (*ApplicationMigration, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	if err := checkAnalysis(an); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	var out *ApplicationMigration
	err = t.withTenantTarget(ctx, a, permissions.ApplicationMigrate, app+"/migration/"+id, func(tx *sql.Tx) error {
		head, err := t.lockSource(ctx, tx, a, app)
		if err != nil {
			return err
		}
		if err := t.dockerSource(ctx, tx, a, app); err != nil {
			return err
		}
		if an.Revision != head {
			return ErrMigrationStale
		}
		dest, err := t.migrationDestination(ctx, tx, a, "", start.DestinationEndpointID)
		if err != nil {
			return err
		}
		if !slices.Contains(dest.Namespaces, start.Namespace) {
			return ErrNamespaceUnknown
		}
		var open int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM application_migrations WHERE organization_id=? AND environment_id=? AND application_id=? AND `+openMigration), a.OrganizationID, a.EnvironmentID, app).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return ErrMigrationOpen
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_migrations(id,organization_id,environment_id,application_id,destination_endpoint_id,namespace,status,source_revision,ready,report,choices,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), id, a.OrganizationID, a.EnvironmentID, app, start.DestinationEndpointID, start.Namespace, MigrationAnalyzed, an.Revision, boolInt(an.Ready), string(an.Report), `{}`, a.ActorID, now, now); err != nil {
			return err
		}
		out, err = t.migrationByID(ctx, tx, a, id, "source")
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (t *tenancyStore) migrationByID(ctx context.Context, tx *sql.Tx, a TenantAccess, id, role string) (*ApplicationMigration, error) {
	m, err := scanMigration(tx.QueryRowContext(ctx, t.store.rebind(selectMigration+`WHERE m.organization_id=? AND m.environment_id=? AND m.id=?`), a.OrganizationID, a.EnvironmentID, id))
	if err != nil {
		return nil, err
	}
	m.Role = role
	return m, nil
}

// openMigrationOf reads app's open migration as its source, ErrNotFound when it has none.
func (t *tenancyStore) openMigrationOf(ctx context.Context, tx *sql.Tx, a TenantAccess, app string) (*ApplicationMigration, error) {
	m, err := scanMigration(tx.QueryRowContext(ctx, t.store.rebind(selectMigration+`WHERE m.organization_id=? AND m.environment_id=? AND m.application_id=? AND m.`+openMigration), a.OrganizationID, a.EnvironmentID, app))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Role = "source"
	return m, nil
}

// ReadMigration returns app's open migration when app is its source, else the migration that
// created app as its destination, whatever its status; ErrNotFound when there is neither.
func (t *tenancyStore) ReadMigration(ctx context.Context, a TenantAccess, app string) (*ApplicationMigration, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	var out *ApplicationMigration
	err = t.readTenant(ctx, a, permissions.ApplicationRead, func(tx *sql.Tx) error {
		out, err = t.openMigrationOf(ctx, tx, a, app)
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		out, err = scanMigration(tx.QueryRowContext(ctx, t.store.rebind(selectMigration+`WHERE m.organization_id=? AND m.environment_id=? AND m.destination_application_id=? ORDER BY m.created_at DESC LIMIT 1`), a.OrganizationID, a.EnvironmentID, app))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err == nil {
			out.Role = "destination"
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// changeMigration runs op on app's open migration under application.migrate, locked behind the
// application row and audited on <app>/migration/<id>.
func (t *tenancyStore) changeMigration(ctx context.Context, a TenantAccess, app string, op func(tx *sql.Tx, head int, m *ApplicationMigration) error) (*ApplicationMigration, error) {
	app, err := migrationApp(a, app)
	if err != nil {
		return nil, err
	}
	target := app + "/migration"
	var out *ApplicationMigration
	err = t.run(ctx, a, permissions.ApplicationMigrate, &target, nil, true, func(tx *sql.Tx) error {
		head, err := t.lockSource(ctx, tx, a, app)
		if err != nil {
			return err
		}
		m, err := t.openMigrationOf(ctx, tx, a, app)
		if err != nil {
			return err
		}
		target = app + "/migration/" + m.ID
		if err := op(tx, head, m); err != nil {
			return err
		}
		out, err = t.migrationByID(ctx, tx, a, m.ID, "source")
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AnalyzeMigration replaces an analyzed migration's choices and report together: the choices
// name named volumes of the analyzed revision, each with a StorageClass the destination reports
// (or "" while it reports a default) and a valid size.
func (t *tenancyStore) AnalyzeMigration(ctx context.Context, a TenantAccess, app string, choices KubernetesExtension, an MigrationAnalysis) (*ApplicationMigration, error) {
	if err := checkAnalysis(an); err != nil {
		return nil, err
	}
	if len(choices.Volumes) > protocol.MaxKubernetesClaims {
		return nil, ErrInvalid
	}
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, head int, m *ApplicationMigration) error {
		if m.Status != MigrationAnalyzed {
			return ErrMigrationState
		}
		if an.Revision != head {
			return ErrMigrationStale
		}
		spec, err := t.revisionSpec(ctx, tx, a, m.ApplicationID, head)
		if err != nil {
			return err
		}
		dest, err := t.migrationDestination(ctx, tx, a, "", m.DestinationEndpointID)
		if err != nil {
			return err
		}
		if err := checkChoices(spec, dest.StorageClasses, choices); err != nil {
			return err
		}
		if choices.Volumes == nil {
			choices.Volumes = map[string]KubernetesVolume{}
		}
		raw, err := json.Marshal(choices)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`UPDATE application_migrations SET choices=?,report=?,ready=?,source_revision=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`), string(raw), string(an.Report), boolInt(an.Ready), an.Revision, time.Now().UTC(), a.OrganizationID, a.EnvironmentID, m.ID)
		return err
	})
}

// checkChoices holds each choice to a named volume of spec and to the destination's classes.
func checkChoices(spec ApplicationSpec, classes []protocol.StorageClass, choices KubernetesExtension) error {
	named := spec.namedVolumes()
	for name, v := range choices.Volumes {
		if !slices.Contains(named, name) {
			return ErrVolumeUnknown
		}
		if _, ok := protocol.StorageSizeBytes(v.Size); !ok {
			return ErrSizeInvalid
		}
		known := slices.ContainsFunc(classes, func(c protocol.StorageClass) bool {
			return c.Name == v.StorageClass || (v.StorageClass == "" && c.Default)
		})
		if !protocol.ValidStorageClass(v.StorageClass) || !known {
			return ErrStorageClassUnknown
		}
		if v.AccessMode != protocol.AccessReadWriteOnce {
			return ErrInvalid
		}
	}
	return nil
}

// CreateMigrationDestination creates the destination application "<name> on <endpoint>": revision
// 1 is the analyzed revision with the storage choices, its values are the analyzed revision's
// sealed afresh for the new application, and it is mapped to the migration's namespace. The
// migration must be analyzed and ready, and the source must still be at the analyzed revision.
// An audit row on each application records it.
func (t *tenancyStore) CreateMigrationDestination(ctx context.Context, a TenantAccess, app string, key []byte) (*ApplicationMigration, error) {
	if len(key) != 32 {
		return nil, ErrInvalid
	}
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, head int, m *ApplicationMigration) error {
		switch {
		case m.Status != MigrationAnalyzed:
			return ErrMigrationState
		case !m.Ready:
			return ErrMigrationNotReady
		case m.SourceRevision != head:
			return ErrMigrationStale
		}
		if err := t.dockerSource(ctx, tx, a, m.ApplicationID); err != nil {
			return err
		}
		spec, values, _, err := t.resolveApplicationValues(ctx, tx, a, m.ApplicationID, m.SourceRevision, key)
		if err != nil {
			return err
		}
		dest, err := t.migrationDestination(ctx, tx, a, m.ApplicationName, m.DestinationEndpointID)
		if err != nil {
			return err
		}
		if !slices.Contains(dest.Namespaces, m.Namespace) {
			return ErrNamespaceUnknown
		}
		var taken int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT (SELECT COUNT(*) FROM applications WHERE organization_id=? AND environment_id=? AND name=?)+(SELECT COUNT(*) FROM application_instances WHERE organization_id=? AND environment_id=? AND endpoint_id=? AND project=?)`), a.OrganizationID, a.EnvironmentID, dest.Name, a.OrganizationID, a.EnvironmentID, dest.EndpointID, dest.Project).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return ErrApplicationNameTaken
		}
		spec.Kubernetes = nil
		if len(m.Choices.Volumes) > 0 {
			spec.Kubernetes = &KubernetesExtension{Volumes: m.Choices.Volumes}
		}
		now := time.Now().UTC()
		created := Application{ID: uuid.NewString(), OrganizationID: a.OrganizationID, EnvironmentID: a.EnvironmentID, Name: dest.Name, LatestRevision: 1, CreatedBy: a.ActorID, CreatedAt: now}
		if err := t.insertApplication(ctx, tx, a, created, spec, values, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`INSERT INTO application_instances(id,organization_id,environment_id,application_id,endpoint_id,project,revision,created_by,created_at,mapping_version,mapped_revision,namespace) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), uuid.NewString(), a.OrganizationID, a.EnvironmentID, created.ID, dest.EndpointID, dest.Project, 1, a.ActorID, now, 1, 1, m.Namespace); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_migrations SET destination_application_id=?,status=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`), created.ID, MigrationDestinationCreated, now, a.OrganizationID, a.EnvironmentID, m.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.store.rebind(`INSERT INTO audit_records (user_id,action,resource,details,ip_address,created_at,scope,organization_id,environment_id,correlation_id,result) VALUES (?,?,?,?,?,?,?,?,?,?,?)`), a.ActorID, string(permissions.ApplicationMigrate), created.ID+"/migration/"+m.ID, "source="+m.ApplicationID, a.IPAddress, now, "organization", a.OrganizationID, a.EnvironmentID, a.CorrelationID, "success")
		return err
	})
}

// validNote is 1..MaxMigrationNoteRunes characters of printable text, not only spaces.
func validNote(note string) bool {
	n := utf8.RuneCountInString(note)
	return utf8.ValidString(note) && n >= 1 && n <= MaxMigrationNoteRunes && strings.TrimSpace(note) != "" && !strings.ContainsFunc(note, func(r rune) bool {
		return !unicode.IsPrint(r) && r != ' '
	})
}

// ConfirmMigration records an operator's confirmation with its note: "validated" after the
// destination is created, "cutover" after validation, which closes the migration.
func (t *tenancyStore) ConfirmMigration(ctx context.Context, a TenantAccess, app, step, note string) (*ApplicationMigration, error) {
	if !validNote(note) || (step != "validated" && step != "cutover") {
		return nil, ErrInvalid
	}
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, _ int, m *ApplicationMigration) error {
		now := time.Now().UTC()
		query, from := `UPDATE application_migrations SET status=?,validated_by=?,validated_at=?,validated_note=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`, MigrationDestinationCreated
		to := MigrationValidated
		if step == "cutover" {
			query, from, to = `UPDATE application_migrations SET status=?,confirmed_by=?,confirmed_at=?,cutover_note=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`, MigrationValidated, MigrationCutoverConfirmed
		}
		if m.Status != from {
			return ErrMigrationState
		}
		_, err := tx.ExecContext(ctx, t.store.rebind(query), to, a.ActorID, now, note, now, a.OrganizationID, a.EnvironmentID, m.ID)
		return err
	})
}

// AbandonMigration closes app's open migration. A destination it created stays, with its
// deployments; nothing on either runtime changes.
func (t *tenancyStore) AbandonMigration(ctx context.Context, a TenantAccess, app string) (*ApplicationMigration, error) {
	return t.changeMigration(ctx, a, app, func(tx *sql.Tx, _ int, m *ApplicationMigration) error {
		_, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE application_migrations SET status=?,updated_at=? WHERE organization_id=? AND environment_id=? AND id=?`), MigrationAbandoned, time.Now().UTC(), a.OrganizationID, a.EnvironmentID, m.ID)
		return err
	})
}
```

In `internal/store/store.go`, replace

```go
	SetApplicationMapping(ctx context.Context, access TenantAccess, applicationID string, request MappingRequest) error
```

with

```go
	SetApplicationMapping(ctx context.Context, access TenantAccess, applicationID string, request MappingRequest) error
	// Migrations (docs/application-schema.md, Migration): writes under application.migrate,
	// audited on <app>/migration/<id>; ReadMigration under application.read. The API runs the
	// analyzer between ReadMigrationSource and the write that stores its report.
	ReadMigrationSource(ctx context.Context, access TenantAccess, applicationID, endpointID string) (*MigrationSource, error)
	CreateMigration(ctx context.Context, access TenantAccess, applicationID string, start MigrationStart, analysis MigrationAnalysis) (*ApplicationMigration, error)
	ReadMigration(ctx context.Context, access TenantAccess, applicationID string) (*ApplicationMigration, error)
	AnalyzeMigration(ctx context.Context, access TenantAccess, applicationID string, choices KubernetesExtension, analysis MigrationAnalysis) (*ApplicationMigration, error)
	CreateMigrationDestination(ctx context.Context, access TenantAccess, applicationID string, key []byte) (*ApplicationMigration, error)
	ConfirmMigration(ctx context.Context, access TenantAccess, applicationID, step, note string) (*ApplicationMigration, error)
	AbandonMigration(ctx context.Context, access TenantAccess, applicationID string) (*ApplicationMigration, error)
```

In `internal/store/application_deployment.go`, replace

```go
	// Validation is the deployment's health validation: nil for a plan, a removal or an apply
	// that did not succeed.
	Validation *Validation `json:"validation,omitempty"`
}
```

with

```go
	// Validation is the deployment's health validation: nil for a plan, a removal or an apply
	// that did not succeed.
	Validation *Validation `json:"validation,omitempty"`
	// MigrationID names the open migration whose destination this apply deployed, set when the
	// row turned applying.
	MigrationID string `json:"migration_id,omitempty"`
}
```

in `selectDeployments` replace

```go
d.detail,d.result,d.correlation_id,` + validationColumns
```

with

```go
d.detail,d.result,d.correlation_id,COALESCE(d.migration_id,''),` + validationColumns
```

and in `scanDeployment` replace

```go
&d.Detail, &result, &d.CorrelationID}, vs.dest()...)...)
```

with

```go
&d.Detail, &result, &d.CorrelationID, &d.MigrationID}, vs.dest()...)...)
```

In `internal/store/application_apply.go`, `applyDeployment`, replace

```go
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='applying',applied_by=?,applied_at=?,deadline=?,policy_run_id=? WHERE id=? AND state='planned'`), a.ActorID, now, frame.Deadline, run, d.ID)
```

with

```go
		// An open migration's destination records the migration on every apply, as a policy run does.
		res, err := tx.ExecContext(ctx, t.store.rebind(`UPDATE deployments SET state='applying',applied_by=?,applied_at=?,deadline=?,policy_run_id=?,migration_id=(SELECT m.id FROM application_migrations m WHERE m.organization_id=? AND m.environment_id=? AND m.destination_application_id=? AND m.`+openMigration+`) WHERE id=? AND state='planned'`), a.ActorID, now, frame.Deadline, run, a.OrganizationID, a.EnvironmentID, d.ApplicationID, d.ID)
```

replace

```go
		d.State, d.AppliedBy, d.AppliedAt, d.Deadline = "applying", a.ActorID, &now, &frame.Deadline
		out, req = d, &frame
		return nil
```

with

```go
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COALESCE(migration_id,'') FROM deployments WHERE organization_id=? AND environment_id=? AND id=?`), a.OrganizationID, a.EnvironmentID, d.ID).Scan(&d.MigrationID); err != nil {
			return err
		}
		d.State, d.AppliedBy, d.AppliedAt, d.Deadline = "applying", a.ActorID, &now, &frame.Deadline
		out, req = d, &frame
		return nil
```

and in `RemoveApplication` replace

```go
		if r.Confirm != project {
			return ErrInvalid
		}
		if endpointState != "active" {
			return ErrEndpointOffline
		}
```

with

```go
		if r.Confirm != project {
			return ErrInvalid
		}
		// The source of an open migration stays until the operator confirms cutover or abandons.
		var migrating int
		if err := tx.QueryRowContext(ctx, t.store.rebind(`SELECT COUNT(*) FROM application_migrations WHERE organization_id=? AND environment_id=? AND application_id=? AND `+openMigration), a.OrganizationID, a.EnvironmentID, appID.String()).Scan(&migrating); err != nil {
			return err
		}
		if migrating > 0 {
			return ErrMigrationOpen
		}
		if endpointState != "active" {
			return ErrEndpointOffline
		}
```

- [ ] **Step 6: Run the tests to verify they pass, on both databases**

Run: `gofmt -w internal/store internal/permissions && go vet ./... && go test -race -count=1 ./internal/store/... ./internal/permissions/ && PG=… go test -count=1 ./internal/store/...`
Expected: PASS on SQLite and PostgreSQL, the whole store suite included (`TestTenancyUpgradeAndReopen` replays migration 35; every existing deployment read scans the new column).

- [ ] **Step 7: DOX and commit**

In `internal/store/AGENTS.md`, `## Local Contracts`, append after the `- Kubernetes plans` bullet:

```markdown
- Migrations (`application_migration.go`, migration 35): `application_migrations` holds one row per migration of a Docker source to a cluster namespace, at most one open (`status` not `cutover_confirmed`/`abandoned`, a partial unique index) per source, with the analyzed `source_revision`, `ready`, the analyzer's `report` (JSON, at most `MaxMigrationReportBytes`), the `choices` (`KubernetesExtension`), and the actor, time and note (1..500 printable characters) of each confirmation. The source is a composite tenant FK (cascade), the destination endpoint a composite FK (restrict), the destination application `applications(id)` alone (`ON DELETE SET NULL`, which a composite key would spread to the tenant columns). Writes run under `application.migrate` on `<app>/migration/<id>`; `ReadMigration` under `application.read` answers the source's open migration (`role` `source`) or the migration that created a destination (`role` `destination`, any status). The store never runs the analyzer: the API passes `MigrationAnalysis{Revision, Report, Ready}` and the store checks the source is adopted on Docker (`ErrMappingRequired`, `ErrRuntimeUnsupported`), the namespace is granted (`ErrNamespaceUnknown`), no other migration is open (`ErrMigrationOpen`), the analysis is of the latest revision (`ErrMigrationStale`), and choices name the analyzed revision's named volumes (`ErrVolumeUnknown`), a valid size (`ErrSizeInvalid`) and a StorageClass the destination inventory reports, `""` only while it reports a default (`ErrStorageClassUnknown`). `AnalyzeMigration` (choices and report together) is `analyzed` only (`ErrMigrationState`). `CreateMigrationDestination` needs `ready` (`ErrMigrationNotReady`), creates `<name> on <endpoint>` (`ErrApplicationNameTaken` when the name or its project on the cluster is taken) through `insertApplication` with revision 1 = the analyzed revision plus the `kubernetes` extension and the analyzed revision's values sealed afresh, maps it to the namespace (mapping version 1), and writes a second audit row on `<destination>/migration/<id>`. `ConfirmMigration` takes `validated` after `destination_created` and `cutover` after `validated`. `AbandonMigration` leaves a created destination. `ApplyDeployment` writes `deployments.migration_id` for an open migration's destination in the flip to `applying` (`Deployment.MigrationID`); `RemoveApplication` refuses an open migration's source with `ErrMigrationOpen`. `ApplicationSpec.Kubernetes` (`KubernetesExtension`: 1..16 declared volumes, each `KubernetesVolume.Valid`) is covered by the spec digest; the Compose importer never sets it.
```

In `internal/permissions/AGENTS.md`, `## Local Contracts`, append after the `application.policy` bullet:

```markdown
- `application.migrate` (analyze an application for a cluster, record its storage choices, create the destination application, confirm and abandon) is organization-administrator-only: destination creation copies the source's secret values, which only that role may reveal. Reading a migration is `application.read`.
```

```bash
git add internal/store internal/permissions && make tidy-check lint && git commit -m "feat(store): application migrations, the kubernetes spec extension and migration_id" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 3: `internal/migration` — the pure analyzer

**Files:**
- Create: `internal/migration/migration.go`
- Create: `internal/migration/checklist.go`
- Create: `internal/migration/migration_test.go`
- Create (generated, reviewed): `internal/migration/testdata/named_volume.json`, `named_volume_chosen.json`, `blocked.json`
- Create (generated): `web/src/migration-codes.json`
- Create: `internal/migration/AGENTS.md`
- Modify: `AGENTS.md` (root Child DOX Index)

**Interfaces:**
- Consumes: `store.ApplicationSpec`, `store.ApplicationService`, `store.DeclaredVolume`, `store.KubernetesExtension`, `store.KubernetesVolume.Valid`, `store.VolumeHostName` (Task 2 and existing); `protocol.Container`, `protocol.ContainerInspection`, `protocol.Volume`, `protocol.StorageClass`, `protocol.KubernetesNames`, `protocol.AccessReadWriteOnce` (Task 1 and existing).
- Produces (package `migration`):
  ```go
  const Version = 1
  const Supported, ChoiceRequired, Blocked = "supported", "operator_choice_required", "blocked"
  const AxisStorage, AxisNetworking, AxisPorts, AxisSecrets, AxisProbes, AxisResources, AxisScheduling, AxisFlags = "storage", "networking", "ports", "secrets", "probes", "resources", "scheduling", "flags"
  var Codes, AssumptionCodes, ChecklistCodes []string
  type Input struct{ Spec store.ApplicationSpec; Project string; Containers map[string]protocol.Container; Inspections map[string]protocol.ContainerInspection; Volumes []protocol.Volume; Destination Destination; Choices store.KubernetesExtension }
  type Destination struct{ Namespace, Project string; StorageClasses []protocol.StorageClass }
  type Report struct{ Version int; Ready bool; Services []ServiceReport; Checklist []Step; Assumptions []string } // json version, ready, services, checklist, assumptions
  type ServiceReport struct{ Name, Class string; Findings []Finding }                                             // json name, class, findings
  type Finding struct{ Axis, Class, Code, Detail string }                                                         // json axis, class, code, detail,omitempty
  type Step struct{ Code string; Commands []string }                                                              // json code, commands,omitempty
  func Analyze(in Input) Report
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/migration/migration_test.go`:

```go
package migration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

var classes = []protocol.StorageClass{{Name: "standard", Default: true}, {Name: "fast"}}

func verified() protocol.ContainerInspection {
	return protocol.ContainerInspection{State: "running", RestartPolicy: "always", NetworkMode: "custom", NetworkCount: 1, Health: "none", Unsupported: []string{}, ConfigurationVerified: true}
}

// stateful is db mounting named volume data and web publishing 8080, both inspected clean.
func stateful(choices store.KubernetesExtension) Input {
	return Input{
		Spec: store.ApplicationSpec{Kind: "compose.v1", Volumes: []store.DeclaredVolume{{Name: "data"}}, Services: []store.ApplicationService{
			{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Ports: []store.ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}},
			{Name: "db", Image: "ghcr.io/org/db:1", Restart: "unless-stopped", Volumes: []store.ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}}},
		}},
		Project:     "shop",
		Containers:  map[string]protocol.Container{"web": {Name: "shop-web", Networks: []string{"shop_default"}}, "db": {Name: "shop-db", Networks: []string{"shop_default"}}},
		Inspections: map[string]protocol.ContainerInspection{"web": verified(), "db": verified()},
		Volumes:     []protocol.Volume{{Name: "shop_data"}},
		Destination: Destination{Namespace: "shop", Project: "shop-on-cluster", StorageClasses: classes},
		Choices:     choices,
	}
}

// blocked has a bind, a volume two services share, a host-address port, an inspection reporting
// privileged and resource limits, a service never inspected, a healthcheck and a restart policy
// a Deployment cannot express.
func blocked() Input {
	privileged := verified()
	privileged.Unsupported, privileged.ConfigurationVerified, privileged.Health = []string{"privileged", "resource_limits"}, false, "healthy"
	return Input{
		Spec: store.ApplicationSpec{Kind: "compose.v1", Volumes: []store.DeclaredVolume{{Name: "cache"}}, Services: []store.ApplicationService{
			{Name: "api", Image: "ghcr.io/org/api:1", Restart: "on-failure", Ports: []store.ApplicationPort{{Target: 80, Published: 8080, HostIP: "127.0.0.1", Protocol: "tcp"}}, Volumes: []store.ApplicationVolume{{Kind: "bind", Source: "/srv/api", Target: "/etc/api", ReadOnly: true}, {Kind: "named", Source: "cache", Target: "/cache"}}},
			{Name: "worker", Image: "ghcr.io/org/worker:1", Volumes: []store.ApplicationVolume{{Kind: "named", Source: "cache", Target: "/cache"}}},
		}},
		Project:     "shop",
		Containers:  map[string]protocol.Container{"api": {Networks: []string{"shop_default", "proxy"}}, "worker": {Networks: []string{"host"}}},
		Inspections: map[string]protocol.ContainerInspection{"api": privileged},
		Volumes:     []protocol.Volume{{Name: "shop_cache"}},
		Destination: Destination{Namespace: "shop", Project: "shop-on-cluster", StorageClasses: classes},
	}
}

var chosenData = store.KubernetesExtension{Volumes: map[string]store.KubernetesVolume{"data": {StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}}}

// golden compares got with testdata/name.json; KY_UPDATE_FIXTURES=1 rewrites it.
func golden(t *testing.T, name string, got Report) {
	t.Helper()
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("testdata", name+".json")
	if os.Getenv("KY_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, want) {
		t.Fatalf("%s differs (%v):\n%s", path, err, raw)
	}
}

// A named volume asks for a choice until one with a reported class is made; then the report is
// ready and the checklist copies the volume into the destination's Deployment.
func TestAnalyzeNamedVolume(t *testing.T) {
	golden(t, "named_volume", Analyze(stateful(store.KubernetesExtension{})))
	golden(t, "named_volume_chosen", Analyze(stateful(chosenData)))
	if !Analyze(stateful(chosenData)).Ready || Analyze(stateful(store.KubernetesExtension{})).Ready {
		t.Fatal("readiness does not follow the choice")
	}
	defaulted := store.KubernetesExtension{Volumes: map[string]store.KubernetesVolume{"data": {Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}}}
	if !Analyze(stateful(defaulted)).Ready {
		t.Fatal("the default class while one is reported")
	}
	gone := stateful(chosenData)
	gone.Destination.StorageClasses = []protocol.StorageClass{{Name: "standard"}}
	if r := Analyze(gone); r.Ready || r.Services[0].Findings[0] != (Finding{AxisStorage, ChoiceRequired, "volume_named", "data"}) {
		t.Fatalf("a choice whose class the cluster no longer reports: %+v", r.Services[0])
	}
}

// Binds, shared volumes, host addresses, host networking, privileged and scheduling flags block;
// dropped limits and healthchecks ask for a choice; a service never inspected is never read as
// supported on the axes an inspection decides.
func TestAnalyzeBlocked(t *testing.T) {
	r := Analyze(blocked())
	golden(t, "blocked", r)
	if r.Ready || r.Services[0].Class != Blocked || r.Services[1].Class != Blocked {
		t.Fatalf("report %+v", r)
	}
}

// Every code the analyzer can produce is in Codes, and every code in Codes is produced by some
// input, so the vocabulary the web translates is exactly the one reported.
func TestVocabularyIsExact(t *testing.T) {
	scheduling := verified()
	scheduling.Unsupported = []string{"pid_mode", "ulimits", "network", "read_only_rootfs", "dns"}
	extra := stateful(store.KubernetesExtension{})
	extra.Spec.Volumes = append(extra.Spec.Volumes, store.DeclaredVolume{Name: "ext", External: true})
	extra.Spec.Services[0].Volumes = []store.ApplicationVolume{{Kind: "named", Source: "ext", Target: "/ext"}}
	extra.Inspections["web"] = scheduling
	seen := map[string]bool{}
	for _, in := range []Input{stateful(store.KubernetesExtension{}), stateful(chosenData), blocked(), extra} {
		for _, s := range Analyze(in).Services {
			for _, f := range s.Findings {
				if !slices.Contains(Codes, f.Code) {
					t.Errorf("%s: %s is not in Codes", s.Name, f.Code)
				}
				seen[f.Code] = true
			}
		}
	}
	for _, c := range Codes {
		if !seen[c] {
			t.Errorf("%s is never produced", c)
		}
	}
}

// The checklist is fixed, and a mount target reaches the copy recipe as one shell word.
func TestChecklist(t *testing.T) {
	in := stateful(chosenData)
	in.Spec.Services[1].Volumes[0].Target = "/var/lib/it's data"
	steps := Analyze(in).Checklist
	var codes []string
	for _, s := range steps {
		codes = append(codes, s.Code)
	}
	if !slices.Equal(codes, ChecklistCodes) {
		t.Fatalf("steps %v", codes)
	}
	if want := `docker run --rm -v shop_data:/from:ro busybox tar -C /from -cf - . | kubectl -n shop exec -i deploy/shop-on-cluster-db -- tar -C '/var/lib/it'\''s data' -xf -`; steps[2].Commands[0] != want {
		t.Fatalf("copy %q", steps[2].Commands[0])
	}
	in.Volumes = nil
	if Analyze(in).Checklist[2].Code != "validate_destination" {
		t.Fatal("a copy step for a volume the host does not have")
	}
}

// The web's code tables are checked against this vocabulary through a generated fixture.
// KY_UPDATE_FIXTURES=1 rewrites it.
func TestWebVocabularyFixture(t *testing.T) {
	want, err := json.MarshalIndent(map[string][]string{"codes": Codes, "checklist": ChecklistCodes, "assumptions": AssumptionCodes}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("..", "..", "web", "src", "migration-codes.json")
	if os.Getenv("KY_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is stale (%v): run KY_UPDATE_FIXTURES=1 go test ./internal/migration -run TestWebVocabularyFixture", path, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/migration/`
Expected: FAIL to compile: `undefined: Analyze`, `undefined: Input`, `undefined: Codes`.

- [ ] **Step 3: Implement**

Create `internal/migration/migration.go`:

```go
// Package migration analyzes how an application adopted on a Docker host would run on a
// Kubernetes cluster: every service on every axis, as supported, operator_choice_required or
// blocked, with a code from a closed vocabulary and a parameter. It is pure: it reads store and
// protocol types, never the store's SQL, a runtime or a Kubernetes library.
package migration

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Version is the report format.
const Version = 1

// Classes, in increasing severity.
const (
	Supported      = "supported"
	ChoiceRequired = "operator_choice_required"
	Blocked        = "blocked"
)

// Axes, in report order.
const (
	AxisStorage    = "storage"
	AxisNetworking = "networking"
	AxisPorts      = "ports"
	AxisSecrets    = "secrets"
	AxisProbes     = "probes"
	AxisResources  = "resources"
	AxisScheduling = "scheduling"
	AxisFlags      = "flags"
)

// Codes is the closed finding vocabulary; the web has a sentence for each
// (web/src/migration-codes.json).
var Codes = []string{
	"volume_named", "volume_named_shared", "volume_bind", "volume_external", "storage_supported",
	"network_host", "networks_multiple", "networking_supported",
	"port_published", "port_host_ip", "port_unpublished",
	"secrets_supported",
	"healthcheck_dropped", "probes_supported",
	"resource_limits_dropped", "resources_supported",
	"scheduling_blocked", "scheduling_supported",
	"flag_blocked", "restart_policy", "read_only_rootfs", "flags_supported",
	"inspection_unavailable",
}

// AssumptionCodes are the report's fixed assumptions.
var AssumptionCodes = []string{"volume_size_unknown"}

// Input is one source analyzed against one destination.
type Input struct {
	// Spec is the source's latest revision, Project its Compose project.
	Spec    store.ApplicationSpec
	Project string
	// Containers and Inspections are keyed by service: the mapped container from the endpoint's
	// inventory and, when the plan-time inspection ran, what it observed.
	Containers  map[string]protocol.Container
	Inspections map[string]protocol.ContainerInspection
	// Volumes are the source endpoint's volumes.
	Volumes     []protocol.Volume
	Destination Destination
	Choices     store.KubernetesExtension
}

// Destination is the cluster side: the namespace, the destination application's project and the
// StorageClasses the cluster reports.
type Destination struct {
	Namespace      string
	Project        string
	StorageClasses []protocol.StorageClass
}

type Report struct {
	Version int `json:"version"`
	// Ready is true when no finding is blocked or operator_choice_required.
	Ready       bool            `json:"ready"`
	Services    []ServiceReport `json:"services"`
	Checklist   []Step          `json:"checklist"`
	Assumptions []string        `json:"assumptions"`
}

// ServiceReport's Class is its most severe finding's.
type ServiceReport struct {
	Name     string    `json:"name"`
	Class    string    `json:"class"`
	Findings []Finding `json:"findings"`
}

// Finding's Detail is its code's parameter: a volume name, a mount target, a port as
// <published>/<protocol>, a restart policy or a code from protocol.UnsupportedCodes.
type Finding struct {
	Axis   string `json:"axis"`
	Class  string `json:"class"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

var severity = map[string]int{Supported: 0, ChoiceRequired: 1, Blocked: 2}

// Inspection codes by the axis and finding they become; any other code is flag_blocked.
var (
	probeCodes      = map[string]bool{"image_config": true}
	resourceCodes   = map[string]bool{"resource_limits": true, "ulimits": true}
	schedulingCodes = map[string]bool{"pid_mode": true, "ipc_mode": true, "cgroup_parent": true, "userns_mode": true, "runtime": true}
	networkCodes    = map[string]bool{"network": true}
)

// Analyze classifies every service of in.Spec, sorted by name, and lists the checklist.
func Analyze(in Input) Report {
	declared := map[string]store.DeclaredVolume{}
	for _, v := range in.Spec.Volumes {
		declared[v.Name] = v
	}
	users := map[string]int{}
	for _, s := range in.Spec.Services {
		seen := map[string]bool{}
		for _, v := range s.Volumes {
			if v.Kind == "named" && !seen[v.Source] {
				seen[v.Source], users[v.Source] = true, users[v.Source]+1
			}
		}
	}
	r := Report{Version: Version, Ready: true, Services: []ServiceReport{}, Assumptions: []string{}}
	for _, s := range in.Spec.Services {
		inspection, inspected := in.Inspections[s.Name]
		var f []Finding
		f = append(f, storage(s, declared, users, in.Choices, in.Destination.StorageClasses)...)
		f = append(f, networking(in.Containers[s.Name], inspection, inspected))
		f = append(f, ports(s)...)
		f = append(f, Finding{AxisSecrets, Supported, "secrets_supported", ""})
		f = append(f, inspectedAxes(s, inspection, inspected)...)
		sr := ServiceReport{Name: s.Name, Class: Supported, Findings: f}
		for _, x := range f {
			if severity[x.Class] > severity[sr.Class] {
				sr.Class = x.Class
			}
		}
		r.Ready = r.Ready && sr.Class == Supported
		r.Services = append(r.Services, sr)
	}
	slices.SortFunc(r.Services, func(a, b ServiceReport) int { return strings.Compare(a.Name, b.Name) })
	if len(users) > 0 {
		r.Assumptions = append(r.Assumptions, "volume_size_unknown")
	}
	r.Checklist = checklist(in, users)
	return r
}

func storage(s store.ApplicationService, declared map[string]store.DeclaredVolume, users map[string]int, choices store.KubernetesExtension, classes []protocol.StorageClass) []Finding {
	var out []Finding
	for _, v := range s.Volumes {
		switch {
		case v.Kind == "bind":
			out = append(out, Finding{AxisStorage, Blocked, "volume_bind", v.Target})
		case users[v.Source] > 1:
			out = append(out, Finding{AxisStorage, Blocked, "volume_named_shared", v.Source})
		default:
			code := "volume_named"
			if declared[v.Source].External {
				code = "volume_external"
			}
			class := ChoiceRequired
			if chosen(choices, classes, v.Source) {
				class = Supported
			}
			out = append(out, Finding{AxisStorage, class, code, v.Source})
		}
	}
	if len(out) == 0 {
		out = append(out, Finding{AxisStorage, Supported, "storage_supported", ""})
	}
	return out
}

// chosen reports a valid choice for volume whose StorageClass the destination still reports ("":
// while it reports a default).
func chosen(choices store.KubernetesExtension, classes []protocol.StorageClass, volume string) bool {
	c, ok := choices.Volumes[volume]
	return ok && c.Valid() && slices.ContainsFunc(classes, func(sc protocol.StorageClass) bool {
		return sc.Name == c.StorageClass || (c.StorageClass == "" && sc.Default)
	})
}

func networking(c protocol.Container, in protocol.ContainerInspection, inspected bool) Finding {
	switch {
	case slices.Contains(c.Networks, "host") || (inspected && in.NetworkMode == "host"):
		return Finding{AxisNetworking, Blocked, "network_host", ""}
	case len(c.Networks) > 1 || (inspected && (in.NetworkCount > 1 || slices.ContainsFunc(in.Unsupported, func(code string) bool { return networkCodes[code] }))):
		return Finding{AxisNetworking, Supported, "networks_multiple", ""}
	}
	return Finding{AxisNetworking, Supported, "networking_supported", ""}
}

func ports(s store.ApplicationService) []Finding {
	var out []Finding
	for _, p := range s.Ports {
		detail := fmt.Sprintf("%d/%s", p.Published, p.Protocol)
		if p.HostIP != "" {
			out = append(out, Finding{AxisPorts, Blocked, "port_host_ip", detail})
		} else {
			out = append(out, Finding{AxisPorts, Supported, "port_published", detail})
		}
	}
	if len(out) == 0 {
		out = append(out, Finding{AxisPorts, Supported, "port_unpublished", ""})
	}
	return out
}

// inspectedAxes are probes, resources, scheduling and flags: each needs the inspection, so
// without one each is inspection_unavailable rather than read as support. The restart policy is
// the definition's and is judged either way.
func inspectedAxes(s store.ApplicationService, in protocol.ContainerInspection, inspected bool) []Finding {
	var out []Finding
	restart := func() {
		if s.Restart == "no" || s.Restart == "on-failure" {
			out = append(out, Finding{AxisFlags, Blocked, "restart_policy", s.Restart})
		}
	}
	if !inspected {
		for _, axis := range []string{AxisProbes, AxisResources, AxisScheduling, AxisFlags} {
			out = append(out, Finding{axis, ChoiceRequired, "inspection_unavailable", ""})
		}
		restart()
		return out
	}
	if slices.ContainsFunc(in.Unsupported, func(c string) bool { return probeCodes[c] }) || (in.Health != "" && in.Health != "none") {
		out = append(out, Finding{AxisProbes, ChoiceRequired, "healthcheck_dropped", ""})
	} else {
		out = append(out, Finding{AxisProbes, Supported, "probes_supported", ""})
	}
	var resources, scheduling, flags []Finding
	for _, c := range in.Unsupported {
		switch {
		case probeCodes[c], networkCodes[c]:
		case resourceCodes[c]:
			resources = append(resources, Finding{AxisResources, ChoiceRequired, "resource_limits_dropped", c})
		case schedulingCodes[c]:
			scheduling = append(scheduling, Finding{AxisScheduling, Blocked, "scheduling_blocked", c})
		case c == "read_only_rootfs":
			flags = append(flags, Finding{AxisFlags, ChoiceRequired, "read_only_rootfs", ""})
		default:
			flags = append(flags, Finding{AxisFlags, Blocked, "flag_blocked", c})
		}
	}
	if len(resources) == 0 {
		resources = []Finding{{AxisResources, Supported, "resources_supported", ""}}
	}
	if len(scheduling) == 0 {
		scheduling = []Finding{{AxisScheduling, Supported, "scheduling_supported", ""}}
	}
	out = append(append(out, resources...), scheduling...)
	out = append(out, flags...)
	restart()
	if !slices.ContainsFunc(out, func(f Finding) bool { return f.Axis == AxisFlags }) {
		out = append(out, Finding{AxisFlags, Supported, "flags_supported", ""})
	}
	return out
}
```

Create `internal/migration/checklist.go`:

```go
package migration

import (
	"slices"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// ChecklistCodes are the operator's steps, in order; the web has a sentence for each. The
// checklist is display only: the migration's status moves by the operator's confirmations.
var ChecklistCodes = []string{"grant_namespace", "create_destination", "copy_volume", "validate_destination", "switch_traffic", "confirm_cutover"}

// Step is one checklist step with its commands, the real names filled in.
type Step struct {
	Code     string   `json:"code"`
	Commands []string `json:"commands,omitempty"`
}

// checklist lists the steps; copy_volume appears when a named volume a single service mounts
// exists on the source host, with one copy recipe per volume into its destination claim.
func checklist(in Input, users map[string]int) []Step {
	ns := in.Destination.Namespace
	out := []Step{{Code: "grant_namespace", Commands: []string{"kubectl label namespace " + ns + " pod-security.kubernetes.io/enforce=baseline --overwrite"}}, {Code: "create_destination"}}
	services := make([]string, 0, len(in.Spec.Services))
	for _, s := range in.Spec.Services {
		services = append(services, s.Name)
	}
	names := protocol.KubernetesNames(in.Destination.Project, services)
	var copies []string
	for _, v := range in.Spec.Volumes {
		host := store.VolumeHostName(in.Project, v)
		if users[v.Name] != 1 || !slices.ContainsFunc(in.Volumes, func(pv protocol.Volume) bool { return pv.Name == host }) {
			continue
		}
		for _, s := range in.Spec.Services {
			for _, m := range s.Volumes {
				if m.Kind == "named" && m.Source == v.Name {
					copies = append(copies, "docker run --rm -v "+host+":/from:ro busybox tar -C /from -cf - . | kubectl -n "+ns+" exec -i deploy/"+names[s.Name]+" -- tar -C "+shellQuote(m.Target)+" -xf -")
				}
			}
		}
	}
	if len(copies) > 0 {
		out = append(out, Step{Code: "copy_volume", Commands: copies})
	}
	return append(out, Step{Code: "validate_destination"}, Step{Code: "switch_traffic"}, Step{Code: "confirm_cutover"})
}

// shellQuote makes s one POSIX shell word: a mount target is any display-safe absolute path.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
```

- [ ] **Step 4: Generate the golden reports and the web fixture, then review them**

Run: `mkdir -p internal/migration/testdata && KY_UPDATE_FIXTURES=1 go test ./internal/migration/`
Expected: PASS, writing the three golden files and `web/src/migration-codes.json`. Read each file before committing; they must be exactly:

`internal/migration/testdata/named_volume.json`:

```json
{
  "version": 1,
  "ready": false,
  "services": [
    {
      "name": "db",
      "class": "operator_choice_required",
      "findings": [
        {
          "axis": "storage",
          "class": "operator_choice_required",
          "code": "volume_named",
          "detail": "data"
        },
        {
          "axis": "networking",
          "class": "supported",
          "code": "networking_supported"
        },
        {
          "axis": "ports",
          "class": "supported",
          "code": "port_unpublished"
        },
        {
          "axis": "secrets",
          "class": "supported",
          "code": "secrets_supported"
        },
        {
          "axis": "probes",
          "class": "supported",
          "code": "probes_supported"
        },
        {
          "axis": "resources",
          "class": "supported",
          "code": "resources_supported"
        },
        {
          "axis": "scheduling",
          "class": "supported",
          "code": "scheduling_supported"
        },
        {
          "axis": "flags",
          "class": "supported",
          "code": "flags_supported"
        }
      ]
    },
    {
      "name": "web",
      "class": "supported",
      "findings": [
        {
          "axis": "storage",
          "class": "supported",
          "code": "storage_supported"
        },
        {
          "axis": "networking",
          "class": "supported",
          "code": "networking_supported"
        },
        {
          "axis": "ports",
          "class": "supported",
          "code": "port_published",
          "detail": "8080/tcp"
        },
        {
          "axis": "secrets",
          "class": "supported",
          "code": "secrets_supported"
        },
        {
          "axis": "probes",
          "class": "supported",
          "code": "probes_supported"
        },
        {
          "axis": "resources",
          "class": "supported",
          "code": "resources_supported"
        },
        {
          "axis": "scheduling",
          "class": "supported",
          "code": "scheduling_supported"
        },
        {
          "axis": "flags",
          "class": "supported",
          "code": "flags_supported"
        }
      ]
    }
  ],
  "checklist": [
    {
      "code": "grant_namespace",
      "commands": [
        "kubectl label namespace shop pod-security.kubernetes.io/enforce=baseline --overwrite"
      ]
    },
    {
      "code": "create_destination"
    },
    {
      "code": "copy_volume",
      "commands": [
        "docker run --rm -v shop_data:/from:ro busybox tar -C /from -cf - . | kubectl -n shop exec -i deploy/shop-on-cluster-db -- tar -C '/var/lib/db' -xf -"
      ]
    },
    {
      "code": "validate_destination"
    },
    {
      "code": "switch_traffic"
    },
    {
      "code": "confirm_cutover"
    }
  ],
  "assumptions": [
    "volume_size_unknown"
  ]
}
```

`internal/migration/testdata/named_volume_chosen.json`:

```json
{
  "version": 1,
  "ready": true,
  "services": [
    {
      "name": "db",
      "class": "supported",
      "findings": [
        {
          "axis": "storage",
          "class": "supported",
          "code": "volume_named",
          "detail": "data"
        },
        {
          "axis": "networking",
          "class": "supported",
          "code": "networking_supported"
        },
        {
          "axis": "ports",
          "class": "supported",
          "code": "port_unpublished"
        },
        {
          "axis": "secrets",
          "class": "supported",
          "code": "secrets_supported"
        },
        {
          "axis": "probes",
          "class": "supported",
          "code": "probes_supported"
        },
        {
          "axis": "resources",
          "class": "supported",
          "code": "resources_supported"
        },
        {
          "axis": "scheduling",
          "class": "supported",
          "code": "scheduling_supported"
        },
        {
          "axis": "flags",
          "class": "supported",
          "code": "flags_supported"
        }
      ]
    },
    {
      "name": "web",
      "class": "supported",
      "findings": [
        {
          "axis": "storage",
          "class": "supported",
          "code": "storage_supported"
        },
        {
          "axis": "networking",
          "class": "supported",
          "code": "networking_supported"
        },
        {
          "axis": "ports",
          "class": "supported",
          "code": "port_published",
          "detail": "8080/tcp"
        },
        {
          "axis": "secrets",
          "class": "supported",
          "code": "secrets_supported"
        },
        {
          "axis": "probes",
          "class": "supported",
          "code": "probes_supported"
        },
        {
          "axis": "resources",
          "class": "supported",
          "code": "resources_supported"
        },
        {
          "axis": "scheduling",
          "class": "supported",
          "code": "scheduling_supported"
        },
        {
          "axis": "flags",
          "class": "supported",
          "code": "flags_supported"
        }
      ]
    }
  ],
  "checklist": [
    {
      "code": "grant_namespace",
      "commands": [
        "kubectl label namespace shop pod-security.kubernetes.io/enforce=baseline --overwrite"
      ]
    },
    {
      "code": "create_destination"
    },
    {
      "code": "copy_volume",
      "commands": [
        "docker run --rm -v shop_data:/from:ro busybox tar -C /from -cf - . | kubectl -n shop exec -i deploy/shop-on-cluster-db -- tar -C '/var/lib/db' -xf -"
      ]
    },
    {
      "code": "validate_destination"
    },
    {
      "code": "switch_traffic"
    },
    {
      "code": "confirm_cutover"
    }
  ],
  "assumptions": [
    "volume_size_unknown"
  ]
}
```

`internal/migration/testdata/blocked.json`:

```json
{
  "version": 1,
  "ready": false,
  "services": [
    {
      "name": "api",
      "class": "blocked",
      "findings": [
        {
          "axis": "storage",
          "class": "blocked",
          "code": "volume_bind",
          "detail": "/etc/api"
        },
        {
          "axis": "storage",
          "class": "blocked",
          "code": "volume_named_shared",
          "detail": "cache"
        },
        {
          "axis": "networking",
          "class": "supported",
          "code": "networks_multiple"
        },
        {
          "axis": "ports",
          "class": "blocked",
          "code": "port_host_ip",
          "detail": "8080/tcp"
        },
        {
          "axis": "secrets",
          "class": "supported",
          "code": "secrets_supported"
        },
        {
          "axis": "probes",
          "class": "operator_choice_required",
          "code": "healthcheck_dropped"
        },
        {
          "axis": "resources",
          "class": "operator_choice_required",
          "code": "resource_limits_dropped",
          "detail": "resource_limits"
        },
        {
          "axis": "scheduling",
          "class": "supported",
          "code": "scheduling_supported"
        },
        {
          "axis": "flags",
          "class": "blocked",
          "code": "flag_blocked",
          "detail": "privileged"
        },
        {
          "axis": "flags",
          "class": "blocked",
          "code": "restart_policy",
          "detail": "on-failure"
        }
      ]
    },
    {
      "name": "worker",
      "class": "blocked",
      "findings": [
        {
          "axis": "storage",
          "class": "blocked",
          "code": "volume_named_shared",
          "detail": "cache"
        },
        {
          "axis": "networking",
          "class": "blocked",
          "code": "network_host"
        },
        {
          "axis": "ports",
          "class": "supported",
          "code": "port_unpublished"
        },
        {
          "axis": "secrets",
          "class": "supported",
          "code": "secrets_supported"
        },
        {
          "axis": "probes",
          "class": "operator_choice_required",
          "code": "inspection_unavailable"
        },
        {
          "axis": "resources",
          "class": "operator_choice_required",
          "code": "inspection_unavailable"
        },
        {
          "axis": "scheduling",
          "class": "operator_choice_required",
          "code": "inspection_unavailable"
        },
        {
          "axis": "flags",
          "class": "operator_choice_required",
          "code": "inspection_unavailable"
        }
      ]
    }
  ],
  "checklist": [
    {
      "code": "grant_namespace",
      "commands": [
        "kubectl label namespace shop pod-security.kubernetes.io/enforce=baseline --overwrite"
      ]
    },
    {
      "code": "create_destination"
    },
    {
      "code": "validate_destination"
    },
    {
      "code": "switch_traffic"
    },
    {
      "code": "confirm_cutover"
    }
  ],
  "assumptions": [
    "volume_size_unknown"
  ]
}
```

`web/src/migration-codes.json`:

```json
{
  "assumptions": [
    "volume_size_unknown"
  ],
  "checklist": [
    "grant_namespace",
    "create_destination",
    "copy_volume",
    "validate_destination",
    "switch_traffic",
    "confirm_cutover"
  ],
  "codes": [
    "volume_named",
    "volume_named_shared",
    "volume_bind",
    "volume_external",
    "storage_supported",
    "network_host",
    "networks_multiple",
    "networking_supported",
    "port_published",
    "port_host_ip",
    "port_unpublished",
    "secrets_supported",
    "healthcheck_dropped",
    "probes_supported",
    "resource_limits_dropped",
    "resources_supported",
    "scheduling_blocked",
    "scheduling_supported",
    "flag_blocked",
    "restart_policy",
    "read_only_rootfs",
    "flags_supported",
    "inspection_unavailable"
  ]
}
```

- [ ] **Step 5: Run the tests to verify they pass, and the import rule**

Run: `gofmt -w internal/migration && go vet ./internal/migration && go test -race -count=1 ./internal/migration/ && test "$(go list -deps ./internal/migration | grep -c k8s.io)" = 0 && test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0`
Expected: PASS and both counts 0 (`cmd/server` does not import the package yet; Task 7 does, and re-runs this check).

- [ ] **Step 6: DOX and commit**

Create `internal/migration/AGENTS.md`:

```markdown
# Migration

## Purpose
The pure analyzer of a Docker-to-Kubernetes migration: `Analyze(Input) Report` classifies every service of a source definition on every axis and lists the operator's checklist.

## Ownership
Owns the finding, checklist and assumption vocabularies and the report format. `internal/store` owns the migration rows, choices validation and destination creation; `internal/api` gathers the inputs, runs the analyzer and stores its report; `web` owns the sentences.

## Local Contracts
- Pure: imports `internal/store` types and `internal/agent/protocol`, never SQL, a runtime or a `k8s.io` package (`go list -deps ./internal/migration | grep -c k8s.io` prints 0). The store never imports this package.
- `Report{version 1, ready, services (sorted by name), checklist, assumptions}`; a service's `class` is its most severe finding's; `ready` is true when no finding is `blocked` or `operator_choice_required`.
- Axes `storage`, `networking`, `ports`, `secrets`, `probes`, `resources`, `scheduling`, `flags`, each with at least one finding per service. `Codes` is the closed vocabulary; `TestVocabularyIsExact` proves every code is produced and nothing else is. A finding's `detail` is its code's parameter: a volume name, a mount target, `<published>/<protocol>`, a restart policy or an `UnsupportedCodes` code.
- Storage: a bind is `volume_bind` (blocked); a named volume two services mount is `volume_named_shared` (blocked); otherwise `volume_named` (`volume_external` when declared external) is `operator_choice_required` until `Choices` holds a valid choice whose StorageClass the destination still reports (`""` while it reports a default), then `supported`.
- Probes, resources, scheduling and flags need the inspection: without one each is `inspection_unavailable` (choice). `image_config` or an inspected health other than `none` is `healthcheck_dropped` (choice); `resource_limits`/`ulimits` `resource_limits_dropped` (choice); `pid_mode`, `ipc_mode`, `cgroup_parent`, `userns_mode`, `runtime` `scheduling_blocked`; `read_only_rootfs` a choice (the definition cannot carry it); `network` counts as `networks_multiple`; every other inspection code is `flag_blocked`. A restart policy `no` or `on-failure` is `restart_policy` (blocked) with or without an inspection.
- Checklist `grant_namespace`, `create_destination`, `copy_volume` (only when a single-service named volume exists on the source host; one `docker run ... | kubectl exec -i deploy/<name> -- tar` recipe per volume, the target shell-quoted), `validate_destination`, `switch_traffic`, `confirm_cutover`. Assumption `volume_size_unknown` whenever a named volume is mounted.
- `TestWebVocabularyFixture` writes `Codes`, `ChecklistCodes` and `AssumptionCodes` to `web/src/migration-codes.json` (`KY_UPDATE_FIXTURES=1`) and fails while it is stale; the web tests fail while a sentence table misses a code.

## Work Guidance

## Verification
- `go test ./internal/migration` (golden reports in `testdata/`, regenerated with `KY_UPDATE_FIXTURES=1` and reviewed in the diff).

## Child DOX Index
None.
```

In the root `AGENTS.md`, `## Child DOX Index`, add after the `internal/registry/AGENTS.md` line:

```markdown
- [internal/migration/AGENTS.md](internal/migration/AGENTS.md): The pure Docker-to-Kubernetes migration analyzer: report format, finding, checklist and assumption vocabularies.
```

```bash
git add internal/migration web/src/migration-codes.json AGENTS.md && make tidy-check lint && git commit -m "feat(migration): the pure Docker-to-Kubernetes analyzer" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 4: Store — claims in the Kubernetes preflight, plan, frame and removal settle

**Files:**
- Modify: `internal/store/application_preflight.go` (`PreflightService.Details`, `buildKubernetesPreflight`, `volumeUsers`, `kubernetesVolumeCodes`)
- Modify: `internal/store/application_spec.go` (`KubernetesExtension.volume`, `kubernetesClaims`)
- Modify: `internal/store/application_deployment.go` (`PlannedService.ClaimMounts`, `DeploymentPlan.Claims`, `BlockedService.Details`, `draftPlan`)
- Modify: `internal/store/application_apply.go` (`kubernetesFrame`, `settleRemoval`)
- Test: `internal/store/kubernetes_deployment_test.go`
- Docs: `internal/store/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.KubernetesClaim`, `protocol.KubernetesMount`, `protocol.MaxKubernetesMounts`, `protocol.DetailRetained`, `KubernetesTarget.Claims`, `DeploymentService.Volumes` (Task 1); `KubernetesExtension`, `KubernetesVolume` (Task 2); test helpers `kubernetesPlanFixture`, `kubePlanRequest`, `twoServiceSpec`, `fakeResolver`, `digestOf` (existing).
- Produces (package `store`):
  ```go
  // PreflightService and BlockedService gain Details map[string]string `json:"details,omitempty"`:
  //   {"k8s_volume": "choice_required"} when every volume behind k8s_volume only lacks a choice.
  // Unsupported may now carry k8s_volume_shared.
  // PlannedService gains ClaimMounts []protocol.KubernetesMount `json:"claim_mounts,omitempty"`.
  // DeploymentPlan gains Claims []protocol.KubernetesClaim `json:"claims,omitempty"`.
  func kubernetesClaims(project string, spec ApplicationSpec) ([]protocol.KubernetesClaim, map[string][]protocol.KubernetesMount)
  func volumeUsers(spec ApplicationSpec) map[string]int
  // A cluster removal settles with skipped volume steps carrying detail retained.
  ```

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/kubernetes_deployment_test.go` (it imports `reflect` since Task 1):

```go
func claimSpec(k *KubernetesExtension) ApplicationSpec {
	return ApplicationSpec{Kind: "compose.v1", Volumes: []DeclaredVolume{{Name: "data"}, {Name: "logs"}}, Services: []ApplicationService{
		{Name: "db", Image: "ghcr.io/org/db:1", Volumes: []ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}, {Kind: "named", Source: "logs", Target: "/var/log/db", ReadOnly: true}}},
		{Name: "web", Image: "ghcr.io/org/web:1"},
	}, Kubernetes: k}
}

var bothChosen = &KubernetesExtension{Volumes: map[string]KubernetesVolume{
	"data": {StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce},
	"logs": {Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce},
}}

// Chosen named volumes plan as claims <project>-<volume> mounted by their one service, and the
// frame carries both; the claims are what the agent creates and nothing else.
func TestKubernetesPlanClaims(t *testing.T) {
	st, a, app, _, m := kubernetesPlanFixture(t, claimSpec(bothChosen), nil)
	ctx := context.Background()
	ts := st.Tenancy()
	resolver := &fakeResolver{reply: map[string]fakeReply{"ghcr.io/org/db:1": {digest: digestOf("b")}, "ghcr.io/org/web:1": {digest: digestOf("c")}}}
	d, err := ts.PlanDeployment(ctx, a, app.ID, kubePlanRequest(m), resolver, imageCheckKey, false)
	if err != nil {
		t.Fatal(err)
	}
	claims := []protocol.KubernetesClaim{
		{Name: "shop-front-data", StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce},
		{Name: "shop-front-logs", Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce},
	}
	mounts := []protocol.KubernetesMount{{Claim: "shop-front-data", MountPath: "/var/lib/db"}, {Claim: "shop-front-logs", MountPath: "/var/log/db", ReadOnly: true}}
	if !reflect.DeepEqual(d.Plan.Claims, claims) || !reflect.DeepEqual(d.Plan.Services[0].ClaimMounts, mounts) || len(d.Plan.Services[1].ClaimMounts) != 0 {
		t.Fatalf("plan %+v %+v", d.Plan.Claims, d.Plan.Services)
	}
	_, req, err := ts.ApplyDeployment(ctx, a, app.ID, d.ID, "shop-front", imageCheckKey, protocol.MaxDeploymentRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Kubernetes.Claims, claims) || !reflect.DeepEqual(req.Services[0].Volumes, mounts) || len(req.Services[1].Volumes) != 0 || len(req.Volumes) != 0 || len(req.Services[0].Mounts) != 0 {
		t.Fatalf("frame %+v %+v", req.Kubernetes, req.Services)
	}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// A named volume without a choice is k8s_volume with detail choice_required; a bind keeps
// k8s_volume without one; a volume two services mount is k8s_volume_shared.
func TestKubernetesPlanVolumeBlockers(t *testing.T) {
	unchosen := claimSpec(&KubernetesExtension{Volumes: map[string]KubernetesVolume{"data": bothChosen.Volumes["data"]}})
	bind := claimSpec(bothChosen)
	bind.Services[0].Volumes = append(bind.Services[0].Volumes, ApplicationVolume{Kind: "bind", Source: "/srv/db", Target: "/etc/db"})
	shared := claimSpec(bothChosen)
	shared.Services[1].Volumes = []ApplicationVolume{{Kind: "named", Source: "data", Target: "/data"}}
	for name, tc := range map[string]struct {
		spec ApplicationSpec
		want map[string]BlockedService
	}{
		"no choice": {unchosen, map[string]BlockedService{"db": {Unsupported: []string{"k8s_volume"}, Details: map[string]string{"k8s_volume": "choice_required"}}}},
		"a bind":    {bind, map[string]BlockedService{"db": {Unsupported: []string{"k8s_volume"}}}},
		"shared":    {shared, map[string]BlockedService{"db": {Unsupported: []string{"k8s_volume_shared"}}, "web": {Unsupported: []string{"k8s_volume_shared"}}}},
	} {
		st, a, app, _, m := kubernetesPlanFixture(t, tc.spec, nil)
		_, err := st.Tenancy().PlanDeployment(context.Background(), a, app.ID, kubePlanRequest(m), &fakeResolver{reply: map[string]fakeReply{}}, imageCheckKey, false)
		var blocked *PreflightBlockedError
		if !errors.As(err, &blocked) || len(blocked.Services) != len(tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
		for _, s := range blocked.Services {
			if want := tc.want[s.Name]; !slices.Equal(s.Unsupported, want.Unsupported) || !reflect.DeepEqual(s.Details, want.Details) {
				t.Errorf("%s: %s %+v", name, s.Name, s)
			}
		}
	}
}

// A cluster removal reports each kept claim as a skipped volume step with detail retained, and
// settles; any other volume step is refused.
func TestRemoveKubernetesRetainsClaims(t *testing.T) {
	st, a, app, cluster, m := kubernetesPlanFixture(t, twoServiceSpec(), map[string]string{"web.TOKEN": "x"})
	ctx := context.Background()
	ts := st.Tenancy()
	d, _, err := ts.RemoveApplication(ctx, a, app.ID, RemovalBody{InstanceID: m.InstanceID, Confirm: "shop-front"})
	if err != nil {
		t.Fatal(err)
	}
	steps := []protocol.DeploymentStep{
		{Service: "web", Step: protocol.StepPrecondition, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepRemove, Outcome: protocol.OutcomeSucceeded},
		{Service: "web", Step: protocol.StepVolume, Outcome: protocol.OutcomeSkipped, Detail: protocol.DetailRetained},
	}
	res := protocol.DeploymentResult{Deployment: d.ID, RequestID: d.CorrelationID, Outcome: protocol.OutcomeSucceeded, Services: []protocol.DeploymentIdentity{}, Steps: steps}
	bad := res
	bad.Steps = append(slices.Clone(steps[:2]), protocol.DeploymentStep{Service: "web", Step: protocol.StepVolume, Outcome: protocol.OutcomeSucceeded})
	if err := ts.SettleDeployment(ctx, cluster, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a volume step that is not a kept claim: %v", err)
	}
	if err := ts.SettleDeployment(ctx, cluster, res); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReadApplicationInstance(ctx, a, app.ID, m.InstanceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("instance kept: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestKubernetesPlanClaims|TestKubernetesPlanVolumeBlockers|TestRemoveKubernetesRetainsClaims'`
Expected: FAIL to compile: `d.Plan.Claims undefined`, `ClaimMounts undefined`, `unknown field Details in struct literal of type BlockedService`.

- [ ] **Step 3: Implement the preflight codes**

In `internal/store/application_preflight.go`, replace

```go
	// Unsupported are the codes a plan-time live inspection reported (configuration_unsupported).
	Unsupported []string `json:"unsupported,omitempty"`
}
```

with

```go
	// Unsupported are the codes a plan-time live inspection reported (configuration_unsupported).
	Unsupported []string `json:"unsupported,omitempty"`
	// Details is a code's parameter, where one has one: k8s_volume is choice_required when every
	// volume behind it is a named volume that only lacks a storage choice.
	Details map[string]string `json:"details,omitempty"`
}
```

in `buildKubernetesPreflight` replace

```go
	blocked := false
	for _, s := range spec.Services {
		row := PreflightService{Name: s.Name, Reference: s.Image, Blockers: []string{}, Mounts: []protocol.Mount{}, DroppedBinds: []protocol.Mount{}, DroppedMounts: []protocol.Mount{}, UnsupportedMounts: []protocol.Mount{}}
		var codes []string
		if len(s.Volumes) > 0 {
			codes = append(codes, "k8s_volume")
		}
```

with

```go
	users := volumeUsers(spec)
	blocked := false
	for _, s := range spec.Services {
		row := PreflightService{Name: s.Name, Reference: s.Image, Blockers: []string{}, Mounts: []protocol.Mount{}, DroppedBinds: []protocol.Mount{}, DroppedMounts: []protocol.Mount{}, UnsupportedMounts: []protocol.Mount{}}
		codes, details := kubernetesVolumeCodes(s, spec.Kubernetes, users)
```

replace

```go
		if len(codes) > 0 {
			row.Blockers, row.Unsupported = append(row.Blockers, "kubernetes_unsupported"), codes
		}
```

with

```go
		if len(codes) > 0 {
			row.Blockers, row.Unsupported, row.Details = append(row.Blockers, "kubernetes_unsupported"), codes, details
		}
```

and insert before `// resolveMounts turns a service's volumes into runtime mounts`:

```go
// volumeUsers counts, per named volume, the services that mount it.
func volumeUsers(spec ApplicationSpec) map[string]int {
	users := map[string]int{}
	for _, s := range spec.Services {
		seen := map[string]bool{}
		for _, v := range s.Volumes {
			if v.Kind == "named" && !seen[v.Source] {
				seen[v.Source], users[v.Source] = true, users[v.Source]+1
			}
		}
	}
	return users
}

// kubernetesVolumeCodes says why a service's volumes cannot become claims: k8s_volume for a bind
// or more named mounts than a service may carry, k8s_volume with detail choice_required for a
// named volume with no storage choice, and k8s_volume_shared for one another service mounts too
// (a ReadWriteOnce claim serves one pod).
func kubernetesVolumeCodes(s ApplicationService, k *KubernetesExtension, users map[string]int) ([]string, map[string]string) {
	var bind, unchosen, shared bool
	mounts := 0
	for _, v := range s.Volumes {
		if v.Kind != "named" {
			bind = true
			continue
		}
		mounts++
		_, chosen := k.volume(v.Source)
		switch {
		case users[v.Source] > 1:
			shared = true
		case !chosen:
			unchosen = true
		}
	}
	var codes []string
	var details map[string]string
	switch {
	case bind || mounts > protocol.MaxKubernetesMounts:
		codes = append(codes, "k8s_volume")
	case unchosen:
		codes, details = append(codes, "k8s_volume"), map[string]string{"k8s_volume": "choice_required"}
	}
	if shared {
		codes = append(codes, "k8s_volume_shared")
	}
	return codes, details
}

```

- [ ] **Step 4: Implement claims in the plan and the frame**

In `internal/store/application_spec.go`, insert before `// Valid checks one choice's grammar`:

```go
// volume is the choice for a declared volume, on a nil extension too.
func (k *KubernetesExtension) volume(name string) (KubernetesVolume, bool) {
	if k == nil {
		return KubernetesVolume{}, false
	}
	v, ok := k.Volumes[name]
	return v, ok
}

// kubernetesClaims turns spec's chosen named volumes into the claims <project>-<volume> (the
// shared naming rule over the declared volumes) and each service's mounts of them, in mount
// order. A volume with no choice, or one two services mount, gets no claim: the preflight
// refused it already.
func kubernetesClaims(project string, spec ApplicationSpec) ([]protocol.KubernetesClaim, map[string][]protocol.KubernetesMount) {
	declared := make([]string, 0, len(spec.Volumes))
	for _, v := range spec.Volumes {
		declared = append(declared, v.Name)
	}
	names := protocol.KubernetesNames(project, declared)
	users := volumeUsers(spec)
	claims, mounts := []protocol.KubernetesClaim{}, map[string][]protocol.KubernetesMount{}
	for _, s := range spec.Services {
		for _, v := range s.Volumes {
			choice, ok := spec.Kubernetes.volume(v.Source)
			if v.Kind != "named" || !ok || users[v.Source] != 1 {
				continue
			}
			claim := names[v.Source]
			if !slices.ContainsFunc(claims, func(c protocol.KubernetesClaim) bool { return c.Name == claim }) {
				claims = append(claims, protocol.KubernetesClaim{Name: claim, StorageClass: choice.StorageClass, Size: choice.Size, AccessMode: choice.AccessMode})
			}
			mounts[s.Name] = append(mounts[s.Name], protocol.KubernetesMount{Claim: claim, MountPath: v.Target, ReadOnly: v.ReadOnly})
		}
	}
	return claims, mounts
}

```

In `internal/store/application_deployment.go`, replace

```go
	// Object is the Deployment (and Service) a Kubernetes plan applies for this service; nil on
	// Docker. Replaces, ImageID and Mounts are then empty and the service always pulls.
	Object *KubernetesObject `json:"object,omitempty"`
}
```

with

```go
	// Object is the Deployment (and Service) a Kubernetes plan applies for this service; nil on
	// Docker. Replaces, ImageID and Mounts are then empty and the service always pulls.
	Object *KubernetesObject `json:"object,omitempty"`
	// ClaimMounts mount the plan's claims into a Kubernetes service.
	ClaimMounts []protocol.KubernetesMount `json:"claim_mounts,omitempty"`
}
```

replace

```go
	// Namespace is set exactly for a Kubernetes plan or removal.
	Namespace string `json:"namespace,omitempty"`
}
```

with

```go
	// Namespace is set exactly for a Kubernetes plan or removal.
	Namespace string `json:"namespace,omitempty"`
	// Claims are the PersistentVolumeClaims a Kubernetes apply ensures.
	Claims []protocol.KubernetesClaim `json:"claims,omitempty"`
}
```

replace

```go
type BlockedService struct {
	Name        string   `json:"name"`
	Blockers    []string `json:"blockers"`
	Unsupported []string `json:"unsupported,omitempty"`
}
```

with

```go
type BlockedService struct {
	Name        string            `json:"name"`
	Blockers    []string          `json:"blockers"`
	Unsupported []string          `json:"unsupported,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
}
```

and in `draftPlan` replace

```go
			refused = append(refused, BlockedService{Name: row.Name, Blockers: row.Blockers, Unsupported: row.Unsupported})
```

with

```go
			refused = append(refused, BlockedService{Name: row.Name, Blockers: row.Blockers, Unsupported: row.Unsupported, Details: row.Details})
```

replace

```go
	var objects map[string]string
	if m.Runtime == protocol.RuntimeKubernetes {
		plan.Namespace = m.Namespace
		objects = protocol.KubernetesNames(m.Preview.Project, spec.serviceNames())
	}
```

with

```go
	var objects map[string]string
	var mounts map[string][]protocol.KubernetesMount
	if m.Runtime == protocol.RuntimeKubernetes {
		plan.Namespace = m.Namespace
		objects = protocol.KubernetesNames(m.Preview.Project, spec.serviceNames())
		plan.Claims, mounts = kubernetesClaims(m.Preview.Project, spec)
	}
```

and replace

```go
		if plan.Namespace != "" {
			ps.Object = &KubernetesObject{Namespace: plan.Namespace, Name: objects[s.Name]}
		}
```

with

```go
		if plan.Namespace != "" {
			ps.Object, ps.ClaimMounts = &KubernetesObject{Namespace: plan.Namespace, Name: objects[s.Name]}, mounts[s.Name]
		}
```

In `internal/store/application_apply.go`, `kubernetesFrame`, replace

```go
		Kubernetes: &protocol.KubernetesTarget{Namespace: namespace, ApplicationID: d.ApplicationID, InstanceID: d.InstanceID, SpecDigest: d.SpecDigest}}
```

with

```go
		Kubernetes: &protocol.KubernetesTarget{Namespace: namespace, ApplicationID: d.ApplicationID, InstanceID: d.InstanceID, SpecDigest: d.SpecDigest, Claims: d.Plan.Claims}}
```

replace

```go
		svc := protocol.DeploymentService{Name: ps.Name, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}, Pull: &protocol.ImagePull{Reference: ps.PullReference, Digest: ps.PullDigest}}
```

with

```go
		svc := protocol.DeploymentService{Name: ps.Name, Restart: ps.Restart, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}, Pull: &protocol.ImagePull{Reference: ps.PullReference, Digest: ps.PullDigest}, Volumes: ps.ClaimMounts}
```

and in `settleRemoval` replace

```go
	// A cluster removal reports precondition and remove steps per service it found labelled;
	// only a success releases the instance.
	if plan.Namespace != "" {
		for _, s := range res.Steps {
			if s.Step != protocol.StepPrecondition && s.Step != protocol.StepRemove {
```

with

```go
	// A cluster removal reports precondition and remove steps per service it found labelled, and
	// a skipped volume step per claim it kept; only a success releases the instance.
	if plan.Namespace != "" {
		for _, s := range res.Steps {
			retained := s.Step == protocol.StepVolume && s.Outcome == protocol.OutcomeSkipped && s.Detail == protocol.DetailRetained
			if s.Step != protocol.StepPrecondition && s.Step != protocol.StepRemove && !retained {
```

- [ ] **Step 5: Run the tests to verify they pass, on both databases**

Run: `gofmt -w internal/store && go vet ./... && go test -race -count=1 ./internal/store/... && PG=… go test -count=1 ./internal/store/... && go test -count=1 ./internal/api/`
Expected: PASS. `TestKubernetesPlanBlockers` keeps its `k8s_volume` for a named volume with no choice; `internal/api` still passes `TestKubernetesPlanBlockersOverTheAPI` unchanged.

- [ ] **Step 6: DOX and commit**

In `internal/store/AGENTS.md`, in the `- Kubernetes plans` bullet, replace

```markdown
per service `kubernetes_unsupported` with `Unsupported` codes `k8s_volume`, 
```

with

```markdown
per service `kubernetes_unsupported` with `Unsupported` codes `k8s_volume` (a bind, more than 8 named mounts, or, with `Details` `{"k8s_volume": "choice_required"}`, a named volume without a `kubernetes.volumes` choice), `k8s_volume_shared` (a named volume two services mount), 
```

and append to the same bullet:

```markdown
 A chosen named volume one service mounts plans as a claim (`DeploymentPlan.Claims`, `kubernetesClaims`: name `<project>-<volume>` from `protocol.KubernetesNames` over the declared volumes, the choice's class, size and access mode) and a mount (`PlannedService.ClaimMounts`: claim, target, read-only); `kubernetesFrame` sends them as `KubernetesTarget.Claims` and `DeploymentService.Volumes`. A cluster `settleRemoval` also takes skipped `volume` steps with detail `retained` (a kept claim) and nothing else new.
```

```bash
git add internal/store && make tidy-check lint && git commit -m "feat(store): chosen named volumes plan as claims on a cluster" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 5: The cluster agent renders, creates and retains claims and reports StorageClasses

**Files:**
- Modify: `internal/runtime/kubernetes/render/render.go` (`Set.Claims`, pod volumes and mounts, `claim`)
- Modify: `internal/runtime/kubernetes/deploy.go` (claim precondition and create)
- Modify: `internal/runtime/kubernetes/remove.go` (claims kept and reported)
- Modify: `internal/runtime/kubernetes/kubernetes.go` (`Snapshot` lists StorageClasses)
- Create: `internal/runtime/kubernetes/claims_test.go`
- Modify test: `internal/runtime/kubernetes/render/render_test.go`
- Docs: `internal/runtime/kubernetes/AGENTS.md`

**Interfaces:**
- Consumes: `protocol.KubernetesClaim`, `protocol.KubernetesMount`, `protocol.StorageClass`, `protocol.MaxStorageClasses`, `protocol.DetailRetained`, `KubernetesTarget.Claims`, `DeploymentService.Volumes`, the `claim_immutable` step code (Task 1); test helpers `deployRequest`, `deployCluster`, `writes`, `owned`, `cluster`, `testInstance`, `testApp`, `testSpec` (package `kubernetes`), `request`, `web`, `api` (package `render_test`).
- Produces:
  ```go
  // package render: Set gains Claims []*corev1.PersistentVolumeClaim (the claims the service mounts).
  // package kubernetes: Deploy refuses name_taken / claim_immutable (detail PersistentVolumeClaim/<name>)
  //   at the precondition and creates missing claims first in create; Remove never deletes a claim
  //   and reports each as {Service, StepVolume, OutcomeSkipped, DetailRetained};
  //   Snapshot fills KubernetesInventory.StorageClasses (sorted by name, cut named storage_classes).
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/runtime/kubernetes/claims_test.go`:

```go
package kubernetes

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// claimRequest is deployRequest with web mounting claim shop-data of class (none for "").
func claimRequest(class string) protocol.DeploymentRequest {
	req := deployRequest(time.Minute)
	req.Kubernetes.Claims = []protocol.KubernetesClaim{{Name: "shop-data", StorageClass: class, Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}}
	req.Services[0].Volumes = []protocol.KubernetesMount{{Claim: "shop-data", MountPath: "/data"}}
	return req
}

// ownedClaim is a claim of this instance under web, of class and size.
func ownedClaim(class *string, size string) *corev1.PersistentVolumeClaim {
	m := owned("web")
	m.Name = "shop-data"
	return &corev1.PersistentVolumeClaim{ObjectMeta: m, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: class, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
}

func ptr[T any](v T) *T { return &v }

// A first apply creates the claim before the service's other objects and mounts it; a second
// apply finds it owned and unchanged and writes nothing to it.
func TestDeployCreatesAClaimOnce(t *testing.T) {
	c, cs := deployCluster(t, true, false)
	res := c.Deploy(context.Background(), claimRequest("fast"), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("result %+v", res)
	}
	if !slices.Equal(writes(cs), []string{"create persistentvolumeclaims", "create configmaps", "create secrets", "create deployments", "create services", "create configmaps", "create deployments"}) {
		t.Fatalf("writes %v", writes(cs))
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims("shop").Get(context.Background(), "shop-data", metav1.GetOptions{})
	if err != nil || *pvc.Spec.StorageClassName != "fast" || pvc.Labels[render.LabelInstance] != testInstance || pvc.Labels[render.LabelService] != "web" || pvc.Spec.Resources.Requests.Storage().String() != "10Gi" {
		t.Fatalf("claim %+v %v", pvc, err)
	}
	d, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "shop-web", metav1.GetOptions{})
	if v := d.Spec.Template.Spec.Volumes; len(v) != 1 || v[0].PersistentVolumeClaim.ClaimName != "shop-data" || d.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != "/data" {
		t.Fatalf("pod %+v", d.Spec.Template.Spec)
	}
	cs.ClearActions()
	again := claimRequest("fast")
	again.Deployment = "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b"
	if res := c.Deploy(context.Background(), again, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("again %+v", res)
	}
	if slices.ContainsFunc(writes(cs), func(w string) bool {
		return w == "create persistentvolumeclaims" || w == "update persistentvolumeclaims"
	}) {
		t.Fatalf("a second apply wrote the claim: %v", writes(cs))
	}
}

// A claim planned with the cluster default keeps whatever class the cluster assigned: the next
// apply is not claim_immutable.
func TestDeployClaimDefaultClassIsStable(t *testing.T) {
	c, cs := deployCluster(t, true, false, ownedClaim(ptr("standard"), "10Gi"))
	res := c.Deploy(context.Background(), claimRequest(""), func() {})
	if res.Outcome != protocol.OutcomeSucceeded || slices.Contains(writes(cs), "create persistentvolumeclaims") {
		t.Fatalf("result %+v writes %v", res, writes(cs))
	}
}

// A claim someone else holds under the planned name, or this instance's claim of another class,
// size or access mode, stops the run at the precondition with nothing written.
func TestDeployClaimPreconditions(t *testing.T) {
	foreign := ownedClaim(ptr("fast"), "10Gi")
	foreign.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	many := ownedClaim(ptr("fast"), "10Gi")
	many.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	for name, tc := range map[string]struct {
		have *corev1.PersistentVolumeClaim
		code string
	}{
		"another owner":  {foreign, "name_taken"},
		"another size":   {ownedClaim(ptr("fast"), "20Gi"), "claim_immutable"},
		"another class":  {ownedClaim(ptr("slow"), "10Gi"), "claim_immutable"},
		"no class":       {ownedClaim(nil, "10Gi"), "claim_immutable"},
		"another access": {many, "claim_immutable"},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, false, tc.have)
			started := false
			res := c.Deploy(context.Background(), claimRequest("fast"), func() { started = true })
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			if res.Outcome != protocol.OutcomeDenied || i < 0 || res.Validate() != nil {
				t.Fatalf("result %+v", res)
			}
			if s := res.Steps[i]; s.Service != "web" || s.Step != protocol.StepPrecondition || s.Code != tc.code || s.Detail != "PersistentVolumeClaim/shop-data" {
				t.Fatalf("step %+v", s)
			}
			if started || len(writes(cs)) != 0 {
				t.Fatalf("wrote %v", writes(cs))
			}
		})
	}
}

// A claim that appears between the precondition and the create is taken when it is this
// instance's and as planned, and refused otherwise.
func TestDeployClaimRace(t *testing.T) {
	for name, tc := range map[string]struct {
		appears *corev1.PersistentVolumeClaim
		code    string
	}{
		"as planned":   {ownedClaim(ptr("fast"), "10Gi"), ""},
		"another size": {ownedClaim(ptr("fast"), "1Gi"), "claim_immutable"},
	} {
		t.Run(name, func(t *testing.T) {
			c, cs := deployCluster(t, true, false)
			cs.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				if err := cs.Tracker().Add(tc.appears); err != nil {
					t.Fatal(err)
				}
				return false, nil, nil
			})
			res := c.Deploy(context.Background(), claimRequest("fast"), func() {})
			i := slices.IndexFunc(res.Steps, func(s protocol.DeploymentStep) bool { return s.Code != "" })
			switch {
			case tc.code == "" && res.Outcome != protocol.OutcomeSucceeded:
				t.Fatalf("result %+v", res)
			case tc.code != "" && (i < 0 || res.Steps[i].Step != protocol.StepCreate || res.Steps[i].Code != tc.code || res.Validate() != nil):
				t.Fatalf("result %+v", res)
			}
		})
	}
}

// Removal keeps every claim of the instance, deletes none, and reports each as a skipped
// volume step with detail retained under its service; another's claim is not even reported.
func TestRemoveRetainsClaims(t *testing.T) {
	foreign := ownedClaim(ptr("fast"), "1Gi")
	foreign.Name, foreign.Labels = "shop-other", map[string]string{render.LabelInstance: "99999999-7777-4888-9999-aaaaaaaaaaaa", render.LabelService: "web"}
	d := &appsv1.Deployment{ObjectMeta: owned("web")}
	d.Name = "shop-web"
	c, cs := deployCluster(t, true, false, d, ownedClaim(ptr("fast"), "10Gi"), foreign)
	now := time.Now()
	req := protocol.RemovalRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_1", Project: "shop", IssuedAt: now, Deadline: now.Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: "shop", ApplicationID: testApp, InstanceID: testInstance, SpecDigest: testSpec}, Services: []string{"web"}}
	res := c.Remove(context.Background(), req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("result %+v %v", res, res.Validate())
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Service+" "+s.Step+" "+s.Outcome+" "+s.Detail)
	}
	if !slices.Equal(steps, []string{"web precondition succeeded ", "web remove succeeded ", "web volume skipped retained"}) {
		t.Fatalf("steps %q", steps)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "persistentvolumeclaims" {
			t.Fatal("a claim was deleted")
		}
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("shop").Get(context.Background(), "shop-data", metav1.GetOptions{}); err != nil {
		t.Fatalf("the claim is gone: %v", err)
	}
}

// The snapshot reports StorageClasses by name, the default one marked by either annotation.
func TestSnapshotReadsStorageClasses(t *testing.T) {
	c, _ := cluster(t,
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "rancher.io/local-path"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Annotations: map[string]string{"storageclass.beta.kubernetes.io/is-default-class": "true"}}, Provisioner: "x"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "false"}}, Provisioner: "x"},
	)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.StorageClass{{Name: "fast"}, {Name: "legacy", Default: true}, {Name: "standard", Default: true}}
	if !slices.Equal(snap.Kubernetes.StorageClasses, want) || len(snap.Truncated) != 0 {
		t.Fatalf("classes %+v truncated %v", snap.Kubernetes.StorageClasses, snap.Truncated)
	}
}
```

In `internal/runtime/kubernetes/render/render_test.go`, add `"k8s.io/apimachinery/pkg/api/resource"` to the imports (after `corev1 "k8s.io/api/core/v1"`) and append:

```go
// A service mounting claims renders one ReadWriteOnce claim per name, labelled like its other
// objects, in its StorageClass or none, mounted as a pod volume named after the claim.
func TestRenderClaims(t *testing.T) {
	db := api()
	db.Name = "db"
	db.Volumes = []protocol.KubernetesMount{{Claim: "shop-data", MountPath: "/var/lib/db"}, {Claim: "shop-data", MountPath: "/backup", ReadOnly: true}, {Claim: "shop-logs", MountPath: "/var/log/db"}}
	req := request("shop", db, web())
	req.Kubernetes.Claims = []protocol.KubernetesClaim{{Name: "shop-data", StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}, {Name: "shop-logs", Size: "512Mi", AccessMode: protocol.AccessReadWriteOnce}}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		t.Fatal(err)
	}
	sets := render.Request(req)
	fast := "fast"
	want := []*corev1.PersistentVolumeClaim{
		{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: sets[0].ConfigMap.ObjectMeta, Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: &fast,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}}},
		{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: sets[0].ConfigMap.ObjectMeta, Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("512Mi")}}}},
	}
	want[0].ObjectMeta.Name, want[1].ObjectMeta.Name = "shop-data", "shop-logs"
	if !reflect.DeepEqual(sets[0].Claims, want) || len(sets[1].Claims) != 0 {
		got, _ := json.MarshalIndent(sets[0].Claims, "", "  ")
		t.Fatalf("claims:\n%s", got)
	}
	pod := sets[0].Deployment.Spec.Template.Spec
	volumes := []corev1.Volume{
		{Name: "shop-data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shop-data"}}},
		{Name: "shop-logs", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shop-logs"}}},
	}
	mounts := []corev1.VolumeMount{{Name: "shop-data", MountPath: "/var/lib/db"}, {Name: "shop-data", MountPath: "/backup", ReadOnly: true}, {Name: "shop-logs", MountPath: "/var/log/db"}}
	if !reflect.DeepEqual(pod.Volumes, volumes) || !reflect.DeepEqual(pod.Containers[0].VolumeMounts, mounts) || len(sets[1].Deployment.Spec.Template.Spec.Volumes) != 0 {
		t.Fatalf("pod %+v %+v", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
	decoder := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, obj := range []any{sets[0].Claims[0], sets[0].Claims[1], sets[0].Deployment} {
		raw, _ := json.Marshal(obj)
		if _, _, err := decoder.Decode(raw, nil, nil); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runtime/kubernetes/...`
Expected: FAIL to compile: `sets[0].Claims undefined (type render.Set has no field or method Claims)`; with that fixed, `TestDeployCreatesAClaimOnce` fails on its writes and `TestSnapshotReadsStorageClasses` on an empty list.

- [ ] **Step 3: Render claims**

In `internal/runtime/kubernetes/render/render.go`, add `"k8s.io/apimachinery/pkg/api/resource"` to the imports (after `corev1 "k8s.io/api/core/v1"`), replace

```go
// Set is one service's objects. Secret is nil when the service has no secret-backed value, and
// Service is nil when it publishes no port.
type Set struct {
	Service    string
	Name       string
	ConfigMap  *corev1.ConfigMap
	Secret     *corev1.Secret
	Deployment *appsv1.Deployment
	Endpoint   *corev1.Service
}
```

with

```go
// Set is one service's objects. Secret is nil when the service has no secret-backed value, and
// Service is nil when it publishes no port. Claims are the PersistentVolumeClaims it mounts,
// each mounted by this service alone.
type Set struct {
	Service    string
	Name       string
	ConfigMap  *corev1.ConfigMap
	Secret     *corev1.Secret
	Deployment *appsv1.Deployment
	Endpoint   *corev1.Service
	Claims     []*corev1.PersistentVolumeClaim
}
```

in `service` replace

```go
	if len(servicePorts) > 0 {
		set.Endpoint = &corev1.Service{
```

with

```go
	for _, m := range s.Volumes {
		spec := &set.Deployment.Spec.Template.Spec
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: m.Claim, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
		if slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == m.Claim }) {
			continue
		}
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: m.Claim, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.Claim}}})
		i := slices.IndexFunc(k.Claims, func(c protocol.KubernetesClaim) bool { return c.Name == m.Claim })
		set.Claims = append(set.Claims, claim(k.Claims[i], meta(m.Claim)))
	}
	if len(servicePorts) > 0 {
		set.Endpoint = &corev1.Service{
```

and append to the file:

```go
// claim is a ReadWriteOnce PersistentVolumeClaim of the requested size, in the named StorageClass
// or, with none, the cluster's default.
func claim(c protocol.KubernetesClaim, meta metav1.ObjectMeta) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: meta, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(c.Size)}},
	}}
	if c.StorageClass != "" {
		class := c.StorageClass
		pvc.Spec.StorageClassName = &class
	}
	return pvc
}
```

- [ ] **Step 4: Check and create claims in `Deploy`**

In `internal/runtime/kubernetes/deploy.go`, in the `Deploy` doc comment replace

```go
// when one exists without this instance's label. Then per service create (ConfigMap, Secret,
```

with

```go
// when one exists without this instance's label, and each claim, refused claim_immutable when
// this instance's differs from the plan. Then per service create (missing claims, ConfigMap, Secret,
```

replace the `existing` type

```go
// existing is what a precondition found under a service's planned names: nil where nothing is.
type existing struct {
	configMap  *corev1.ConfigMap
	secret     *corev1.Secret
	deployment *appsv1.Deployment
	service    *corev1.Service
}
```

with

```go
// existing is what a precondition found under a service's planned names: nil where nothing is.
// claims names the service's claims that already exist, owned and as planned.
type existing struct {
	configMap  *corev1.ConfigMap
	secret     *corev1.Secret
	deployment *appsv1.Deployment
	service    *corev1.Service
	claims     map[string]bool
}

// claimImmutable refuses a claim the apply would have to change: KyYard never updates a claim.
func claimImmutable(name string) (string, string, string) {
	return protocol.OutcomeDenied, "claim_immutable", "PersistentVolumeClaim/" + name
}

// claimDiffers reports an existing claim that is not the planned one: another access mode or
// size, or, when the plan names one, another StorageClass. A claim planned with the cluster
// default takes whatever class the cluster gave it, so a second apply finds it unchanged.
func claimDiffers(have, want *corev1.PersistentVolumeClaim) bool {
	if want.Spec.StorageClassName != nil && (have.Spec.StorageClassName == nil || *have.Spec.StorageClassName != *want.Spec.StorageClassName) {
		return true
	}
	h, w := have.Spec.Resources.Requests[corev1.ResourceStorage], want.Spec.Resources.Requests[corev1.ResourceStorage]
	return h.Cmp(w) != 0 || !slices.Equal(have.Spec.AccessModes, want.Spec.AccessModes)
}

// checkClaim is a claim's precondition: absent, or this instance's and as planned.
func (r *run) checkClaim(ctx context.Context, want *corev1.PersistentVolumeClaim) (bool, string, string, string) {
	have, found, err := get(ctx, objectAPI[*corev1.PersistentVolumeClaim](r.c.cs.CoreV1().PersistentVolumeClaims(r.namespace)), want.Name)
	switch {
	case err != nil:
		o, code, detail := r.failure(ctx, err)
		return false, o, code, detail
	case !found:
		o, code, detail := succeeded()
		return false, o, code, detail
	case !r.owned(have):
		o, code, detail := nameTaken("PersistentVolumeClaim", want.Name)
		return true, o, code, detail
	case claimDiffers(have, want):
		o, code, detail := claimImmutable(want.Name)
		return true, o, code, detail
	}
	o, code, detail := succeeded()
	return true, o, code, detail
}

```

replace

```go
// read is a service's precondition: each planned object, owned or absent.
func (r *run) read(ctx context.Context, set render.Set) (existing, string, string, string) {
	var e existing
```

with

```go
// read is a service's precondition: each planned object, owned or absent, and each claim absent
// or owned and as planned.
func (r *run) read(ctx context.Context, set render.Set) (existing, string, string, string) {
	e := existing{claims: map[string]bool{}}
	for _, want := range set.Claims {
		found, o, code, detail := r.checkClaim(ctx, want)
		if o != protocol.OutcomeSucceeded {
			return e, o, code, detail
		}
		e.claims[want.Name] = found
	}
```

and replace

```go
// apply writes a service's objects in order and returns the Deployment as written.
func (r *run) apply(ctx context.Context, set render.Set, e existing) (*appsv1.Deployment, string, string, string) {
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
```

with

```go
// apply writes a service's objects in order, its missing claims first, and returns the
// Deployment as written. A claim is created, never updated: one that appeared since the
// precondition must be this instance's and as planned.
func (r *run) apply(ctx context.Context, set render.Set, e existing) (*appsv1.Deployment, string, string, string) {
	core, apps := r.c.cs.CoreV1(), r.c.cs.AppsV1()
	for _, want := range set.Claims {
		if e.claims[want.Name] {
			continue
		}
		_, err := core.PersistentVolumeClaims(r.namespace).Create(ctx, want, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			if _, o, code, detail := r.checkClaim(ctx, want); o != protocol.OutcomeSucceeded {
				return nil, o, code, detail
			}
			continue
		}
		if err != nil {
			o, code, detail := r.refusedWrite(ctx, err, "PersistentVolumeClaim", want.Name)
			return nil, o, code, detail
		}
	}
```

- [ ] **Step 5: Keep claims in `Remove`, list StorageClasses in `Snapshot`**

In `internal/runtime/kubernetes/remove.go`, add `"maps"` to the imports (after `"context"`) and replace the whole `Remove` function, from `// Remove deletes the instance's objects` down to its closing brace, with:

```go
// Remove deletes the instance's objects: the Deployments, Services and ConfigMaps in the
// namespace carrying its instance label, and each named service's Secret when it carries the
// label too. An object without it is never touched. The first service's precondition does the
// reads; each service then has a remove step, skipped when nothing of it is left. Deletes are
// foreground, so a Deployment's pods go with it. The instance's claims are kept: each is a
// skipped volume step, detail retained, under the service that mounts it.
func (c *Client) Remove(parent context.Context, req protocol.RemovalRequest, started func()) protocol.DeploymentResult {
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Steps: []protocol.DeploymentStep{}, Services: []protocol.DeploymentIdentity{}}
	if err := req.ValidateFor(protocol.RuntimeKubernetes, time.Now()); err != nil {
		return refused(res, err)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	defer cancel()
	r := &run{c: c, parent: parent, res: res, started: started, namespace: req.Kubernetes.Namespace, instance: req.Kubernetes.InstanceID}
	found, kept := map[string][]doomed{}, map[string][]string{}
	services := slices.Clone(req.Services)
	r.step(services[0], protocol.StepPrecondition, func() (string, string, string) {
		o, code, detail := r.find(ctx, req, found, kept)
		for _, s := range slices.Concat(slices.Collect(maps.Keys(found)), slices.Collect(maps.Keys(kept))) {
			if !slices.Contains(services, s) {
				services = append(services, s)
			}
		}
		// services[0] already ran this precondition step; only the services appended after it
		// (named or found by label) are sorted, so the step order stays stable across runs.
		slices.Sort(services[1:])
		return o, code, detail
	})
	for i, s := range services {
		if i > 0 {
			r.step(s, protocol.StepPrecondition, succeeded)
		}
		if len(found[s]) == 0 {
			r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: s, Step: protocol.StepRemove, Outcome: protocol.OutcomeSkipped})
		} else {
			r.step(s, protocol.StepRemove, func() (string, string, string) { return r.remove(ctx, found[s]) })
		}
		// A claim is never deleted: the data in it is the operator's to delete deliberately.
		for range kept[s] {
			r.res.Steps = append(r.res.Steps, protocol.DeploymentStep{Service: s, Step: protocol.StepVolume, Outcome: protocol.OutcomeSkipped, Detail: protocol.DetailRetained})
		}
	}
	if r.res.Outcome == "" {
		r.res.Outcome = protocol.OutcomeSucceeded
	}
	return r.res
}

// remove deletes one service's objects in the foreground, each guarded by the UID it was read at.
func (r *run) remove(ctx context.Context, objects []doomed) (string, string, string) {
	r.begin()
	foreground := metav1.DeletePropagationForeground
	for _, d := range objects {
		uid := d.uid
		err := d.remove(ctx, d.name, metav1.DeleteOptions{PropagationPolicy: &foreground, Preconditions: &metav1.Preconditions{UID: &uid}})
		switch {
		case err == nil, apierrors.IsNotFound(err):
		case apierrors.IsConflict(err):
			// The UID precondition failed: something else now holds this name. The object this
			// removal found is already gone from the cluster's perspective, so this is treated
			// like NotFound rather than a failure.
		default:
			return r.failure(ctx, err)
		}
	}
	return succeeded()
}

```

replace

```go
func (r *run) find(ctx context.Context, req protocol.RemovalRequest, found map[string][]doomed) (string, string, string) {
```

with

```go
func (r *run) find(ctx context.Context, req protocol.RemovalRequest, found map[string][]doomed, kept map[string][]string) (string, string, string) {
```

and replace

```go
	// Fallback for a named service whose Deployment is already gone: the name this removal
	// request would itself compute.
```

with

```go
	// Claims are listed only to be reported kept, under the service that mounts them, at most
	// one volume step per possible volume.
	claims, err := core.PersistentVolumeClaims(r.namespace).List(ctx, selector)
	if err != nil {
		return r.failure(ctx, err)
	}
	for i, c := range claims.Items {
		if s := c.Labels[render.LabelService]; protocol.ValidServiceName(s) && i < protocol.MaxDeploymentVolumes {
			kept[s] = append(kept[s], c.Name)
		}
	}
	// Fallback for a named service whose Deployment is already gone: the name this removal
	// request would itself compute.
```

In `internal/runtime/kubernetes/kubernetes.go`, add `storagev1 "k8s.io/api/storage/v1"` to the imports (after `corev1 "k8s.io/api/core/v1"`), replace

```go
	k := &protocol.KubernetesInventory{Nodes: []protocol.Node{}, Namespaces: []string{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}}
```

with

```go
	k := &protocol.KubernetesInventory{Nodes: []protocol.Node{}, Namespaces: []string{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}, StorageClasses: []protocol.StorageClass{}}
```

replace

```go
		{"claims", reader(ctx, protocol.MaxClaims, c.claims, claim, &k.Claims)},
	}
```

with

```go
		{"claims", reader(ctx, protocol.MaxClaims, c.claims, claim, &k.Claims)},
		{"storage_classes", reader(ctx, protocol.MaxStorageClasses, c.storageClasses, storageClass, &k.StorageClasses)},
	}
```

replace

```go
	for list := range truncated {
```

with

```go
	slices.SortFunc(k.StorageClasses, func(a, b protocol.StorageClass) int { return strings.Compare(a.Name, b.Name) })
	for list := range truncated {
```

and insert before `func namespace(ns corev1.Namespace) string { return ns.Name }`:

```go
func (c *Client) storageClasses(ctx context.Context, o metav1.ListOptions) ([]storagev1.StorageClass, string, error) {
	l, err := c.cs.StorageV1().StorageClasses().List(ctx, o)
	if err != nil {
		return nil, "", err
	}
	return l.Items, l.Continue, nil
}

// storageClass is default when either the current or the beta is-default-class annotation is
// "true", as the DefaultStorageClass admission plugin reads it.
func storageClass(sc storagev1.StorageClass) protocol.StorageClass {
	return protocol.StorageClass{Name: sc.Name, Default: sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" || sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"}
}

```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `gofmt -w internal/runtime && go vet ./... && go test -race -count=1 ./internal/runtime/... ./internal/agent/... && test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0`
Expected: PASS, the existing deploy, remove, render and snapshot tests included (a service without claims renders exactly as before), and the server still links no client-go.

- [ ] **Step 7: DOX and commit**

In `internal/runtime/kubernetes/AGENTS.md`, `## Local Contracts`, in the first bullet replace

```markdown
services and persistentvolumeclaims
```

with

```markdown
services, persistentvolumeclaims and storageclasses (`StorageClasses`: name, default when the `storageclass.kubernetes.io/is-default-class` or its beta annotation is `true`, sorted by name)
```

and append after the `- Remove validates` bullet:

```markdown
- Claims: `render.Request` gives a service mounting claims one `PersistentVolumeClaim` per claim in `Set.Claims` (labels and annotations like its other objects, `ReadWriteOnce`, the requested size, `storageClassName` only when the plan names one) and a pod volume named after the claim with the service's mounts. `Deploy`'s precondition reads each claim: another owner's is `name_taken` and this instance's with another class (when the plan names one), size or access mode is `claim_immutable`, both with detail `PersistentVolumeClaim/<name>` and before any write; `create` creates a missing claim before the ConfigMap and never updates one (one that appeared since must pass the same check). `Remove` never deletes a claim: it lists the instance's claims by label and reports each as a skipped `volume` step with detail `retained` under the service label that mounts it (at most `MaxDeploymentVolumes` steps).
```

```bash
git add internal/runtime/kubernetes && make tidy-check lint && git commit -m "feat(k8s): claims rendered, created once and retained; StorageClasses in the snapshot" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 6: Claim and StorageClass RBAC, the manifest note, and the real-cluster claim

**Files:**
- Modify: `internal/runtime/kubernetes/manifest/manifest.go` (Role and ClusterRole rules, header)
- Modify: `internal/runtime/kubernetes/manifest/manifest_test.go`
- Modify: `internal/runtime/kubernetes/cluster_test.go` (access reviews, a claim of the default class, a second apply, the claim kept)
- Modify: `internal/api/endpoint_handlers.go` (`manifestNote`)
- Docs: `internal/runtime/kubernetes/AGENTS.md`

**Interfaces:**
- Consumes: Task 5's claim-aware `Deploy`, `Remove` and `Snapshot`; Task 1's `KubernetesClaim`, `KubernetesMount`, `DetailRetained`, `StorageClass`.
- Produces: the deploy Role `kyyard-agent-deploy` gains `{"" persistentvolumeclaims [get list create]}`; the ClusterRole `kyyard-agent-read` gains `{storage.k8s.io storageclasses [get list]}`. `manifestNote` tells the operator to re-apply after an upgrade.

- [ ] **Step 1: Write the failing tests**

In `internal/runtime/kubernetes/manifest/manifest_test.go`, `TestManifestRBACIsReadOnlyWithoutSecrets`, replace

```go
	want := []string{"/events", "/namespaces", "/nodes", "/persistentvolumeclaims", "/pods", "/pods/log", "/services", "apps/daemonsets", "apps/deployments", "apps/statefulsets"}
```

with

```go
	want := []string{"/events", "/namespaces", "/nodes", "/persistentvolumeclaims", "/pods", "/pods/log", "/services", "apps/daemonsets", "apps/deployments", "apps/statefulsets", "storage.k8s.io/storageclasses"}
```

and in `TestManifestGrantsDeployInListedNamespaces` replace its doc comment and rule list

```go
// Each listed namespace gets the deploy Role, exactly: Deployments, Services and ConfigMaps
// read and written, Secrets written and read by name but never listed, bound to the agent.
```

with

```go
// Each listed namespace gets the deploy Role, exactly: Deployments, Services and ConfigMaps
// read and written, Secrets written and read by name but never listed, claims read and created
// but never updated or deleted, bound to the agent.
```

and

```go
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "create", "update", "patch", "delete"}},
			}
```

with

```go
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "create", "update", "patch", "delete"}},
				{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"get", "list", "create"}},
			}
```

In `internal/runtime/kubernetes/cluster_test.go`, add `"slices"` to the imports (after `"os"`), replace the doc comment of `TestManifestOnARealCluster` from `// Secret round-trips, and a snapshot names` to the end of the comment with

```go
// Secret round-trips, and a snapshot names the cluster's nodes and a default StorageClass with
// nothing forbidden. In the one granted namespace, which enforces Pod Security baseline, the agent
// may write Deployments, read Secrets by name but not list them, and create claims but neither
// update nor delete them; it deploys KY_TEST_DEPLOY_IMAGE (a digest-pinned image that keeps
// running, such as registry.k8s.io/pause@sha256:...) as one service mounting a claim of the
// default StorageClass, sees it available and the claim bound, applies it again unchanged, and
// removes it, leaving the claim.
```

replace

```go
		{"create", "apps", "deployments", "default", false},
		{"create", "", "secrets", "default", false},
	} {
```

with

```go
		{"create", "apps", "deployments", "default", false},
		{"create", "", "secrets", "default", false},
		{"create", "", "persistentvolumeclaims", deployNamespace, true},
		{"update", "", "persistentvolumeclaims", deployNamespace, false},
		{"delete", "", "persistentvolumeclaims", deployNamespace, false},
		{"create", "", "persistentvolumeclaims", "default", false},
		{"list", "storage.k8s.io", "storageclasses", "", true},
	} {
```

replace

```go
		t.Fatalf("snapshot as the agent: %v truncated %v nodes %+v", err, snap.Truncated, snap.Kubernetes.Nodes)
	}
```

with

```go
		t.Fatalf("snapshot as the agent: %v truncated %v nodes %+v", err, snap.Truncated, snap.Kubernetes.Nodes)
	}
	if !slices.ContainsFunc(snap.Kubernetes.StorageClasses, func(c protocol.StorageClass) bool { return c.Default }) {
		t.Fatalf("no default StorageClass reported: %+v", snap.Kubernetes.StorageClasses)
	}
```

replace the block from `target := &protocol.KubernetesTarget{` through the `remove as the agent` check

```go
	target := &protocol.KubernetesTarget{Namespace: deployNamespace, ApplicationID: "11111111-2222-4333-8444-555555555555", InstanceID: "66666666-7777-4888-9999-aaaaaaaaaaaa", SpecDigest: "sha256:" + strings.Repeat("f", 64)}
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", Revision: 1, IssuedAt: now, Deadline: now.Add(2 * time.Minute), Kubernetes: target,
		Services: []protocol.DeploymentService{{Name: "idle", Pull: &protocol.ImagePull{Reference: image, Digest: digest}, Ports: []protocol.Port{{Container: 8080, Host: 80, Protocol: "tcp"}}, Env: map[string]string{"TOKEN": "x"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{}}}}
	res := c.Deploy(ctx, req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("deploy as the agent: %+v", res)
	}
	d, err := cs.AppsV1().Deployments(deployNamespace).Get(ctx, "kind-idle", metav1.GetOptions{})
	if err != nil || d.Status.ReadyReplicas != 1 || string(d.UID) != res.Services[0].UID {
		t.Fatalf("deployed %+v %v", d, err)
	}
	removal := protocol.RemovalRequest{Deployment: "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", IssuedAt: time.Now(), Deadline: time.Now().Add(time.Minute), Kubernetes: target, Services: []string{"idle"}}
	if res := c.Remove(ctx, removal, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("remove as the agent: %+v", res)
	}
```

with

```go
	target := &protocol.KubernetesTarget{Namespace: deployNamespace, ApplicationID: "11111111-2222-4333-8444-555555555555", InstanceID: "66666666-7777-4888-9999-aaaaaaaaaaaa", SpecDigest: "sha256:" + strings.Repeat("f", 64),
		Claims: []protocol.KubernetesClaim{{Name: "kind-data", Size: "64Mi", AccessMode: protocol.AccessReadWriteOnce}}}
	req := protocol.DeploymentRequest{Deployment: "3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", Revision: 1, IssuedAt: now, Deadline: now.Add(2 * time.Minute), Kubernetes: target,
		Services: []protocol.DeploymentService{{Name: "idle", Pull: &protocol.ImagePull{Reference: image, Digest: digest}, Ports: []protocol.Port{{Container: 8080, Host: 80, Protocol: "tcp"}}, Env: map[string]string{"TOKEN": "x"}, SecretKeys: []string{"TOKEN"}, Mounts: []protocol.Mount{},
			Volumes: []protocol.KubernetesMount{{Claim: "kind-data", MountPath: "/data"}}}}}
	res := c.Deploy(ctx, req, func() {})
	if res.Outcome != protocol.OutcomeSucceeded || res.Validate() != nil {
		t.Fatalf("deploy as the agent: %+v", res)
	}
	d, err := cs.AppsV1().Deployments(deployNamespace).Get(ctx, "kind-idle", metav1.GetOptions{})
	if err != nil || d.Status.ReadyReplicas != 1 || string(d.UID) != res.Services[0].UID {
		t.Fatalf("deployed %+v %v", d, err)
	}
	// The pod mounted the claim, so the default StorageClass bound it and filled in its name; a
	// second apply takes the claim as it is.
	pvc, err := cs.CoreV1().PersistentVolumeClaims(deployNamespace).Get(ctx, "kind-data", metav1.GetOptions{})
	if err != nil || pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		t.Fatalf("claim %+v %v", pvc, err)
	}
	again := req
	again.Deployment, again.Revision, again.IssuedAt, again.Deadline = "5b4c3d2e-8d4a-4e6f-9a0b-1c2d3e4f5a6b", 2, time.Now(), time.Now().Add(2*time.Minute)
	if res := c.Deploy(ctx, again, func() {}); res.Outcome != protocol.OutcomeSucceeded {
		t.Fatalf("second deploy as the agent: %+v", res)
	}
	removal := protocol.RemovalRequest{Deployment: "4a3b2c1d-8d4a-4e6f-9a0b-1c2d3e4f5a6b", RequestID: "0123456789abcdef0123456789abcdef", Endpoint: "ep_kind", Project: "kind", IssuedAt: time.Now(), Deadline: time.Now().Add(time.Minute),
		Kubernetes: &protocol.KubernetesTarget{Namespace: target.Namespace, ApplicationID: target.ApplicationID, InstanceID: target.InstanceID, SpecDigest: target.SpecDigest}, Services: []string{"idle"}}
	removed := c.Remove(ctx, removal, func() {})
	if removed.Outcome != protocol.OutcomeSucceeded || !slices.Contains(removed.Steps, protocol.DeploymentStep{Service: "idle", Step: protocol.StepVolume, Outcome: protocol.OutcomeSkipped, Detail: protocol.DetailRetained}) {
		t.Fatalf("remove as the agent: %+v", removed)
	}
```

and replace the end of the test

```go
		if !apierrors.IsNotFound(errCM) || !apierrors.IsNotFound(errS) {
			t.Fatalf("%s left behind: %v %v", name, errCM, errS)
		}
	}
}
```

with

```go
		if !apierrors.IsNotFound(errCM) || !apierrors.IsNotFound(errS) {
			t.Fatalf("%s left behind: %v %v", name, errCM, errS)
		}
	}
	if kept, err := cs.CoreV1().PersistentVolumeClaims(deployNamespace).Get(ctx, "kind-data", metav1.GetOptions{}); err != nil || kept.DeletionTimestamp != nil {
		t.Fatalf("the removal took the claim: %+v %v", kept, err)
	}
}
```

(The removal target drops the claims: a removal carries none, Task 1. The namespace, claim included, goes in `removeAgent`'s cleanup.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runtime/kubernetes/manifest/`
Expected: FAIL: `cluster role grants [... apps/statefulsets]` and `deploy role in billing: ...` (the new rules are missing).

- [ ] **Step 3: Implement**

In `internal/runtime/kubernetes/manifest/manifest.go`, in the template header replace

```
# The agent reads get/list on namespaces, nodes, pods, pod logs, events, services,
# persistentvolumeclaims, deployments, statefulsets and daemonsets in every namespace. It cannot
# read Secrets or ConfigMaps; in its own namespace it may read and write its identity Secret.
{{- if .Namespaces}}
# In each namespace listed below (Role kyyard-agent-deploy) it may create, update and delete
# Deployments, Services, ConfigMaps and Secrets; Secrets are read by name, never listed. Create
# the namespaces first. A namespace dropped from a later manifest keeps its Role until you run
# kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy
{{- end}}
```

with

```
# The agent reads get/list on namespaces, nodes, pods, pod logs, events, services,
# persistentvolumeclaims, deployments, statefulsets, daemonsets and storageclasses in every
# namespace. It cannot read Secrets or ConfigMaps; in its own namespace it may read and write its
# identity Secret.
{{- if .Namespaces}}
# In each namespace listed below (Role kyyard-agent-deploy) it may create, update and delete
# Deployments, Services, ConfigMaps and Secrets; Secrets are read by name, never listed. It may
# create PersistentVolumeClaims but never update or delete one: a claim KyYard created stays
# until you delete it. Create the namespaces first. A namespace dropped from a later manifest
# keeps its Role until you run
# kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy
{{- end}}
```

in the ClusterRole replace

```
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets]
    verbs: [get, list]
  # The agent asks the API server what it may do before every apply.
```

with

```
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets]
    verbs: [get, list]
  # A migration's storage choices pick from these.
  - apiGroups: [storage.k8s.io]
    resources: [storageclasses]
    verbs: [get, list]
  # The agent asks the API server what it may do before every apply.
```

and in the deploy Role replace

```
  # No list: the agent reads each Secret it owns by name.
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, create, update, patch, delete]
```

with

```
  # No list: the agent reads each Secret it owns by name.
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, create, update, patch, delete]
  # Claims are created once and kept: no update, patch or delete.
  - apiGroups: [""]
    resources: [persistentvolumeclaims]
    verbs: [get, list, create]
```

In `internal/api/endpoint_handlers.go`, replace

```go
const manifestNote = "Apply it with a cluster-admin kubeconfig: kubectl apply -f on the saved file. Create the namespaces first. A namespace you removed keeps its Role until you run kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy."
```

with

```go
const manifestNote = "Apply it with a cluster-admin kubeconfig: kubectl apply -f on the saved file. Create the namespaces first. Apply it again after upgrading KyYard: a release can add rules, as migrations added PersistentVolumeClaims and StorageClasses. A namespace you removed keeps its Role until you run kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy."
```

- [ ] **Step 4: Run the tests to verify they pass, and on a real cluster**

Run: `gofmt -w internal && go vet ./... && go test -race -count=1 ./internal/runtime/kubernetes/... ./internal/api/ -run 'Manifest|Enroll|Kubernetes'`
Expected: PASS (`TestManifestOnARealCluster` skips without `KY_TEST_KUBECONFIG`).

With a disposable kind cluster (it creates and deletes cluster-scoped RBAC and the namespaces `kyyard-agent` and `kyyard-test-deploy`; do not point it at a cluster anyone else is using):

Run: `KY_TEST_KUBECONFIG=<kubeconfig> KY_TEST_DEPLOY_IMAGE=registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a go test -count=1 -run TestManifestOnARealCluster -v ./internal/runtime/kubernetes/`
Expected: `--- PASS: TestManifestOnARealCluster` (about 45 s on kind v1.35 with the `standard` local-path default class: the claim binds when the pod schedules, the second apply leaves it alone, the removal keeps it).

- [ ] **Step 5: DOX and commit**

In `internal/runtime/kubernetes/AGENTS.md`, in the `manifest/` bullet append:

```markdown
 The deploy Role also grants `persistentvolumeclaims` `get, list, create` (never `update`, `patch` or `delete`: a claim KyYard created stays until the operator deletes it), and the read ClusterRole `storage.k8s.io` `storageclasses` `get, list`; an agent under an older manifest reports `storage_classes` truncated and its claim creates are refused `admission_denied`/`forbidden` until the regenerated manifest is applied.
```

and in `## Verification`, in the `KY_TEST_KUBECONFIG=...` bullet append: ` The run also creates a claim of the default StorageClass mounted by the pod, applies twice, and proves the removal keeps the claim.`

```bash
git add internal/runtime/kubernetes internal/api/endpoint_handlers.go && make tidy-check lint && git commit -m "feat(k8s): claim and StorageClass RBAC; the real-cluster test mounts and keeps a claim" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 7: API — the migration routes over the plan-time inspection primitive

**Files:**
- Create: `internal/api/migration_handlers.go`
- Create: `internal/api/migration_test.go`
- Modify: `internal/api/server.go` (routes)
- Modify: `internal/api/tenant_handlers.go` (`tenantError` codes)
- Docs: `internal/api/AGENTS.md`

**Interfaces:**
- Consumes: `store.ReadMigrationSource`, `CreateMigration`, `ReadMigration`, `AnalyzeMigration`, `CreateMigrationDestination`, `ConfirmMigration`, `AbandonMigration`, the `Err*` migration errors, `ApplicationMigration.Role` (Task 2); `migration.Analyze`, `migration.Input`, `migration.Destination`, `migration.Report`, `migration.Finding` (Task 3); `PlannedService.ClaimMounts`, `DeploymentPlan.Claims`, `Deployment.MigrationID` (Tasks 2 and 4); `s.planInspections`, `strictJSON`, `tenantRoute`, `tenantError` (existing); test helpers `newClusterHost`, `clusterCapabilities`, `enrollAgent`, `inspecting`, `verifiedInspector`, `loginAs`, `tenantRequest`, `readEnvelope`, `writeEnvelope`, `api.SetPlanInspectorForTest` (existing, package `api_test`).
- Produces (routes under `/api/organizations/{organization}/environments/{environment}/applications/{application}`):
  ```
  POST   /migration              {destination_endpoint_id, namespace}  201 migration | 400 namespace_unknown | 409 runtime_unsupported, migration_open, mapping_required
  GET    /migration                                                    200 migration (role source|destination) | 404
  PUT    /migration/choices      {volumes: {name: {storage_class, size, access_mode}}}  200 | 400 storage_class_unknown, size_invalid, volume_unknown | 409 migration_state, migration_stale
  POST   /migration/analyze                                            200 | 409 migration_state, migration_stale
  POST   /migration/destination                                        201 | 409 migration_not_ready, migration_state, migration_stale, application_name_taken
  POST   /migration/validated    {note}                                200 | 400 | 409 migration_state
  POST   /migration/cutover      {note}                                200 | 400 | 409 migration_state
  DELETE /migration                                                    200 migration + destination_kept | 404
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/api/migration_test.go`:

```go
package api_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/api"
	"github.com/Busnes-app/kyyard-server/internal/migration"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const migrationCanary = "migration-secret-canary"

// migrationFleet is a clusterHost whose cluster reports the default StorageClass standard, plus
// a Docker endpoint "docker-1" (no socket: its inventory is accepted directly and inspections
// answer through the plan-time hook) where "shop" is adopted and mapped: db mounts named volume
// data and holds a secret, web publishes 8080.
func migrationFleet(t *testing.T) (clusterHost, string, string) {
	t.Helper()
	h := newClusterHost(t, clusterCapabilities...)
	ctx := context.Background()
	ts := h.st.Tenancy()
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) + 1, Engine: protocol.Engine{Runtime: protocol.RuntimeKubernetes, Version: "v1.36.0"},
		Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{},
		Kubernetes: &protocol.KubernetesInventory{Nodes: []protocol.Node{{Name: "n1", Ready: true}}, Namespaces: []string{"shop"}, Workloads: []protocol.Workload{}, Pods: []protocol.Pod{}, Services: []protocol.Service{}, Claims: []protocol.Claim{}, StorageClasses: []protocol.StorageClass{{Name: "standard", Default: true}}}})
	h.sync(t)
	host := enrollAgent(t, h.s, h.st, h.admin, "docker-1")
	h.do(t, "POST", "/api/organizations/a/endpoints/"+host.id+"/approve", `{"fingerprint":"`+host.fp+`"}`, 204)
	if err := ts.SetEndpointCapabilities(ctx, host.id, inspecting); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	snapshot := protocol.Snapshot{Engine: protocol.Engine{Version: "1"}, Volumes: []protocol.Volume{{Name: "shop_data"}}}
	bindings := map[string]string{}
	for i, name := range []string{"db", "web"} {
		id, image := strings.Repeat(string("12"[i]), 64), "sha256:"+strings.Repeat(string("ab"[i]), 64)
		snapshot.Containers = append(snapshot.Containers, protocol.Container{ID: id, Name: "shop-" + name, ImageID: image, ComposeProject: "shop", CreatedAt: created, Mounts: []protocol.Mount{}, Networks: []string{"shop_default"}})
		snapshot.Images = append(snapshot.Images, protocol.Image{ID: image, Tags: []string{"ghcr.io/org/" + name + ":1"}})
		bindings[name] = id
	}
	raw, _ := json.Marshal(snapshot)
	if _, err := ts.AcceptInventory(ctx, host.id, uint64(time.Now().Unix()), time.Now(), raw); err != nil {
		t.Fatal(err)
	}
	app := h.importApp(t, "shop", "services: {db: {image: ghcr.io/org/db:1, restart: always, volumes: ['data:/var/lib/db'], environment: {PASSWORD: "+migrationCanary+"}}, web: {image: ghcr.io/org/web:1, restart: always, ports: [{target: 80, published: 8080}]}}\nvolumes: {data: {}}")
	var preview store.AdoptionPreview
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/adoption?endpoint="+host.id+"&project=shop", "", 200)), &preview); err != nil {
		t.Fatal(err)
	}
	adoption, _ := json.Marshal(store.AdoptionRequest{EndpointID: host.id, Project: "shop", Digest: preview.Digest, Confirm: "shop"})
	var instance store.ApplicationInstance
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/adoption", string(adoption), 201)), &instance); err != nil {
		t.Fatal(err)
	}
	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", app+"/mapping", "", 200)), &mapped); err != nil {
		t.Fatal(err)
	}
	mapping, _ := json.Marshal(store.MappingRequest{InstanceID: instance.ID, Version: mapped.Version, Digest: mapped.Preview.Digest, Confirm: "shop", Bindings: bindings})
	h.do(t, "PUT", app+"/mapping", string(mapping), 204)
	api.SetPlanInspectorForTest(h.s, verifiedInspector)
	return h, app, host.id
}

func readReport(t *testing.T, m store.ApplicationMigration) migration.Report {
	t.Helper()
	var r migration.Report
	if err := json.Unmarshal(m.Report, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The whole flow over the API: analysis from the Docker source against the cluster, storage
// choices validated and re-analyzed, the destination created and linked, then planned and
// applied through PR 21's path with the claim in the frame and the migration on the
// deployment, confirmed and closed.
func TestMigrationOverTheAPI(t *testing.T) {
	h, app, docker := migrationFleet(t)
	start := func(endpoint, namespace string, status int) string {
		t.Helper()
		body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: endpoint, Namespace: namespace})
		return h.do(t, "POST", app+"/migration", string(body), status)
	}
	if body := start(h.ag.id, "billing", 400); !strings.Contains(body, "namespace_unknown") {
		t.Fatalf("an ungranted namespace: %s", body)
	}
	if body := start(docker, "shop", 409); !strings.Contains(body, "runtime_unsupported") {
		t.Fatalf("a Docker destination: %s", body)
	}
	h.do(t, "GET", app+"/migration", "", 404)
	var m store.ApplicationMigration
	if err := json.Unmarshal([]byte(start(h.ag.id, "shop", 201)), &m); err != nil {
		t.Fatal(err)
	}
	report := readReport(t, m)
	db := report.Services[0]
	if m.Status != store.MigrationAnalyzed || m.Ready || report.Ready || db.Name != "db" || db.Findings[0] != (migration.Finding{Axis: migration.AxisStorage, Class: migration.ChoiceRequired, Code: "volume_named", Detail: "data"}) || db.Class != migration.ChoiceRequired {
		t.Fatalf("analysis %+v %+v", m, report)
	}
	if slices.ContainsFunc(db.Findings, func(f migration.Finding) bool { return f.Code == "inspection_unavailable" }) {
		t.Fatal("the plan-time inspection did not reach the analyzer")
	}
	if body := start(h.ag.id, "shop", 409); !strings.Contains(body, "migration_open") {
		t.Fatalf("a second migration: %s", body)
	}
	if body := h.do(t, "POST", app+"/migration/destination", "", 409); !strings.Contains(body, "migration_not_ready") {
		t.Fatalf("destination before ready: %s", body)
	}
	for choice, code := range map[string]string{
		`{"volumes":{"data":{"storage_class":"gold","size":"1Gi","access_mode":"ReadWriteOnce"}}}`:     "storage_class_unknown",
		`{"volumes":{"data":{"storage_class":"standard","size":"1G","access_mode":"ReadWriteOnce"}}}`:  "size_invalid",
		`{"volumes":{"logs":{"storage_class":"standard","size":"1Gi","access_mode":"ReadWriteOnce"}}}`: "volume_unknown",
	} {
		if body := h.do(t, "PUT", app+"/migration/choices", choice, 400); !strings.Contains(body, code) {
			t.Errorf("%s: %s", code, body)
		}
	}
	if err := json.Unmarshal([]byte(h.do(t, "PUT", app+"/migration/choices", `{"volumes":{"data":{"storage_class":"","size":"1Gi","access_mode":"ReadWriteOnce"}}}`, 200)), &m); err != nil || !m.Ready || !readReport(t, m).Ready {
		t.Fatalf("chosen %+v %v", m, err)
	}
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/analyze", "", 200)), &m); err != nil || !m.Ready || m.Choices.Volumes["data"].Size != "1Gi" {
		t.Fatalf("re-analyzed %+v %v", m, err)
	}
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/destination", "", 201)), &m); err != nil || m.Status != store.MigrationDestinationCreated || m.DestinationApplicationName != "shop on cluster-1" {
		t.Fatalf("destination %+v %v", m, err)
	}
	dest := h.base + "/" + m.DestinationApplicationID
	var linked store.ApplicationMigration
	if err := json.Unmarshal([]byte(h.do(t, "GET", dest+"/migration", "", 200)), &linked); err != nil || linked.Role != "destination" || linked.ApplicationID != strings.TrimPrefix(app, h.base+"/") {
		t.Fatalf("destination link %+v %v", linked, err)
	}
	h.do(t, "PUT", dest+"/migration/choices", `{"volumes":{}}`, 404)

	var mapped store.ApplicationMapping
	if err := json.Unmarshal([]byte(h.do(t, "GET", dest+"/mapping", "", 200)), &mapped); err != nil || mapped.Namespace != "shop" || mapped.Preview.Project != "shop-on-cluster-1" {
		t.Fatalf("destination mapping %+v %v", mapped, err)
	}
	planBody, _ := json.Marshal(store.PlanRequest{InstanceID: mapped.InstanceID, MappingVersion: mapped.Version, Revision: 1, Confirm: "shop-on-cluster-1"})
	var planned store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", dest+"/deployments", string(planBody), 201)), &planned); err != nil {
		t.Fatal(err)
	}
	claim := protocol.KubernetesClaim{Name: "shop-on-cluster-1-data", Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}
	if !reflect.DeepEqual(planned.Plan.Claims, []protocol.KubernetesClaim{claim}) || planned.Plan.Services[0].ClaimMounts[0] != (protocol.KubernetesMount{Claim: claim.Name, MountPath: "/var/lib/db"}) {
		t.Fatalf("plan %+v", planned.Plan)
	}
	var applying store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "POST", dest+"/deployments/"+planned.ID+"/apply", `{"confirm":"shop-on-cluster-1"}`, 202)), &applying); err != nil || applying.MigrationID != m.ID {
		t.Fatalf("apply %+v %v", applying, err)
	}
	frame := readEnvelope(t, h.ctx, h.sock.conn)
	var sent protocol.DeploymentRequest
	if frame.Type != protocol.TypeDeploymentApply || json.Unmarshal(frame.Payload, &sent) != nil {
		t.Fatalf("frame %s", frame.Type)
	}
	if !reflect.DeepEqual(sent.Kubernetes.Claims, []protocol.KubernetesClaim{claim}) || sent.Services[0].Volumes[0].Claim != claim.Name || sent.Services[0].Env["PASSWORD"] != migrationCanary || sent.ValidateFor(protocol.RuntimeKubernetes, time.Now()) != nil {
		t.Fatalf("frame %+v", sent)
	}
	var ids []protocol.DeploymentIdentity
	for _, ps := range planned.Plan.Services {
		ids = append(ids, protocol.DeploymentIdentity{Service: ps.Name, Kind: protocol.KindDeployment, Namespace: "shop", Name: ps.Object.Name, UID: "0f1e2d3c-4b5a-4968-8776-655443322110", Generation: 1, ImageDigest: ps.PullDigest})
	}
	writeEnvelope(t, h.ctx, h.sock.conn, protocol.TypeDeploymentResult, protocol.DeploymentResult{Deployment: planned.ID, RequestID: planned.CorrelationID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{}, Services: ids})
	h.sync(t)
	var settled store.Deployment
	if err := json.Unmarshal([]byte(h.do(t, "GET", dest+"/deployments/"+planned.ID, "", 200)), &settled); err != nil || settled.State != protocol.OutcomeSucceeded || settled.MigrationID != m.ID {
		t.Fatalf("settled %+v %v", settled, err)
	}
	if strings.Contains(h.do(t, "GET", app+"/migration", "", 200), migrationCanary) || strings.Contains(h.do(t, "GET", dest+"/deployments", "", 200), migrationCanary) {
		t.Fatal("a secret value reached a response")
	}

	if body := h.do(t, "POST", app+"/migration/cutover", `{"note":"early"}`, 409); !strings.Contains(body, "migration_state") {
		t.Fatalf("cutover before validation: %s", body)
	}
	h.do(t, "POST", app+"/migration/validated", `{"note":""}`, 400)
	h.do(t, "POST", app+"/migration/validated", `{"note":"orders page answers on the cluster"}`, 200)
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/cutover", `{"note":"DNS points at the ingress"}`, 200)), &m); err != nil || m.Status != store.MigrationCutoverConfirmed || m.CutoverNote != "DNS points at the ingress" {
		t.Fatalf("cutover %+v %v", m, err)
	}
	h.do(t, "GET", app+"/migration", "", 404)
	h.do(t, "DELETE", app+"/migration", "", 404)
}

// Abandoning keeps a created destination and says so.
func TestMigrationAbandonKeepsTheDestination(t *testing.T) {
	h, app, _ := migrationFleet(t)
	body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: h.ag.id, Namespace: "shop"})
	h.do(t, "POST", app+"/migration", string(body), 201)
	h.do(t, "PUT", app+"/migration/choices", `{"volumes":{"data":{"storage_class":"standard","size":"1Gi","access_mode":"ReadWriteOnce"}}}`, 200)
	var m store.ApplicationMigration
	if err := json.Unmarshal([]byte(h.do(t, "POST", app+"/migration/destination", "", 201)), &m); err != nil {
		t.Fatal(err)
	}
	var abandoned struct {
		Status                   string `json:"status"`
		DestinationApplicationID string `json:"destination_application_id"`
		DestinationKept          bool   `json:"destination_kept"`
	}
	if err := json.Unmarshal([]byte(h.do(t, "DELETE", app+"/migration", "", 200)), &abandoned); err != nil || abandoned.Status != store.MigrationAbandoned || !abandoned.DestinationKept || abandoned.DestinationApplicationID != m.DestinationApplicationID {
		t.Fatalf("abandoned %+v %v", abandoned, err)
	}
	h.do(t, "GET", h.base+"/"+m.DestinationApplicationID+"/mapping", "", 200)
}

// Only an organization administrator migrates; other members read. A source that is not
// adopted on Docker, and a destination that is not a cluster, are runtime_unsupported.
func TestMigrationRolesAndRuntimes(t *testing.T) {
	h, app, _ := migrationFleet(t)
	body, _ := json.Marshal(store.MigrationStart{DestinationEndpointID: h.ag.id, Namespace: "shop"})
	envAdmin := loginAs(t, h.s, h.st, "envadmin", "user")
	if err := h.st.Tenancy().SetMembership(context.Background(), &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_envadmin", Role: store.RoleEnvironmentAdmin, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	h.do(t, "POST", app+"/migration", string(body), 201)
	for _, route := range []struct{ method, path, body string }{
		{"POST", app + "/migration", string(body)},
		{"PUT", app + "/migration/choices", `{"volumes":{}}`},
		{"POST", app + "/migration/analyze", ""},
		{"POST", app + "/migration/destination", ""},
		{"POST", app + "/migration/validated", `{"note":"x"}`},
		{"POST", app + "/migration/cutover", `{"note":"x"}`},
		{"DELETE", app + "/migration", ""},
	} {
		if w := tenantRequest(h.s, envAdmin, route.method, route.path, route.body, true); w.Code != 403 || !strings.Contains(w.Body.String(), "tenant_access_denied") {
			t.Errorf("an environment administrator %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	if w := tenantRequest(h.s, envAdmin, "GET", app+"/migration", "", false); w.Code != 200 {
		t.Fatalf("an environment administrator reads: %d", w.Code)
	}
	h.do(t, "DELETE", app+"/migration", "", 200)
	cluster := h.importApp(t, "cart", "services: {web: {image: ghcr.io/org/web:1}}")
	h.do(t, "PUT", cluster+"/mapping", `{"endpoint_id":"`+h.ag.id+`","namespace":"shop"}`, 204)
	if body := h.do(t, "POST", cluster+"/migration", string(body), 409); !strings.Contains(body, "runtime_unsupported") {
		t.Fatalf("a cluster source: %s", body)
	}
	draft := h.importApp(t, "draft", "services: {web: {image: ghcr.io/org/web:1}}")
	if body := h.do(t, "POST", draft+"/migration", string(body), 409); !strings.Contains(body, "mapping_required") {
		t.Fatalf("an unadopted source: %s", body)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api/ -run TestMigration`
Expected: FAIL: `POST .../migration: 405` (no route) in each test.

- [ ] **Step 3: Implement**

Create `internal/api/migration_handlers.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/migration"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// analyzeMigration reads app's migration inputs against the cluster endpoint, inspects the
// mapped containers through the plan-time primitive (the plan's per-actor budget, results used
// once and dropped), and runs the analyzer with choices. It writes the refusal and reports false
// when the inputs cannot be read.
func (s *Server) analyzeMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess, app, endpoint, namespace string, choices store.KubernetesExtension) (*store.MigrationAnalysis, bool) {
	src, err := s.store.Tenancy().ReadMigrationSource(r.Context(), a, app, endpoint)
	if err == nil && !slices.Contains(src.Destination.Namespaces, namespace) {
		err = store.ErrNamespaceUnknown // before any inspection is spent; the store checks again
	}
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, src.EndpointID)
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	pre, err := s.store.Tenancy().PreflightApplication(r.Context(), a, app)
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	observed := s.planInspections(w, r, a, ep, pre)
	inspections := map[string]protocol.ContainerInspection{}
	for service, c := range src.Containers {
		if in, ok := observed[c.ID]; ok {
			inspections[service] = in
		}
	}
	report := migration.Analyze(migration.Input{Spec: src.Spec, Project: src.Project, Containers: src.Containers, Inspections: inspections, Volumes: src.Volumes, Choices: choices,
		Destination: migration.Destination{Namespace: namespace, Project: src.Destination.Project, StorageClasses: src.Destination.StorageClasses}})
	raw, err := json.Marshal(report)
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	return &store.MigrationAnalysis{Revision: src.Revision, Report: raw, Ready: report.Ready}, true
}

// sourceMigration reads app's open migration as its source; a destination has nothing to change.
func (s *Server) sourceMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) (*store.ApplicationMigration, bool) {
	m, err := s.store.Tenancy().ReadMigration(r.Context(), a, r.PathValue("application"))
	if err == nil && m.Role != "source" {
		err = store.ErrNotFound
	}
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	return m, true
}

func (s *Server) handleStartMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input store.MigrationStart
	if strictJSON(r, &input) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	app := r.PathValue("application")
	an, ok := s.analyzeMigration(w, r, a, app, input.DestinationEndpointID, input.Namespace, store.KubernetesExtension{})
	if !ok {
		return
	}
	m, err := s.store.Tenancy().CreateMigration(r.Context(), a, app, input, *an)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	m, err := s.store.Tenancy().ReadMigration(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, m)
}

// handleMigrationChoices stores the choices and the analysis they produce together.
func (s *Server) handleMigrationChoices(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var choices store.KubernetesExtension
	if strictJSON(r, &choices) != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	s.reanalyze(w, r, a, &choices)
}

func (s *Server) handleAnalyzeMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	s.reanalyze(w, r, a, nil)
}

// reanalyze analyzes the open migration again with choices, or its stored ones when nil.
func (s *Server) reanalyze(w http.ResponseWriter, r *http.Request, a store.TenantAccess, choices *store.KubernetesExtension) {
	m, ok := s.sourceMigration(w, r, a)
	if !ok {
		return
	}
	if choices == nil {
		choices = &m.Choices
	}
	an, ok := s.analyzeMigration(w, r, a, m.ApplicationID, m.DestinationEndpointID, m.Namespace, *choices)
	if !ok {
		return
	}
	m, err := s.store.Tenancy().AnalyzeMigration(r.Context(), a, m.ApplicationID, *choices, *an)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleMigrationDestination(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	m, err := s.store.Tenancy().CreateMigrationDestination(r.Context(), a, r.PathValue("application"), s.config.Security.EncryptionKey)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, m)
}

// handleConfirmMigration records the operator's confirmation step with its note.
func (s *Server) handleConfirmMigration(step string) func(http.ResponseWriter, *http.Request, store.TenantAccess) {
	return func(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
		var input struct {
			Note string `json:"note"`
		}
		if strictJSON(r, &input) != nil {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		m, err := s.store.Tenancy().ConfirmMigration(r.Context(), a, r.PathValue("application"), step, input.Note)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, m)
	}
}

// handleAbandonMigration closes the open migration and says whether a destination it created
// stays: it does, with its deployments, until someone removes it.
func (s *Server) handleAbandonMigration(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	m, err := s.store.Tenancy().AbandonMigration(r.Context(), a, r.PathValue("application"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, struct {
		*store.ApplicationMigration
		DestinationKept bool `json:"destination_kept"`
	}{m, m.DestinationApplicationID != ""})
}
```

In `internal/api/server.go`, insert before the `GET .../applications/{application}/adoption` route:

```go
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/migration", s.tenantRoute(s.handleStartMigration))
	s.mux.HandleFunc("GET /api/organizations/{organization}/environments/{environment}/applications/{application}/migration", s.tenantRoute(s.handleMigration))
	s.mux.HandleFunc("DELETE /api/organizations/{organization}/environments/{environment}/applications/{application}/migration", s.tenantRoute(s.handleAbandonMigration))
	s.mux.HandleFunc("PUT /api/organizations/{organization}/environments/{environment}/applications/{application}/migration/choices", s.tenantRoute(s.handleMigrationChoices))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/migration/analyze", s.tenantRoute(s.handleAnalyzeMigration))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/migration/destination", s.tenantRoute(s.handleMigrationDestination))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/migration/validated", s.tenantRoute(s.handleConfirmMigration("validated")))
	s.mux.HandleFunc("POST /api/organizations/{organization}/environments/{environment}/applications/{application}/migration/cutover", s.tenantRoute(s.handleConfirmMigration("cutover")))
```

In `internal/api/tenant_handlers.go`, `tenantError`, replace

```go
	case errors.Is(err, store.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, "Invalid tenant input")
```

with

```go
	case errors.Is(err, store.ErrMigrationOpen):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The application has an open migration; confirm its cutover or abandon it first", "code": "migration_open"})
	case errors.Is(err, store.ErrMigrationNotReady):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The migration report still has blocked findings or choices to make", "code": "migration_not_ready"})
	case errors.Is(err, store.ErrMigrationState):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The migration's status does not allow this step", "code": "migration_state"})
	case errors.Is(err, store.ErrMigrationStale):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The source definition changed since the analysis; analyze again", "code": "migration_stale"})
	case errors.Is(err, store.ErrApplicationNameTaken):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "An application with the destination's name already exists", "code": "application_name_taken"})
	case errors.Is(err, store.ErrStorageClassUnknown):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The cluster does not report that StorageClass", "code": "storage_class_unknown"})
	case errors.Is(err, store.ErrSizeInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A size is a whole number of Mi, Gi or Ti from 1Mi to 16Ti", "code": "size_invalid"})
	case errors.Is(err, store.ErrVolumeUnknown):
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The source mounts no named volume by that name", "code": "volume_unknown"})
	case errors.Is(err, store.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, "Invalid tenant input")
```

- [ ] **Step 4: Run the tests to verify they pass, on both databases, and the link rule**

Run: `gofmt -w internal/api && go vet ./... && go test -race -count=1 ./internal/api/ && PG=… go test -count=1 ./internal/api/ -run 'Migration|Kubernetes|RuntimeGate' && test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0`
Expected: PASS; the server, which now imports `internal/migration`, still links no client-go.

- [ ] **Step 5: DOX and commit**

In `internal/api/AGENTS.md`, `## Local Contracts`, append after the `- On a Kubernetes instance` bullet:

```markdown
- `migration_handlers.go`: `POST|GET|DELETE .../applications/{application}/migration`, `PUT .../migration/choices`, `POST .../migration/analyze|destination|validated|cutover`. Start, choices and analyze read `store.ReadMigrationSource` (under `application.migrate`, so nobody else causes an inspection), refuse a namespace the cluster does not grant (400 `namespace_unknown`) before any inspection, then read `PreflightApplication` for the mapped containers' targets and `planInspections` (the plan's primitive and its `inspection:<actor>` budget; observations are used once and dropped), run `migration.Analyze` in the handler and pass `store.MigrationAnalysis` to one store write. Choices and analyze act on the source's open migration only (a destination's `GET` answers `role: destination`, and its writes 404). Abandon answers the migration plus `destination_kept`. New `tenantError` codes: 409 `migration_open`, `migration_not_ready`, `migration_state`, `migration_stale`, `application_name_taken`; 400 `storage_class_unknown`, `size_invalid`, `volume_unknown`. `manifestNote` asks the operator to re-apply the regenerated manifest after an upgrade.
```

and in `## Verification`, first bullet, add the clause `migration_test.go the whole migration over the API with a Docker source and the fake cluster agent;` before the `backup_test.go` clause.

```bash
git add internal/api && make tidy-check lint && git commit -m "feat(api): migration analysis, choices, destination and confirmations" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 8: Web — the Migration card, the new code sentences and the build

**Files:**
- Create: `web/src/components/ApplicationMigration.tsx`
- Create: `web/src/components/ApplicationMigration.test.tsx`
- Modify: `web/src/components/Applications.tsx` (the card under each instance)
- Modify: `web/src/components/ApplicationDeploymentPlan.tsx` (`claim_immutable`, the retained line, claims in a plan, the migration mark in history, `OBJECT`)
- Modify: `web/src/components/ApplicationInspection.tsx` (`k8s_volume`, `k8s_volume_shared`, `K8S_VOLUME_CHOICE`)
- Modify: `web/src/components/ApplicationPreflight.tsx` (`serviceFindings` reads `details`)
- Modify: `web/src/tenant.ts` (`StorageClass`, `storage_classes`, `canMigrate`)
- Modify test: `web/src/components/ApplicationDeploymentPlan.test.tsx`
- Rebuild: `web/dist`, `web/tsconfig.tsbuildinfo`
- Docs: `web/AGENTS.md`

**Interfaces:**
- Consumes: the routes of Task 7; `web/src/migration-codes.json` (Task 3) and `web/src/protocol-codes.json` (Task 1); `Deployment.migration_id`, `plan.claims`, `claim_mounts`, `details` (Tasks 2 and 4); `useTenantResource`, `secureFetch`, `StateNotice`, `displayName`, `unsupportedNames`, `ApplicationInstance`, `Endpoint`, `Inventory` (existing).
- Produces:
  ```ts
  export const MIGRATION_CODES, CHECKLIST_STEPS, ASSUMPTIONS, MIGRATION_ERRORS: Record<string, string>;
  export function findingText(f: { axis: string; class: string; code: string; detail?: string }): string;
  export function ApplicationMigration(props: { base: string; org: string; env: string; instance: ApplicationInstance; admin: boolean; onOpen: (application: string) => void }): JSX.Element | null;
  export const CLAIM_RETAINED: string;        // ApplicationDeploymentPlan.tsx
  export const K8S_VOLUME_CHOICE: string;     // ApplicationInspection.tsx
  export const canMigrate: (role: string | undefined) => boolean; // tenant.ts, mirrors application.migrate
  export interface StorageClass { name: string; default: boolean }
  ```

- [ ] **Step 1: Write the failing tests**

Create `web/src/components/ApplicationMigration.test.tsx`:

```tsx
import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { ApplicationMigration, ASSUMPTIONS, CHECKLIST_STEPS, MIGRATION_CODES, MIGRATION_ERRORS, findingText } from './ApplicationMigration';
import { STEP_CODES } from './ApplicationDeploymentPlan';
import { unsupportedNames } from './ApplicationInspection';
import migrationCodes from '../migration-codes.json';
import protocolCodes from '../protocol-codes.json';

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); document.cookie = 'ky_csrf=; Max-Age=0'; });
const sorted = (xs: string[]) => [...xs].sort();
const docker = { id: 'i1', application_id: 'app', endpoint_id: 'ep_d', endpoint_name: 'docker-1', project: 'shop', revision: 1, current_revision: 0, previous_revision: 0, mapping_version: 2, container_count: 2, containers: [] };
const cluster = { id: 'ep_k', name: 'prod', runtime: 'kubernetes', state: 'active', deploy_namespaces: ['shop'] };
const inventory = { snapshot: { kubernetes: { storage_classes: [{ name: 'standard', default: true }, { name: 'fast', default: false }] } } };
const findings = (volume: string) => [
  { axis: 'storage', class: volume, code: 'volume_named', detail: 'data' },
  { axis: 'networking', class: 'supported', code: 'networking_supported' },
  { axis: 'ports', class: 'supported', code: 'port_unpublished' },
  { axis: 'flags', class: 'blocked', code: 'flag_blocked', detail: 'privileged' },
];
const migration = (over: Record<string, unknown> = {}) => ({
  id: 'm1', application_id: 'app', application_name: 'shop', destination_endpoint_id: 'ep_k', namespace: 'shop', status: 'analyzed', ready: false, role: 'source', choices: { volumes: {} },
  report: { version: 1, ready: false, services: [{ name: 'db', class: 'blocked', findings: findings('operator_choice_required') }], checklist: [{ code: 'grant_namespace', commands: ['kubectl label namespace shop pod-security.kubernetes.io/enforce=baseline --overwrite'] }, { code: 'create_destination' }], assumptions: ['volume_size_unknown'] },
  ...over,
});

// Every code the Go vocabularies define has exactly one sentence here: the fixtures are
// generated from internal/migration and internal/agent/protocol.
it('has a sentence for every generated code, and no other', () => {
  expect(sorted(Object.keys(MIGRATION_CODES))).toEqual(sorted(migrationCodes.codes));
  expect(sorted(Object.keys(CHECKLIST_STEPS))).toEqual(sorted(migrationCodes.checklist));
  expect(sorted(Object.keys(ASSUMPTIONS))).toEqual(sorted(migrationCodes.assumptions));
  expect(sorted(Object.keys(STEP_CODES))).toEqual(sorted(protocolCodes.step_codes));
  expect(sorted(Object.keys(unsupportedNames))).toEqual(sorted(protocolCodes.unsupported_codes));
  for (const code of ['namespace_unknown', 'runtime_unsupported', 'migration_open', 'storage_class_unknown', 'size_invalid', 'volume_unknown', 'application_name_taken', 'migration_not_ready', 'migration_state', 'migration_stale']) {
    expect(MIGRATION_ERRORS[code]).toBeTruthy();
  }
});

it('renders a finding with its parameter and nothing for an unknown code', () => {
  expect(findingText({ axis: 'flags', class: 'blocked', code: 'flag_blocked', detail: 'privileged' })).toBe(`${MIGRATION_CODES.flag_blocked} (runs privileged)`);
  expect(findingText({ axis: 'ports', class: 'supported', code: 'port_published', detail: '8080/tcp' })).toBe(`${MIGRATION_CODES.port_published} (8080/tcp)`);
  expect(findingText({ axis: 'x', class: 'blocked', code: '<b>secret-canary</b>' })).toBe('');
});

it('offers an administrator the analysis of a Docker source and posts the cluster and namespace', async () => {
  document.cookie = 'ky_csrf=csrf';
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'POST') return json(migration(), 201);
    return String(url).endsWith('/migration') ? json({ error: 'not found' }, 404) : json([cluster]);
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  await screen.findByRole('option', { name: 'prod' });
  fireEvent.change(screen.getByLabelText('Destination cluster'), { target: { value: 'ep_k' } });
  const analyze = screen.getByRole('button', { name: 'Analyze' });
  expect(analyze.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Destination namespace'), { target: { value: 'shop' } });
  fireEvent.click(analyze);
  await vi.waitFor(() => expect(fetcher.mock.calls.some((c) => c[1]?.method === 'POST')).toBe(true));
  const post = fetcher.mock.calls.find((c) => c[1]?.method === 'POST');
  expect(String(post?.[0])).toBe('/app/migration');
  expect(JSON.parse(String(post?.[1]?.body))).toEqual({ destination_endpoint_id: 'ep_k', namespace: 'shop' });
});

it('renders nothing for a response it cannot read', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => json({ role: 'source', status: 'analyzed', report: '<b>secret-canary</b>' })));
  const { container } = render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  await vi.waitFor(() => expect(container.textContent).toBe(''));
});

it('renders nothing for a member who may not migrate, or for a cluster instance with no migration', async () => {
  for (const [instance, admin] of [[docker, false], [{ ...docker, namespace: 'shop' }, true]] as const) {
    const fetcher = vi.fn(async () => json({}, 404));
    vi.stubGlobal('fetch', fetcher);
    const { container } = render(<ApplicationMigration base="/app" org="a" env="e" instance={instance} admin={admin} onOpen={vi.fn()} />);
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalled());
    await vi.waitFor(() => expect(container.textContent).toBe(''));
    cleanup();
  }
});

it('shows the report, takes storage choices from the cluster, and holds the destination until ready', async () => {
  document.cookie = 'ky_csrf=csrf';
  const fetcher = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === 'PUT') return json({ code: 'storage_class_unknown', error: 'secret-canary' }, 400);
    return String(url).endsWith('/inventory') ? json(inventory) : json(migration());
  });
  vi.stubGlobal('fetch', fetcher);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={vi.fn()} />);
  const table = await screen.findByRole('table');
  expect(within(table).getByText(`${MIGRATION_CODES.volume_named} (data)`)).toBeTruthy();
  expect(within(table).getAllByText('Blocked').length).toBe(1);
  expect(within(table).getByText('Choice required')).toBeTruthy();
  expect(screen.getByText(ASSUMPTIONS.volume_size_unknown)).toBeTruthy();
  expect(screen.getByText('kubectl label namespace shop pod-security.kubernetes.io/enforce=baseline --overwrite')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Create destination' }).hasAttribute('disabled')).toBe(true);
  const storage = await screen.findByLabelText('StorageClass for data');
  await vi.waitFor(() => expect([...storage.querySelectorAll('option')].map((o) => o.textContent)).toEqual(['Cluster default (standard)', 'standard', 'fast']));
  fireEvent.change(storage, { target: { value: 'fast' } });
  fireEvent.change(screen.getByLabelText('Size for data'), { target: { value: '10Gi' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save storage choices' }));
  expect((await screen.findByRole('alert')).textContent).toBe(MIGRATION_ERRORS.storage_class_unknown);
  const put = fetcher.mock.calls.find((c) => c[1]?.method === 'PUT');
  expect(String(put?.[0])).toBe('/app/migration/choices');
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ volumes: { data: { storage_class: 'fast', size: '10Gi', access_mode: 'ReadWriteOnce' } } });
  expect(document.body.textContent).not.toContain('secret-canary');
});

it('links the created destination, takes the confirmations with a note, and says an abandoned destination stays', async () => {
  document.cookie = 'ky_csrf=csrf';
  const open = vi.fn();
  const created = migration({ status: 'destination_created', ready: true, destination_application_id: 'dest', destination_application_name: 'shop on prod' });
  const fetcher = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => init?.method === 'DELETE' ? json({ ...created, status: 'abandoned', destination_kept: true }) : json(created));
  vi.stubGlobal('fetch', fetcher);
  vi.stubGlobal('confirm', () => true);
  render(<ApplicationMigration base="/app" org="a" env="e" instance={docker} admin onOpen={open} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Open shop on prod' }));
  expect(open).toHaveBeenCalledWith('dest');
  expect(screen.queryByRole('button', { name: 'Create destination' })).toBeNull();
  const confirm = screen.getByRole('button', { name: 'Confirm validation' });
  expect(confirm.hasAttribute('disabled')).toBe(true);
  fireEvent.change(screen.getByLabelText('Note for confirm validation'), { target: { value: 'orders page answers' } });
  fireEvent.click(confirm);
  await vi.waitFor(() => expect(fetcher.mock.calls.some((c) => String(c[0]) === '/app/migration/validated')).toBe(true));
  expect(JSON.parse(String(fetcher.mock.calls.find((c) => String(c[0]) === '/app/migration/validated')?.[1]?.body))).toEqual({ note: 'orders page answers' });
  fireEvent.click(await screen.findByRole('button', { name: 'Abandon migration' }));
  expect((await screen.findByRole('alert')).textContent).toContain('The destination application stays');
});

it('shows a destination where it came from', async () => {
  const open = vi.fn();
  vi.stubGlobal('fetch', vi.fn(async () => json(migration({ role: 'destination', status: 'validated' }))));
  render(<ApplicationMigration base="/dest" org="a" env="e" instance={{ ...docker, namespace: 'shop' }} admin={false} onOpen={open} />);
  expect(await screen.findByText('Migration destination of shop · Validated')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Open shop' }));
  expect(open).toHaveBeenCalledWith('app');
});
```

In `web/src/components/ApplicationDeploymentPlan.test.tsx`, replace

```tsx
import { ApplicationDeploymentPlan } from './ApplicationDeploymentPlan';
```

with

```tsx
import { ApplicationDeploymentPlan, CLAIM_RETAINED, STEP_CODES } from './ApplicationDeploymentPlan';
```

replace

```tsx
import { KUBERNETES_UNVERIFIED } from './ApplicationValidation';
```

with

```tsx
import { KUBERNETES_UNVERIFIED } from './ApplicationValidation';
import { K8S_VOLUME_CHOICE, unsupportedNames } from './ApplicationInspection';
```

in `names each service a Kubernetes plan refuses, with the fix` replace

```tsx
    if (init?.method === 'POST') return new Response(JSON.stringify({ code: 'preflight_blocked', blockers: ['kubernetes_unsupported', 'k8s_namespace'], services: [{ name: 'db', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_volume'] }, { name: 'web', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_host_ip', 'k8s_restart'] }] }), { status: 409 });
```

with

```tsx
    if (init?.method === 'POST') return new Response(JSON.stringify({ code: 'preflight_blocked', blockers: ['kubernetes_unsupported', 'k8s_namespace'], services: [{ name: 'db', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_volume'], details: { k8s_volume: 'choice_required' } }, { name: 'web', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_host_ip', 'k8s_restart'] }, { name: 'cache', blockers: ['kubernetes_unsupported'], unsupported: ['k8s_volume', 'k8s_volume_shared'] }] }), { status: 409 });
```

and replace

```tsx
  expect(alert.textContent).toContain('db: mounts a volume; Kubernetes deployment of stateful services arrives with the migration analyzer');
```

with

```tsx
  expect(alert.textContent).toContain(`db: ${K8S_VOLUME_CHOICE}`);
  expect(alert.textContent).toContain(`cache: ${unsupportedNames.k8s_volume}, ${unsupportedNames.k8s_volume_shared}`);
```

and append:

```tsx
it('names an immutable claim, shows the claims a plan creates, and says a removal kept its claims', async () => {
  const kube = { ...plan, state: 'failed', migration_id: 'm1', plan: { project: 'shop', namespace: 'shop', claims: [{ name: 'shop-data', storage_class: '', size: '1Gi', access_mode: 'ReadWriteOnce' }], services: [{ ...plan.plan.services[0], image_id: '', container_id: '', pull_digest: `sha256:${'d'.repeat(64)}`, object: { namespace: 'shop', name: 'shop-web' }, claim_mounts: [{ claim: 'shop-data', mount_path: '/data' }] }] },
    result: { code: 'step_failed', steps: [
      { service: 'web', step: 'precondition', outcome: 'denied', code: 'claim_immutable', detail: 'PersistentVolumeClaim/shop-data' },
      { service: 'web', step: 'precondition', outcome: 'denied', code: 'name_taken', detail: 'PersistentVolumeClaim/shop-data' },
    ], services: [] } };
  const removal = { ...plan, id: 'd2', kind: 'remove', state: 'succeeded', plan: { project: 'shop', namespace: 'shop' },
    result: { steps: [{ service: 'web', step: 'remove', outcome: 'succeeded', detail: '' }, { service: 'web', step: 'volume', outcome: 'skipped', detail: 'retained' }], services: [] } };
  vi.stubGlobal('fetch', stubFetch([kube, removal]));
  render(<ApplicationDeploymentPlan {...props} />);
  fireEvent.click(screen.getByRole('button', { name: 'Deployment plan' }));
  expect(await screen.findByText(`${STEP_CODES.claim_immutable}: PersistentVolumeClaim/shop-data.`)).toBeTruthy();
  expect(screen.getByText('Something else already holds that name: PersistentVolumeClaim/shop-data.')).toBeTruthy();
  expect(screen.getByText('Claims to create when missing (never changed or deleted): shop-data (cluster default, 1Gi)')).toBeTruthy();
  expect(screen.getByText('shop-data → /data')).toBeTruthy();
  expect(screen.getByText('Apply (migration)')).toBeTruthy();
  const rows = screen.getAllByRole('button', { name: 'Show steps' });
  fireEvent.click(rows[rows.length - 1]);
  expect(await screen.findByText(CLAIM_RETAINED)).toBeTruthy();
  expect(screen.getByText(/Its PersistentVolumeClaims and their data are kept/)).toBeTruthy();
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm ci && npx vitest run src/components/ApplicationMigration.test.tsx src/components/ApplicationDeploymentPlan.test.tsx`
Expected: FAIL: `Failed to resolve import "./ApplicationMigration"`, and `CLAIM_RETAINED`/`K8S_VOLUME_CHOICE` are not exported.

- [ ] **Step 3: Implement the card**

Create `web/src/components/ApplicationMigration.tsx`:

```tsx
import { useState } from 'react';
import { secureFetch } from '../api';
import { useTenantResource, type Endpoint, type Inventory } from '../tenant';
import { StateNotice } from './StateNotice';
import { displayName } from './Endpoints';
import { unsupportedNames } from './ApplicationInspection';
import type { ApplicationInstance } from './ApplicationAdoption';

// The analyzer's codes (internal/migration), checked against web/src/migration-codes.json.
export const MIGRATION_CODES: Record<string, string> = {
  volume_named: 'A named volume becomes a PersistentVolumeClaim once you choose its StorageClass and size.',
  volume_named_shared: 'Two services mount this volume; a ReadWriteOnce claim serves one pod, so it cannot move as it is.',
  volume_bind: 'A host path cannot follow the service to a cluster.',
  volume_external: 'An external volume moves as a new claim (KyYard adopts no existing claim); choose its StorageClass and size.',
  storage_supported: 'No volumes.',
  network_host: 'Host networking has no equivalent on the cluster.',
  networks_multiple: 'On the cluster, services reach each other only as <project>-<service> on published ports.',
  networking_supported: 'The project network becomes cluster networking.',
  port_published: 'Published as a ClusterIP Service; exposing it outside the cluster is your Ingress.',
  port_host_ip: 'A port bound to one host address has no Service equivalent; drop the address.',
  port_unpublished: 'Publishes no port, so other services cannot reach it.',
  secrets_supported: "Environment values move into a Secret, copied from the source's encrypted values.",
  healthcheck_dropped: "The container's healthcheck is not rendered as a probe; add one after cutover or rely on the rollout wait.",
  probes_supported: 'No healthcheck to carry over.',
  resource_limits_dropped: 'Resource limits are not rendered; set them on the Deployment after cutover, or run without them.',
  resources_supported: 'No resource limits to carry over.',
  scheduling_blocked: 'Shares a host namespace or uses a runtime setting a Deployment does not express.',
  scheduling_supported: 'Nothing host-specific to schedule.',
  flag_blocked: "Runs with a host privilege or setting that Pod Security baseline refuses or KyYard does not carry.",
  restart_policy: 'A Deployment always restarts its pod; set the restart policy to always or unless-stopped first.',
  read_only_rootfs: 'The root filesystem is read-only on the host; the destination runs it writable until you set readOnlyRootFilesystem.',
  flags_supported: 'No privileged flags.',
  inspection_unavailable: 'The host answered no live inspection, so this is unknown; analyze again when the host is online.',
};
export const CHECKLIST_STEPS: Record<string, string> = {
  grant_namespace: "Label the namespace to enforce Pod Security baseline, then regenerate the cluster's manifest and apply it: migrations need its claim and StorageClass rules.",
  create_destination: 'Create the destination below, then plan and apply it from its own entry in Applications.',
  copy_volume: 'Stop writes to the source, then copy each volume into its claim once the destination pod runs:',
  validate_destination: 'Check the destination works, then confirm validation below.',
  switch_traffic: 'Point your DNS or Ingress at the destination.',
  confirm_cutover: "Confirm cutover below, then remove the source with KyYard's removal. KyYard never stops or removes it for you.",
};
export const ASSUMPTIONS: Record<string, string> = {
  volume_size_unknown: 'Docker reports no volume size; size each claim yourself.',
};
export const MIGRATION_ERRORS: Record<string, string> = {
  namespace_unknown: "The cluster's manifest does not grant that namespace.",
  runtime_unsupported: 'The source must be adopted on a Docker host, and the destination must be a Kubernetes cluster.',
  mapping_required: 'Adopt and map the application on a Docker host first.',
  adoption_changed: "The source host's inventory is stale or changed; refresh it and try again.",
  migration_open: 'This application already has an open migration.',
  storage_class_unknown: 'The cluster does not report that StorageClass. If the list is empty, apply the regenerated manifest.',
  size_invalid: 'A size is a whole number of Mi, Gi or Ti, from 1Mi to 16Ti.',
  volume_unknown: 'The source mounts no named volume by that name.',
  application_name_taken: 'An application named like the destination already exists. Rename or discard it first.',
  migration_not_ready: 'Resolve every blocked finding and make every choice first.',
  migration_state: 'The migration moved on; refresh it.',
  migration_stale: 'The source definition changed since the analysis; analyze again.',
};
const CLASSES: Record<string, string> = { supported: 'Supported', operator_choice_required: 'Choice required', blocked: 'Blocked' };
const STATUSES: Record<string, string> = { analyzed: 'Analyzed', destination_created: 'Destination created', validated: 'Validated', cutover_confirmed: 'Cutover confirmed', abandoned: 'Abandoned' };
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';

type Finding = { axis: string; class: string; code: string; detail?: string };
type Report = { version: number; ready: boolean; services: { name: string; class: string; findings: Finding[] }[]; checklist: { code: string; commands?: string[] }[]; assumptions: string[] };
type Choice = { storage_class: string; size: string; access_mode: string };
export type Migration = { id: string; application_id: string; application_name: string; destination_application_id?: string; destination_application_name?: string; destination_endpoint_id: string; namespace: string; status: string; ready: boolean; report: Report; choices: { volumes: Record<string, Choice> }; role: 'source' | 'destination' };

// isMigration holds a response to the shape this card renders; anything else renders nothing.
function isMigration(x: unknown): x is Migration {
  if (!x || typeof x !== 'object') return false;
  const m = x as Record<string, unknown>;
  const r = m.report as Record<string, unknown> | null | undefined;
  return (m.role === 'source' || m.role === 'destination') && typeof m.status === 'string' && !!r && typeof r === 'object' && Array.isArray(r.services) && Array.isArray(r.checklist) && Array.isArray(r.assumptions);
}

// findingText is a finding's sentence with its parameter: a code from UnsupportedCodes by its
// name, anything else as inert text; an unknown code renders nothing.
export function findingText(f: Finding): string {
  const text = fixed(MIGRATION_CODES, f.code);
  if (!text || !f.detail) return text;
  if (Object.hasOwn(unsupportedNames, f.detail)) return `${text} (${unsupportedNames[f.detail]})`;
  return `${text} (${f.detail})`;
}

// ApplicationMigration is the Migration card: on a Docker-adopted source, an administrator
// analyzes it against a cluster, makes the storage choices, creates the destination and confirms
// the operator's steps; a destination shows where it came from. KyYard never stops the source.
export function ApplicationMigration({ base, org, env, instance, admin, onOpen }: { base: string; org: string; env: string; instance: ApplicationInstance; admin: boolean; onOpen: (application: string) => void }) {
  const migration = useTenantResource<Migration>(`${base}/migration`);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const m = migration.data;
  const write = async (method: string, path: string, body?: unknown) => {
    setBusy(true); setMessage('');
    try {
      const r = await secureFetch(`${base}/migration${path}`, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
      if (r.ok) {
        const payload = await r.json().catch(() => ({})) as { destination_kept?: unknown };
        if (method === 'DELETE') setMessage(payload.destination_kept === true ? 'Migration abandoned. The destination application stays; remove it yourself if you no longer want it.' : 'Migration abandoned.');
        migration.reload();
        return;
      }
      const payload = await r.json().catch(() => ({})) as { code?: unknown };
      const code = typeof payload.code === 'string' ? payload.code : '';
      setMessage(r.status === 403 ? 'Only an organization administrator can migrate applications.' : fixed(MIGRATION_ERRORS, code) || 'The request was refused. Refresh the migration and try again.');
    } catch { setMessage('Offline: the server could not be reached.'); } finally { setBusy(false); }
  };
  if (migration.state === 'notfound') return !instance.namespace && admin ? <MigrationStart org={org} env={env} busy={busy} message={message} onStart={(body) => void write('POST', '', body)} /> : null;
  if (migration.state !== 'ready') return <StateNotice state={migration.state} onRetry={migration.reload} />;
  if (!isMigration(m)) return null;
  if (m.role === 'destination') return <section className="dr-stack" aria-label="Migration">
    <p>Migration destination of {m.application_name} · {fixed(STATUSES, m.status)}</p>
    <button type="button" className="btn-secondary" onClick={() => onOpen(m.application_id)}>Open {m.application_name}</button>
  </section>;
  const report = m.report;
  return <section className="dr-stack" aria-label="Migration" style={{ overflowWrap: 'anywhere' }}>
    <h3>Migration to namespace {m.namespace} · {fixed(STATUSES, m.status)}</h3>
    <p>{report.ready ? 'Ready: every service can run on the cluster as analyzed.' : 'Not ready: resolve the blocked findings and make the choices below, then analyze again.'}</p>
    <table className="ky-table ky-responsive-table"><thead><tr><th>Service</th><th>Axis</th><th>Class</th><th>Finding</th></tr></thead><tbody>{report.services.flatMap((s) => s.findings.map((f, i) => <tr key={`${s.name}/${i}`}>
      <td data-label="Service">{i === 0 ? <strong>{s.name}</strong> : ''}</td>
      <td data-label="Axis">{f.axis}</td>
      <td data-label="Class"><span className="badge">{fixed(CLASSES, f.class)}</span></td>
      <td data-label="Finding">{findingText(f)}</td>
    </tr>))}</tbody></table>
    {report.assumptions.map((a) => <p key={a}>{fixed(ASSUMPTIONS, a)}</p>)}
    {admin && m.status === 'analyzed' && <MigrationChoices key={JSON.stringify(m.choices)} org={org} migration={m} busy={busy} onSave={(volumes) => void write('PUT', '/choices', { volumes })} />}
    {admin && m.status === 'analyzed' && <div className="ky-inline-form">
      <button type="button" className="btn-secondary" disabled={busy} onClick={() => void write('POST', '/analyze')}>Analyze again</button>
      <button type="button" disabled={busy || !m.ready} onClick={() => void write('POST', '/destination')}>Create destination</button>
    </div>}
    {m.destination_application_id && <div className="ky-inline-form"><p>Destination application {m.destination_application_name}: plan and apply it from its own entry.</p><button type="button" className="btn-secondary" onClick={() => onOpen(m.destination_application_id ?? '')}>Open {m.destination_application_name}</button></div>}
    <h4>Checklist</h4>
    <ol>{report.checklist.map((step) => <li key={step.code}>{fixed(CHECKLIST_STEPS, step.code)}{step.commands?.map((c) => <pre key={c}><code>{c}</code></pre>)}</li>)}</ol>
    {admin && m.status === 'destination_created' && <MigrationConfirm label="Confirm validation" busy={busy} onConfirm={(note) => void write('POST', '/validated', { note })} />}
    {admin && m.status === 'validated' && <MigrationConfirm label="Confirm cutover" busy={busy} onConfirm={(note) => void write('POST', '/cutover', { note })} />}
    {admin && <button type="button" className="btn-danger" disabled={busy} onClick={() => { if (window.confirm('Abandon this migration? The source keeps running and a created destination stays.')) void write('DELETE', ''); }}>Abandon migration</button>}
    {message && <p role="alert">{message}</p>}
  </section>;
}

function MigrationStart({ org, env, busy, message, onStart }: { org: string; env: string; busy: boolean; message: string; onStart: (body: { destination_endpoint_id: string; namespace: string }) => void }) {
  const endpoints = useTenantResource<Endpoint[]>(`/api/organizations/${encodeURIComponent(org)}/environments/${encodeURIComponent(env)}/endpoints?limit=200`);
  const clusters = (Array.isArray(endpoints.data) ? endpoints.data : []).filter((e) => e.runtime === 'kubernetes');
  const [cluster, setCluster] = useState('');
  const [namespace, setNamespace] = useState('');
  const chosen = clusters.find((c) => c.id === cluster);
  if (endpoints.state === 'ready' && clusters.length === 0) return null;
  return <section className="dr-stack" aria-label="Migration">
    <h3>Migrate to a Kubernetes cluster</h3>
    <p>Analysis reads the definition and a live inspection of each container. Nothing runs and the source keeps running.</p>
    <StateNotice state={endpoints.state} onRetry={endpoints.reload} />
    <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); onStart({ destination_endpoint_id: cluster, namespace }); }}>
      <label>Destination cluster<select value={cluster} disabled={busy} onChange={(e) => { setCluster(e.target.value); setNamespace(''); }}>
        <option value="">Choose a cluster</option>
        {clusters.map((c) => <option key={c.id} value={c.id}>{displayName(c.name)}</option>)}
      </select></label>
      {chosen && <label>Destination namespace<select value={namespace} disabled={busy} onChange={(e) => setNamespace(e.target.value)}>
        <option value="">Choose a namespace</option>
        {(chosen.deploy_namespaces ?? []).map((ns) => <option key={ns} value={ns}>{ns}</option>)}
      </select></label>}
      <button disabled={busy || !cluster || !namespace}>Analyze</button>
      {message && <p role="alert">{message}</p>}
    </form>
  </section>;
}

function MigrationChoices({ org, migration, busy, onSave }: { org: string; migration: Migration; busy: boolean; onSave: (volumes: Record<string, Choice>) => void }) {
  const inventory = useTenantResource<Inventory>(`/api/organizations/${encodeURIComponent(org)}/endpoints/${encodeURIComponent(migration.destination_endpoint_id)}/inventory`);
  const classes = inventory.data?.snapshot.kubernetes?.storage_classes ?? [];
  const fallback = classes.find((c) => c.default);
  const volumes = [...new Set(migration.report.services.flatMap((s) => s.findings.filter((f) => (f.code === 'volume_named' || f.code === 'volume_external') && f.detail).map((f) => f.detail ?? '')))];
  const [choices, setChoices] = useState<Record<string, Choice>>(() => Object.fromEntries(volumes.map((v) => [v, migration.choices.volumes[v] ?? { storage_class: '', size: '', access_mode: 'ReadWriteOnce' }])));
  if (volumes.length === 0) return null;
  const set = (v: string, patch: Partial<Choice>) => setChoices({ ...choices, [v]: { ...choices[v], ...patch } });
  return <form className="dr-stack" aria-label="Storage choices" onSubmit={(e) => { e.preventDefault(); onSave(choices); }}>
    <h4>Storage choices</h4>
    <StateNotice state={inventory.state} onRetry={inventory.reload} />
    {volumes.map((v) => <fieldset key={v}>
      <legend>Volume {v}</legend>
      <label>StorageClass for {v}<select value={choices[v]?.storage_class ?? ''} disabled={busy} onChange={(e) => set(v, { storage_class: e.target.value })}>
        {fallback ? <option value="">Cluster default ({displayName(fallback.name)})</option> : <option value="">Choose a StorageClass</option>}
        {classes.map((c) => <option key={c.name} value={c.name}>{displayName(c.name)}</option>)}
      </select></label>
      <label>Size for {v}<input value={choices[v]?.size ?? ''} placeholder="10Gi" disabled={busy} onChange={(e) => set(v, { size: e.target.value })} /></label>
      <p>Access mode: ReadWriteOnce.</p>
    </fieldset>)}
    <button disabled={busy}>Save storage choices</button>
  </form>;
}

function MigrationConfirm({ label, busy, onConfirm }: { label: string; busy: boolean; onConfirm: (note: string) => void }) {
  const [note, setNote] = useState('');
  return <form className="dr-stack" onSubmit={(e) => { e.preventDefault(); onConfirm(note); }}>
    <label>Note for {label.toLowerCase()}<input value={note} maxLength={500} disabled={busy} onChange={(e) => setNote(e.target.value)} /></label>
    <button disabled={busy || !note.trim()}>{label}</button>
  </form>;
}
```

In `web/src/tenant.ts`, replace

```ts
export interface KubernetesInventory { nodes: KubeNode[]; namespaces: string[]; workloads: Workload[]; pods: Pod[]; services: KubeService[]; claims: Claim[] }
```

with

```ts
// default is the cluster's is-default-class annotation; a migration's storage choices pick from these.
export interface StorageClass { name: string; default: boolean }
export interface KubernetesInventory { nodes: KubeNode[]; namespaces: string[]; workloads: Workload[]; pods: Pod[]; services: KubeService[]; claims: Claim[]; storage_classes?: StorageClass[] }
```

and replace

```ts
// Mirrors permissions.Allows(role, ApplicationPolicy): only organization admins edit update policies.
```

with

```ts
// Mirrors permissions.Allows(role, ApplicationMigrate): only organization admins migrate applications.
export const canMigrate = (role: string | undefined) => role === 'organization_admin';

// Mirrors permissions.Allows(role, ApplicationPolicy): only organization admins edit update policies.
```

In `web/src/components/Applications.tsx`, add `import { ApplicationMigration } from './ApplicationMigration';` after the `ApplicationComparison` import, change the tenant import to `import { canEnroll, canManagePolicies, canMigrate, useTenantResource, type MemberOrganization } from '../tenant';`, and replace

```tsx
<ApplicationDeploymentPlan base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} latestRevision={draft.latest_revision} instance={i} refreshKey={planKey} /></div>)}
```

with

```tsx
<ApplicationDeploymentPlan base={`${base}/${encodeURIComponent(draft.id)}`} instanceID={i.id} latestRevision={draft.latest_revision} instance={i} refreshKey={planKey} /><ApplicationMigration key={`migration/${i.id}`} base={`${base}/${encodeURIComponent(draft.id)}`} org={org} env={env} instance={i} admin={canMigrate(role)} onOpen={(id) => { drafts.reload(); instances.reload(); setSelected(id); }} /></div>)}
```

- [ ] **Step 4: Implement the code sentences**

In `web/src/components/ApplicationInspection.tsx`, replace

```tsx
  k8s_volume: 'mounts a volume; Kubernetes deployment of stateful services arrives with the migration analyzer',
```

with

```tsx
  k8s_volume: 'mounts a host path, or more than eight named volumes; a Deployment takes named volumes only, as claims',
```

and replace

```tsx
  k8s_namespace: "is mapped to a namespace the cluster's manifest no longer grants",
};
```

with

```tsx
  k8s_namespace: "is mapped to a namespace the cluster's manifest no longer grants",
  k8s_volume_shared: 'mounts a named volume another service mounts too; a ReadWriteOnce claim serves one pod',
};
// K8S_VOLUME_CHOICE replaces k8s_volume's sentence when its detail is choice_required.
export const K8S_VOLUME_CHOICE = "mounts a named volume with no storage choice; choose its StorageClass and size in the migration that created this application";
```

In `web/src/components/ApplicationPreflight.tsx`, change the first import to `import { ApplicationInspection, K8S_VOLUME_CHOICE, unsupportedNames, type InspectionTarget } from './ApplicationInspection';` and in `serviceFindings` replace

```tsx
    const { name, blockers, unsupported } = s as { name?: unknown; blockers?: unknown; unsupported?: unknown };
    if (typeof name !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name)) continue;
    const codes = Array.isArray(unsupported) ? unsupported.filter((c): c is string => typeof c === 'string' && Object.hasOwn(unsupportedNames, c)) : [];
    if (codes.length) lines.push(`${name}: ${codes.map(c => unsupportedNames[c]).join(', ')}`);
```

with

```tsx
    const { name, blockers, unsupported, details } = s as { name?: unknown; blockers?: unknown; unsupported?: unknown; details?: unknown };
    if (typeof name !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name)) continue;
    const codes = Array.isArray(unsupported) ? unsupported.filter((c): c is string => typeof c === 'string' && Object.hasOwn(unsupportedNames, c)) : [];
    const choice = details !== null && typeof details === 'object' && (details as Record<string, unknown>).k8s_volume === 'choice_required';
    if (codes.length) lines.push(`${name}: ${codes.map(c => c === 'k8s_volume' && choice ? K8S_VOLUME_CHOICE : unsupportedNames[c]).join(', ')}`);
```

In `web/src/components/ApplicationDeploymentPlan.tsx`, replace

```tsx
type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[]; pull_reference?: string; pull_digest?: string; mounts?: Mount[]; dropped_mounts?: Mount[]; object?: { namespace: string; name: string } };
```

with

```tsx
type ClaimMount = { claim: string; mount_path: string; read_only?: boolean };
type Claim = { name: string; storage_class: string; size: string; access_mode: string };
type PlannedService = { name: string; reference: string; image_id: string; image_digest: string; container_id: string; replaces: { container_id: string; image_id: string; created_unix: number }; restart: string; ports: { target: number; published: number; protocol: string; host_ip: string }[]; secret_refs: string[]; pull_reference?: string; pull_digest?: string; mounts?: Mount[]; dropped_mounts?: Mount[]; object?: { namespace: string; name: string }; claim_mounts?: ClaimMount[] };
```

replace

```tsx
plan: { project: string; services?: PlannedService[]; containers?: RemovalTarget[]; volumes?: string[]; namespace?: string }; validation?: Validation };
```

with

```tsx
plan: { project: string; services?: PlannedService[]; containers?: RemovalTarget[]; volumes?: string[]; namespace?: string; claims?: Claim[] }; validation?: Validation; migration_id?: string };
```

replace

```tsx
  admission_denied: "The cluster refused the object (quota or policy); check the namespace's quotas and admission policies",
  legacy: LEGACY_OUTCOME,
};
```

with

```tsx
  admission_denied: "The cluster refused the object (quota or policy); check the namespace's quotas and admission policies",
  claim_immutable: 'The claim exists with another StorageClass, size or access mode, and KyYard never changes a claim; delete it deliberately or choose its current settings',
  legacy: LEGACY_OUTCOME,
};
// CLAIM_RETAINED is a removal's skipped volume step with detail retained.
export const CLAIM_RETAINED = 'The claim and its data were kept; delete the PersistentVolumeClaim with kubectl when you no longer need it.';
```

replace

```tsx
export function stepText(s: { code?: string; detail?: string }): string {
  const code = s.code ?? '';
  if (!code) return '';
```

with

```tsx
export function stepText(s: { step?: string; outcome?: string; code?: string; detail?: string }): string {
  const code = s.code ?? '';
  if (!code) return s.step === 'volume' && s.outcome === 'skipped' && s.detail === 'retained' ? CLAIM_RETAINED : '';
```

replace

```tsx
    case 'name_taken':
    case 'conflict':
```

with

```tsx
    case 'name_taken':
    case 'conflict':
    case 'claim_immutable':
```

replace

```tsx
const OBJECT = /^(Deployment|Service|ConfigMap|Secret)\/[a-z0-9][-a-z0-9.]{0,252}$/;
```

with

```tsx
const OBJECT = /^(Deployment|Service|ConfigMap|Secret|PersistentVolumeClaim)\/[a-z0-9][-a-z0-9.]{0,252}$/;
```

replace

```tsx
  if (d.kind === 'remove' && d.plan.namespace) return <p>Deletes the objects labelled as this instance's in namespace {d.plan.namespace}.</p>;
```

with

```tsx
  if (d.kind === 'remove' && d.plan.namespace) return <p>Deletes the objects labelled as this instance's in namespace {d.plan.namespace}. Its PersistentVolumeClaims and their data are kept.</p>;
```

replace

```tsx
    {d.plan.volumes?.length ? <p>Volumes to ensure: {d.plan.volumes.join(', ')}</p> : null}
```

with

```tsx
    {d.plan.volumes?.length ? <p>Volumes to ensure: {d.plan.volumes.join(', ')}</p> : null}
    {d.plan.claims?.length ? <p>Claims to create when missing (never changed or deleted): {d.plan.claims.map(c => `${c.name} (${c.storage_class || 'cluster default'}, ${c.size})`).join(', ')}</p> : null}
```

replace

```tsx
      <td data-label="Mounts">{s.mounts?.length ? <MountList mounts={s.mounts} /> : 'None'}{s.dropped_mounts?.length
```

with

```tsx
      <td data-label="Mounts">{s.mounts?.length ? <MountList mounts={s.mounts} /> : s.claim_mounts?.length ? <MountList mounts={s.claim_mounts.map(c => ({ kind: 'volume', source: c.claim, target: c.mount_path, read_only: c.read_only }))} /> : 'None'}{s.dropped_mounts?.length
```

and replace

```tsx
          <td data-label="Kind">{d.kind === 'remove' ? 'Removal' : 'Apply'}</td>
```

with

```tsx
          <td data-label="Kind">{d.kind === 'remove' ? 'Removal' : d.migration_id ? 'Apply (migration)' : 'Apply'}</td>
```

- [ ] **Step 5: Run the tests to verify they pass, then build**

Run: `cd web && npx tsc -b && npx vitest run`
Expected: PASS, every existing test included (a response the card cannot read renders nothing, so the Applications tests' catch-all `{}` stays inert).

Run (never while `make ci` runs): `make build-web`
Expected: `web/dist` rebuilt; `git status` shows new hashed assets and `web/tsconfig.tsbuildinfo`.

- [ ] **Step 6: DOX and commit**

In `web/AGENTS.md`, `## Local Contracts`, append after the `KubernetesMapping` bullet:

```markdown
- Under each instance, `ApplicationMigration` reads `GET .../migration`. An organization administrator (`canMigrate`, mirroring `application.migrate`) on a Docker instance with none sees "Migrate to a Kubernetes cluster": cluster and namespace selects (clusters and their `deploy_namespaces` only; absent without a cluster) and "Analyze" (`POST .../migration`). An open migration shows the report as one table (service, axis, class badge, `findingText`: the fixed sentence of `MIGRATION_CODES` plus its parameter, an `UnsupportedCodes` detail by its name; unknown codes render nothing), the assumptions, the storage choices form while `analyzed` (per named volume: a StorageClass select from the destination inventory's `storage_classes`, "Cluster default (<name>)" only while a default is reported; a size; access mode fixed `ReadWriteOnce`; `PUT .../choices`), "Analyze again", "Create destination" (disabled until `ready`), "Open <destination>" (selects the destination in the list), the checklist (`CHECKLIST_STEPS` with each step's commands), the two confirmations each with a required note, and "Abandon migration" (confirmed; the destination-kept sentence when `destination_kept`). A destination shows "Migration destination of <source> · <status>" and "Open <source>". Refusals map through `MIGRATION_ERRORS`; server text never renders, and a response that is not a migration renders nothing. Plans show their claims and claim mounts, `claim_immutable` names its `PersistentVolumeClaim/<name>`, a removal's kept claim reads `CLAIM_RETAINED`, a cluster removal says its claims are kept, history marks `Apply (migration)`, and a `k8s_volume` with detail `choice_required` reads `K8S_VOLUME_CHOICE`. `ApplicationMigration.test.tsx` compares `MIGRATION_CODES`, `CHECKLIST_STEPS`, `ASSUMPTIONS`, `STEP_CODES` and `unsupportedNames` with the generated `migration-codes.json` and `protocol-codes.json` exactly.
```

```bash
git add web && make tidy-check lint && git commit -m "feat(web): the Migration card, claim sentences and the migration mark" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 9: Documents, the implementation status and the full gate

**Files:**
- Modify: `docs/application-schema.md` (Kubernetes claims, the Migration section)
- Modify: `docs/agent-protocol.md` (claims, mounts, the retained step, StorageClasses)
- Modify: `docs/authorization-matrix.md` (`application.migrate`)
- Modify: `docs/threat-model.md` (cluster RBAC, the migration row)
- Modify: `README.md` (Kubernetes endpoints; the migration walkthrough and checklist)
- Modify: `KyYard-Implementation-Plan.md` (§4 table, §8 status and the M8 gate)
- Modify: `KyYard-Engineering-Handoff.md` (§9 deferred lines reconciled)

**Interfaces:**
- Consumes: every contract of Tasks 1–8, named exactly as they landed.
- Produces: no code. The operator and protocol documents match the code; the root `AGENTS.md` (Task 3) already indexes `internal/migration/AGENTS.md`.

- [ ] **Step 1: Update the documents**

In `docs/application-schema.md`, replace

```markdown
per service `kubernetes_unsupported` with `k8s_volume` (any volume), `k8s_host_ip`
```

with

```markdown
per service `kubernetes_unsupported` with `k8s_volume` (a bind, more than 8 named mounts, or, with detail `choice_required`, a named volume the revision has no storage choice for), `k8s_volume_shared` (a named volume two services mount), `k8s_host_ip`
```

In `docs/application-schema.md`, replace

```markdown
Removal deletes, by the instance label (Secrets by name), only KyYard's objects. PR 22 adds StatefulSets, persistent volume claims and the migration analyzer that classifies each service as `supported`, `operator_choice_required` or `blocked`.
```

with

```markdown
Removal deletes, by the instance label (Secrets by name), only KyYard's objects, and keeps every claim (below). StatefulSets stay out of scope.

**Claims (M8 PR 22).** A revision's `kubernetes` extension (`{"volumes": {"<declared volume>": {"storage_class", "size", "access_mode"}}}`: 1..16 declared volumes, `storage_class` `""` for the cluster default or a DNS-1123 subdomain, `size` a whole number of `Mi`, `Gi` or `Ti` from 1Mi to 16Ti, `access_mode` `ReadWriteOnce`) is covered by the spec digest; the Compose importer never sets it, a migration's destination revision does. A chosen named volume one service mounts plans as the claim `<project>-<volume>` (the same naming rule over the declared volumes) and a mount of it; the frame carries both. The agent creates a missing claim before the service's other objects and never updates one: another owner's claim is `name_taken`, this instance's with another class (when one is named), size or access mode is `claim_immutable`, both at the precondition before any write. A claim created with the cluster default keeps whatever class the cluster gave it. Removal reports each claim as a skipped `volume` step with detail `retained`; deleting the data is the operator's deliberate `kubectl delete pvc`.

## Migration (M8 PR 22)

An organization administrator (`application.migrate`) analyzes an application adopted on a Docker host against a namespace a cluster grants; KyYard creates a destination application on the cluster and never stops, changes or removes the source. Migration 35 adds `application_migrations` (one open migration per source: status `analyzed`, `destination_created`, `validated`, then `cutover_confirmed` or `abandoned`; the analyzed revision, `ready`, the report, the choices, each confirmation's actor, time and note) and `deployments.migration_id`, which every apply of an open migration's destination records.

- Analysis reads the source's latest revision, its mapped containers and volumes from a fresh inventory, and a plan-time inspection of each container (the plan's budget), and classifies every service on every axis: storage (`volume_named` and `volume_external` need a StorageClass and size, then are supported; `volume_named_shared` and `volume_bind` are blocked), networking (`network_host` blocked; `networks_multiple`, `networking_supported`), ports (`port_published`, `port_unpublished`; `port_host_ip` blocked), secrets (`secrets_supported`: values are copied), probes (`healthcheck_dropped`, a choice), resources (`resource_limits_dropped`, a choice), scheduling (`scheduling_blocked` for `pid_mode`, `ipc_mode`, `cgroup_parent`, `userns_mode`, `runtime`), flags (`flag_blocked` for `privileged`, `capabilities`, `security_opt`, `devices`, `user` and any other inspection code; `restart_policy` blocked for `no` and `on-failure`; `read_only_rootfs` a choice, because the definition cannot carry it). Without an inspection the probes, resources, scheduling and flags axes are `inspection_unavailable`, a choice: absence is never read as support. The report is ready when nothing is blocked or awaits a choice.
- Storage choices are held to the analyzed revision's named volumes and the StorageClasses the destination reports (`""` only while it reports a default); every choice change re-analyzes.
- The destination is a new application `<name> on <endpoint>`: revision 1 is the analyzed revision with the `kubernetes` extension, its values are the analyzed revision's re-sealed for the new application, and it is mapped to the namespace. It plans, applies and removes through the Kubernetes path above. A source whose definition changed since the analysis is refused (`migration_stale`).
- The checklist (label and grant the namespace, create and apply the destination, copy each volume, validate, switch traffic, confirm cutover) is display only; status moves by the operator's `validated` and `cutover` confirmations. The source cannot be removed while its migration is open (`migration_open`). Abandoning keeps a created destination.
- Out of scope: copying data, switching traffic, StatefulSets, `ReadWriteMany`, rendering probes, resources or `readOnlyRootFilesystem`, deleting claims, Kubernetes to Docker, more than one open migration per application.
```

In `docs/agent-protocol.md`, replace

```markdown
- Unsupported codes add `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace` (a plan's refusal; never an agent's). A cluster snapshot's Deployments carry `application` and `instance` from KyYard's labels.
```

with

```markdown
- Unsupported codes add `k8s_volume`, `k8s_host_ip`, `k8s_restart`, `k8s_name`, `k8s_namespace`, `k8s_volume_shared` (a plan's refusal; never an agent's). A cluster snapshot's Deployments carry `application` and `instance` from KyYard's labels.
- Claims (M8 PR 22): `kubernetes.claims` lists at most 16 `{name, storage_class, size, access_mode}` (name a DNS-1123 label, class `""` or a DNS-1123 subdomain, size a whole number of `Mi`, `Gi` or `Ti` from 1Mi to 16Ti, access mode `ReadWriteOnce`), each mounted by exactly one service through `volumes: [{claim, mount_path, read_only}]` (at most 8, clean absolute paths distinct per service); a Docker service carries no `volumes` and a removal no claims. The agent reads each claim at the precondition (`name_taken` or `claim_immutable`, detail `PersistentVolumeClaim/<name>`), creates a missing one first in `create`, and never updates or deletes one. A removal lists the instance's claims and reports each as `{step: volume, outcome: skipped, detail: retained}` under the service label that mounts it; that is the only step with an outcome of `skipped` that carries a detail.
- `kubernetes.storage_classes` (`{name, default}`, at most 100, cut named `storage_classes`) is read cluster-wide; `default` is the `storageclass.kubernetes.io/is-default-class` or its beta annotation.
```

In `docs/authorization-matrix.md`, replace

```markdown
| Update-policy runs (implemented, M7b) |
```

with

```markdown
| `application.migrate` (analyze for a cluster, storage choices, create the destination, confirm validation and cutover, abandon; implemented, M8) | ✓ | – | – | – | – | the destination receives a re-sealed copy of the source's values, never shown | success and failure on `<app>/migration/<id>` (a refusal before the migration is resolved on `<app>/migration`), and a second success row on `<destination>/migration/<id>` when the destination is created; reading a migration is `application.read` |
| Update-policy runs (implemented, M7b) |
```

In `docs/threat-model.md`, replace

```markdown
persistentvolumeclaims, deployments, statefulsets and daemonsets, and `create` on `selfsubjectaccessreviews`
```

with

```markdown
persistentvolumeclaims, deployments, statefulsets, daemonsets and storageclasses, and `create` on `selfsubjectaccessreviews`
```

In `docs/threat-model.md`, replace

```markdown
Deployments, Services, ConfigMaps and Secrets, Secrets without `list`;
```

with

```markdown
Deployments, Services, ConfigMaps and Secrets, Secrets without `list`, and PersistentVolumeClaims with `get`, `list` and `create` only, so neither a compromised agent nor a removal can delete a claim's data;
```

In `docs/threat-model.md`, replace

```markdown
| Compromised control plane | Out of scope for containment
```

with

```markdown
| Migration of an application to a cluster | Only an organization administrator migrates. Analysis inspects the source's containers through the plan-time primitive, under the plan's per-actor budget and the actor's re-checked authority; observations are used once and dropped, and the report carries codes and names, never values or command lines. The destination's values are a copy of the source's bundle, decrypted inside the destination-creation transaction and sealed under the same key bound to the new application and revision; nothing reaches a response or the audit trail. KyYard never stops or removes the source: removal is refused while its migration is open, and the operator confirms validation and cutover with notes recorded against their names. Claims are created once, never updated, and kept on removal, so an apply or a removal cannot destroy data | `TestMigrationLifecycle`, `TestMigrationKeepsTheSource`, `TestMigrationAuthorization`, `TestMigrationOverTheAPI`, `TestMigrationRolesAndRuntimes`, `TestDeployClaimPreconditions`, `TestRemoveRetainsClaims`, `TestManifestOnARealCluster` |
| Compromised control plane | Out of scope for containment
```

In `README.md`, replace

```markdown
Stateless only in this release: a service with a volume, a port bound to a host address, or a
restart policy other than `always`/`unless-stopped` stops at the plan with the reason.
```

with

```markdown
A service with a host path, a port bound to a host address, or a restart policy other than
`always`/`unless-stopped` stops at the plan with the reason; a named volume needs a storage choice,
which a migration (below) records.
```

In `README.md`, replace

```markdown
## Persistent keys
```

with

```markdown
### Migrating a Docker application to a cluster

An organization administrator opens an application adopted on a Docker host and, under
**Migrate to a Kubernetes cluster**, picks a cluster and one of its namespaces and selects
**Analyze**. KyYard reads the definition and a live inspection of each container and shows every
service on every axis (storage, networking, ports, secrets, probes, resources, scheduling, flags)
as supported, a choice to make, or blocked, each with its reason. Host paths, a volume two
services share, host networking, host-bound ports, privileged and host-namespace settings, and a
restart policy of `no` or `on-failure` are blocked: change the definition or the container, then
**Analyze again**. For each named volume choose a StorageClass the cluster reports and a size
(`Mi`, `Gi` or `Ti`; Docker reports no volume size); it becomes a `ReadWriteOnce`
PersistentVolumeClaim named `<application>-<volume>`.

When the report is ready, **Create destination** makes a new application, `<name> on <cluster>`,
mapped to the namespace, with a copy of the source's environment values. Then follow the
checklist; KyYard never stops, changes or removes the source for you:

1. Label the namespace `pod-security.kubernetes.io/enforce=baseline` and apply the regenerated
   manifest: this release adds `persistentvolumeclaims` `get, list, create` to each namespace's
   Role and `storageclasses` `get, list` to the ClusterRole.
2. Open the destination application, plan and apply it.
3. Stop writes to the source, then copy each volume into its claim with the recipe the checklist
   prints (`docker run --rm -v <volume>:/from:ro busybox tar -C /from -cf - . | kubectl -n <ns> exec -i deploy/<name> -- tar -C <path> -xf -`).
4. Validate the destination and select **Confirm validation** with a note.
5. Point your DNS or Ingress at the destination.
6. Select **Confirm cutover** with a note, then remove the source with KyYard's removal, which
   is refused while the migration is open.

**Abandon migration** closes it and keeps a created destination. Claims survive the removal of
their application; delete one deliberately with `kubectl -n <ns> delete pvc <name>` when its data
is no longer needed. Health probes, resource limits and a read-only root filesystem are not
carried over: add them to the Deployment after cutover.

## Persistent keys
```

In `KyYard-Implementation-Plan.md`, replace

```markdown
| Migration | Proposed `internal/migration`, created in M8 only |
```

with

```markdown
| Migration | `internal/migration` (M8 PR 22): the pure Docker-to-Kubernetes analyzer |
```

In `KyYard-Implementation-Plan.md`, replace

```markdown
Next: M8 PR 22 (StatefulSets, persistent volume claims and the migration analyzer).
```

with

```markdown
Implemented M8 PR 22 (`feat/migration-analysis`): an organization administrator analyzes a Docker-adopted application against a granted namespace of a cluster (`internal/migration`, pure: every service on every axis as supported, a choice or blocked, with a fixed code), records a StorageClass and size per named volume, and creates a destination application (`<name> on <cluster>`, revision 1 = the analyzed revision with a `kubernetes` storage extension, the values copied and re-sealed) that plans and applies through PR 21's path with PersistentVolumeClaims created once and kept on removal; migration 35 records the migration and each destination apply's `migration_id`. Data copy and traffic switching are the operator's checklist steps, confirmed with notes; the source cannot be removed while its migration is open. StatefulSets and `ReadWriteMany` stay out of scope. The real-cluster test mounts a claim of the default StorageClass, applies twice and removes, locally with `KY_TEST_KUBECONFIG`.

M8 gate: the stateless reference definition previews and deploys on Docker and Kubernetes (PR 21); a stateful definition stops with specific blockers and required operator actions until its storage choices are made through a migration (PR 22); Docker operations are unchanged. Next: the 0.1 human acceptance script (section 7) on both runtimes.
```

In `KyYard-Engineering-Handoff.md`, replace

```markdown
- Kubernetes adapter.

```

with

```markdown
- Kubernetes adapter (delivered after 0.1 in M8 PRs 20 and 21).

```

In `KyYard-Engineering-Handoff.md`, replace

```markdown
- Docker-to-Kubernetes migration analyzer.

```

with

```markdown
- Docker-to-Kubernetes migration analyzer (delivered after 0.1 in M8 PR 22: analysis, storage choices and a destination application; copying data and switching traffic stay the operator's steps).

```

- [ ] **Step 2: DOX closeout**

Re-read the DOX chain for every path the branch touched: root `AGENTS.md`, `internal/agent/AGENTS.md`, `internal/store/AGENTS.md`, `internal/permissions/AGENTS.md`, `internal/migration/AGENTS.md`, `internal/runtime/AGENTS.md`, `internal/runtime/kubernetes/AGENTS.md`, `internal/api/AGENTS.md`, `web/AGENTS.md`. Run `git diff master --stat -- '*AGENTS.md'` and confirm each directory whose contract changed has its bullet (Tasks 1–8). `internal/runtime/AGENTS.md` is left unchanged: its Kubernetes child owns every claim rule, and the parent's index entry still describes it. Search for text the slice made stale and fix it where found: `grep -rn "arrives with the migration analyzer\|Stateless only in this release\|PR 22 adds" --include='*.md' --include='*.tsx' --include='*.go' --exclude-dir=superpowers --exclude-dir=node_modules .` prints nothing (the plans and specs under `docs/superpowers` quote the old text on purpose).

- [ ] **Step 3: The full gate**

Run: `gofmt -l cmd internal && test "$(go list -deps ./cmd/server | grep -c k8s.io)" = 0 && test "$(go list -deps ./internal/migration | grep -c k8s.io)" = 0`
Expected: no file listed; both counts 0.

Run: `make ci`
Expected: `==> Local CI checks passed` (tidy, gofmt and vet, `go test -race ./...`, the web suite, the smoke test). Do not run `make build-web` meanwhile. The existing `applies on typed confirmation and polls until settled` web test reads the fetch mock synchronously after a click and failed once in the planner's run while the race suite and smoke test loaded the machine (it passes alone and on rerun); if it fails, rerun `make test-web` before looking further, and do not paper over a different failure.

Run: `PG=… make test-postgres`
Expected: every package `ok` against PostgreSQL 17 (`PG=…` sets `KY_TEST_POSTGRES_DSN` as in Global Constraints).

With a disposable kind cluster (see Task 6): `KY_TEST_KUBECONFIG=<kubeconfig> KY_TEST_DEPLOY_IMAGE=registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a go test -count=1 -run TestManifestOnARealCluster ./internal/runtime/kubernetes/`
Expected: `ok`.

Run: `git status --short web/dist` after `make ci`
Expected: nothing: the committed `web/dist` matches the source (Task 8 rebuilt it).

- [ ] **Step 4: Commit**

```bash
git add README.md docs KyYard-Implementation-Plan.md KyYard-Engineering-Handoff.md && make tidy-check lint && git commit -m "docs: migration analysis, claims and the M8 gate" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
