# Kubernetes Run Workload Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An organization administrator creates a new Deployment on a cluster from an image, from a "Run a workload" page that mirrors Docker's "Run a container", and lands on its workload page.

**Architecture:** The existing `workload.apply` frame gains a `create` flag. On create the agent renders a plain Deployment (no KyYard-managed label, so the workload page can edit it afterwards) from the `WorkloadConfiguration`, checks the namespace's Pod Security enforcement and an access review for `create deployments`, refuses an existing name, creates the object and waits for its rollout. The server accepts `POST .../workloads` with `{spec, confirm}` under `container.configure`, requires a granted namespace and no inventory workload of that name, and stores a `workload.run` direct command settled by the same `deployment.result` path. The web adds a run route, a `mode: 'run'` for the workload form, a toolbar link on the cluster view, and the "Last change" panel with a link to the new workload.

**Spec:** `docs/superpowers/specs/2026-09-29-container-management-design.md` section 3 (parity with 2.6's run form). Decisions not in the spec are rulings recorded in the plan.

## Global Constraints

- Permission `container.configure` (organization administrators; service tokens refused); rate bucket `configure:` 12/min; capability `kubernetes.workloads.run` (501 without; parity-era agents advertise only `kubernetes.workloads`); namespace must be in `deploy_namespaces` (422 `namespace_not_granted`); Pod Security baseline/restricted enforced in the namespace (`pod_security`), same as apply.
- Only `deployment` kind can be run. The created Deployment carries `app.kubernetes.io/name: <name>`, `app.kubernetes.io/instance: <name>` and `kyyard.busnes.app/run: <name>` (object, selector and pod labels), never `app.kubernetes.io/managed-by: kyyard`, `restartPolicy: Always`, `automountServiceAccountToken: false`, strategy from the spec (default `RollingUpdate`), replicas from the spec (default 1, 0..1000), containers from the spec (image, command, args, env literal/secret/configmap refs, resources), `progressDeadlineSeconds` 540 as Deploy; nothing else is set (no securityContext, probes, volumes).
- Wire change additive: `WorkloadApply.Create bool` (`json:"create,omitempty"`); when set, `ResourceVersion` and `Spec.ResourceVersion` must be empty and the target kind `deployment`; existing apply frames are byte-identical (golden).
- Result: service `workload`, steps `precondition`, `create`, `rollout`; codes `name_taken` (object exists or inventory lists it), `forbidden`, `pod_security`, `admission_denied`, `rollout_timeout`, `configuration_unreported` (unsupported non-empty).
- Fixed texts only; `secureFetch`; every control labelled; `web/dist` rebuilt in the last task; DOX docs updated in the same PR. Commit trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Real-cluster behaviour stays unproven (no cluster); the PR says so.

## Review Focus

1. A name that exists in the cluster but not yet in the last inventory must be refused by the agent (`name_taken`), never overwritten. Task 1.
2. A run into a namespace that is granted but lacks Pod Security enforcement must be refused (`pod_security`) before the create. Task 1.
3. A created Deployment must be editable afterwards through the workload page (not treated as managed) and must not be adopted by application removal paths. Task 1 (labels) and Task 2 (managed check reads the inventory `application` label only).
4. A second run while one is in flight on the endpoint must be refused `command_in_progress`. Task 2.
5. The form must require namespace, name, image and the typed name, and must not post refs with values. Task 3.

---

### Task 1: Protocol flag and runtime create path

**Files:** `internal/agent/protocol/kubernetes_workloads.go` (+test), `internal/runtime/kubernetes/workloads.go` (+test), `internal/runtime/kubernetes/AGENTS.md`.

- Protocol: `WorkloadApply.Create bool`; `Validate`: when `Create`, `ResourceVersion == ""`, `Spec.ResourceVersion == ""`, `Target.Kind == "deployment"`; `validSpec` takes a flag so the resource version is required only for edits; golden test that a non-create frame marshals identically; refusal cases (create with version, create of a statefulset, create with unsupported).
- Runtime: `ApplyWorkload` branches on `req.Create`: `precondition` = `review(create apps/deployments in ns)` → `forbidden`; `podSecurity`; `Get` → exists → `name_taken`; `create` = `renderWorkload(ref, spec)` (new function, unit-tested field by field: labels, selector, replicas, strategy, containers via `envVars`/`setQuantity`, restart policy, automount false, no managed label) then `Create`; `AlreadyExists` → `name_taken`; 403 → `admission_denied`; `rollout` = existing `rollout` wait; results validate. Fake-clientset tests for each refusal and the happy path (created object matches the render, rollout waited).
- Commit `kubernetes: create a Deployment from a workload spec`.

### Task 2: Store and API route

**Files:** `internal/store/commands_workload.go` (+test), `internal/store/commands.go` (action `workload.run` → `container.configure`; `SettleDirectCommand` accepts `workload.run` rows with service `workload`), `internal/api/workload_handlers.go` (+test), `internal/api/server.go`, `internal/api/AGENTS.md`, `internal/store/AGENTS.md`.

- `POST /api/organizations/{organization}/endpoints/{endpoint}/workloads` body `{spec, confirm}`; `spec.target` = `{namespace, kind: "deployment", name}`; `confirm == name`; `workloadGate` order (service token 403, rate, permission, runtime, capability); `CreateWorkloadRun`: namespace granted (422 `namespace_not_granted`), inventory present, no workload of that kind/name in the inventory (422 `invalid_spec` blocker `name_taken`), `sweepDirect`/`directBusy` (409 `command_in_progress`), unsupported empty (`configuration_incomplete`), frame `Create: true` validated (`spec_invalid:<field>`), row action `workload.run`, reference `<ns>/deployment/<name>`, audit `workload.run` intent (`containers=N replicas=R`) and outcome via the existing settle audit; dispatch as `TypeWorkloadApply`; 202 with the command; offline 409; delivery failure `deployment_not_sent`.
- Tests: permission matrix; Docker endpoint 409; non-granted 422; name in inventory 422; in-flight 409; happy path settles from `deployment.result` and appears under `?reference=`; no env value in audit/logs (sentinel).
- Commit `api: run a workload on a cluster`.

### Task 3: Web run page

**Files:** `web/src/router.ts` (+test: `workload-new` at `.../workloads/new`, `workloadRunPath`), `web/src/App.tsx`, `web/src/pages/WorkloadPage.tsx` (`WorkloadRunPage`), `web/src/components/WorkloadConfigurationForm.tsx` (`mode: 'run'` with `initial` optional: namespace `<select>` from `deploy_namespaces`, name, image, replicas default 1, containers add/remove allowed in run mode, env/resources as today; Save needs namespace, name, image and the typed name; posts `POST .../workloads` `{spec, confirm}`), `web/src/components/KubernetesCluster.tsx` ("Run a workload" link in the namespace toolbar, gated by `canConfigure` and `kubernetes.workloads` and non-empty `deploy_namespaces`), `web/src/components/workloadTexts.ts` (`name_taken` text, `workload.run` action text), tests, `web/AGENTS.md`.
- After 202: `SentCommand`/"Last change" as on the container run page; on `succeeded`, "Open the new workload" link to `workloadPath(org, endpoint, ns, 'deployment', name, 'overview')`.
- Commit `web: run a workload on a cluster`.

### Task 4: Docs, dist, CI

**Files:** `docs/agent-protocol.md` (Workload apply bullet: `create`), `docs/authorization-matrix.md` (row 84 mentions run), `docs/threat-model.md` (one sentence), `docs/ACCEPTANCE.md` (3c: run step), `README.md` (cluster section), `web/dist`.
- `cd web && npm run build`; `make ci`; commit `docs: run a workload; rebuild web dist`.

## Rulings

- The run creates an unmanaged Deployment (no managed-by label) so it is editable and deletable through the workload page; applications keep their own label space.
- Only Deployments can be run (StatefulSets need claims and DaemonSets need node semantics the form does not carry).
- No securityContext or probes are set; the Pod Security gate plus admission decide what runs.
