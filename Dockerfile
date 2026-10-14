# Multi-stage build for kyyard-server

# Stage 1: Go binaries. The frontend is the committed web/dist that CI rebuilds and checks
# against source, embedded as is, so the image ships exactly the checked bytes. The tag must
# match go.mod's toolchain line (CI checks); only what the build reads enters this stage.
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/embed.go web/
COPY web/dist/ web/dist/
# The agent reports this to the server; release-image.yml passes the release version.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o kyyard-server ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o kyyard-agent ./cmd/agent

# Stage 2: Minimal Production Container
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=backend-builder /app/kyyard-server /app/kyyard-agent /app/
# /data/backups holds sealed local capsules inside the data volume; KY_BACKUP_DIR is set by the
# operator (compose does), so an image run bare keeps no local copies.
RUN mkdir -p /data /data/backups

ENV KY_PORT=9273
ENV KY_HOST=0.0.0.0
ENV KY_DATA_DIR=/data

EXPOSE 9273
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/app/kyyard-server", "healthcheck"]

ENTRYPOINT ["/app/kyyard-server"]
