# Multi-stage build for ky_server_base

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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ky_server_base ./cmd/server

# Stage 3: Minimal Production Container
FROM alpine:3.24
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=backend-builder /app/ky_server_base /app/ky_server_base
# /app/backups is the optional mount for sealed local capsules; KY_BACKUP_DIR is set by the
# operator (compose does), so an image run bare keeps no local copies.
RUN mkdir -p /app/data /app/backups

ENV KY_PORT=8080
ENV KY_HOST=0.0.0.0
ENV KY_DATA_DIR=/app/data

EXPOSE 8080
VOLUME ["/app/data", "/app/backups"]

ENTRYPOINT ["/app/ky_server_base"]
