# CLI reference

The `openseal` binary uses explicit subcommands. Running it without arguments
opens the TUI.

```text
openseal tui       # default; open the terminal workspace
openseal daemon    # start the durable daemon
openseal run       # execute one HCL workflow directly
openseal validate  # validate an HCL workflow
openseal workflow  # create an HCL workflow skeleton
openseal bundle    # inspect portable workforce artifacts
openseal version
openseal help
```

The command line does not expose direct CRUD commands for Agents or Teams.
Compose and operate them through the prompt-first TUI, the versioned API, or
the public Go facade.

## `openseal bundle`

```text
openseal bundle validate <path>
openseal bundle inspect <path>
openseal bundle diff <from-path> <to-path>
openseal bundle plan-upgrade <current-path> <target-path>
```

These commands decode the strict YAML format and emit JSON. They never import
resources or resolve target credentials. See [Portable workforce
bundles](workforce-bundles.md) for trust and installation semantics.

## `openseal tui`

```text
openseal tui [options]

--endpoint <url>     kernel origin or explicit API root
                     (default http://127.0.0.1:8080)
--header <name=value>
                     non-secret host selector; repeatable
--scope <kind:id>    workspace scope (default local:default)
--owner <type:id>    owner for new work (default agent:operator)
--download-dir <dir> verified artifact destination (default artifacts)
--poll <duration>    refresh interval (default 5s; negative disables polling)
--help
```

`OPENSEAL_API_URL` changes the default endpoint. `Content-Type` and
`Idempotency-Key` cannot be supplied with `--header` because the kernel protocol
owns them. Do not put authorization secrets in command-line headers; use an
authorization-aware transport or local proxy.

See [Terminal UI](tui.md) for interactions.

## `openseal daemon`

```text
openseal daemon [options]

--config <path>       daemon YAML (default daemon.yaml)
--context <path>      local Vault/provider context (default context.yaml)
--scope <kind:id>     standalone authoring scope (default local:default)
--listen <addr>      override configured API address; port 0 selects a free port
--standalone-operator enable local action, ClawHub, outreach, and authoring retry/refinement authority on loopback
--desktop-operator enable authenticated local workspace review and installation
--help
```

If the configuration path does not exist, OpenSeal writes the default
configuration before starting. `--standalone-operator` is rejected unless the
API address is loopback. `--desktop-operator` additionally requires a nonempty
`OPENSEAL_API_TOKEN` and a local workspace scope. The API token protects all
routes but does not provide multi-user roles.
The context file is optional; when present, it maps opaque credential
references to environment variables or private files and can configure the
standalone authoring model. See [Standalone context](standalone-context.md).

## `openseal validate`

```bash
openseal validate [--json] path/to/workflow.hcl
```

The command checks HCL parsing, graph structure, node schemas, required fields,
and connections. It exits non-zero on an invalid workflow.

## `openseal run`

```bash
openseal run path/to/workflow.hcl
```

This is a direct, process-bounded execution command with a 30-minute context.
It prints node outputs and does not use the daemon's durable Agent Run queue.
Use the kernel runtime for long-running autonomous work.

## `openseal workflow create`

```text
openseal workflow create --name <name> [options]

--description <text>
--output <path>       default: lowercase name plus .hcl
--help
```

The generated skeleton contains a webhook node followed by an HTTP node. Edit
and validate it before use.
