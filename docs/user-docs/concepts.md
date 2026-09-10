# Core Concepts

OpenSeal separates the thing that wants an outcome from the durable record of work toward it. Understanding that separation, and the capability discovery that follows from it, explains most of the system's behavior.

## The Execution Model

A prompt or an inbound event is attributed to an owner. An owner holds a portfolio of objectives. An objective produces Runs. A Run advances in bounded Turns. A Turn may invoke a governed Skill action, which either executes or stops at a durable approval checkpoint.

```text
Prompt or event → Agent or Team → Objective → Run → Turn → Skill action
```

An action that policy permits produces activity and artifacts. An action that policy holds produces an approval, and the Turn resumes only once that approval is decided:

```text
Skill action → allowed        → activity and artifacts
Skill action → needs approval → durable checkpoint → Turn resumes
```

Every stage is persisted before it is acted on. A Turn claims work under a lease with an expiry, so a worker that dies mid-Turn releases its claim rather than stranding the Run. Approval checkpoints are durable records rather than in-memory waits, so a Run waiting on a human decision survives a daemon restart.

Bounding work at the Turn level is what makes a long Run interruptible and recoverable without a bespoke checkpoint format for each kind of work.

## Authority Is Never Manufactured

Two properties follow from that design and shape the rest of the system.

- **The kernel never invents authority** — If no component has been wired to approve an action, evaluate a policy, or execute a Run, the corresponding operation is not advertised and not served. Absence produces a refusal, never a silent self-approval.
- **Clients discover the surface rather than assume it** — The same kernel runs standalone and mounted inside a larger host that supplies its own identity and policy layers. A client written against the capability document works against both.

## Capability Discovery

`GET /api/v1/capabilities` returns a document listing every capability the running daemon can serve, each with an identifier, a version, and the set of operations available on it. The document is computed per request from what the process actually has wired — it is not a static manifest.

Three separate conditions decide whether a capability appears.

- **Store contracts** — Some capabilities require the configured store to implement an optional interface. Artifacts require an artifact store, channels require a conversation store, projects require a project store, and agent definitions require an agent registry.
- **Wired adapters** — Some operations require a component to be installed. Run creation requires a Run dispatcher, outreach delivery requires the same, approval resolution requires an approval authorizer, and workforce apply requires a lifecycle authorizer.
- **Configured services** — Some capabilities require configuration to be complete. Workforce authoring appears only when a model endpoint, a credential, and a model name are all set.

An operation that is not advertised is also not served. A route whose capability is missing returns `501 Not Implemented` with a message naming the missing component. Fifty-five such responses exist across the route handlers, so a substantial part of the API is conditional in a standalone deployment.

> **Read the capability document; do not assume a route exists.** The practical consequence is that two OpenSeal daemons at the same version can present materially different surfaces. Client code that branches on the capability document behaves correctly against both; client code that hard-codes a route does not.

## Scopes

Every durable record in OpenSeal is scoped. A scope is a two-part identifier and is the only tenancy primitive in the kernel.

| Field | Type | Validation |
|---|---|---|
| `kind` | `string` | Must be non-empty after trimming |
| `id` | `string` | Must be non-empty after trimming |

Scope validation is deliberately minimal: any non-empty pair is valid. There is no registry of known scopes, no hierarchy, and no membership check. The kernel uses the scope to partition storage and to route work to the correct worker.

Scopes reach the system three ways. The daemon's `--scope` flag sets the authoring scope in `kind:id` form, defaulting to `local:default`. Configuration attaches scopes to source policies and credential entries. API requests carry them as `scopeKind` and `scopeId` query parameters on reads, or inside the body on writes.

> **A scope is a partition key, not an access control boundary.** It determines where records live, not who may read them. [Security](security.md) covers what that means for a deployment.

## Owners

Every objective names an owner, and an owner is always one of two kinds.

| Field | Value |
|---|---|
| `type` | `agent` or `team` |
| `id` | Non-empty identifier string |

Both kinds own work identically. What differs is internal structure: a Team additionally carries semantic roles, a roster, a policy, and its own Skill bindings. See [Agents and Teams](agents-and-teams.md).

## The Durable Record

Several record types recur throughout the API, and knowing which is which makes the rest of the documentation easier to read.

| Property | Description |
|---|---|
| Objective | A durable statement of a desired outcome, held by an owner |
| Project | A coarser grouping used for source monitoring and outreach |
| Run | The durable unit of execution working toward an objective |
| Turn | A bounded, leased slice of work within a Run |
| Action call | The durable record of one Skill invocation |
| Approval | A durable checkpoint holding an action call pending a decision |
| Artifact | Immutable versioned metadata plus digest-addressed content |
| Activity event | An append-only record of a state transition |

Activity events are written in the same durable transaction as the change they describe, so the feed cannot drift from the state it reports. That property is what makes `GET /api/v1/activity` usable as an audit surface — see [Operations](operations.md).

## Two Execution Paths

OpenSeal carries a second, separate execution path alongside the durable kernel: a deterministic runbook layer of typed nodes defined in HCL and executed in one process by `openseal run`.

| Property | Durable kernel | Deterministic runbooks |
|---|---|---|
| Unit of work | Runs and Turns | A directed graph of typed nodes |
| Durability | Persisted, leased, restart-safe | Single process, single invocation |
| Approvals | Participates | Does not participate |
| Activity feed | Records every transition | Does not appear |
| Entry point | The API and the terminal client | `openseal run` |

The two do not share a lifecycle. A runbook executed by `openseal run` creates no Run, produces no activity events, and passes through no approval. See [API](api.md) for the runbook node reference.

## Next Steps

- [Agents and Teams](agents-and-teams.md) covers definitions, deployments, activation, and amendment.
- [Objectives and Runs](objectives-and-runs.md) covers the portfolio and the Run lifecycle.
- [Skills and Approvals](skills-and-approvals.md) covers governed action execution.
