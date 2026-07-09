# OpenSeal Autonomous Agent Runtime Architecture

**Status:** Accepted target architecture

**Date:** 2026-07-10

**Tracking:** `axiom-zipb`, `axiom-zipb.1`

## Decision

OpenSeal agents are prompt-first, durable autonomous workers that may own and pursue
multiple objectives concurrently. Skills are typed, governed capabilities. A
workflow node is an optional adapter for composing or visualizing a skill; it is
not the canonical capability model.

OpenSeal is the agent kernel and canonical implementation of portable execution
semantics. It exposes the runtime as a stable Go library and as a standalone
daemon over the same `Engine`. Atlas is the agent OS: it embeds that library and
registers enterprise operating services, adapters, and additional capabilities.
OpenSeal must not import Cortex or depend on an Atlas service to provide its
core behavior.

All work enters one execution model:

```text
AgentDefinition + AgentDeployment
                 |
                 v
        Objective portfolio
                 |
     event / chat / schedule / API / handoff
                 |
                 v
              Run DAG
                 |
        leased durable worker
                 |
    plan -> decide -> skill -> checkpoint
                 |
     wait / delegate / approve / continue
                 |
                 v
        outcome + artifacts + evaluation
```

Agent chat and team chat are command and observation surfaces over this model.
They do not own separate execution engines, task types, or audit histories.

## Repository and product boundaries

```text
OpenSeal
  Portable models and contracts
  Engine, scheduler, workers, checkpoints, local stores
  Agent brain, skills, workflows, triggers, collaboration
  Standalone daemon, API, CLI, and lightweight OSS UI
       ^
       | github.com/axiom-studio/openseal/pkg/openseal
       |
Atlas
  Agent OS built around the embedded OpenSeal Engine
  Enterprise capability and infrastructure adapters
  Cluster-local execution and skill transports
       ^
       | enterprise control and product APIs
       |
Cortex / Sentinel
  Tenant and RBAC control plane, marketplace, policy administration,
  deployment, vault, kubelink, NATS, enterprise persistence
       ^
       | capability schemas, commands, events, and projections
       |
Studio
  Prompt-first authoring, observation, approvals, intervention, and audit
```

The import direction is one-way: Atlas imports OpenSeal. OpenSeal never imports
Cortex packages. Atlas currently imports `cortex/pkg/agent/executor` and
`cortex/pkg/agent/resolver`; those imports are transitional and will be replaced
by the OpenSeal public facade and Atlas-owned adapters.

The public facade must cover runtime construction, definitions, objectives,
runs, skill contracts, activity subscriptions, and extension registration. A
downstream consumer must not need to import OpenSeal `internal` packages or copy
OpenSeal implementation packages into its own repository.

Atlas extensions fall into two categories:

- **Adapters** implement OpenSeal interfaces for persistence, scope/identity,
  authorization and policy, secrets, artifacts, event publication, models, and
  skill transport.
- **Capabilities** register Atlas-only skills and event sources such as
  dashboards, tenant-aware kubelink operations, deployment systems, and other
  enterprise services.

Studio obtains a capability catalog and schemas from Atlas. It does not assume
that the OpenSeal OSS capability set and the Atlas capability set are identical.
The same API envelope and activity projection render both.

## Product principles

1. **Prompts describe intent; contracts govern execution.** Users should be able
   to create and change agents using natural language. The compiler produces
   versioned definitions, typed skill bindings, policies, objectives, and event
   subscriptions that can be validated before activation.
2. **Long-running work is durable.** Work that matters never depends on an HTTP
   request, process-local map, goroutine, pod, or LLM response remaining alive.
3. **Agents own portfolios, not one task.** An agent can pursue many independent
   objectives and several runs at once, subject to priority, budget, policy, and
   concurrency limits.
4. **Everything important is observable.** Plans, decisions, skill calls,
   approvals, handoffs, artifacts, failures, retries, and outcomes share one
   ordered activity model that can be projected anywhere.
5. **Authority is explicit and least-privileged.** Skills execute with the
   receiving agent's tenant-scoped bindings. Agents request outcomes from one
   another; they do not exchange raw credentials.
6. **Collaboration is composition.** A team adds roster, delegation, review,
   memory, and escalation policy over the same objectives and runs used by an
   individual agent.
