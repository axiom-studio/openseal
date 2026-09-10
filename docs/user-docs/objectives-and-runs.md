# Objectives, Projects, and Runs

An objective is a durable statement of a desired outcome held by an owner. Objectives produce Runs, Runs advance in Turns, and Projects are the coarser grouping used for source monitoring and outreach.

## Objectives

Objectives are grouped into portfolios and can be attached to projects. An objective's status controls whether it is eligible to produce work.

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

## Projects

A project groups objectives for the purposes of source monitoring and outreach. Projects own the source monitors that observe external inputs and the outreach threads that carry governed external communication.

| Property | Description |
|---|---|
| Objectives | Objectives attached to the project |
| Source monitors | Observations and checkpoints for watched external sources |
| Outreach threads | Governed external communication, with per-message delivery |

Source monitor observations and checkpoints are read-only over the API. Outreach is covered in [Skills and Approvals](skills-and-approvals.md), including the conditions under which delivery is available at all.

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

> **Run creation requires a worker, and the standalone daemon derives workers from configuration.** The `create` operation joins the Agent Runs capability only when a Run dispatcher has been installed, and the standalone daemon installs one only when at least one worker scope has been derived — which requires either a source policy with outreach enabled, or at least one credential entry in the standalone context. With neither, inspection and lifecycle intervention work but nothing can create new Runs. See [Configuration](configuration.md).

## Events and Scheduling

Runs can be produced by inbound events as well as by objectives directly. Event source subscriptions describe what is watched; events are posted to the kernel and routed to the subscriptions that match.

| Property | Description |
|---|---|
| Subscription | A durable description of an external event source |
| Checkpoint | How far the subscription has consumed its source |
| Health report | An operational status report for a subscription |
| Retirement | Explicit withdrawal of a subscription |

Subscriptions carry their own checkpoints and advance them explicitly, so a restart resumes from the last recorded position rather than replaying from the beginning. Runbook schedules are reconciled through a separate route, `POST /api/v1/runbooks/schedule-reconciliations`.

Full route listings are in the [API reference](api.md).

## Next Steps

- Grant governed capabilities in [Skills and Approvals](skills-and-approvals.md).
- Configure the credentials and policies that decide which workers exist in [Configuration](configuration.md).
- Watch work as it happens through the **Work** and **Activity** destinations in the [CLI reference](cli.md).
