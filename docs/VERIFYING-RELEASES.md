# Verifying releases

[Product overview](../README.md) · [Restore and pinning](RESTORE.md) · [Assurance case](ASSURANCE-CASE.md)

A KyYard release is the container image `ghcr.io/busnes-app/kyyard:X.Y.Z` (or `X.Y.Z.N` for a
dependency-only release). It carries two binaries: `/app/kyyard-server`, the control plane, and
`/app/kyyard-agent`, which additional hosts run from the same image. The release has no other
downloads. This page shows how to check that an image was built by this repository's release
workflow from the tagged commit, and how to rebuild both binaries yourself and get the same bytes.

## What is signed, and by whom

- `release-image.yml` builds each release from its tag and publishes a SLSA build-provenance
  attestation for the image digest through GitHub's Sigstore integration. It verifies that
  attestation before it moves `:stable`, so `:stable` never points at an unattested image.
- Every push to `master` publishes `:<commit sha>`, attested by `ci.yml` on `master`.
  [RESTORE.md](RESTORE.md) covers pinning those.
- There is no long-lived signing key to download. The signature is tied to a workflow identity,
  and that identity is the thing you check:
  `https://github.com/Busnes-app/KyYard-server/.github/workflows/release-image.yml@refs/tags/v<version>`.
  `gh` fetches Sigstore's public trust root itself.
- Each image also carries an SPDX software bill of materials made by BuildKit. The SBOM sits in
  the image index and is **not** separately signed; trust it as far as you trust the digest you
  verified.

## Verify a release image

You need `gh` (logged in) and `docker` with buildx (`imagetools` talks to the registry; it does
not need a running daemon). Name the version yourself, and verify the version tag, not `:stable`:
tags can move, so the check resolves the digest first and then requires the attestation to name
the commit the release tag points at.

```bash
v=<release version, e.g. 0.4.4>
sha=$(gh api repos/Busnes-app/KyYard-server/commits/refs/tags/v$v --jq .sha) \
  && d=$(docker buildx imagetools inspect ghcr.io/busnes-app/kyyard:$v --format '{{.Manifest.Digest}}') \
  && gh attestation verify "oci://ghcr.io/busnes-app/kyyard@$d" --repo Busnes-app/KyYard-server \
       --cert-identity https://github.com/Busnes-app/KyYard-server/.github/workflows/release-image.yml@refs/tags/v$v \
  && [ "$(gh attestation verify "oci://ghcr.io/busnes-app/kyyard@$d" --repo Busnes-app/KyYard-server \
       --cert-identity https://github.com/Busnes-app/KyYard-server/.github/workflows/release-image.yml@refs/tags/v$v \
       --format json --jq '.[0].verificationResult.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit')" = "$sha" ] \
  && echo "OK: ghcr.io/busnes-app/kyyard@$d is release v$v, built from $sha"
```

Then run that digest, not the tag: `KY_IMAGE=ghcr.io/busnes-app/kyyard@$d` in `.env`
([RESTORE.md](RESTORE.md) has a version of this command that writes `.env` safely). Use the same
digest for the agent on additional hosts.

Any failure means: do not run that image. A different workflow, a branch instead of the tag, or
another repository all fail the `--cert-identity` or `--repo` check.

To read the SBOM:

```bash
docker buildx imagetools inspect ghcr.io/busnes-app/kyyard@$d --format '{{ json .SBOM }}'
```

## Rebuild the binaries and compare

Releases after 0.4.4 are built with `-trimpath` and without VCS stamping, so the same commit, Go
version and version string give the same binaries on any machine and in any directory. CI checks
this on every push. The commands below use Podman; with Docker, write `docker` for `podman` and
drop the `:Z` volume suffix.

```bash
v=<release version>
git clone --branch v$v https://github.com/Busnes-app/KyYard-server.git kyyard-v$v && cd kyyard-v$v
go_version=$(sed -n 's/^FROM golang:\([0-9.]*\)-.*/\1/p' Dockerfile)
for p in server agent; do
  GOTOOLCHAIN=go$go_version CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath \
    -ldflags="-s -w -X github.com/Busnes-app/kyyard-server/internal/appversion.release=$v" \
    -o kyyard-$p-from-source ./cmd/$p
done
cid=$(podman create ghcr.io/busnes-app/kyyard@$d) \
  && podman cp "$cid:/app/kyyard-server" kyyard-server-from-image \
  && podman cp "$cid:/app/kyyard-agent" kyyard-agent-from-image && podman rm "$cid" >/dev/null
sha256sum kyyard-*-from-source kyyard-*-from-image
```

Each `-from-source` hash must match its `-from-image` partner. This works because the image embeds
the committed `web/dist` as is, and CI fails any commit whose `web/dist` differs from a fresh
`npm run build`. The image layers around the binaries (Alpine, CA certificates, time zones,
`su-exec`, the entrypoint script) are not rebuilt bit for bit; the binaries are what you are
checking.

`-buildvcs=false` is needed only because a clone has a `.git` directory; the image build context
excludes `.git`, so Go stamps nothing there.

### Releases up to 0.4.4

These were built without `-trimpath`, so their binaries record the build directory, `/app`, and
the `go build` above gives different bytes. Build them in that same directory instead, inside the
Go image the Dockerfile names, from a `git archive` of the tag (no `.git`, as in the image build).
From the clone, after the `podman cp` lines above:

```bash
mkdir ../app && git archive v$v | tar -x -C ../app
go_image=$(sed -n 's/^FROM \(golang:[^ ]*\) AS backend-builder$/\1/p' Dockerfile)
podman run --rm -v "$(cd ../app && pwd):/app:Z" -w /app "docker.io/library/$go_image" sh -c \
  "for p in server agent; do CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w -X github.com/Busnes-app/kyyard-server/internal/appversion.release=$v' -o /app/kyyard-\$p-from-source ./cmd/\$p || exit 1; done"
sha256sum ../app/kyyard-*-from-source kyyard-*-from-image
```
