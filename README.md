# OpenSeal

![OpenSeal - autonomous agent kernel](./openseal.jpeg)

A prompt-first, durable kernel for autonomous Agents and Teams. OpenSeal keeps
multi-objective work, collaboration, skills, approvals, activity, and recovery
semantics portable across standalone and embedded deployments. Deterministic
workflows remain an optional runbook layer rather than the primary authoring
model.

## Quick Start

Build OpenSeal and start its durable local daemon:

```bash
git clone https://github.com/axiom-studio/openseal.git
cd openseal
make build
./openseal daemon
```

In another terminal, open the prompt-first workspace:

```bash
./openseal
```

Running the binary without a subcommand opens the TUI. It discovers the
server's versioned capabilities before rendering actions and stores no
authoritative state of its own. Canonical work is persisted by default in
`data/openseal.db`, so it remains available after either terminal exits or the
daemon restarts. See [Terminal UI](docs/tui.md) for workspace and keyboard
options.

To run the daemon through Docker Compose instead:

```bash
make docker-up
make docker-logs
make docker-down
```

**What's included:**
- Durable OpenSeal kernel and worker pool
- Prompt-first terminal workspace
- REST API at `http://localhost:8080/api/v1`
- Webhook server at `http://localhost:9090`
- SQLite persistence
- Optional deterministic runbooks and triggers

## Manual Build

```bash
# Build the binary
make build

# Run daemon
./openseal daemon --config docker/daemon.yaml

# Open the TUI (also the default with no subcommand)
./openseal tui --owner team:platform

# Validate a workflow
./openseal validate workflows/hello.hcl

# Run a workflow directly
./openseal run workflows/hello.hcl
```

## Project Structure

```
openseal/
├── cmd/              # Binary entrypoints
├── pkg/              # Core packages
│   ├── executor/     # Workflow execution engine
│   ├── runtime/      # Objectives, Runs, recovery, policy, collaboration
│   ├── kernelapi/    # Versioned public HTTP contract
│   ├── client/       # Thin kernel API clients
│   ├── validation/   # HCL workflow validation
├── internal/         # Internal utilities
│   ├── server/       # Versioned REST API
│   ├── daemon/       # Daemon config and trigger manager
│   ├── tui/          # Prompt-first terminal client
│   └── workflow/     # HCL parser
├── docker/           # Docker config files
├── workflows/        # Sample workflows
├── charts/           # Helm charts
├── skills/           # Core skills
└── embedded_nodes/   # Node schema definitions
```

## REST API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/health` | GET | Health check |
| `/api/v1/capabilities` | GET | Discover versioned operations |
| `/api/v1/agent-runs` | POST, GET | Create or list canonical durable work |
| `/api/v1/agent-runs/{id}` | GET | Inspect canonical work |
| `/api/v1/agent-runs/{id}/commands` | POST | Pause, resume, stop, or guide work |
| `/api/v1/skills` | GET | List available skills |
| `/api/v1/workflows` | GET | List loaded workflows |
| `/api/v1/workflows/{id}/run` | POST | Trigger execution |
| `/api/v1/runs` | GET | List execution runs |
| `/api/v1/runs/{id}` | GET | Get run detail |

## Development

```bash
# Run tests
make test

# Run vet
make vet

```

## License

Apache License 2.0 - see LICENSE file for details.
