# Command Line Reference

The `openseal` binary carries every entry point: the terminal client, the daemon, the deterministic runbook tools, and the offline Skill and bundle utilities. Running it with no subcommand opens the terminal client.

## Commands

| Command | Description |
|---|---|
| `tui` | Open the prompt-first Agent and Team workspace; the default |
| `run` | Execute a runbook from an HCL file |
| `daemon` | Start the durable Agent and Team kernel |
| `validate` | Validate a runbook HCL file |
| `workflow` | Manage runbook files |
| `skill` | Export and validate canonical Skill manifests |
| `bundle` | Validate, inspect, diff, and plan portable workforce bundles |
| `version` | Print the version |
| `help` | Print usage |

An unrecognized subcommand prints usage to standard error and exits `1`.

> **The subcommand is `workflow`, the API resource is `runbooks`.** The deterministic HCL layer is named both ways depending on where it is being addressed. Both refer to the same thing.

## openseal daemon

Starts the durable kernel and the versioned API. See [Configuration](configuration.md) for the files it reads.

| Field | Value | Default |
|---|---|---|
| `--config` | Path to the daemon configuration file | `daemon.yaml` |
| `--context` | Path to the standalone context file | `context.yaml` |
| `--scope` | Durable standalone authoring scope as `kind:id` | `local:default` |
| `--standalone-operator` | Grant local-operator authorities; requires a loopback listen address | off |
| `--help` | Print help | — |

A missing `--config` file is created with defaults. A missing `--context` file is treated as an unconfigured local credential source, not an error.

## openseal tui

Opens the terminal client. This is what runs when the binary is invoked with no subcommand.

```bash
openseal
openseal tui --endpoint http://127.0.0.1:8080 --scope local:default
```

| Field | Value | Default |
|---|---|---|
| `--endpoint` | Kernel origin or explicit versioned API root | `http://127.0.0.1:8080` |
| `--header` | Non-secret host selector header as `name=value`, repeatable | none |
| `--scope` | Workspace scope as `kind:id` | `local:default` |
| `--owner` | Work owner as `agent:id` or `team:id` | `agent:operator` |
| `--download-dir` | Directory for verified artifact downloads | `artifacts` |
| `--poll` | Refresh interval; a negative duration disables polling | `5s` |
| `--help` | Print help | — |

`OPENSEAL_API_URL` sets the default endpoint, and `--endpoint` overrides it. An endpoint with a path component is treated as an explicit canonical API root rather than an origin, which is how the client operates against a kernel mounted under a host's own route prefix. An endpoint with no path has `/api/v1` appended.

## The Terminal Interface

The terminal client is a thin client over the API. It owns no authoritative state, and closing it leaves the daemon and all durable work running. It renders only the destinations the connected server advertises in its capability document, so a daemon without a given capability produces a client without that destination.

### Layout

The workspace is a single full-terminal view. A row of primary destination labels sits across the top, with a second **More** row beneath it holding the remainder, and a hint line showing the arrow-key and help bindings. Below the navigation, the main panel renders the selected destination — a list of Runs, a channel transcript, a bundle inspection, and so on. A composer occupies the lower portion of the view on destinations that accept input, and `Tab` moves focus between the composer and the panel above it.

**Home** presents the next useful action rather than a dashboard, and states the shape of the workflow directly: describe, review the exact proposal, create, activate, observe. Creation and activation stay separate throughout — the client never starts unreviewed work.

### Destinations

Seven destinations are primary. The remainder appear under the **More** row. Each carries a single-key shortcut.

| Section | Key | Group |
|---|---|---|
| Home | — | Primary |
| Create | `f` | Primary |
| Agents | `h` | Primary |
| Teams | `T` | Primary |
| Marketplace | `s` | Primary |
| Work | `w` | Primary |
| Channels | `c` | Primary |
| Goals | `o` | More |
| Projects | `i` | More |
| Inbox | `R` | More |
| Approvals | `A` | More |
| Sources | `S` | More |
| Integrations | `I` | More |
| Outreach | `O` | More |
| Activity | `t` | More |
| Evidence | `a` | More |
| Imports | `B` | More |

