# Permissions

## Purpose
Named actions and fixed role mappings for product authorization.

## Ownership
Owns the permission matrix; store owns live membership checks and transactional enforcement, API owns session authentication and trusted request context.

## Local Contracts
- application.adopt also gates explicit service mapping within an adopted instance. application.read may inspect mapping choices; only organization/environment administrators save them. Mapping is neither deployment nor secret authority.
- `application.adopt` and `application.release` belong to organization/environment administrators only. They change recorded ownership without runtime operations or secret access.
- `secret.reveal` permits only organization administrators to use the internal audited application-value resolver. Import authority does not imply reveal authority.
- Application persistence: every active tenant role holds `application.read`; organization/environment administrators hold `application.import` and `application.destroy` (explicit draft discard); those administrators and developers hold `application.edit`. These permissions save desired state only and imply no adoption, deployment, secret-value read or platform-admin bypass.
- `container.exec` is organization-administrator-only, separate from lifecycle operations, logs and environment administration. Platform administrators gain no tenant exec authority.
- Unknown actions/roles deny. Platform `admin` authorizes only platform administration; it never substitutes for organization membership.
- Organization administrators manage all implemented tenant actions, and are the only role holding `organization.members.manage`. Environment administrators read organization/environment data, manage environments, and hold `endpoint.enroll`, `endpoint.update` and `endpoint.revoke`. Every membership role holds `endpoint.read`. Operator, developer and read-only roles read organization/environment data; workload permissions will be added alongside workload APIs.
- Every membership role holds `registry.read` (rows carry no secret); only organization administrators hold `registry.manage` (registries, credentials, anonymous-pull opt-in).
- Membership status and user status are database facts, not part of this pure mapping.

## Work Guidance

## Verification
- `go test ./internal/permissions`

## Child DOX Index
None.

- `container.logs` is separate from `container.read` because a log is the application's own output, which is where credentials and customer data turn up. Every role but the read-only member holds it, including the developer, who operates nothing else.
