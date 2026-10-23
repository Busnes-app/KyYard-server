# Multi-stage build for kyyard-server

# Stage 1: Go binaries. The frontend is the committed web/dist that CI rebuilds and checks
# against source, embedded as is, so the image ships exactly the checked bytes. The tag must
# match go.mod's toolchain line (CI checks); only what the build reads enters this stage.
FROM golang:1.27.3-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/embed.go web/
COPY web/dist/ web/dist/
# Release builds pass the tag's version (X.Y.Z or X.Y.Z.N); other builds report internal/appversion/VERSION.
# Both binaries carry it: the server in capsule manifests and MCP, the agent in its hello frame.
ARG VERSION=
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/Busnes-app/kyyard-server/internal/appversion.release=${VERSION}" -o kyyard-server ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/Busnes-app/kyyard-server/internal/appversion.release=${VERSION}" -o kyyard-agent ./cmd/agent

# Stage 2: Minimal Production Container
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
# No USER: the entrypoint starts as root only to hand /data and the Docker socket's group to
# kyyard (uid 10001), then runs the server as kyyard. /app stays root-owned.
RUN apk --no-cache add ca-certificates tzdata su-exec \
 && addgroup -g 10001 -S kyyard \
 && adduser -u 10001 -S -G kyyard -H -h /nonexistent kyyard
WORKDIR /app
COPY --from=backend-builder /app/kyyard-server /app/kyyard-agent /app/
COPY --chmod=0755 scripts/docker-entrypoint.sh /app/docker-entrypoint.sh
# /data/backups holds sealed local capsules inside the data volume; KY_BACKUP_DIR is set by the
# operator (compose does), so an image run bare keeps no local copies.
RUN mkdir -p /data/backups && chown -R kyyard:kyyard /data

ENV KY_PORT=9273
ENV KY_HOST=0.0.0.0
ENV KY_DATA_DIR=/data

EXPOSE 9273
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/app/kyyard-server", "healthcheck"]

ENTRYPOINT ["/app/docker-entrypoint.sh"]
