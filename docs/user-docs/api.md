# API Reference

OpenSeal serves a versioned REST API, executes a deterministic runbook layer defined in HCL, and is importable as a Go library. This page is the reference for all three.

## REST Conventions

All routes are served under `/api/v1` from the address in `api.listenAddr`. Responses are JSON. Errors return `{"error": "<message>"}`, except ClawHub lifecycle errors, which add a `code` field carrying a canonical error code.

| Property | Description |
|---|---|
| Scope on reads | `scopeKind` and `scopeId` query parameters |
| Scope on writes | Inside the request body |
| Idempotency | An `Idempotency-Key` request header on creating and deciding routes |
| Path parameters | `{id}` is the primary resource identifier |
| Conditional availability | A route whose capability is not advertised returns `501 Not Implemented` |

### Status Codes

| Status | Description |
|---|---|
| 200 | Success |
| 400 | Malformed request, missing scope, or invalid body |
| 403 | Refused by policy |
| 404 | Resource does not exist |
| 409 | Revision conflict or lifecycle conflict |
| 413 | Artifact content exceeds the 100 MiB upload limit |
| 422 | Semantically invalid, including failed verification |
| 428 | A Skill reference upgrade requires approval before it can be applied |
| 500 | Internal failure |
| 501 | The capability is not configured in this deployment |
| 502 | An upstream registry or transport failed |
| 503 | A required dependency is temporarily unavailable |

> **No route returns `401 Unauthorized`, because no route authenticates.** This is not an omission in the documentation. See [Security](security.md).

### Reading the Capability Document First

A substantial part of this surface is conditional. Fifty-five distinct `501` responses exist across the handlers, each naming the component that is missing. Client code should branch on `GET /api/v1/capabilities` rather than assume a route exists — two daemons at the same version can present materially different surfaces. See [Concepts](concepts.md).

## Service

| Method | Path |
|---|---|
| GET | `/api/v1/health` |
| GET | `/api/v1/capabilities` |
| GET | `/api/v1/activity` |

## Objectives

| Method | Path |
|---|---|
| POST | `/api/v1/objectives` |
| GET | `/api/v1/objectives` |
| GET | `/api/v1/objectives/{id}` |
| PUT | `/api/v1/objectives/{id}` |

## Projects

| Method | Path |
|---|---|
| POST | `/api/v1/projects` |
| GET | `/api/v1/projects` |
| GET | `/api/v1/projects/{id}` |
| PATCH | `/api/v1/projects/{id}` |

## Agent Runs and Turns

| Method | Path |
|---|---|
| POST | `/api/v1/agent-runs` |
| GET | `/api/v1/agent-runs` |
| GET | `/api/v1/agent-runs/admission` |
| GET | `/api/v1/agent-runs/{id}` |
| GET | `/api/v1/agent-runs/{id}/runbook-audit` |
| POST | `/api/v1/agent-runs/{id}/commands` |
| GET | `/api/v1/agent-turns` |
| GET | `/api/v1/agent-turns/{id}` |

The commands route carries pause, resume, cancel, and intervene. The admission route reports why a Run would or would not be admitted, and is the first thing to check when a Run stays `queued`. See [Objectives and Runs](objectives-and-runs.md).

## Agent Requests

| Method | Path |
|---|---|
| POST | `/api/v1/agent-requests` |
| GET | `/api/v1/agent-requests` |
| GET | `/api/v1/agent-requests/{id}` |
| POST | `/api/v1/agent-requests/{id}/responses` |
| POST | `/api/v1/agent-requests/{id}/completions` |
| POST | `/api/v1/agent-requests/{id}/completion-review` |

## Action Calls and Approvals

| Method | Path |
|---|---|
| GET | `/api/v1/action-calls` |
| GET | `/api/v1/action-calls/{id}` |
| GET | `/api/v1/action-approvals` |
| GET | `/api/v1/action-approvals/{id}` |
| POST | `/api/v1/action-approvals/{id}/decisions` |

Action calls are read-only; they are created by the execution path. The decisions route requires an approval authorizer, which standalone mode installs only under `--standalone-operator`. See [Skills and Approvals](skills-and-approvals.md).

## Agent Deployments

