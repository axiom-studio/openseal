# Developer documentation

Start with the [repository README](../../README.md) for the product and quick
starts. The [user guides](../user-docs/index.md) describe supported behavior;
these developer guides explain implementation, extension points, and verification.
Check `/api/v1/capabilities` on a running deployment before assuming an optional
operation is available.

## First contribution

1. [Build and start a daemon](getting-started.md), then run `make test` and
   `make vet` for code changes.
2. Read [core concepts](concepts.md) for Agents, Teams, Runs, Skills, approvals,
   conversations, and evidence.
3. Follow the area relevant to the change:

| Area | Guides |
| --- | --- |
| Kernel and embedding | [Runtime architecture](architecture/autonomous-agent-runtime.md), [REST API](api.md), [Operations](operations.md) |
| Terminal client and commands | [Terminal UI](tui.md), [CLI reference](cli.md) |
| Desktop app | [Desktop quick start](../../desktop/README.md), [Implementation notes](../../desktop/implementation-notes.md), [REST API](api.md) |
| Model and credential setup | [Standalone context](standalone-context.md), [Operations](operations.md) |
| Teams and workforces | [Team coordination](team-coordination.md), [Portable bundles](workforce-bundles.md), [Workforce identities](architecture/workforce-identities.md) |
| Skills and deterministic work | [OpenClaw compatibility](skills/openclaw-compatibility.md), [Callable runbooks](callable-runbooks.md), [Runbook verification](runbook-verification.md) |

The user and developer guides cover the same product from different angles. When
behavior changes, update the relevant user guide and the implementation guide
in the same change. Optional behavior must state which authority or adapter
makes it available.
