# OpenSeal

![Openseal - AI Automation platform](./openseal.jpeg)

A modern agent execution platform for building, deploying, and running AI-powered automation workflows.

## Quick Start (Docker Compose)

The fastest way to run OpenSeal with the full stack:

```bash
# Clone and start
git clone https://github.com/axiom-studio/openseal.git
cd openseal
make docker-up

# Open the web GUI
open http://localhost:8080

# View logs
make docker-logs

# Stop and remove
make docker-down
```

**What's included:**
- OpenSeal daemon with worker pool execution
- Web GUI at `http://localhost:8080`
- REST API at `http://localhost:8080/api/v1`
- Webhook server at `http://localhost:9090`
- SQLite persistence in a named Docker volume
- Sample workflow in `workflows/hello.hcl`

## Manual Build

```bash
# Build binary (includes web frontend)
make build

# Run daemon
./openseal daemon --config docker/daemon.yaml

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
│   ├── runtime/      # Execution store, worker pool, scheduler
│   ├── validation/   # HCL workflow validation
│   └── webui/        # Embedded frontend assets
├── internal/         # Internal utilities
│   ├── server/       # REST API + web GUI server
│   ├── daemon/       # Daemon config and trigger manager
│   └── workflow/     # HCL parser
├── web/              # React SPA frontend source
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
| `/api/v1/skills` | GET | List available executors |
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

# Build web frontend only
make build-web
```

## License

Apache License 2.0 - see LICENSE file for details.