| Method | Path |
|---|---|
| GET | `/api/v1/agent-deployments` |
| POST | `/api/v1/agent-installations` |
| GET | `/api/v1/agent-deployments/{id}` |
| PUT | `/api/v1/agent-deployments/{id}` |
| GET | `/api/v1/agent-deployments/{id}/compilations` |
| POST | `/api/v1/agent-deployments/{id}/activations` |
| GET | `/api/v1/agent-deployments/{id}/activations` |
| POST | `/api/v1/agent-deployments/{id}/rollbacks` |
| POST | `/api/v1/agent-deployments/{id}/amendments` |
| GET | `/api/v1/agent-deployments/{id}/amendments` |
| GET | `/api/v1/agent-deployments/{id}/amendments/{amendmentId}` |
| POST | `/api/v1/agent-deployments/{id}/amendments/{amendmentId}/evaluations` |
| POST | `/api/v1/agent-deployments/{id}/amendments/{amendmentId}/decisions` |
| POST | `/api/v1/agent-deployments/{id}/amendments/{amendmentId}/activations` |

## Team Definitions and Deployments

| Method | Path |
|---|---|
| POST | `/api/v1/team-definitions` |
| GET | `/api/v1/team-definitions/{id}` |
| POST | `/api/v1/team-deployments` |
| GET | `/api/v1/team-deployments` |
| GET | `/api/v1/team-deployments/{id}` |
| PUT | `/api/v1/team-deployments/{id}` |
| POST | `/api/v1/team-deployments/{id}/activations` |
| GET | `/api/v1/team-deployments/{id}/activations` |
| POST | `/api/v1/team-deployments/{id}/amendments` |
| GET | `/api/v1/team-deployments/{id}/amendments` |
| GET | `/api/v1/team-deployments/{id}/amendments/{amendmentId}` |
| POST | `/api/v1/team-deployments/{id}/amendments/{amendmentId}/evaluations` |
| POST | `/api/v1/team-deployments/{id}/amendments/{amendmentId}/decisions` |
| POST | `/api/v1/team-deployments/{id}/amendments/{amendmentId}/activations` |

Agent deployments carry a direct rollback route; Team deployments do not. Otherwise the two lifecycles are identical. See [Agents and Teams](agents-and-teams.md).

## Skill Actions and Bindings

| Method | Path |
|---|---|
| GET | `/api/v1/agent-deployments/{deploymentId}/skill-actions` |
| GET | `/api/v1/agent-deployments/{id}/skill-bindings` |
| PUT | `/api/v1/agent-deployments/{id}/skill-bindings/{bindingId}` |
| POST | `/api/v1/agent-deployments/{id}/skill-bindings/{bindingId}/disable` |
| POST | `/api/v1/agent-deployments/{id}/skill-bindings/{bindingId}/upgrade-plan` |
| POST | `/api/v1/agent-deployments/{id}/skill-bindings/{bindingId}/upgrade` |
| GET | `/api/v1/team-deployments/{id}/skill-bindings` |
| PUT | `/api/v1/team-deployments/{id}/skill-bindings/{bindingId}` |
| POST | `/api/v1/team-deployments/{id}/skill-bindings/{bindingId}/disable` |
| POST | `/api/v1/team-deployments/{id}/skill-bindings/{bindingId}/upgrade-plan` |
| POST | `/api/v1/team-deployments/{id}/skill-bindings/{bindingId}/upgrade` |

The upgrade-plan and upgrade routes are withdrawn from the advertised capability when the configured store does not implement the upgrade contract.

## ClawHub

| Method | Path |
|---|---|
| GET | `/api/v1/clawhub/catalog/{reference}` |
| GET | `/api/v1/clawhub/catalog/{reference}/versions` |
| GET | `/api/v1/clawhub/catalog/{reference}/file` |
| POST | `/api/v1/clawhub/catalog/{reference}/verify` |
| POST | `/api/v1/clawhub/catalog/{reference}/install` |
| GET | `/api/v1/clawhub/installed` |
| POST | `/api/v1/clawhub/installed/update-all` |
| POST | `/api/v1/clawhub/installed/{reference}/verify` |
| POST | `/api/v1/clawhub/installed/{reference}/pin` |
| POST | `/api/v1/clawhub/installed/{reference}/unpin` |
| POST | `/api/v1/clawhub/installed/{reference}/update` |
| DELETE | `/api/v1/clawhub/installed/{reference}` |

Read operations are always available once ClawHub is wired. Install, update, pin, unpin, and uninstall are filtered out unless mutation authority has been granted. These are the only routes that return a canonical `code` alongside the error message — see [Skills and Approvals](skills-and-approvals.md).

