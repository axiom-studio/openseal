# Terminal UI

The OpenSeal TUI is the primary standalone interactive surface. It is a thin
client over the versioned kernel API: closing it does not stop work, and the TUI
never maintains a second execution or persistence layer.

Start the daemon, then launch the workspace:

```bash
openseal daemon --config ./local.yaml
openseal
```

## Connect and select context

Standalone OpenSeal defaults to scope `local:default` and owner
`agent:operator`. Both are explicit and configurable:

```bash
openseal tui \
  --endpoint http://127.0.0.1:8080 \
  --scope local:research \
  --owner team:market-research \
  --download-dir ./research-artifacts
```

`--endpoint` accepts a server origin or an explicit versioned API root.
`OPENSEAL_API_URL` supplies its default. Repeatable `--header name=value`
options carry non-secret host selectors. Authentication secrets belong in an
authorization-aware transport or local proxy, not command history.

The TUI refreshes every five seconds. `--poll=-1s` disables polling for
deterministic terminal testing.

## Capability-driven workspace

At connection time the TUI reads `/api/v1/capabilities`. Only advertised
sections and actions become interactive. An unavailable, unauthorized, stale,
or incompatible operation remains read-only instead of falling back to local
state.

Depending on the connected server, the workspace can expose:

- **Workforce** — describe Agents and Teams, answer sequential refinements,
  place approved credential references and binding configuration, inspect
  evaluation, resolve eligible requirements, and apply a reviewed ChangeSet
- **Readiness** — Agent deployment state, compilation history, and governed
  definition amendments, including self-proposed conversation changes and
  direct invocation of callable operations from the exact active runbook
- **Teams** — Team definitions, roster, lifecycle, and governed amendments
- **Objectives / Projects** — multi-objective portfolios, schedule supervision,
  and project context
- **Sources** — durable event-source subscriptions, connector health, checkpoints,
  and CAS-protected pause, resume, and retirement
- **Work** — Runs, lifecycle commands, guidance, evidence, and grounding
- **Requests / Approvals** — delegation, handoffs, clarification, completion,
  and exact action decisions
- **Channels** — durable Team conversations, coordination audit, read cursors,
  and presence
- **Integrations** — capability-advertised conversation gateway registration,
  lifecycle, and connection state supplied by the connected host
- **Skills** — ClawHub lifecycle, deployment bindings, model-visible actions,
  and governed source policy lifecycle
- **Outreach** — evidence-linked drafts and governed delivery Runs
- **Activity / Evidence** — canonical audit projection and artifact downloads

Workforce authoring persists a reviewable ChangeSet. The TUI displays the
server-provided lifecycle, actor, reason, policy decisions, approval
eligibility, exact candidate digest, receipt, and created resource references.
It does not infer authority from candidate content.

When an Agent candidate contains a deterministic runbook, Workforce also shows
its named callable operations, typed input/output fields, exact pinned Skill
actions, matching schedule or event triggers, budgets, approval threshold, and
checkpoint failure behavior. Use `j`/`k` to select an operation and `m` to
describe a scoped change. The resulting prompt creates a child ChangeSet and
preserves unrelated candidate state; reviewing an operation never activates it.
Agents whose work is entirely cognitive do not show an Automations section.

After activation, **Readiness** projects callable operations from the exact
active Agent definition. Select an operation with `j`/`k`, press `n`, and
provide one credential-free JSON object matching its displayed input contract.
`Ctrl+S` creates a canonical durable Run with the active deployment as owner
and assignee and the selected entrypoint; the TUI then opens that Run in
**Work**. Failed transport attempts preserve both the draft and idempotency key
for safe retry. Paused deployments, historical or malformed definitions,
cognitive-only Agents, and kernels without advertised Run-create authority
remain read-only.

## Navigation keys

Section shortcuts work while the list panel is focused:

| Key | Section |
| --- | --- |
| `f` | Workforce authoring |
| `h` | Agent readiness |
| `T` | Teams |
| `o` | Objectives |
| `S` | Event sources |
| `i` | Projects |
| `O` | Outreach |
| `I` | Conversation integrations |
| `s` | Skills |
| `w` | Work / Runs |
| `R` | Agent requests |
| `A` | Action approvals |
| `t` | Activity |
| `c` | Channels |
| `a` | Artifacts / evidence |

Common controls:

