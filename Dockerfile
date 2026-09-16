# Multi-stage build for kyyard-server

# Stage 1: Build React PWA Frontend
FROM node:26-alpine AS frontend-builder
WORKDIR /app/web
COPY web/package*.json ./
RUN npm install
COPY web/ ./
RUN npm run build

# Stage 2: Build Go Standalone Binary
FROM golang:1.27.1-alpine AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=frontend-builder /app/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o kyyard-server ./cmd/server

# Stage 3: Minimal Production Container
FROM alpine:3.24
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=backend-builder /app/kyyard-server /app/kyyard-server
# /data/backups holds sealed local capsules inside the data volume; KY_BACKUP_DIR is set by the
# operator (compose does), so an image run bare keeps no local copies.
RUN mkdir -p /data /data/backups

ENV KY_PORT=8080
ENV KY_HOST=0.0.0.0
ENV KY_DATA_DIR=/data

EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/app/kyyard-server", "healthcheck"]

ENTRYPOINT ["/app/kyyard-server"]