## Artifacts

| Method | Path |
|---|---|
| POST | `/api/v1/artifacts` |
| GET | `/api/v1/artifacts` |
| GET | `/api/v1/artifacts/{id}` |
| POST | `/api/v1/artifact-content` |
| GET | `/api/v1/artifacts/{id}/content` |
| POST | `/api/v1/artifacts/{id}/resolve` |

Upload and download require an artifact content store, which the daemon wires from `storage.artifactsPath`. Uploads are capped at 100 MiB and exceed it with `413`.

> **Resolution requires a component the daemon does not wire.** `POST /api/v1/artifacts/{id}/resolve` returns an ephemeral authorized URL rather than bytes, and needs a separate content resolver. In a standalone deployment it always returns `501 Not Implemented`.

## Conversations and Channels

| Method | Path |
|---|---|
| POST | `/api/v1/conversations` |
| GET | `/api/v1/conversations` |
| GET | `/api/v1/conversations/{id}` |
| POST | `/api/v1/conversations/{id}/messages` |
| GET | `/api/v1/conversations/{id}/messages` |
| GET | `/api/v1/conversations/{id}/messages/{messageId}` |
| GET | `/api/v1/conversations/{id}/changes` |
| POST | `/api/v1/conversations/{id}/participation-rounds` |
| GET | `/api/v1/conversations/{id}/participation-rounds` |
| GET | `/api/v1/conversations/{id}/participation-rounds/{roundId}` |
| PUT | `/api/v1/conversations/{id}/cursor` |
| GET | `/api/v1/conversations/{id}/cursor` |
| PUT | `/api/v1/conversations/{id}/presence` |
| DELETE | `/api/v1/conversations/{id}/presence` |
| GET | `/api/v1/conversations/{id}/presence` |

Participation rounds are how multiple Agents take turns in one channel without talking over each other. Cursors record how far each participant has read; presence records who is currently attached.

## Runbooks

| Method | Path |
|---|---|
| GET | `/api/v1/runbooks` |
| GET | `/api/v1/runbooks/{id}` |
| PATCH | `/api/v1/runbooks/{id}` |
| POST | `/api/v1/runbooks/{id}/runs` |
| POST | `/api/v1/runbooks/schedule-reconciliations` |

## Event Sources and Routing

| Method | Path |
|---|---|
| POST | `/api/v1/event-source-subscriptions` |
| GET | `/api/v1/event-source-subscriptions` |
| GET | `/api/v1/event-source-subscriptions/{id}` |
| PATCH | `/api/v1/event-source-subscriptions/{id}` |
| POST | `/api/v1/event-source-subscriptions/{id}/retirements` |
| POST | `/api/v1/event-source-subscriptions/{id}/health-reports` |
| GET | `/api/v1/event-source-subscriptions/{id}/checkpoint` |
| POST | `/api/v1/event-source-subscriptions/{id}/checkpoint-advancements` |
| POST | `/api/v1/events` |

## Source Monitors and Outreach

| Method | Path |
|---|---|
| GET | `/api/v1/projects/{id}/source-monitors/{monitorId}/observations` |
| GET | `/api/v1/projects/{id}/source-monitors/{monitorId}/checkpoint` |
| POST | `/api/v1/projects/{id}/outreach` |
| GET | `/api/v1/projects/{id}/outreach` |
| GET | `/api/v1/projects/{id}/outreach/{threadId}` |
| POST | `/api/v1/projects/{id}/outreach/{threadId}/messages/{messageId}/deliveries` |

Outreach is advertised only when the store implements the project, source monitor, outreach, and outreach action contracts together. Thread creation additionally requires a Skill catalog, and delivery additionally requires a wired dispatcher. Even under `--standalone-operator`, delivery is refused for any scope whose source policy does not enable outreach.

## Source Policies

| Method | Path |
|---|---|
| POST | `/api/v1/source-policies/versions` |
| GET | `/api/v1/source-policies` |
| GET | `/api/v1/source-policies/{id}` |
| GET | `/api/v1/source-policies/{id}/versions` |
| GET | `/api/v1/source-policies/{id}/versions/{version}` |
| POST | `/api/v1/source-policies/{id}/activations` |
| POST | `/api/v1/source-policies/{id}/revocations` |
| GET | `/api/v1/source-policies/{id}/activations` |

