# Permissions

## Purpose
Named actions and fixed role mappings for product authorization.

## Ownership
Owns the permission matrix; store owns live membership checks and transactional enforcement, API owns session authentication and trusted request context.

## Local Contracts
- Unknown actions/roles deny. Platform `admin` authorizes only platform administration; it never substitutes for organization membership.
- Organization administrators manage all implemented tenant actions, and are the only role holding `organization.members.manage`. Environment administrators read organization/environment data, manage environments, and hold `endpoint.enroll`, `endpoint.update` and `endpoint.revoke`. Every membership role holds `endpoint.read`. Operator, developer and read-only roles read organization/environment data; workload permissions will be added alongside workload APIs.
- Membership status and user status are database facts, not part of this pure mapping.

## Work Guidance

## Verification
- `go test ./internal/permissions`

## Child DOX Index
None.
