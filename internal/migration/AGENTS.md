# Migration

## Purpose
The pure analyzer of a Docker-to-Kubernetes migration: `Analyze(Input) Report` classifies every service of a source definition on every axis and lists the operator's checklist.

## Ownership
Owns the finding, checklist and assumption vocabularies and the report format. `internal/store` owns the migration rows, choices validation and destination creation; `internal/api` gathers the inputs, runs the analyzer and stores its report; `web` owns the sentences.

## Local Contracts
- Pure: imports `internal/store` types and `internal/agent/protocol`, never SQL, a runtime or a `k8s.io` package (`go list -deps ./internal/migration | grep -c k8s.io` prints 0; the tests may import `render` to check label keys). The store never imports this package.
- `Report{version 1, ready, services (sorted by name), checklist, assumptions}`; a service's `class` is its most severe finding's; `ready` is true when no finding is `blocked` or `operator_choice_required`.
- Axes `storage`, `networking`, `ports`, `secrets`, `probes`, `resources`, `scheduling`, `flags`, each with at least one finding per service. `Codes` is the closed vocabulary; `TestVocabularyIsExact` proves every code is produced and nothing else is. A finding's `detail` is its code's parameter: a volume name, a mount target, `<published>/<protocol>`, a restart policy, an `UnsupportedCodes` code or a destination name.
- Networking and ports: `network_host` (blocked) and `networks_multiple` come from the container. With more than one service each service also gets `network_references` (detail its destination name, `KubernetesNames(<destination project>, services)`) and `port_unpublished` is a choice; with one service `networking_supported` fills an otherwise empty axis and `port_unpublished` is supported. Both choices are answered by acknowledging them: `Input.Choices` is `store.MigrationChoices`, and a code in its `Acknowledged` (`store.MigrationAcknowledgeable`) is `supported` with its code and detail kept. The drops `healthcheck_dropped`, `resource_limits_dropped` and `read_only_rootfs` are acknowledged the same way; acknowledged, one without a parameter gets detail `acknowledged`. `inspection_unavailable` is never acknowledgeable.
- Storage: a bind is `volume_bind` (blocked); a named volume two services mount is `volume_named_shared` (blocked); otherwise `volume_named` (`volume_external` when declared external) is `operator_choice_required` until `Choices` holds a valid choice whose StorageClass the destination still reports (`""` while it reports a default), then `supported`.
- Probes, resources, scheduling and flags need the inspection: without one each is `inspection_unavailable` (choice). `image_config` or an inspected health other than `none` is `healthcheck_dropped` (choice); an empty health (the agent lacks `container.inspect.health`) is `inspection_unavailable` with detail `agent` on the probes axis (the web says to upgrade the Docker agent); `resource_limits`/`ulimits` `resource_limits_dropped` (choice); `pid_mode`, `ipc_mode`, `cgroup_parent`, `userns_mode`, `runtime` `scheduling_blocked`; `read_only_rootfs` a choice (the definition cannot carry it); `network` counts as `networks_multiple`; every other inspection code is `flag_blocked`. A restart policy `no` or `on-failure` is `restart_policy` (blocked) with or without an inspection.
- Checklist `grant_namespace` (`kubectl label namespace` without `--overwrite`), `create_destination`, `update_references` (more than one service; `Commands` holds `<service> → <destination name>` pairs), `copy_volume` (only when a single-service named volume exists on the source host; per service: scale the Deployment to 0, `wait --for=delete` its pods by render's `app.kubernetes.io/instance` and `kyyard.busnes.app/service` labels, per claim a `<name>-copy` helper pod (`kubectl run --image="$HELPER_IMAGE" --override-type=strategic` mounting the claim at `/to`, `sleep infinity`), wait, clear `/to` (the destination's first-start data), `docker run ... tar | kubectl exec -i <name>-copy -- tar -C /to -xf -`, delete it, then scale to 1; every name is grammar-held, so nothing needs quoting), `validate_destination`, `switch_traffic`, `confirm_cutover`. Assumption `volume_size_unknown` whenever a named volume is mounted.
- `TestWebVocabularyFixture` writes `Codes`, `ChecklistCodes`, `AssumptionCodes` and `store.MigrationAcknowledgeable` to `web/src/migration-codes.json` (`KY_UPDATE_FIXTURES=1`) and fails while it is stale; the web tests fail while a sentence table misses a code.

## Work Guidance

## Verification
- `go test ./internal/migration` (golden reports in `testdata/`, regenerated with `KY_UPDATE_FIXTURES=1` and reviewed in the diff).

## Child DOX Index
None.
