# Devices

## Purpose
Manages 90-second ephemeral QR-code device pairing protocols and push notification client registration.

## Ownership
Owns ephemeral PIN code generation, QR payload creation, device verification, and push token linking.

## Local Contracts
- Pairing codes are 6-digit random PINs with strict 90-second TTL (`InitPairing`).
- Pairing requires an authenticated initiating account; successful verification atomically consumes the pending pairing and cannot be replayed.

## Verification
- `go test -v ./internal/devices/...`

## Child DOX Index
None.