7. **Behavior changes are versioned.** An agent may learn and propose changes to
   its instructions, skills, heuristics, and objectives. Activation follows the
   definition's amendment policy and remains auditable and reversible.
8. **The UI is optional for authoring.** The primary interface is prompt/API.
   Studio exists for discovery, observation, intervention, approvals, policy,
   audit, and advanced inspection.

## Canonical entities

Every persisted entity below is tenant-owned. Cross-entity references must be
validated in the active tenant by repositories and services, not only handlers.

### AgentDefinition

An immutable, versioned description of what an agent is and how it should
behave. It contains:

- identity, purpose, personality, and system instructions;
- operating principles and domain context;
- declared skill requirements;
- default authority, risk, budget, memory, and escalation policies;
- objective templates and evaluation criteria;
- allowed self-amendments and the approval policy for activating them.

The definition must not contain live credentials, environment placement, or
mutable execution state.

### AgentDeployment

A tenant-local activation of an `AgentDefinition` version. This replaces the
overloaded runtime meaning of `AgentInstance`. It owns:

- environment and runtime placement;
- enabled skill bindings and credential references;
- active definition version and rollout state;
- capacity, concurrency, and operational health;
- deployment-specific policy restrictions.

Deployment policy can only narrow the definition's authority unless an
explicitly authorized amendment widens it.

### Objective

A persistent desired outcome owned by one agent or team. An agent owns a
portfolio of objectives. Examples include:

- keep a production service healthy;
- turn shipped product changes into marketing material;
- follow up with qualified leads;
- monitor transactions for anomalous financial behavior.

An objective contains purpose, status, priority, scheduling and event rules,
success measures, budgets, constraints, review cadence, dependencies, and its
current progress summary. Objectives may be long-lived and may generate many
runs. The term `standing_goal` is a legacy storage/API name and will be migrated
to `Objective`; `mandate` is accepted product language for an objective with an
explicit operating charter.

Objective states are `draft`, `active`, `paused`, `satisfied`, `failed`, and
`retired`. Satisfied objectives can be reactivated when their desired condition
ceases to hold.

### Run

A durable, bounded workstream created to advance an objective or satisfy a
one-off request. Runs form a DAG through parent/child and dependency links. A
run owns:

- goal and source envelope;
- assignment and lease state;
- priority and scheduling metadata;
- plan and durable checkpoint;
- budget consumption;
- policy snapshot;
- activity events, skill calls, decisions, approvals, handoffs, and artifacts;
- outcome and evaluation.

Run states are `queued`, `planning`, `running`, `sleeping`,
`waiting_for_dependency`, `waiting_for_agent`, `waiting_for_approval`,
`waiting_for_event`, `completed`, `failed`, and `canceled`.

A waiting run holds no worker. Its checkpoint and wake condition are persisted.

### AgentTurn

One resumable reasoning step within a run. It records the input context
references, definition and model configuration used, plan revision, decisions,
requested actions, compacted output, token usage, and continuation checkpoint.
Raw provider chain-of-thought is neither required nor exposed. The audit trail
records concise decision rationale and evidence suitable for operators.

### Skill and SkillBinding

A `Skill` is a versioned capability contract. Each action declares:

- semantic name and description;
- typed input and output schemas;
- side-effect and risk classification;
- required permissions and credential kinds;
- timeout, retry, idempotency, and concurrency semantics;
- dry-run and compensation support;
- emitted artifacts and events;
- execution endpoint and health metadata.

A `SkillBinding` enables selected actions for an `AgentDeployment`, applies
field restrictions, and references tenant-owned credentials. The runtime
provides the model only the allowed action schemas; it resolves secrets at the
execution boundary and never places secret values in prompts or activity
events.

### AgentRequest and Handoff

An agent collaborates by sending a typed `AgentRequest` to an agent or team. It
contains the requested outcome, context references, acceptance criteria,
priority, deadline, budget offer, and artifact references. The receiver may
accept, reject, ask for clarification, or negotiate constraints.

Acceptance creates a child run assigned to the receiver. Completion returns a
typed result and artifacts to the requesting run. The receiver executes using
its own skill bindings and credentials. Credential references cannot be copied
between deployments through a request.

