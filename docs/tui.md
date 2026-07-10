# OpenSeal Terminal UI

The OpenSeal TUI is the primary standalone interactive surface. It is a thin
client over the versioned kernel HTTP API: closing the TUI does not stop work,
and the TUI never maintains a second execution or persistence layer.

Start the daemon first, then launch the workspace:

```bash
openseal daemon
openseal
```

The current vertical slice operates canonical Agent- or Team-owned Runs. It
can create work from an outcome prompt, list durable work, pause and resume it,
stop it, and record operator guidance. Controls appear only when the server's
`agent-runs` capability advertises the matching operation. An unavailable or
incompatible server produces an explicit read-only error state.

## Workspaces

Standalone OpenSeal defaults to the `local:default` scope and the
`agent:operator` owner. Both are explicit and configurable:

```bash
openseal tui \
  --endpoint http://127.0.0.1:8080 \
  --scope local:research \
  --owner team:market-research
```

`OPENSEAL_API_URL` can supply the default endpoint. The TUI refreshes work every
five seconds; `--poll=-1s` disables automatic refresh for deterministic
terminal testing.

## Keyboard model

| Key | Result |
|---|---|
| `Ctrl+S` | Start work or submit guidance from the composer |
| `Tab` | Move between the composer and current work |
| `↑` / `↓` or `k` / `j` | Select work |
| `n` | Compose new work |
| `r` | Refresh from the kernel |
| `p` | Pause or resume selected work when advertised |
| `g` | Guide selected work when advertised |
| `x` | Stop selected work when advertised |
| `Esc` | Cancel guidance composition |
| `Ctrl+C` | Exit the TUI without stopping work |

Creation uses a client-generated idempotency key. A failed request preserves
both the prompt and that key, so retrying cannot duplicate work. Lifecycle
commands include the selected Run revision and refresh after conflicts rather
than overwriting concurrent changes.

Future Agent/Team composition, objectives, approvals, skills, artifacts, and
activity views will use the same capability-discovered client boundary. Until
their public APIs exist, the TUI deliberately does not render placeholder
controls for them.
