# OpenSeal

![Openseal - AI Automation platform](./openseal.jpeg)

A modern agent execution platform for building, deploying, and running AI-powered automation workflows.

## Overview

OpenSeal provides a flexible runtime environment for executing AI agents with support for skills, triggers, and event-driven workflows.

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
# Build binary
make build

# Run tests
make test

# Run vet
make vet
```

## License

Apache License 2.0 - see LICENSE file for details.
