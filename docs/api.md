# REST API

The standalone daemon serves a versioned JSON API under `/api/v1`. The same
resource contracts are used by the terminal client and embedding applications.

## Capability discovery

Start every client session by reading:

```http
GET /api/v1/capabilities
```

The response uses `apiVersion: agent-kernel/v1` and lists each available
capability, its independent version, and its operations. Some responses also
contain resource-specific context such as a ChangeSet revision, approval
eligibility, credential choices, binding-configuration fields, or blocking
requirements.

Routes may exist in the HTTP server while their backing capability is not
wired. A client must not interpret route existence as permission or readiness.
Render only advertised operations and refresh capabilities after a governed
state transition.

## Conventions

- JSON request and response bodies use camel-case fields.
- Scope is explicit. Depending on the route, it is carried in the body or in
  `scopeKind` and `scopeId` query parameters.
- Creation and governed mutations use an `Idempotency-Key` header where their
  request contract requires it.
- Mutable resources use `expectedRevision`; stale writes fail instead of
  overwriting concurrent state.
- Governed application uses the reviewed candidate digest as well as its
  revision.
- Errors are JSON objects with an `error` string.
- List filters and pagination are route-specific. Follow typed response fields;
  do not invent client-side pagination.

Use `GET /api/v1/health` for a process liveness check.

## Runtime and work

| Resource | Routes | Purpose |
| --- | --- | --- |
| Capabilities | `GET /capabilities` | Discover exact server features and operations |
| Objectives | `POST, GET /objectives`; `GET, PUT /objectives/{id}` | Create, list, inspect, and revise owner portfolios |
| Runbooks | `GET /runbooks`; `GET /runbooks/{id}`; `POST /runbooks/schedule-reconciliations` | Inspect Objective-owned execution methods and reconcile due schedule triggers |
| Event sources | `POST, GET /event-source-subscriptions`; item read/update, retirement, health, and checkpoint routes | Durable event watches and connector-owned progress |
| Agent Runs | `POST, GET /agent-runs`; `GET /agent-runs/{id}`; `POST /agent-runs/{id}/commands` | Durable work and pause/resume/cancel/intervention commands |
| Agent turns | `GET /agent-turns`; `GET /agent-turns/{id}` | Durable model-turn lifecycle and usage projection |
| Action calls | `GET /action-calls`; `GET /action-calls/{id}` | Governed Skill execution lifecycle and receipt projection |
| Activity | `GET /activity` | Scoped canonical activity projection |
| Events | `POST /events` | Route a normalized event through activated Runbook event triggers |
| Agent requests | `POST, GET /agent-requests`; item, response, and completion routes | Delegation, clarification, handoff, and completion |
| Action approvals | `GET /action-approvals`; item and decision routes | Inspect and resolve exact action checkpoints |

`POST /agent-runs` is advertised only when the server has a real Run creation
dispatcher. Read and lifecycle operations can remain available independently.

## Agents and Teams

| Resource | Routes | Purpose |
| --- | --- | --- |
| Agent deployments | `GET /agent-deployments`; `GET, PUT /agent-deployments/{id}` | Inspect and operate scoped Agent deployments |
| Agent lifecycle | `/agent-deployments/{id}/compilations`, `/activations`, `/rollbacks` | Compilation history and version activation |
| Agent amendments | `/agent-deployments/{id}/amendments/...` | Propose, evaluate, decide, and activate behavior changes |
| Team definitions | `POST /team-definitions`; `GET /team-definitions/{id}` | Register and inspect immutable Team definitions |
| Team deployments | `POST, GET /team-deployments`; `GET, PUT /team-deployments/{id}` | Create, list, inspect, and operate Teams |
| Team lifecycle | `/team-deployments/{id}/activations` | Activate and audit Team definition versions |
| Team amendments | `/team-deployments/{id}/amendments/...` | Propose, evaluate, decide, and activate Team changes |

Activation and amendment operations bind actor, reason, expected revision, and
the exact reviewed definition or amendment.

The same lifecycle is available prompt-first through portable kernel Skills:
`openseal.agents/amend_behavior` is self-only, `openseal.teams/update_role`
changes current Team role behavior, and `openseal.skills/*` manages exact Skill
authority. Their deployment selectors and concurrency revisions are
kernel-resolved; conversation models never choose those authority facts.

