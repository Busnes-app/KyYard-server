# Devices

## Purpose
Manages 90-second ephemeral QR-code device pairing protocols and push notification client registration.

## Ownership
Owns QR secret and payload creation, device verification, and push token linking.

## Local Contracts
- This is retained legacy code for storage/backward-compatibility tests. KyYard exposes no SCIM or phone-pairing HTTP routes.
- Verification rejects inactive or password-restricted accounts and returns the pre-consumption user snapshot; session issuance checks that snapshot against concurrent password replacement.
- Pairings are carried only by a 24-byte QR secret with a strict 90-second TTL (`InitPairing`). Never add a short typed code: verification issues a full session wherever it is exposed, so anything guessable inside the window is an account takeover. Unknown, expired and consumed secrets return the same `ErrPairingNotFound`.
- Pairing requires an authenticated initiating account; successful verification atomically consumes the pending pairing and cannot be replayed.

## Verification
- `go test -v ./internal/devices/...`

## Child DOX Index
None.
