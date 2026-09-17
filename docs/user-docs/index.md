# OpenSeal Documentation

These pages describe OpenSeal: what it is, how to run it, and what its API, terminal client, and Go facade provide. Use them when evaluating OpenSeal, standing up a first deployment, or building against its API.

## What is OpenSeal?

OpenSeal is a prompt-first, durable kernel for autonomous Agents and Teams. It ships as a single Go binary that runs a background daemon, a versioned HTTP API, and a terminal client, and it is also importable as a Go library.

Objectives, long-running Runs, governed Skill actions, approvals, artifacts, and recovery share one portable execution model. A deterministic runbook layer defined in HCL is available alongside it as a separate execution path.

The design holds to one rule throughout: the kernel never manufactures authority. If no component has been wired to approve an action, evaluate a policy, or execute a Run, the corresponding operation is not advertised and not served. Absence produces a refusal, never a silent self-approval.

### Key Capabilities

- **Durable execution** — Objectives produce Runs, Runs advance in leased Turns, and every stage is persisted before it is acted on. A worker that dies mid-Turn releases its claim rather than stranding the Run.
- **Governed Skill actions** — Skills are registered in a catalog and bound per deployment, so a binding is the unit of least privilege. An action that policy holds stops at a durable approval checkpoint that survives a restart.
- **Versioned Agents and Teams** — Immutable definitions plus mutable deployments make activation, rollback, and amendment first-class operations rather than destructive edits.
- **Capability discovery** — The daemon computes what it can actually do per request, so clients read the surface rather than assume it. The same kernel runs standalone and mounted inside a larger host.
- **Prompt-to-workforce authoring** — A prompt compiles into an inspectable, refinable proposal for Agents, Teams, roles, and boundaries. Nothing is created until the proposal is explicitly applied.
- **Portable workforce bundles** — A signed YAML document moves a complete workforce between deployments, with validate, inspect, compare, preview, and upgrade-plan operations available offline.
- **Skill distribution through ClawHub** — Skills are discovered, verified, and installed with recorded provenance, including the source digest that ties an installed Skill to the exact bytes that produced it.
- **Execution-time secret resolution** — Skill definitions name opaque credential references. Values are resolved by a worker for one action call and never enter durable state, capability responses, prompts, or the terminal client.
- **A thin terminal client** — The workspace renders only what the connected server advertises, owns no authoritative state, and can be closed without stopping durable work.
- **Deterministic runbooks** — A directed graph of typed HCL nodes executes in one process when an outcome needs to be exact rather than reasoned.

## How It Works

1. **Configure** — Point the daemon at a configuration file and a standalone context. The context declares credential references; the configuration declares storage, the listen address, and source policies.
2. **Start** — The daemon opens its store, runs migrations, derives worker scopes from the configured credentials and policies, and serves a versioned API on loopback.
3. **Discover** — A client reads the capability document, which is computed from what this particular process actually has wired, and renders only the operations it advertises.
4. **Define** — Agents and Teams are created as immutable definition versions carried by durable deployments, and granted Skills through per-deployment bindings.
5. **Execute** — An objective produces Runs, a Run advances in bounded Turns, and a Turn invokes governed Skill actions that either execute or stop at a durable approval.
6. **Observe** — Every state transition writes an activity event in the same transaction as the change itself, so the feed cannot drift from the state it reports.

## Documentation Sections

| Section | Description |
|---|---|
| [Getting Started](getting-started.md) | Requirements, building, starting the daemon, and connecting the terminal client |
| [Core Concepts](concepts.md) | The execution model, capability discovery, scopes, owners, and the two execution paths |
| [Agents and Teams](agents-and-teams.md) | Definitions, deployments, activation, rollback, the amendment sequence, and agent requests |
| [Objectives, Projects, and Runs](objectives-and-runs.md) | The portfolio, the Run and Turn lifecycles, steering commands, and event routing |
| [Skills, Actions, and Approvals](skills-and-approvals.md) | Bindings as least privilege, action calls, durable approvals, ClawHub distribution, and governed outreach |
| [Workforces](workforces.md) | Prompt-to-workforce authoring, change sets, and portable bundles |
| [Configuration](configuration.md) | The daemon configuration file, the standalone context, credential references, and environment variables |
| [Security Boundaries](security.md) | What OpenSeal does not provide, the two guardrails, scopes as partitions, and secret handling |
| [Operations](operations.md) | Deployment shapes, SQLite and PostgreSQL persistence, migrations, and observability |
| [Command Line Reference](cli.md) | Every subcommand and flag, and the terminal interface |
| [API Reference](api.md) | REST conventions and routes, deterministic runbook nodes, and the Go embedding facade |

## Next Steps

- **Evaluating OpenSeal** — Read [Core Concepts](concepts.md) for the execution model, then [Security Boundaries](security.md) for the deployment assumptions it makes.
- **Standing up a deployment** — Start with [Getting Started](getting-started.md), then [Configuration](configuration.md) and [Operations](operations.md).
- **Building a client** — Read the capability discovery section of [Core Concepts](concepts.md) before the route tables in the [API Reference](api.md).
- **Embedding the kernel in a Go program** — Go to the embedding section of the [API Reference](api.md), then [Operations](operations.md) for the PostgreSQL store.
