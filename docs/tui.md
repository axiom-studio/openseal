# OpenSeal Terminal UI

The OpenSeal TUI is the primary standalone interactive surface. It is a thin
client over the versioned kernel HTTP API: closing the TUI does not stop work,
and the TUI never maintains a second execution or persistence layer.

Start the daemon first, then launch the workspace:

```bash
openseal daemon
openseal
```

The workspace composes and operates canonical Agents, Teams, objectives, Runs,
Team channels, and the durable Artifact/Evidence catalog. Workforce authoring
starts with an outcome prompt and persists a reviewable ChangeSet. When the
connected host advertises the exact contextual authority, the TUI can resolve a
specific approval requirement and atomically Apply the reviewed Agent, Team,
and standing-objective resources. It then shows the durable policy decisions,
lifecycle, actor, reason, receipt, and created resource references.

Every control comes from the server's versioned capability document, including
resource-specific revision and approval eligibility. The TUI does not infer
authority from a ChangeSet or manufacture a local policy evaluator. An
unavailable, unauthorized, stale, or incompatible operation remains visibly
read-only.

## Workspaces

Standalone OpenSeal defaults to the `local:default` scope and the
`agent:operator` owner. Both are explicit and configurable. Team deployments
are listed from the selected scope and retain their immutable definition
version, semantic roster, policy bounds, status, and optimistic revision:

```bash
openseal tui \
  --endpoint http://127.0.0.1:8080 \
  --scope local:research \
  --owner team:market-research \
  --download-dir ./research-artifacts
```

`--endpoint` accepts either a standalone server origin or an explicit
versioned kernel API root mounted by a host. Hosts that require non-secret
scope-routing metadata can be reached with repeatable `--header name=value`
selectors. Authentication secrets belong in an authorization-aware HTTP
transport or local proxy, not command-line headers.

`OPENSEAL_API_URL` can supply the default endpoint. The TUI refreshes work every
five seconds; `--poll=-1s` disables automatic refresh for deterministic
terminal testing.

## Keyboard model

| Key | Result |
|---|---|
| `Ctrl+S` | Start work or submit guidance from the composer |
| `Tab` | Move between the composer and current work |
| `↑` / `↓` or `k` / `j` | Select an item or approval requirement |
| `f` / `T` / `o` / `w` / `c` / `a` | Open Workforce, Teams, Objectives, Work, Channels, or Evidence |
| `n` | Compose a new objective, Run, or Team channel in the current section |
| `r` | Refresh from the kernel |
| `p` | Pause or resume selected Team deployment or work when advertised |
| `g` | Guide selected work when advertised |
| `x` | Stop selected work when advertised |
| `m` | Message the selected Team channel when advertised |
| `y` / `x` | Approve or reject the selected eligible Workforce requirement |
| `[` / `]` | Select an authorized credential choice for the highlighted Workforce requirement |
| `b` | Save the selected typed Workforce credential bindings |
| `e` / `Enter` | Apply a ready Workforce, edit an objective, or expand selected evidence/audit |
| `d` | Download selected artifact when advertised |
| `Esc` | Cancel guidance composition |
| `Ctrl+C` | Exit the TUI without stopping work |

Creation and governed mutations use client-generated idempotency keys. A failed
request preserves the intent and key, so retrying cannot duplicate work or a
permanent approval. Lifecycle commands bind the selected resource revision and
candidate digest and refresh after conflicts rather than overwriting concurrent
changes. Team pause/resume preserves its definition, roster, and restrictions,
binds the exact deployment revision, records an actor and reason, and reloads
the authoritative result. Apply additionally requires an explicit audit reason.

Artifact downloads stream directly from the kernel into a temporary file,
verify the catalog size and SHA-256 digest, and are atomically published with
private file permissions. Failed or corrupted transfers leave no partial file.
The TUI never persists content URLs or treats an opaque content reference as a
filesystem path.

Skills remain capability inputs to Workforce authoring and runtime execution;
the TUI does not render lifecycle controls that the connected server has not
advertised. The same rule applies to every future surface: no placeholder
production controls or client-only durable state.