Home is always present. Every other destination appears only when its capability is advertised. **Marketplace** appears when any one of the ClawHub, Skill action, Skill binding, or source policy capabilities is available.

> **Integrations does not appear against the bundled daemon.** That destination is gated on the conversation gateway capability, which the standalone daemon never emits. It is reachable only against an embedding host that composes its own capability document.

### Keys

| Field | Value |
|---|---|
| `←` and `→` | Move between destinations |
| `?` | Open help from anywhere |
| `Tab` | Move focus between the composer and the panel |
| `Esc` | Close an editor |
| `Ctrl+C` | Exit |
| `Enter` | Submit a composer |
| `Shift+Enter` | Insert a newline in a composer |

### Headers and Downloads

The client refuses to send `Content-Type` or `Idempotency-Key` through `--header`, rejecting them at flag-parse time with a message naming them as kernel-protocol headers. Header names and values containing whitespace, colons, or line breaks are also rejected, which prevents header injection through the flag.

Downloaded artifacts are verified against their recorded digest before being moved into the download directory.

## openseal run

```bash
openseal run workflow.hcl
```

Loads the HCL file, converts it into an executor pipeline, and executes it with a thirty-minute timeout, starting from the first node in the file. On completion it logs the runbook name, node count, edge count, source file, status, and duration, then prints each node's output as indented JSON.

This is the deterministic layer, not the durable kernel. A runbook executed this way creates no Run, produces no activity events, and passes through no approval. See [Concepts](concepts.md).

## openseal validate

```bash
openseal validate workflow.hcl --json
```

Checks node identifiers for emptiness and duplication, node types against known metadata and the executor registry, edges for missing or unknown endpoints, node configuration against each type's input schema, and the graph for cycles and orphaned nodes.

Self-loops and unknown configuration fields are warnings; everything else is an error. `--json` prints the machine-readable result. A parse failure or any error-level issue exits `1`.

> **Run it from the repository root.** Node metadata is read from the relative path `embedded_nodes/`, resolved against the process's working directory, and the read error is discarded. From the repository root, all 28 node types are known and configuration is checked against their schemas. From anywhere else the metadata map is empty, no configuration is checked at all, and the trigger and tool types are reported as unknown node types.

## openseal workflow create

| Field | Value | Default |
|---|---|---|
| `--name` | Runbook name; required | — |
| `--description` | Runbook description | empty |
| `--output` | Output file path | `<name>.hcl` |

Writes a two-node scaffold: a `webhook` trigger node with a path derived from the name, an `http` node pointing at `https://example.com`, and an edge between them.

> **The generated scaffold is not directly runnable.** Its first node is a `webhook` trigger, and no webhook executor is registered. Because execution starts at the first node in the file, `openseal run` fails immediately on the node the generator itself wrote. Replace the trigger with an executable node before running it. See [API](api.md) for which node types execute.

## openseal skill

```bash
openseal skill manifest <skill-id> --output ./outreach.yaml
openseal skill validate ./outreach.yaml
```

`manifest` encodes one of the four bundled Skill definitions as a YAML manifest, writing to standard output or to `--output` with mode `0644`. An unknown identifier reports the available list. `validate` decodes a manifest file and prints `valid <id>@<version>`.

## openseal bundle

```bash
openseal bundle validate <path>
openseal bundle inspect <path>
openseal bundle diff <from-path> <to-path>
openseal bundle plan-upgrade <current-path> <target-path>
```

All four read files directly and print indented JSON without contacting a daemon. `validate` verifies against an empty trust policy and reports the digest along with any valid signature keys. See [Workforces](workforces.md).

## Next Steps

- Configure the daemon these commands start in [Configuration](configuration.md).
- Reference the routes the client consumes in [API](api.md).
- Understand what `--standalone-operator` grants in [Security](security.md).
