# Agents and Teams

An Agent is a versioned definition plus a durable deployment. A Team is the same, with semantic roles, a roster, a policy, and its own Skill bindings on top. Both are owners: both can hold objectives and produce Runs.

## Definitions and Deployments

The definition is immutable content. The deployment is the mutable record that points at whichever definition version is currently active. Separating them makes activation, rollback, and amendment first-class operations rather than destructive edits.

| Concept | Description |
|---|---|
| Definition | An immutable, versioned Agent or Team specification |
| Compilation | The record produced when a definition is compiled for a deployment |
| Deployment | The durable, scoped record that owns an active definition version |
| Activation | The event that makes a definition version current for a deployment |
| Amendment | A proposed change to the active definition, carried through review |

Because a definition version is never edited in place, the history of what a deployment was running at any point is recoverable, and a rollback is an activation of an earlier version rather than a restoration from backup.

## The Amendment Sequence

An amendment moves through a fixed sequence rather than a single write. Each step is a separate route and a separate durable record.

```text
propose → evaluate → decide → activate
```

A rejected amendment leaves an auditable trail. An amendment that is resolved but not activated does not silently take effect — activation is its own explicit step. The same four-step sequence exists for Agent deployments and Team deployments.

| Step | Description |
|---|---|
| Propose | Record the intended change against the active definition |
| Evaluate | Assess the proposal against policy |
| Decide | Record an explicit decision on the proposal |
| Activate | Make the amended definition version current |

## Rollback

Agent deployments carry a direct rollback operation in addition to the amendment sequence, which activates a previously active definition version without composing an amendment for it. Rollback is an Agent deployment operation; Team deployments reach the same outcome by activating an earlier version.

## Teams

Teams are modeled exactly as Agents are — definitions, deployments, activations, and the same four-step amendment sequence — with four additional structures.

| Property | Description |
|---|---|
| Semantic roles | Named roles the Team's work is organized around |
| Roster | Assignments binding members to roles |
| Policy | The Team's own governance settings |
| Skill bindings | Grants held by the Team, distinct from any individual Agent's |

Team-held Skill bindings are genuinely separate from the bindings held by individual Agent deployments. Granting a Skill to a Team does not grant it to that Team's members individually, and vice versa. See [Skills and Approvals](skills-and-approvals.md).

## Installing an Agent From a Manifest

A portable Agent manifest can be installed directly, producing a deployment in one call, without composing a definition and an activation separately.

```bash
curl -X POST http://127.0.0.1:8080/api/v1/agent-installations \
  -H 'Content-Type: application/json' \
  --data @examples/agents/rowan-greenwood/agent.json
```

The repository carries a worked example at `examples/agents/rowan-greenwood/agent.json`. Portable installation is advertised as a feature of the agent definitions capability, so it appears only when the configured store implements the agent registry contract.

## Agent Requests

Agents ask each other for work through durable requests rather than direct calls. A request is created, answered with one or more responses, completed, and optionally passed through a completion review.

```text
create → respond → complete → completion review
```

Each step is a durable record. Because the exchange is persisted rather than held in a process, a request outlives the Run that raised it and survives a daemon restart. Requests appear in the terminal client's **Inbox** destination.

## Where Owners Appear

An owner reference is a `type` and `id` pair, and it is what connects everything on this page to the rest of the system.

| Property | Description |
|---|---|
| Objectives | Every objective names an owning Agent or Team |
| Runs | A Run inherits the owner of the objective that produced it |
| Skill bindings | Held by a specific Agent deployment or Team deployment |
| Terminal client | The `--owner` flag sets the owner for newly created work |

The terminal client defaults to `agent:operator`. Changing it changes who owns work created from that session, not who is permitted to create it — OpenSeal performs no authentication. See [Security](security.md).

## Next Steps

- Give an owner something to pursue in [Objectives and Runs](objectives-and-runs.md).
- Grant it governed capabilities in [Skills and Approvals](skills-and-approvals.md).
- Generate a whole workforce from a prompt in [Workforces](workforces.md).
