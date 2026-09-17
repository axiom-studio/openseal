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

The desktop host additionally advertises the `agent-runs` operation `create-team`
when it can execute team-owned work. Use the same create route with
`owner: {"type":"team","id":"<team deployment ID>"}` and an
`assignedAgentId` belonging to that team's roster. New work requires an active
team and active roster members. The host constructs the execution budget and
actor; renderer-supplied policy, context, parent linkage, and checkpoints are
rejected. Delegation, acceptance, completion review, and child budgets remain
kernel-owned. Team work can be listed with `ownerType=team&ownerId=<ID>`.

Retain the `Idempotency-Key` and exact request after uncertain delivery. The
desktop host recovers an already accepted matching request before checking
mutable admission state, so a team paused after submission does not prevent
recovering its existing run. A changed request with the same key conflicts;
a new request still requires current admission checks.

`GET /agent-runs` supports scoped pagination with `limit` (1–100) and `offset`.
Use `order=created_desc` for newest-first browsing; the default retains scheduler
ordering. `q` searches the goal and status as a case-insensitive literal substring
(up to 1,000 bytes), before pagination. Repeated or comma-separated `status`
filters combine with the search. `%` and `_` are literal characters, not wildcards.

`GET /action-approvals` supports scoped `status`, `limit` (1–100), and `offset`
filters. Requests are ordered oldest first by default (`order=created_asc`);
`order=created_desc` shows newest requests first. Equal timestamps are ordered by
checkpoint ID for stable page boundaries. Lists are live, so resolving reviews
can move subsequent offset-based pages.

Action approval decisions use `POST /action-approvals/{id}/decisions` with the
checkpoint's `expectedRevision`, a stable `decisionId` (or `Idempotency-Key`),
`principal`, and `decision`: `approve`, `reject`, or `request_changes`. Requesting
changes requires reviewer guidance in `reason`. Rejection and requested changes
block that action while allowing the run to reconsider; approval permits that
exact checkpoint to proceed subject to current runtime authority. The kernel
checks the deadline, waiting run/action pair, and eligible reviewer.

Authenticated `--desktop-operator` mode pins decisions to `user:local-operator`
inside its configured local scope. That local owner may satisfy the default
`role:operator` requirement or an explicit `user:local-operator` requirement;
other users and roles are not impersonated. Ordinary servers remain read-only
until an approval authorizer is configured.

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

Agent deployment lists include retired deployments by default.
Pass `excludeStatuses=retired` to omit them, or repeat the parameter or use commas to exclude multiple rollout statuses.
Supported values are `pending`, `active`, `degraded`, `paused`, and `retired`; unknown values return HTTP 400.
Filtering preserves tenant scope and leaves deployments accessible by ID.
Embedded callers use `Engine.ListAgentDeployments(ctx, openseal.AgentDeploymentFilter{Scope: scope, ExcludeStatuses: []openseal.AgentRolloutStatus{openseal.AgentRolloutRetired}})`.

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

Conversation routes support create/list/get/update, durable message post/list/get,
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

Message history supports `order=desc`, `limit`, and exclusive `beforeSequence`
for browsing older messages, alongside `afterSequence` and `threadRootId`.
The desktop uses 50-message windows plus a lookahead and keeps drafts separate
from immutable messages. Retrying the same idempotency key and unchanged message
returns the saved result, even when the conversation revision has advanced.
A stale revision with a new key returns a conflict; clients must refresh before
explicitly retrying.

`PATCH /conversations/{id}` accepts `scope`, a positive `expectedRevision`, and
at least one of `title` or `status` (`active` or `archived`). It updates the existing
channel without replacing messages or its identity. Archiving stops new messages;
restoring enables posting again. Existing team runs are not canceled. Stale
revisions return 409 and this update endpoint has no idempotency replay contract:
after an uncertain response, read the current channel before making another edit.

In `--desktop-operator` mode, conversation creation and updates are restricted to the configured
local scope, and message posts must name `user:local-operator` as sender. The
bearer-authenticated host enables these checks; renderer-supplied agent or service
identities do not authorize a desktop post. Standalone channel behavior is unchanged.
Cursor PUT also enforces that configured local scope and participant
`user:local-operator`. The `channels` capability includes the `receipts` operation
for the existing cursor GET/PUT contract; receipt availability is independent
of posting. These checks cover creation, updates, posting, and cursor writes,
not a general participant ACL system.
Posting a message through this API does not by itself start agent work.

`GET /conversations` optionally accepts `participantType` and `participantId`.
With a valid reader, the response remains an array and each conversation includes
`readPosition: { participant, readSequence, revision }`. An absent cursor yields
sequence and revision zero without creating or updating a cursor. Omitting both
query fields preserves the original conversation response shape. Invalid reader
parameters return 400; a cursor-storage error fails the request instead of
fabricating unread state. Existing scope, owner, status, and pagination filters
still apply. The server performs a bounded cursor lookup for each listed row;
this is not a new database batch operation. The desktop requests
`participantType=user&participantId=local-operator` when the `channels` capability
includes `receipts`, and shows only positive last-sequence/read-sequence differences.
A missing projection remains unknown. Counts reflect saved reading positions,
not automatic viewport tracking.

