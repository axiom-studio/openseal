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

# Ship the config that CMD names. Without it the daemon finds no file at
# /app/daemon.yaml and writes its own built-in defaults there — which bind
# loopback, leaving a plain `docker run -p 8080:8080` unable to reach the API.
# docker-compose.yml mounts its own copy over this one, so this changes nothing
# for compose users and makes the bare image work on its own.
COPY docker/daemon.yaml /app/daemon.yaml

# Apache-2.0 section 4(a) requires recipients of the Work to receive a copy of
# the License, and pushing to a registry distributes it to anyone who pulls.
# The release tarballs already carry LICENSE; this keeps the container channel
# consistent with them rather than relying on the OCI label alone.
COPY LICENSE /app/LICENSE

# Run as an unprivileged user. The API authenticates nothing, so anything
# reached through a route — or any container escape — would otherwise land as
# uid 0. Port 8080 is above 1024, so binding it needs no capability.
#
# Order matters: create the user and chown /app (including /app/data, where the
# daemon writes openseal.db and artifacts/) BEFORE dropping to it, or the
# daemon cannot open its own database.
RUN addgroup -S openseal && adduser -S -G openseal openseal \
 && mkdir -p /app/data \
 && chown -R openseal:openseal /app

USER openseal

ENV OPENSEAL_DB_PATH=/app/data/openseal.db

EXPOSE 8080

# Address 127.0.0.1 explicitly rather than `localhost`: the image resolves
# `localhost` to ::1 as well as 127.0.0.1, wget tries ::1 first, and the daemon
# has no IPv6 listener — so the check failed against a healthy process.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/v1/health || exit 1

ENTRYPOINT ["./openseal"]
CMD ["daemon", "--config", "/app/daemon.yaml"]