| Key | Result |
| --- | --- |
| `Tab` | Move between composer and current list |
| `Ctrl+S` | Submit the current composer operation |
| `↑` / `↓` or `k` / `j` | Select an item or current requirement |
| `n` | Begin a supported creation or install operation; in Readiness, start the selected active runbook operation |
| `r` | Refresh; in a failed Workforce proposal, prepare a governed retry |
| `m` | Open the contextual message, refinement, or amendment composer; in Activity, load the next page |
| `p` | Pause/resume selected Run, Agent, Team, Project, or event source; pin/unpin a Skill |
| `g` | Guide a selected active Run; in Objectives, reconcile due schedules |
| `y` / `x` | Approve/accept or reject the selected eligible governed item |
| `?` / `M` | Request or provide clarification for an Agent request |
| `e` / `Enter` | Apply, edit, evaluate, complete, or expand according to current section |
| `[` / `]` | Move among relevant credential, configuration, amendment, Skill, or evidence choices |
| `{` / `}` | Move among outreach actions or grounding pages |
| `b` | Save selected Workforce placement or begin a Skill binding |
| `v` / `V` | Verify/activate or expand evidence/grounding according to section |
| `u` / `U` | Update selected or all installed ClawHub Skills |
| `d` | Download selected artifact |
| `D` | Create a delivery Run for a selected outreach draft |
| `P` | In Skills, review and register an immutable source policy version |
| `Y` | In Skills, activate one reviewed source policy version at an exact revision |
| `X` | In Skills, revoke active source authority at an exact revision |
| `Esc` | Cancel the current composer mode |
| `Ctrl+C` | Exit without stopping server work |

The footer shows only actions meaningful for the current selection and
advertised capability. `Ctrl+S` is the consistent commit key for text entered
in the composer.

## Safety and retry behavior

Creation and governed mutations use client-generated idempotency keys. A failed
request preserves the intent and key, so retrying cannot duplicate work or a
permanent decision. Lifecycle commands bind the selected revision and refresh
after a conflict rather than overwriting concurrent changes. Applying a
Workforce additionally binds the candidate digest and requires an audit reason.

Source policy registration and activation are deliberately separate. `P`
records credential-free immutable bounds; it grants no network access. Review
the version, then use `Y` with lifecycle revision `0` for the first activation
or the displayed current revision for a change. `X` revokes authority without
deleting its versions or audit history. Hosts may replace the TUI actor with an
authenticated identity, but credential values never belong in these forms.

Credential choices displayed by the TUI are opaque server-authored references.
Secret values are never entered into the authoring prompt or stored in TUI
state.

Event-source creation also separates configuration from activation. `n` in
Sources opens a reviewable `key: value` form. A new subscription is always
created paused; `p` activates or pauses it at its exact revision, and `x`
opens an explicit `RETIRE` confirmation without deleting its health or
checkpoint history. `connector` is
either `host:<id>[@version]` or
`skill:<id>@<exact-version>#<action>`; Skill connectors additionally require
`binding: <id>@<revision>`. `parameters` is a JSON object and must never contain
credential values. Health reports and checkpoint advancement remain
connector-owned operations: the TUI observes them but cannot fabricate them.

## Run execution history

The Runs inspector loads canonical `AgentTurn` and `ActionCall` records only
for the selected Run when the server advertises `agent-turns` v1 and
`action-calls` v1. Turns show their durable sequence, lifecycle status,
provider/model identity, concise output, and governed action summaries.
Continuation checkpoints, model inputs, private provider payloads, leases, and
hidden reasoning are never available to the TUI.

Skill executions show the immutable Skill, action, binding revision, lifecycle
status, attempts, risk, side effect, and stable call identity. Invocation
arguments, prepared runtime state, and credential references are intentionally
not rendered. Selection changes, manual refresh, and background Run
reconciliation reload both timelines from durable kernel state; the TUI does
not infer execution from chat text or Run checkpoints.

## Artifact downloads

Downloads stream from the kernel into a temporary file, verify catalog size and
SHA-256 digest, and publish atomically with private file permissions. A failed
or corrupted transfer leaves no partial destination. The TUI does not persist
content URLs or interpret an opaque content reference as a filesystem path.

## Governed outreach

Drafting requires an immutable source observation, an external Skill action
whose schema declares target and body semantics, a public profile, truthful
affiliation and disclosure, and an explicit approval policy. Drafting does not
send anything. Delivery creates a canonical durable Run; message status, Run,
ActionCall, approval, terminal outcome, and provider receipt reload from kernel
state after reconnect or restart.
