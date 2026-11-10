# Objectives and Runs

An objective is a durable statement of a desired outcome held by an owner. Objectives produce Runs, and Runs advance in Turns.

## Objectives

Objectives are grouped into portfolios. An objective's status controls whether it is eligible to produce work.

| Status | Description |
|---|---|
| `draft` | Created but not yet admitted to execution |
| `active` | Eligible to produce Runs |
| `paused` | Temporarily withheld from execution |
| `satisfied` | Reached its intended outcome |
| `failed` | Terminated without reaching its outcome |
| `retired` | Withdrawn from the portfolio |

Only `active` objectives produce Runs. An objective moved to `paused` stops producing new Runs but does not cancel Runs already in flight — those are steered through the Run commands described below.

Objectives appear in the terminal client's **Goals** destination.

## Runs

A Run is the durable unit of execution. Every Run carries a kind, which determines which worker family claims it.

| Property | Description |
|---|---|
| `agent_work` | Ordinary Agent or Team work toward an objective |
| `conversation` | Work driven by a channel conversation |
| `workforce_authoring` | Durable generation of a workforce proposal |

### Run Status

Run status covers ordinary progress and every distinct reason a Run can be suspended.

| Status | Description |
|---|---|
| `queued` | Created and awaiting admission |
| `planning` | Determining what to do next |
| `running` | Actively executing a Turn |
| `paused` | Suspended by an explicit command |
| `sleeping` | Suspended until a scheduled time |
| `waiting_for_dependency` | Blocked on another Run |
| `waiting_for_agent` | Blocked on an Agent request |
| `waiting_for_approval` | Blocked on an approval decision |
| `waiting_for_event` | Blocked on an inbound event |
| `completed` | Finished successfully |
| `failed` | Finished with an error |
| `canceled` | Withdrawn before completion |

The four `waiting_*` statuses are deliberately distinct rather than collapsed into one. The distinction is operational: a Run that is `waiting_for_approval` needs a person to make a decision, while a Run that is `waiting_for_dependency` needs another Run to finish. They are resolved by different people through different routes.

```text
queued → planning → running → completed
```

From `running`, a Run can enter any of the six suspended states and return to `running` from each of them, or reach one of the three terminal states:

```text
running → paused | sleeping | waiting_for_dependency | waiting_for_agent | waiting_for_approval | waiting_for_event → running
running → completed | failed | canceled
```

`completed`, `failed`, and `canceled` are terminal. Nothing leaves them.

### Turns

A Run advances in Turns. A Turn is a bounded slice of work claimed by a worker under a lease, and it carries a much simpler status set.

| Status | Description |
|---|---|
| `running` | Claimed and executing |
| `completed` | Finished successfully |
| `failed` | Finished with an error |
| `canceled` | Withdrawn before completion |

The lease is what makes recovery automatic. A worker that dies mid-Turn stops renewing its lease; when the lease expires the claim is released and the work becomes available again, rather than the Run stranding in `running` forever.

## Inspecting and Steering a Run

Runs are inspected and steered through their own routes rather than by editing them.

| Situation | What to do |
|---|---|
| A Run is stuck in `queued` | Read `GET /api/v1/agent-runs/admission`, which reports why a Run would or would not be admitted |
| A Run needs to stop temporarily | `POST /api/v1/agent-runs/{id}/commands` with a pause command |
| A paused Run should continue | The same commands route, with a resume command |
| A Run should be abandoned | The same commands route, with a cancel command |
| A Run needs human correction | The same commands route, with an intervene command |
| A Run's runbook activity needs auditing | `GET /api/v1/agent-runs/{id}/runbook-audit` |

The admission route is the first thing to check when a Run stays `queued`, because the most common cause is that nothing is able to execute it.

> **Run creation requires a worker, and the standalone daemon derives workers from configuration.** The `create` operation joins the Agent Runs capability only when a Run dispatcher has been installed, and the standalone daemon installs one only when at least one worker scope has been derived — which requires at least one credential entry in the standalone context, or a model provider configured under `--desktop-operator` (which adds the daemon's own `--scope`). Without either, inspection and lifecycle intervention work but nothing can create new Runs. See [Configuration](configuration.md).

## Schedules and Event Routines

Runs can start from a schedule or from an event as well as from objectives directly. Both are routines: Objective-owned Runbook activations whose trigger is a schedule or an event. Runbook schedules are reconciled through `POST /api/v1/runbooks/schedule-reconciliations`.

Event routines are created in chat through the `openseal.runbooks` Skill. They start when something happens on a source the Agent or Team already uses:

| Source | What starts the routine |
|---|---|
| Slack, Telegram | A message in a named channel or chat of the Agent's connection; direct messages never start routines |
| GitHub | An event on a repository whose callback registration subscribes the `routines` consumer |
| Gmail | A new email in the routine creator's own mailbox |
| Google Calendar | An event created, updated, about to start, or ended in the creator's own calendar |
| Webhook | A signed request to the routine's own address |

Each routine runs at most one Run at a time and at most a set number of times an hour (20 unless the routine says otherwise). Events that arrive meanwhile wait and go into the next run together. Every event-started Run asks for approval before any action the routine's brief did not grant. Event routines need an embedding host that runs the routine event dispatcher; Gmail and Calendar also need the host's Google sync, and webhooks its hook key. The standalone daemon provides none of these today.

Full route listings are in the [API reference](api.md).

## Next Steps

- Grant governed capabilities in [Skills and Approvals](skills-and-approvals.md).
- Configure the credentials and policies that decide which workers exist in [Configuration](configuration.md).
- Watch work as it happens through the **Work** and **Activity** destinations in the [CLI reference](cli.md).