## Prompt-first authoring

Workforce authoring routes are grouped under
`/authoring/workforce/change-sets`:

| Operation | Route |
| --- | --- |
| Create durable proposal | `POST /authoring/workforce/change-sets` |
| Inspect | `GET /authoring/workforce/change-sets/{id}` |
| Answer one refinement | `POST .../{id}/refinements` |
| Place credentials or binding configuration | `PATCH .../{id}/placement` |
| Retry failed generation | `POST .../{id}/retry` |
| Submit policy evaluation | `POST .../{id}/evaluations` |
| Resolve one approval requirement | `POST .../{id}/approvals` |
| Apply atomically | `POST .../{id}/apply` |

`POST /authoring/workforce/compile` is the non-activating compile endpoint. A
durable ChangeSet is the normal interactive path because generation,
refinements, evaluation, decisions, and the apply receipt survive reconnects.

## Skills and ClawHub

| Resource | Routes | Purpose |
| --- | --- | --- |
| Skill catalog | `GET /skills`; `GET /skills/{id}` | Read canonical definitions |
| Agent actions | `GET /agent-deployments/{id}/skill-actions` | Read exact model-visible action surface |
| Agent bindings | `GET /agent-deployments/{id}/skill-bindings`; item `PUT`; item `/disable` | Revisioned least-privilege authority |
| Team bindings | `GET /team-deployments/{id}/skill-bindings`; item `PUT`; item `/disable` | Team-owned role authority |
| Catalog inspection | `GET /clawhub/catalog/{reference}` plus `/versions` and `/file` | Inspect registry source |
| Verification/install | `POST /clawhub/catalog/{reference}/verify` or `/install` | Verify, compile, preview, and install |
| Installed lifecycle | `GET /clawhub/installed`; update-all; item verify, pin, unpin, update, delete | Govern local installed state |

References such as `@owner/name` must be URL-encoded when placed in a path.
Mutation routes are omitted from capabilities unless a trusted operator boundary
enabled them.

## Conversations

Conversation routes support create/list/get, durable message post/list/get,
incremental changes, participation rounds, per-participant cursors, and leased
presence:

```text
/conversations
/conversations/{id}/messages
/conversations/{id}/changes
/conversations/{id}/participation-rounds
/conversations/{id}/cursor
/conversations/{id}/presence
```

Messages are the collaboration surface. Run, approval, request, artifact, and
decision records remain canonical and are referenced rather than copied into a
parallel chat execution system.

## Initiatives, evidence, outreach, and artifacts

| Resource | Routes | Purpose |
| --- | --- | --- |
| Initiatives | `POST, GET /initiatives`; `GET, PATCH /initiatives/{id}` | Durable project context and linked work |
| Source monitor evidence | `/initiatives/{id}/source-monitors/{monitorId}/observations` and `/checkpoint` | Provenance-linked observations and cursor state |
| Outreach | `/initiatives/{id}/outreach/...` | Draft threads and create governed delivery Runs |
| Artifacts | `POST, GET /artifacts`; `GET /artifacts/{id}` | Immutable metadata and provenance |
| Artifact content | `POST /artifact-content`; `GET /artifacts/{id}/content`; `POST /artifacts/{id}/resolve` | Stream, verify, or resolve content when configured |

Artifact content operations are advertised only when their store or resolver is
wired. A content reference is opaque; clients must not treat it as a local path.

## Source policy lifecycle

When a host configures the lifecycle service, `/source-policies` routes support
registering immutable versions, listing and reading versions, activation,
revocation, and lifecycle audit. Source policies contain credential-free HTTPS
host, path, method, item, retention, and outreach bounds. Network credentials
remain outside policy records.

The daemon deliberately has no generic `/workflows` or numeric `/runs`
compatibility surface. `/agent-runs` is the canonical durable autonomous-work
resource. Optional deterministic HCL runbooks are explicit, process-bounded CLI
operations and do not create daemon state.

## Go clients

`pkg/client` contains the thin HTTP clients used by the TUI. Embedded programs
can avoid HTTP and use `pkg/openseal.Engine` directly. The capability document
still defines the contract graphical and terminal clients should render.
