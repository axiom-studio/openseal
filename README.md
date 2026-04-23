# OpenSeal

A modern agent execution platform for building, deploying, and running AI-powered automation workflows.

## Overview

OpenSeal provides a flexible runtime environment for executing AI agents with support for skills, triggers, and event-driven workflows. It consists of two main binaries:

- **OpenSeal**: The core execution engine
- **Hermes**: The agent runtime coordinator

## Project Structure

```
openseal/
├── cmd/           # Binary entrypoints
├── pkg/           # Core packages
├── internal/      # Internal utilities
├── charts/        # Helm charts
├── skills/        # Core skills
└── embedded_nodes/ # Node schemas
```

## Development

```bash
# Build binaries
make build    # Build openseal
make hermes   # Build hermes

# Run tests
make test

# Run vet
make vet
```

## License

Apache License 2.0 - see LICENSE file for details.
