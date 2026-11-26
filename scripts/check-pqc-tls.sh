#!/usr/bin/env bash
# Refuse classical TLS key-exchange overrides and verify hybrid PQ TLS handshakes.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

forbid_pattern() {
  local msg="$1"
  local pattern="$2"
  shift 2
  local found rc=0
  found=$(git grep -nE "$pattern" -- "$@") || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "$msg" >&2
    echo "$found" >&2
    exit 1
  elif [ "$rc" -ne 1 ]; then
    echo "git grep failed ($rc) while checking $pattern" >&2
    exit "$rc"
  fi
}

# Refuse CurvePreferences in non-test Go code (Go's default offers X25519MLKEM768 first).
forbid_pattern \
  "CurvePreferences is forbidden in non-test Go code (overrides Go's hybrid PQ TLS defaults):" \
  '\bCurvePreferences\b' \
  '*.go' ':!*_test.go'

# Refuse tlsmlkem overrides in non-test code, workflows, Dockerfiles, and Compose files.
forbid_pattern \
  "tlsmlkem override is forbidden in code, workflows, Dockerfiles, and Compose files:" \
  'tlsmlkem' \
  '*.go' ':!*_test.go' \
  '.github/workflows/*' \
  'Dockerfile*' ':(glob)**/Dockerfile*' '*.Dockerfile' ':(glob)**/*.Dockerfile' \
  'docker-compose*.yml' 'docker-compose*.yaml' \
  ':(glob)**/docker-compose*.yml' ':(glob)**/docker-compose*.yaml' \
  'compose*.yml' 'compose*.yaml' \
  ':(glob)**/compose*.yml' ':(glob)**/compose*.yaml' \
  'scripts/*' ':!scripts/check-pqc-tls.sh' \
  'Makefile' '.env.example'

# Verify PQ TLS negotiation and classical fallback under default and GODEBUG=fips140=on.
pattern='^Test(Agent|Registry)Client(NegotiatesHybridKeyExchange|AllowsClassicalFallback)$'
for mode in default fips140=on; do
  if [ "$mode" = default ]; then
    if ! out=$(go test -race -count=1 -run "$pattern" ./cmd/agent ./internal/registry 2>&1); then
      printf '%s\n' "$out" >&2
      exit 1
    fi
  else
    if ! out=$(GODEBUG="$mode" go test -race -count=1 -run "$pattern" ./cmd/agent ./internal/registry 2>&1); then
      printf '%s\n' "$out" >&2
      exit 1
    fi
  fi
  printf '%s\n' "$out"
  if grep -q '\[no tests to run\]' <<< "$out"; then
    echo "PQ TLS tests did not match any test ($mode)" >&2
    exit 1
  fi
done
