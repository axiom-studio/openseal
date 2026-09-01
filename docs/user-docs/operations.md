# Operations

OpenSeal runs as a single binary with a database file and an artifacts directory beside it. This page covers deployment shapes, the two persistent stores, and what the daemon exposes for observability.

## Deployment Shapes

| Situation | What to do |
|---|---|
| Local single-operator use | Run the binary directly with the default loopback bind |
| A contained local service | Use Docker Compose, after reading the port note below |
| A Go program that owns its own lifecycle | Embed the engine and supply a store — see [API](api.md) |
| Multiple processes over one schema | Embed the engine with the PostgreSQL store; the daemon cannot do this |

### Running the Binary

The daemon handles `SIGINT` and `SIGTERM`. On either it stops the kernel and then shuts the HTTP server down gracefully, so durable work is left in a recoverable state rather than truncated mid-Turn.

```bash
./openseal daemon --config ./local.yaml --context ./context.yaml --standalone-operator
```

### Docker

The supplied Dockerfile builds in `golang:1.26-alpine` with `CGO_ENABLED=1` — required for SQLite — and produces an `alpine:3.21` runtime image containing only the binary and CA certificates. It exposes port 8080, declares a health check against `/api/v1/health`, and defaults to `daemon --config /app/daemon.yaml`.

```bash
make docker-up
make docker-logs
make docker-down
```

Compose mounts `docker/daemon.yaml` read-only at `/app/daemon.yaml`, keeps state in a named volume at `/app/data`, publishes `8080:8080`, and applies CPU and memory limits.

> **The published port does not reach the API under the shipped configuration.** The mounted `docker/daemon.yaml` sets `listenAddr: 127.0.0.1:8080`, which binds the container's loopback interface only, while the `8080:8080` mapping forwards to the container's external interface where nothing is listening. The container health check still passes because it runs inside the container against `localhost`. Reaching the API from the host requires editing the mounted configuration to bind `0.0.0.0:8080`. Because OpenSeal has no built-in authentication, do that only behind an authorizing proxy or on an isolated network — see [Security](security.md).

Two environment variables set by the image and Compose are read by nothing: `OPENSEAL_DB_PATH` and `OPENSEAL_LOG_LEVEL`. The database path comes from `storage.path` in the configuration file, and the log level is fixed regardless of either.

## Persistence

OpenSeal ships two persistent stores and an in-memory store. Only one of them is reachable from the daemon.

| Field | Value |
|---|---|
| SQLite | The daemon's only supported driver; also available through the Go facade |
| PostgreSQL | Available through the Go facade only |
| In-memory | The default when an engine is constructed with no store |

> **The daemon rejects every driver except SQLite.** Configuration validation fails if `storage.driver` is not exactly `sqlite`, and the store opener rejects it a second time. PostgreSQL can only be used by a Go program that constructs the store itself and passes it to the engine. There is no configuration path, no flag, and no DSN field that makes the shipped daemon use PostgreSQL.

### SQLite

SQLite is opened with write-ahead logging and a five-second busy timeout, and it runs its migrations on open. Migration is a fixed sequence of 21 steps covering the portfolio, runbook activations, projects, source monitors, source policies, event-source checkpoints and subscriptions, outreach, the agent and team registries, the Skill catalog, Skill source artifacts, action credential lease redemptions, authoring change sets, sensitive authoring prompts, natural team coordination, team coordination defaults, activation continuations, Skill binding lifecycle repair, the callback registry, and embed installations.

The sequence is not versioned in a table. Every step is idempotent and runs on every open.

### PostgreSQL

PostgreSQL is the horizontally safe store, intended for multiple processes sharing one schema. It is materially more careful than the SQLite path.

| Field | Value |
|---|---|
| Default schema | `openseal` |
| Schema name rule | Lowercase SQL identifier, at most 63 characters |
| Version table | `schema_migrations` within the schema |
| Migration leadership | PostgreSQL advisory lock |
| Lock timeout | 20 seconds, configurable up to a maximum of 10 minutes |
| Lock poll interval | 100 milliseconds |
| Max open connections | 16, configurable between 1 and 1024 |
| Max idle connections | 4 |
| Connection lifetime | 30 minutes |
| Connection idle time | 5 minutes |

Every process opening the same schema waits on the same advisory lock, so only one can execute DDL at a time and a rolling deploy cannot race itself into a half-applied schema. A lock wait that exceeds the timeout returns a distinct error rather than proceeding.

The store also exposes an explicit rollback call that removes schema objects introduced after a target version, intended for controlled release rollback.

Both stores implement the same kernel contracts — the agent registry, team registry, Skill catalog, conversations, projects, artifacts, outreach, source monitors, source policies, event-source subscriptions and checkpoints, and the authoring change-set contracts. A deployment does not lose capabilities by choosing PostgreSQL.

### Artifact Content

Artifact content is stored separately from kernel state, in a local content store rooted at `storage.artifactsPath`. Artifact metadata is immutable and versioned; content is addressed by digest and verified on download. Uploads are capped at 100 MiB.

## Observability and Logging

The daemon emits structured JSON logs through a production logger. Startup logs the loaded configuration's log level and API address, the opened store driver and resolved path, the artifact content path, and, when authoring is enabled, the model and scope. Shutdown logs the signal received.

Note that the configured `logLevel` is recorded in that startup line but never applied — the daemon always emits at info level. See [Configuration](configuration.md).

### The Health Route

| Field | Value |
|---|---|
| Path | `GET /api/v1/health` |
| Status | Always `200` |
| Body | `{"status": "ok"}` |

> **There is no metrics endpoint, and the health check is not a readiness check.** No Prometheus client, no expvar, and no `/metrics` route exists. The health handler returns a hard-coded body without touching the store, the workers, or the model provider, so it reports `200` while the database is unreachable, every worker is stalled, and every Run is stuck. Treating it as a liveness signal for the HTTP listener is correct. Treating it as a readiness signal for the system is not.

### The Activity Feed

`GET /api/v1/activity` is the closest thing OpenSeal has to an audit surface, and it is a good one. Every state transition that matters — Run creation, Turn completion, action calls, approvals, agent requests, channel messages — produces an activity event in the same durable transaction as the change it records.

Because the event and the change commit together, the feed cannot drift from the state it describes. An event in the feed is a state change that happened, and a state change that happened has an event.

### PostgreSQL Projections

For hosts embedding the PostgreSQL store, two credential-free projections are available for export.

| Property | Description |
|---|---|
| Pool statistics | Open, in-use, and idle connections, plus wait counts and close counters |
| Migration statistics | Wait duration, total duration, and schema version of the last migration attempt |

A migration observer callback can be installed to receive the same information as lifecycle events. Both projections deliberately exclude the DSN and the schema name.

## Diagnosing a Stalled Deployment

| Symptom | What to check |
|---|---|
| Runs stay `queued` | `GET /api/v1/agent-runs/admission`, then whether any worker scope was derived — see [Configuration](configuration.md) |
| Run creation is not offered at all | Whether any credential entry or outreach-enabled source policy exists; with neither, no workers start |
| A route returns `501` | The capability document; the message names the missing component |
| Approvals cannot be decided | Whether the daemon was started with `--standalone-operator` |
| Workforce apply is refused | The same flag, plus a complete model configuration |
| Health is `200` but nothing progresses | The activity feed, not the health route — health does not inspect workers |
| The container port does not respond | The `listenAddr` in the mounted configuration; the default binds container loopback |

## Next Steps

- Tune the two configuration files in [Configuration](configuration.md).
- Review the exposure model before deploying in [Security](security.md).
- Reference the full route surface in [API](api.md).