A `Handoff` transfers responsibility for an existing workstream. An
`AgentRequest` asks another agent to perform related work while preserving the
requester's responsibility. Both use the same lineage and event primitives.

### ApprovalCheckpoint

An approval is a persisted policy checkpoint on a proposed decision or skill
call. It includes the exact proposed action, diff or dry-run when available,
risk, evidence, expiration, eligible approvers, and continuation checkpoint.

Approvals can be requested from a person, policy group, or another agent that
has explicit approval authority. Approval authority is a permission, not an
incidental team role. Approving never grants access to the approver's
credentials; the original executor resumes under its existing binding.

### Artifact

Artifacts are durable outputs such as pull requests, patches, reports,
dashboards, campaign drafts, messages, datasets, or investigation bundles.
Large content lives in an object or domain store; the run stores a typed,
tenant-owned reference, content hash, provenance, and retention policy.

### ActivityEvent

Every meaningful state change appends an immutable activity event. The common
envelope contains:

```text
id, tenant_id, timestamp, event_type, severity
agent_deployment_id, objective_id, run_id, turn_id
parent_run_id, team_id, conversation_refs
actor_type, actor_id, summary, payload_ref
visibility, correlation_id, causation_id
```

Events are append-only and ordered per run. Producers may publish them through
NATS after the database commit, using an outbox or equivalent delivery
guarantee. Consumers must be idempotent.

## Multi-objective scheduling

Each deployment has an objective portfolio and a scheduler policy. The default
policy considers:

- explicit objective and run priority;
- deadlines and event urgency;
- objective dependencies;
- fairness and starvation age;
- current skill, model, token, monetary, and time budgets;
- per-agent, per-skill, and per-environment concurrency;
- risk and approval availability;
- team commitments and accepted agent requests.

An agent may run several workstreams concurrently, but one run is single-writer:
only the worker holding its lease may advance its checkpoint. Child runs and
independent objectives provide parallelism. The scheduler can preempt between
turns, never in the middle of a non-idempotent skill call.

Objective progress is derived from runs and evaluations, then stored as a
compact current summary for planning. It is not inferred from chat history.

## Durable execution protocol

1. An initiator resolves the tenant and creates or selects an objective.
2. It appends a run and initial activity event in one transaction.
3. A worker atomically claims an eligible run with a bounded lease.
4. The worker loads the definition version, deployment policy, objective,
   checkpoint, compacted memory, and available skill contracts.
5. One `AgentTurn` plans the next bounded set of actions.
6. Each proposed action passes authorization, policy, budget, schema, and
   idempotency checks.
7. Skill calls and approval checkpoints are persisted before dispatch.
8. Results and artifacts are persisted before the run checkpoint advances.
9. The worker renews its lease while active and releases it when waiting.
10. Lease expiry makes the run reclaimable from its last durable checkpoint.
11. Completion evaluates the outcome against objective/run acceptance criteria
    and updates objective progress.

Provider response IDs may optimize a turn but are not the sole durable state.
The platform must be able to reconstruct the next turn from its own persisted
checkpoint and referenced artifacts.

## Event subscriptions and continuous agents

Kubernetes watches, schedules, webhooks, message streams, analytics thresholds,
and domain events are `EventSubscription` resources. A subscription routes a
normalized event to an objective and applies deduplication, coalescing,
rate-limiting, and severity policy before creating a run.

Continuous monitoring is not one immortal LLM call. The subscription is
long-lived; each actionable event creates or wakes a bounded durable run. This
allows an SRE agent to operate indefinitely without keeping reasoning state in
an informer callback or goroutine.

## Teams

A team is a durable roster plus collaboration policy:

- members and functional roles;
- owned objectives;
- routing and delegation rules;
- planning, execution, and review responsibilities;
- shared memory and artifact visibility;
- concurrency, budget, escalation, and approval authority.

Roles are extensible semantic labels such as `finance-lead` or
`release-reviewer`. Permissions and approval authority are separate typed
policy fields. Runtime code must not silently collapse unknown semantic roles
to `worker`.

Team planning creates runs and agent requests. It does not invoke a separate
team-specific brain. Team chat renders the same events as agent chat with team
and member filters.

## Unified activity in chat and operations

Conversations store human and agent messages plus references to objectives and
runs. They do not duplicate execution logs. Any run may reference several
conversation surfaces, and any surface may show the same canonical activity.