These routes manage the durable source-policy lifecycle, which is distinct from the static `sourcePolicies` list in the daemon configuration file.

> **All eight fail closed in a standalone deployment.** They require a source-policy lifecycle service the standalone daemon does not install, so every one returns `501 Not Implemented`. Configure source policies through the daemon configuration file instead — see [Configuration](configuration.md).

## Workforce Authoring

| Method | Path |
|---|---|
| POST | `/api/v1/authoring/workforce/compile` |
| GET | `/api/v1/authoring/workforce/skills` |
| POST | `/api/v1/authoring/workforce/change-sets` |
| GET | `/api/v1/authoring/workforce/change-sets/{id}` |
| PATCH | `/api/v1/authoring/workforce/change-sets/{id}/placement` |
| POST | `/api/v1/authoring/workforce/change-sets/{id}/retry` |
| POST | `/api/v1/authoring/workforce/change-sets/{id}/refinements` |
| POST | `/api/v1/authoring/workforce/change-sets/{id}/evaluations` |
| POST | `/api/v1/authoring/workforce/change-sets/{id}/approvals` |
| POST | `/api/v1/authoring/workforce/change-sets/{id}/activation` |
| POST | `/api/v1/authoring/workforce/change-sets/{id}/apply` |

## Workforce Bundles

| Method | Path |
|---|---|
| POST | `/api/v1/workforce-bundles/validate` |
| POST | `/api/v1/workforce-bundles/inspect` |
| POST | `/api/v1/workforce-bundles/compare` |
| POST | `/api/v1/workforce-bundles/installation-preview` |
| POST | `/api/v1/workforce-bundles/upgrade-plan` |
| POST | `/api/v1/workforce-bundles/install` |

The five read operations work in every deployment. Installation requires an installation store and a host-authenticated actor, which the bundled daemon does not supply, so `POST /api/v1/workforce-bundles/install` always returns `501 Not Implemented`. See [Workforces](workforces.md).

## Deterministic Runbooks

Alongside the durable kernel, OpenSeal carries a deterministic runbook layer: a directed graph of typed nodes defined in HCL and executed in a single process by `openseal run`. It is a separate execution path — it creates no Runs, produces no activity events, and takes part in no approvals.

```hcl
workflow "notify" {
  description = "Post a message when an endpoint responds"

  node http "fetch" {
    url = "https://example.com/status"
  }

  node slack "notify" {
    channel = "#ops"
  }

  edge "fetch" "notify" {
  }
}
```

Node metadata — display names, categories, and the input schemas used by `openseal validate` — lives in 28 YAML files under `embedded_nodes/`, with an identical copy under `cmd/openseal/embedded_nodes/`.

### Node Types

The gap between node types that have metadata and node types that have a registered executor is the most important thing to know about this layer.

| Node Type | Category | Metadata | Registered by `openseal run` |
|---|---|---|---|
| `if` | control | Yes | Yes |
| `switch` | control | Yes | Yes |
| `transform` | data | Yes | Yes |
| `set` | data | Yes | Yes |
| `merge` | data | Yes | Yes |
| `delay` | control | Yes | Yes |
| `filter` | data | Yes | Yes |
| `sort` | data | Yes | Yes |
| `aggregate` | data | Yes | Yes |
| `split` | data | Yes | Yes |
| `join` | control | Yes | Yes |
| `loop` | control | Yes | Yes |
| `webhook_response` | response | Yes | Yes |
| `slack` | communication | Yes | Yes |
| `discord` | communication | Yes | Yes |
| `teams` | communication | Yes | Yes |
| `email` | communication | Yes | Yes |
| `http` | action | Yes | Yes |
| `ai` | action | Yes | Yes |
| `pgvector` | action | Yes | Yes |
| `code` | action | Yes | Only with an in-cluster Kubernetes client |
| `webhook` | trigger | Yes | No |
| `cron` | trigger | Yes | No |
| `manual` | trigger | Yes | No |
| `tool_debug` | tool | Yes | No |
| `tool_mcp` | tool | Yes | No |
| `tool_memory` | tool | Yes | No |
| `tool_pgvector` | tool | Yes | No |

Twenty node types execute unconditionally. The `code` node registers only when an in-cluster Kubernetes client is available, and it runs Python in a job container.

