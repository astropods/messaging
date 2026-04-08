# syntax=docker/dockerfile:1

# Multi-stage build for Go messaging service
# Supports multi-platform builds (linux/amd64, linux/arm64)

# Stage 1: Build playground UI
FROM oven/bun:1 AS playground-builder
WORKDIR /playground
COPY playground/package.json playground/bun.lock* ./
RUN bun install --frozen-lockfile
COPY playground/ .
RUN bun run build

# Stage 2: Build Go binary
FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

WORKDIR /app

# Allow Go to download required toolchain version
ENV GOTOOLCHAIN=auto

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies with cache
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code
COPY . .

# Overwrite the dist placeholder with the real playground build output
COPY --from=playground-builder /playground/dist ./internal/adapter/web/dist/

# Build arguments for version information and target platform
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH

# Build static binary with cache
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-w -s \
      -X 'github.com/astropods/messaging/internal/version.Version=${VERSION}' \
      -X 'github.com/astropods/messaging/internal/version.Commit=${COMMIT}' \
      -X 'github.com/astropods/messaging/internal/version.BuildDate=${BUILD_DATE}'" \
    -o messaging \
    ./cmd/server

# Stage 3: Runtime
FROM debian:bookworm-slim

# Install CA certificates for HTTPS
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN groupadd -g 1000 astro && \
    useradd -u 1000 -g astro -s /bin/sh astro

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/messaging .

# Copy config (optional - can be overridden by env vars)

# Set ownership and switch to non-root user
RUN chown -R astro:astro /app
USER astro

# Expose ports: 8080 (HTTP/SSE), 9090 (gRPC), 9091 (Prometheus metrics)
EXPOSE 8080 9090 9091

# Run the binary
CMD ["./messaging"]
