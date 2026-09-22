# Build stage
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache build-base git

WORKDIR /build

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# SQLite-backed durability requires CGO; Alpine's toolchain produces a
# self-contained musl-linked kernel binary for the runtime image.
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-w -s" -o openseal ./cmd/openseal

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/openseal .

# Create the runtime data directory.
RUN mkdir -p /app/data

ENV OPENSEAL_DB_PATH=/app/data/openseal.db

EXPOSE 8080

# Address 127.0.0.1 explicitly rather than `localhost`: the image resolves
# `localhost` to ::1 as well as 127.0.0.1, wget tries ::1 first, and the daemon
# has no IPv6 listener — so the check failed against a healthy process.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/v1/health || exit 1

ENTRYPOINT ["./openseal"]
CMD ["daemon", "--config", "/app/daemon.yaml"]
