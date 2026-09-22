# Getting Started

OpenSeal is a single binary that runs a background daemon, a versioned HTTP API, and a terminal client. This page covers a first local deployment from source: building, starting the daemon, confirming it serves, and connecting the terminal client.

Building is not required to run OpenSeal. Releases publish a prebuilt binary, a container image, and a desktop application — see [Installing OpenSeal](installing.md) to choose between them. Build from source when you want to modify OpenSeal, or to run an unreleased commit.

## Requirements

OpenSeal's durable store is SQLite through cgo, so a C toolchain is required alongside the Go toolchain.

| Field | Value |
|---|---|
| Go version | 1.26 |
| C toolchain | `gcc` or `clang`, required for SQLite |
| Module path | `github.com/axiom-studio/openseal` |
| Binary | `openseal` |
| License | Apache License 2.0 |

## Building the Binary

The requirements above apply to building. A published binary has no Go or C toolchain requirement — see [Installing OpenSeal](installing.md).

```bash
git clone https://github.com/axiom-studio/openseal.git
cd openseal
make build
```

`make build` compiles `./cmd/openseal` into `./openseal` in the repository root. Two further targets are available: `make test` runs `go test ./...` and `make vet` runs `go vet ./...`.

## Starting the Daemon

The daemon holds all authoritative state. Start it before anything else, and leave it running.

```bash
cp context.example.yaml context.yaml
export OPENAI_API_KEY='...'
./openseal daemon --config ./local.yaml --context ./context.yaml --standalone-operator
```

If the file named by `--config` does not exist, the daemon writes a default configuration to that path and continues, so a first run needs no configuration file. The defaults bind the API to `127.0.0.1:8080`, store kernel state in `data/openseal.db`, and store artifact content in `data/artifacts`.

> **Relative storage paths resolve against the configuration file's directory,** not against the shell's working directory. A daemon started with `--config ./deploy/local.yaml` writes its database to `./deploy/data/openseal.db`.

`--standalone-operator` grants local-operator authority: it enables approval decisions, ClawHub install and update operations, workforce lifecycle decisions, and outreach delivery. Without it those operations are not advertised and not served. The flag refuses to start unless the API listen address is a loopback address. See [Security](security.md) for what the flag does and does not establish.

## Confirming the Daemon Is Serving

```bash
curl http://127.0.0.1:8080/api/v1/health
curl http://127.0.0.1:8080/api/v1/capabilities
```

The health route returns a fixed body and always answers `200`:

```json
{"status": "ok"}
```

The capability document is the more useful of the two. It is computed per request from what the running process actually has wired, and it is the authoritative description of what this particular daemon can do. Clients read it rather than assuming a fixed route surface — see [Concepts](concepts.md) for how capability discovery works and why a large part of the API is conditional.

## Opening the Terminal Client

Run the binary with no subcommand in a second shell.

```bash
./openseal
```

The terminal client is a thin client over the API. It owns no authoritative state, and closing it stops neither the daemon nor any durable work in flight. It renders only the destinations the connected daemon advertises, so a daemon without a given capability produces a client without that destination.

The workspace opens on **Home**. Use the left and right arrow keys to move between destinations, and press `?` for help from anywhere. Full details are in the [CLI reference](cli.md).

## Where State Lives

| Field | Value |
|---|---|
| Kernel state | `data/openseal.db`, from `storage.path` |
| Artifact content | `data/artifacts`, from `storage.artifactsPath` |
| Installed Skills | `skills/`, or the path in `OPENSEAL_SKILLS_DIR` |
| Daemon configuration | The path given to `--config`, default `daemon.yaml` |
| Credential references | The path given to `--context`, default `context.yaml` |

All five resolve relative to the configuration file's directory unless given as absolute paths. A missing `--context` file is not an error; it means no local credential source is configured.

## Next Steps

- Understand the resource model and the execution lifecycle in [Concepts](concepts.md).
- Define the owners that do the work in [Agents and Teams](agents-and-teams.md).
- Give them outcomes to pursue in [Objectives and Runs](objectives-and-runs.md).
- Grant governed capabilities in [Skills and Approvals](skills-and-approvals.md).
- Tune the daemon in [Configuration](configuration.md), and read [Security](security.md) before exposing it to anything.
