# Everyday tasks

## Inspect first

Select the intended host and container, then inspect state, image and logs. A health issue may come from the application rather than the container runtime. Pair kyPulse when you need cross-application health and alerting.

## Lifecycle operations

Review the target, mounts and impact before stopping, restarting, editing or deleting a container or Compose project. Preserve volumes and existing overlay chains. A command that recreates a service is not proof that its data or public route remained intact; verify both afterward.

## Fleet changes

Image updates and policies can affect multiple services. Review the proposed scope and use the documented policy workflow. A Compose import is a draft to inspect, not evidence that it is safe to run.

## Recovery

Keep KyYard's persistent keys and recovery configuration. Its capsule protects control-plane data; protect managed applications through their own backup contracts. Read the restore guide before recovering or migrating hosts.