Eight further Kubernetes node types — `k8s_get`, `k8s_list`, `k8s_logs`, `k8s_events`, `k8s_restart`, `k8s_scale`, `k8s_patch`, and `k8s_delete` — have complete executors but register only when a Kubernetes client is passed to the registry constructor. `openseal run` passes none, so they are never available from the CLI.

> **The trigger and tool categories are metadata-only.** They describe nodes an embedding host is expected to supply. A runbook whose first node is a trigger fails immediately under `openseal run` — which is what happens to the scaffold `openseal workflow create` generates. See [CLI](cli.md).

## Go Embedding

`github.com/axiom-studio/openseal/pkg/openseal` is the supported facade. Packages under `internal/` are not importable from outside the module, and the other `pkg/` packages are implementation surfaces that the facade re-exports.

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

An engine constructed with no store option uses an in-memory store, which is appropriate for tests and nothing else. Durable work requires an explicit persistent store.

### Store Constructors

| Constructor | Description |
|---|---|
| `NewSQLiteStore(path)` | Opens SQLite with write-ahead logging and runs migrations |
| `NewPostgresStore(ctx, dsn, options...)` | Opens PostgreSQL and applies the versioned schema under an advisory lock |

PostgreSQL store options: `WithPostgresSchema`, `WithPostgresPool`, `WithPostgresMigrationLock`, and `WithPostgresMigrationObserver`. Defaults and limits are in [Operations](operations.md).

### Engine Options

| Option | Description |
|---|---|
| `WithStore` / `WithPersistentStore` | Install the kernel store |
| `WithLogger` | Supply a logger |
| `WithWorkerConcurrencyLimit` | Bound total worker concurrency |
| `WithAgentRunWorkers` | Run workers for a fixed scope |
| `WithDynamicAgentRunWorkers` | Run workers that follow a scope source |
| `WithActionWorkers` | Action workers with a credential resolver and dispatcher |
| `WithDynamicActionWorkers` | Action workers that follow a scope source |
| `WithActionCredentialLeaseWorkers` | Action workers backed by credential leases |
| `WithDynamicActionCredentialLeaseWorkers` | Lease-backed action workers over a scope source |
| `WithConversationCoordinator` | Coordinate multi-participant channels |
| `WithDynamicConversationRuns` | Conversation Runs over a scope source |
| `WithExternalConversationTransport` | Bridge channels to an external transport |
| `WithActionPolicy` | Decide which actions require approval |
| `WithActionProposalValidators` | Reject invalid proposals before admission |
| `WithApprovalAuthorizer` | Decide who may resolve an approval |
| `WithSkillCatalog` | Install a Skill catalog |
| `WithSkillManagementActions` | Expose Skill management as Agent actions |
| `WithAgentManagementActions` | Expose Agent management as Agent actions |
| `WithTeamManagementActions` | Expose Team management as Agent actions |
| `WithRunManagementActions` | Expose Run management as Agent actions |
| `WithRunbookManagementActions` | Expose runbook management as Agent actions |
| `WithClawHubRegistry` | Install a ClawHub registry |
| `WithClawHubRegistrySkillsDirectory` | Install a registry with an on-disk skills directory |
| `WithClawHubRegistryPreview` | Install a read-only registry |
| `WithClawHubRegistryClient` | Supply a registry client |
| `WithClawHubSourceArtifactScope` | Scope retained Skill source artifacts |
| `WithSkillSourceArtifactStore` | Install a Skill source artifact store |
| `WithWorkforceAuthoringGenerator` | Install a workforce proposal generator |
| `WithWorkforceChangeSetReadinessValidators` | Add readiness checks before apply |

The management-action options are what let an Agent operate the kernel itself — register a Skill, propose an amendment, command a Run — through the same governed action path as any other Skill.

### The HTTP Client

`pkg/client` provides a client for programs that talk to a remote daemon rather than embedding the engine.

```go
kernel := client.NewKernelHTTPClient("http://127.0.0.1:8080", nil)
```

`NewKernelHTTPClient(baseURL, httpClient, options...)` accepts an origin or an explicit API root and defaults to a thirty-second timeout. `WithRequestHeaders` adds headers to every request, silently dropping `Content-Type` and `Idempotency-Key`. Failures surface as an `APIError` carrying the status code and the server's message.

## Next Steps

- Understand why the surface is conditional in [Concepts](concepts.md).
- Read the exposure model before serving this API in [Security](security.md).
- Operate a deployment using [Operations](operations.md).