The default compact projection groups events into collapsible activity blocks:

```text
Investigating failed checkout deployment                    running
  3 decisions · 5 skill calls · 1 artifact · 8m 24s

  Plan        Inspect rollout, logs, recent GitOps changes
  Evidence    CrashLoopBackOff began after image update
  Action      Opened rollback PR #1842
  Waiting     Release reviewer approval
```

Operators can expand to the full event sequence, inputs with secrets redacted,
outputs, policy decisions, costs, artifacts, lineage, and approval history.
Agent detail, objective detail, team chat, agent chat, and run detail consume
the same projection API and streaming event contract.

## Prompt-first definition compiler

Natural-language creation and editing produce a candidate definition change,
not an unvalidated database mutation. The compiler:

1. resolves requested outcomes into identity, instructions, skills, policies,
   objective templates, subscriptions, and evaluations;
2. identifies missing skills, credentials, permissions, and ambiguous authority;
3. generates a structured version and human-readable change summary;
4. validates schemas, tenant references, RBAC, policy, and deployability;
5. simulates representative scenarios when evaluations exist;
6. activates immediately only when the amendment policy permits it.

The structured definition is canonical. Prompts and conversation are retained
as provenance.

## Current-component disposition

| Current component | Decision |
| --- | --- |
| OpenSeal `pkg/runtime` store, scheduler, worker, retries | Evolve into the Objective/Run kernel; replace channel-authoritative work items with store-driven claims, leases, and checkpoints |
| OpenSeal `pkg/openseal` public facade | Retain and expand as Atlas's only supported embedding API |
| OpenSeal daemon REST execution goroutines | Route through the same `Engine` and durable scheduler used by embedded consumers |
| OpenSeal workflow executor and graph | Retain as deterministic runbook execution beneath the agent runtime |
| OpenSeal agent brain, persona, skills, triggers, and LLM packages | Consolidate behind canonical definition, skill, subscription, and turn contracts |
| Cortex `pkg/autonomy` tasks, events, skill calls, artifacts, leases, handoffs | Migrate useful model and service behavior into OpenSeal; Cortex becomes an enterprise adapter/control plane and deletes its parallel kernel |
| `agent_standing_goal` and mandate routes | Migrate to Objective; preserve temporary compatibility adapters, then delete aliases |
| `AgentInstance` | Migrate runtime responsibilities to AgentDeployment; retain compatibility identity during data migration |
| `Persona` shared primary key and persona CRUD | Move versioned behavior into AgentDefinition; deployment keeps only activation/runtime overrides; remove separate public persona lifecycle |
| `AgentLibrary` and versions | Evolve into AgentDefinition catalog and immutable versions |
| `AgentWorkflow` and visual node graph | Retain as optional deterministic runbook/skill composition, not required agent identity |
| `PersonaTool` workflow wrappers | Replace with SkillBinding and explicit runbook-as-skill adapters |
| OpenSeal and Cortex skill manifests and skill-node adapters | OpenSeal owns the canonical Skill contract; Atlas registers enterprise bindings/transports and removes legacy global/direct-address paths |
| trigger manager and K8s informers | Adapt into tenant-owned EventSubscriptions that create/wake Runs |
| process-local runtime task map | Delete after durable Run checkpoints and status APIs replace it |
| direct background trigger goroutines | Delete after all trigger paths enqueue durable Runs |
| agent chat, team chat, builder chat histories | Keep conversational UX; converge execution and activity on shared Run APIs/events |
| team chat turn/token/cooldown policies | Reuse as scheduler/team policy where generally useful; remove chat-only execution ownership |
| collaboration teams | Retain roster/context value; separate semantic roles from permissions and build delegation on AgentRequest |
| approval service | Retain concept; migrate to ApprovalCheckpoint tied to runs/skill calls and policy authority |
| marketplace agent/team YAML | Evolve schema to definitions, objectives, subscriptions, policies, skill requirements, and evaluations |
| Studio ReactFlow builder | Keep as optional advanced runbook visualization; remove it as the mandatory agent creation path |
| Atlas imports of `cortex/pkg/agent/executor` and `resolver` | Replace with `github.com/axiom-studio/openseal/pkg/openseal` and Atlas adapter packages |

