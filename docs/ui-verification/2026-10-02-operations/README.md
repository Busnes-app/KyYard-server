# Operations UI verification

Checked the real App and navigation with a local Vite preview and explicitly synthetic endpoint data (account name “Preview fixture”). No production session, credential or runtime mutation was used. The temporary preview entry and fetch fixtures were removed after inspection.

- Desktop 1440 × 1000: Home shows connected Docker/Kubernetes endpoints first, with runtime counts and direct run links. The inactive Local Docker endpoint links to history without inventing current containers.
- Docker run: basic name/image and confirmation plus four closed optional sections. The form bottom was 809 CSS pixels, inside the desktop viewport.
- Kubernetes run: namespace/name/image, collapsed scale and container settings. The first container follows the workload name. The form bottom was 892 CSS pixels, inside the desktop viewport.
- Phone 390 × 844: Home and Kubernetes run have no horizontal overflow (document width 375 CSS pixels with the vertical scrollbar). Navigation and disclosures remain reachable; long content scrolls vertically.
- Automated checks separately prove YAML preview/confirmation, immutable image-check identity, CSRF/permission/namespace refusal, capability refusal before native dispatch, full native Deployment field preservation, and pull-latest configuration carry-over.

Screenshots: [Home](home-desktop.png), [Docker run](docker-run-desktop.png), [Kubernetes run](kubernetes-run-desktop.png), [phone run](kubernetes-run-mobile.png).

This is layout verification with synthetic data, not the human production acceptance gate. Native Deployment YAML does not create Services/PVCs; those remain in Applications. The installed cluster agent requires an upgrade before the new manifest capability is available.
