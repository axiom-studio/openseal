# Build stage
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git npm

WORKDIR /build

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy web frontend and build it
COPY web/ ./web/
RUN cd web && npm install && npm run build

# Copy built web assets for embedding
RUN mkdir -p pkg/webui && cp -r web/dist pkg/webui/dist

# Copy source code
COPY . .

# Re-copy web dist to make sure embed has it
RUN cp -r web/dist pkg/webui/dist

# Build binary
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-w -s" -o openseal ./cmd/openseal

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /build/openseal .

# Create directories for runtime data
RUN mkdir -p /app/workflows /app/data

ENV OPENSEAL_WORKFLOWS_DIR=/app/workflows
ENV OPENSEAL_DB_PATH=/app/data/openseal.db

EXPOSE 8080 9090

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/api/v1/health || exit 1

ENTRYPOINT ["./openseal"]
CMD ["daemon", "--config", "/app/daemon.yaml"]
