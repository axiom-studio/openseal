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
- **Readiness** — Agent deployment state and compilation history
- **Teams** — Team definitions, roster, lifecycle, and governed amendments
- **Objectives / Initiatives** — multi-objective portfolios and project context
- **Work** — Runs, lifecycle commands, guidance, evidence, and grounding
- **Requests / Approvals** — delegation, handoffs, clarification, completion,
  and exact action decisions
- **Channels** — durable Team conversations, coordination audit, read cursors,
  and presence
- **Skills** — ClawHub lifecycle, deployment bindings, model-visible actions,
  and governed source policy lifecycle
- **Outreach** — evidence-linked drafts and governed delivery Runs
- **Activity / Evidence** — canonical audit projection and artifact downloads

Workforce authoring persists a reviewable ChangeSet. The TUI displays the
server-provided lifecycle, actor, reason, policy decisions, approval
eligibility, exact candidate digest, receipt, and created resource references.
It does not infer authority from candidate content.

## Navigation keys

Section shortcuts work while the list panel is focused:

| Key | Section |
| --- | --- |
| `f` | Workforce authoring |
| `h` | Agent readiness |
| `T` | Teams |
| `o` | Objectives |
| `i` | Initiatives |
| `O` | Outreach |
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
| `n` | Begin a supported creation or install operation in the section |
| `r` | Refresh; in a failed Workforce proposal, prepare a governed retry |
| `p` | Pause/resume selected Run, Agent, Team, or Initiative; pin/unpin a Skill |
| `g` | Guide a selected active Run |
| `m` | Post to a selected channel, propose a Team purpose amendment, or load more activity |
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
