# OpenSeal

![OpenSeal - autonomous agent kernel](./openseal.jpeg)

OpenSeal is a prompt-first, durable kernel for autonomous Agents and Teams. It
provides one portable execution model for objectives, long-running Runs,
collaboration, governed Skills, approvals, activity, artifacts, and recovery.
Deterministic workflows remain available as an optional runbook layer.

```mermaid
flowchart LR
    Intent[Prompt or event] --> Owner[Agent or Team]
    Owner --> Objective[Objective portfolio]
    Objective --> Run[Durable Run]
    Run --> Turn[Bounded turn]
    Turn --> Skill[Governed Skill action]
    Skill -->|allowed| Result[Activity and artifacts]
    Skill -->|approval required| Approval[Durable checkpoint]
    Approval --> Turn
```

## Start locally

Prerequisites: Go 1.26 and a C toolchain for SQLite (`gcc` or `clang`).

```bash
git clone https://github.com/axiom-studio/openseal.git
cd openseal
make build

# A missing config is created with API port 8080 and durable SQLite paths.
./openseal daemon --config ./local.yaml
```

In another terminal:

```bash
./openseal
```

Running `openseal` without a subcommand opens the TUI. The TUI is a thin client:
closing it does not stop work. The generated configuration stores kernel state
in `data/openseal.db` and artifact content in `data/artifacts`, relative to the
configuration file.

The checked-in `daemon.yaml` intentionally uses API port `18080`. When using
that file, connect with `./openseal tui --endpoint http://127.0.0.1:18080`.

Docker Compose is also supported:

```bash
make docker-up
make docker-logs
# API: http://localhost:8080/api/v1
# Webhook listener: http://localhost:9090
make docker-down
```

## What is implemented

- Versioned Agent definitions and durable Agent deployments
- First-class Teams with semantic roles, roster assignments, policy, and
  Team-owned Skill bindings
- Multi-objective portfolios, Initiatives, schedules, event routing, and
  durable Runs with bounded leases and checkpoints
- Typed Skill definitions, least-privilege deployment bindings, action
  approvals, credential references, and execution-time secret boundaries
- OpenClaw/ClawHub source compilation, verified installation, updates, pins,
  uninstall, provenance, and restart restoration
- Agent requests, handoffs, dependency groups, Team channels, participation
  arbitration, cursors, presence, and shared activity
- Immutable artifact metadata, verified local content storage, source evidence,
  and governed outreach records
- SQLite for standalone durability and PostgreSQL for embedded deployments
- Capability-discovered REST API, prompt-first TUI, and stable Go facade
- Optional HCL workflows, cron triggers, and webhook triggers

The standalone daemon deliberately advertises only the operations it has been
configured to execute. For example, prompt-to-workforce authoring appears only
when its model settings are complete, and an embedding host must wire real
identity, credential, policy, and action adapters before those operations are
available. Clients should read `/api/v1/capabilities` instead of assuming a
fixed surface.

## Documentation

- [Documentation index](docs/README.md) — recommended reading order
- [Getting started](docs/getting-started.md) — install, configure, start, and
  verify a local daemon
- [Core concepts](docs/concepts.md) — Agents, Teams, objectives, Runs, Skills,
  approvals, conversations, evidence, and recovery
- [Architecture](docs/architecture/autonomous-agent-runtime.md) — implemented
  layers, execution lifecycle, and extension boundaries
- [Terminal UI](docs/tui.md) — workspace configuration and keyboard model
- [CLI reference](docs/cli.md) — every implemented command and option
- [REST API](docs/api.md) — capability discovery, conventions, and route groups
- [Operations](docs/operations.md) — persistence, model setup, recovery, and
  production embedding
- [OpenClaw compatibility](docs/skills/openclaw-compatibility.md) — compilation,
  lifecycle, guarantees, and security boundaries

## Library embedding

Use `github.com/axiom-studio/openseal/pkg/openseal` as the supported Go facade.
Do not import `internal` packages.

```go
store, err := openseal.NewSQLiteStore("openseal.db")
if err != nil {
    return err
}
defer store.Close()

engine, err := openseal.New(openseal.WithPersistentStore(store))
if err != nil {
    return err
}
engine.Start(ctx)
defer engine.Stop()
```

An engine defaults to an in-memory store, four workflow workers, and the
default retry policy. Use an explicit persistent store for durable work. See
the [architecture guide](docs/architecture/autonomous-agent-runtime.md) for the
adapter boundary.

## Development

```bash
make test
make vet
```

Optional live tests are documented in [Operations](docs/operations.md). They
are not part of the default deterministic test suite.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