No legacy path is deleted before tenant-safe migration, compatibility reads, and
rollback are proven. Compatibility adapters may not become new extension
points.

## Delivery phases

### Phase 1: kernel

Expand OpenSeal's public facade and introduce Objective, durable Run
checkpoints, worker leases, wake conditions, and the canonical activity
envelope by evolving OpenSeal `pkg/runtime`. Make the standalone daemon use the
same `Engine`. Add recovery, idempotency, scope isolation, and worker lifecycle
tests. Atlas then supplies tenant-aware enterprise implementations of the
portable store and policy interfaces.

### Phase 2: execution convergence

Route schedules, Kubernetes events, webhooks, one-off chat work, mandates, and
handoffs through OpenSeal Run creation. Replace OpenSeal daemon goroutines,
Atlas trigger goroutines, and process-local task status. Attach skill-call
traces and artifacts directly to runs.

### Phase 3: capability and policy convergence

Ship the canonical Skill/SkillBinding contract, credential boundary, policy
engine integration, and ApprovalCheckpoint. Remove legacy global skill exposure
and direct endpoint discovery.

### Phase 4: agent and team model

Add versioned AgentDefinition and AgentDeployment compatibility views. Migrate
Persona and AgentLibrary behavior. Add objective portfolios, AgentRequest, team
delegation, extensible roles, and shared evaluation.

### Phase 5: product surfaces and removal

Ship prompt-first creation and amendment, compact activity projections, and
unified chat/run streaming. Migrate marketplace definitions. Delete superseded
routes, tables, services, and Studio components once usage and rollback gates
are satisfied.

## Operational requirements

- Every user-facing row, event, child record, join, callback, and cache is
  tenant-owned or explicitly documented as platform-internal.
- Repository and service APIs fail closed without tenant context.
- RBAC distinguishes reading definitions/activity, changing behavior, managing
  objectives, executing work, approving risk, binding credentials, and
  administering deployments.
- Every external callback authenticates before resolving its tenant-owned
  subscription.
- Secrets are redacted from prompts, events, logs, errors, and artifacts.
- Worker claim, checkpoint, skill dispatch, outbox publication, and wake-up
  paths have crash and duplicate-delivery tests.
- Run and objective APIs support bounded pagination and retention/compaction.
- The runtime publishes latency, queue age, lease loss, retry, budget, skill,
  model, approval wait, outcome, and evaluation metrics.
- Rollouts support compatibility reads, backfill verification, feature gates,
  and rollback without losing run lineage.

## Embedding and extension contracts

The OpenSeal public package owns small interfaces rather than enterprise data
models. At minimum, the `Engine` accepts implementations for:

- `ExecutionStore`: transactional objective, run, checkpoint, lease, event,
  approval, request, handoff, and artifact metadata persistence;
- `ScopeProvider`: portable ownership/scope identity attached to every command;
- `Authorizer` and `PolicyEvaluator`: permission and risk decisions;
- `SecretResolver`: opaque credential references resolved only at execution;
- `ArtifactStore`: large typed content and provenance;
- `EventPublisher`: post-commit activity and wake notification publication;
- `ModelProvider`: model invocation and accounting;
- `SkillTransport`: local, gRPC, Kubernetes, or enterprise execution transport;
- `Clock` and ID generation for deterministic recovery tests.

OpenSeal supplies local single-user implementations, including SQLite or other
portable storage. Atlas supplies tenant-aware Postgres/control-plane adapters,
Vault resolution, NATS/JetStream publication, kubelink transports, and RBAC
policy. Scope is intentionally generic in OpenSeal but mandatory on persisted
records; Atlas maps it to tenant identity and fails closed when missing.

The embedded and daemon modes must pass the same conformance suite. HTTP
handlers, CLI commands, trigger callbacks, and Atlas dispatchers call the
`Engine`; none may execute a pipeline or brain loop directly.

## Non-goals

- Keeping an LLM request alive for hours.
- Allowing prompts to bypass typed policy or authorization.
- Sharing raw credentials between agents.
- Exposing private chain-of-thought as an audit feature.
- Requiring a visual graph to create an agent.
- Encoding business roles as a fixed authorization enum.
- Building a second execution kernel specifically for teams or chat.
