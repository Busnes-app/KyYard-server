# Troubleshooting

- **Empty instance after an update:** check whether an existing bind mount was replaced by the named-volume default; preserve the original data and use the documented bind overlay.
- **Remote host missing:** inspect agent connectivity, enrollment and fingerprint approval.
- **Permission denied:** check container UID, mounts and agent permissions against the installation reference.
- **Browser cannot connect remotely:** the default listener is loopback; configure the supported HTTPS proxy path.
- **Container restarts but app remains unhealthy:** check application logs and readiness, then public routing.
- **Recovery expectation unclear:** distinguish control-plane recovery from each managed application's data recovery.

When reporting a problem, include the product version, platform, failed task and a redacted error. Exclude pairing links, device secrets, recovery codes, custodian shares and private content.