Cursor GET identifies its participant with `participantType`/`participantId` and
scope. PUT supplies `scope`, `participant`, `expectedRevision`,
`deliveredSequence`, and `readSequence`; the desktop advances read position only
by explicit user action. Reading/opening a channel does not mutate that position.
After conflict or uncertain delivery, GET the saved cursor before another write;
there is no automatic replay. Client checks reject mismatched reader/channel/scope,
regressing revisions/sequences, or delivery below read position. These desktop
checks preserve the existing standalone behavior.

Messages are the collaboration surface. Run, approval, request, artifact, and
decision records remain canonical and are referenced rather than copied into a
parallel chat execution system.

## Projects, evidence, outreach, and artifacts



| Resource | Routes | Purpose |
| --- | --- | --- |
| Projects | `POST, GET /projects`; `GET, PATCH /projects/{id}` | Durable project context and linked work |
| Source monitor evidence | `/projects/{id}/source-monitors/{monitorId}/observations` and `/checkpoint` | Provenance-linked observations and cursor state |
| Outreach | `/projects/{id}/outreach/...` | Draft threads and create governed delivery Runs |
| Artifacts | `POST, GET /artifacts`; `GET /artifacts/{id}` | Immutable metadata and provenance |
| Artifact content | `POST /artifact-content`; `GET /artifacts/{id}/content`; `POST /artifacts/{id}/resolve` | Stream, verify, or resolve content when configured |

Artifact content operations are advertised only when their store or resolver is
wired. A content reference is opaque; clients must not treat it as a local path.

### Desktop model-generated text files

The desktop `submit_agent_turn` model contract accepts completed-turn
`runOutput.generatedFiles` entries `{name, mediaType, text}` for `text/plain`,
`text/markdown`, `text/csv`, and `application/json`: at most eight files and
256 KiB total UTF-8 content, within the turn token budget. Names must be unique
portable filenames without paths; text/JSON are validated. Model-supplied
`artifactRefs` and extra file metadata are rejected. This is generated output,
not arbitrary filesystem access or a new artifact-upload endpoint.

After the accepted turn is durable, the configured output publisher assigns
host-owned scoped identities, integrity metadata, provenance, and classification,
then publishes content/catalog records before completing the run. Run output
contains `artifactRefs` in place of bodies; turn drafts remain for recovery.
Storage failure pauses work; Resume retries publication from the saved turn
without model reinvocation or duplicate records. Cancellation before provider
return discards files; previously accepted output can have partially published
files and is not an atomic multi-file batch.


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

Portable Agent and whole-workforce bundles are available through the public Go
facade. Whole-workforce export closes Agent, Team, Objective, and Runbook
references before signing:

```go
bundle, err := openseal.ExportAgentBundle(exportRequest)
workforce, err := openseal.ExportWorkforceBundle(workforceExportRequest)
preview, err := openseal.PreviewAgentBundleInstallation(bundle, placement)
plan, err := openseal.CompileAgentBundleInstallation(
    openseal.AgentBundleInstallationRequest{
        Bundle: bundle,
        Scope: targetScope,
        Placement: placement,
        ActorType: "user",
        ActorID: actorID,
        IdempotencyKey: idempotencyKey,
    },
)
```

Use `EncodeAgentBundleYAML` and `DecodeAgentBundleYAML` for the strict portable
file format. Preview and compilation do not resolve secrets: the embedding host
must present authorized opaque credential choices, validate target authority,
and materialize `plan` through its canonical stores. Callback placement maps
the exact bound adapter and credential-free provider configuration; the host
must generate a new ingress route when it applies the plan. Never copy a
source-host credential reference, provider identity, or callback URL into
`placement`.

## Task guidance command

`POST /api/v1/agent-runs/{runId}/commands` with the run's scope accepts
`kind: "intervene"`, `instruction`, `expectedRevision`, `actor`, and optional
`interventionId`. Instructions contain 1–16,000 UTF-8 bytes after trimming.
`interventionId`, when supplied, must be a UUID and is valid only for `intervene`.
An existing ID matching the instruction and actor returns the saved run before
revision comparison; mismatched reuse is invalid. New requests retain ordinary
revision and capability/authority checks. The desktop supplies user
`local-operator` and persists its UUID before submitting.

Intervention keeps paused work paused and wakes sleeping work; it does not
resolve approval or dependency waits. Turns persist `InputInterventionIDs` for
the actual invocation. A completed but unapplied turn whose input interventions
are stale cannot publish its old output or request old actions, including during
recovery; usage is retained and work continues subject to remaining budget.
Already-applied/started actions and partial artifact publication are not undone.
Saved intervention history establishes recorded task context, not that the model
followed the instruction.
