# Skills, Actions, and Approvals

A Skill is a typed definition of what an Agent can do outside its own reasoning. Skills are registered in a catalog, bound to specific deployments, and invoked as action calls at execution time — and an action that policy holds stops at a durable approval rather than proceeding.

## Finding a way to complete the task

Agents are instructed to apply relevant Skills proactively and look at what actions can accomplish, rather than requiring a Skill named after the task. The creator receives the same direction when selecting capabilities and describing Agent behavior. Existing hosted Agents receive this guidance at runtime without rewriting their definitions.

For example, graphing the S&P 500 for the last month may use a dedicated data integration, a documented service learned through the generic API Skill, a verified MCP service, or an accessible browser source, followed by an available chart capability. These are routes to investigate, not a guarantee that every deployment has the tools or source access needed.

Agents can continue successful authorized tool chains. A failed operation ends the current reply with a clear explanation of what failed, why, and how the user can retry after correcting the problem. They do not automatically retry the failed operation or run repeated discovery and setup loops. When setup or a decision is missing, they should identify that prerequisite. This guidance does not grant bindings, bypass approvals, or supply credentials, and it does not guarantee model compliance.

## Skills and Bindings

Registering a Skill does not grant it. The binding is the unit of least privilege: each Agent deployment or Team deployment carries only the bindings it has been given, and a Skill in the catalog that is bound to nothing can be invoked by nothing.

| Concept | Description |
|---|---|
| Definition | A typed description of a Skill, registered in the catalog |
| Binding | A grant of one Skill to one Agent or Team deployment |
| Action call | The durable record of one Skill invocation |
| Approval | A durable checkpoint holding an action call pending a decision |

Bindings are managed per deployment. They can be listed, replaced, and disabled, and Agent deployments and Team deployments each carry their own independent set.

### Binding Upgrades

Changing which Skill reference a binding points at is an explicit two-step operation rather than an in-place edit.

```text
upgrade-plan → upgrade
```

`upgrade-plan` computes what the reference change would do; `upgrade` applies it. Both steps exist for Agent deployments and Team deployments. Both are withdrawn from the advertised capability when the configured store does not implement the upgrade contract.

An upgrade that requires review before it can be applied is refused with `428 Precondition Required` rather than being applied silently.

### Credentials from a conversation

`request_credential` asks the user to save one credential in the host vault through a durable in-chat card. It is not Skill setup: built-in Skills such as the live browser are always installed, and nothing about a binding changes. Its input is `kind` (`website_login` with `website` set to the exact page origin, or `payment_card`) and a `reason` shown to the user. It never accepts or returns secret values.

A pending request parks the Run with wake condition `credential_request`. The host verifies that a matching credential was saved after the request (or that the user dismissed the card), then calls `ResolveCredentialRequest`, which resumes the Run with guidance to retry the blocked step. When the Run has already ended, the result reports `continued: false` and the host starts a new reply. Requests appear on conversation change sets as `credentialRequests`.

### Interaction requests never end a Run

When `request_setup` or `request_credential` cannot create its card, the action still succeeds with a `refused` result whose message the model explains to the user. A failed interaction request is never a terminal action failure.

### Setup from a conversation

`request_setup` selects an exact authorized discovery result. Copy its Skill ID and source identity; a publisher's namespace spelling is not an access rule. A general account connection can request `skillVersion: "latest"` without selecting individual actions. Discovery and a pending form grant no access.

Independent conversation tasks and their same-Agent fork children can open the form in the original Agent conversation after the immutable task ledger, source turn, actor, message, thread and work lineage are verified. The setup still records the actual work Run and action call. Copied conversation fields on unrelated background work never authorize a form or reveal pending setup requests.

An existing account keeps its scoped binding ID and revision even when its executable is older than the current installed version. Such a request enters `binding_upgrade` first. The user reviews the canonical upgrade plan and applies it; accepted work using the old version must finish before the upgrade can proceed. A failed or blocked operation reports its reason and waits for the user to retry. It does not repeatedly run setup or poll for completion in the same reply.

After the canonical upgrade commits, the host verifies its persisted lifecycle provenance and rebases the request to `configuration` using the account's new revision. The user must then save configuration or complete reauthorization. Changing the executable version alone never completes setup, and ordinary binding saves cannot change a binding's Skill ID, source, or version.

The optional setup fields `phase` and `bindingVersion`, and the optional binding lifecycle field `skillUpgrade`, persist in existing JSON records. Records without a phase retain the previous configuration semantics. Hosts and clients that support the upgrade phase must exchange these fields together; older clients must not treat an upgrade as configuration completion. Credential references contain no grant freshness evidence: the host's credential and OAuth policy must validate renewal before a reauthorization save. This flow does not extend credential expiry, scopes, or permissions.

## Action Calls

An action call is the durable record of one Skill invocation, and its status covers the full lifecycle including compensation.

| Status | Description |
|---|---|
| `ready` | Admitted and awaiting execution |
| `waiting_for_approval` | Held at a durable approval checkpoint |
| `denied` | Refused by policy |
| `running` | Currently executing |
| `succeeded` | Completed successfully |
| `failed` | Completed with an error |
| `canceled` | Withdrawn before completion |
| `compensating` | Running a compensating action |
| `compensated` | Compensation completed |

Action calls are read-only over the API. They are created by the execution path, not by clients.

### Failed and rejected actions

A failed action, and a single action proposal rejected before it ran (input schema, required evidence or observation checks), is returned to the model as that action's result rather than ending the Run. The next Turn sees the exact arguments and error (`lastAction`, or `_opensealToolFeedbackCorrection.rejectedProposal` for a rejection) and answers with one corrected action or a final explanation in its own words. Bounds:

- an unchanged failed request is never sent again: proposing it denies the call and stops the attempt;
- at most `MaximumToolFeedbackCorrections` (2) corrected proposals per failure while they keep failing; a correction that succeeds ends that failure;
- at most `MaximumRecoveredToolFailures` (4) failures per Run are returned to the model, counted by the kernel across successes;
- the correction Turn cannot schedule, wait, fork or delegate, and every corrected proposal still passes policy, approval and budget checks.

Beyond these bounds, and for platform conditions no correction can fix (a source rate limit, the platform's browser connection), the attempt stops with the kernel's failure reply. A failed change may already have taken effect, so the model is told to verify before doing it again.

An input schema property can declare `x-openseal-observationRef: {roles, requireEnabled, requireFlags}`: the argument must be an element reference from the latest successful observation with an allowed role, enabled, and with each named flag set to `true` by the observing Skill. It is checked when the proposal is admitted, before any approval is created. An omitted optional argument is not checked.

## Approvals

When policy requires review, the action call stops and a durable approval is created. Because the approval is a persisted record rather than a blocked goroutine, a Run waiting on a decision survives a daemon restart.

| Status | Description |
|---|---|
| `pending` | Awaiting a decision |
| `approved` | Permitted to proceed |
| `rejected` | Refused |
| `changes_requested` | Returned for modification |
| `expired` | Passed its decision deadline |
| `canceled` | Withdrawn before decision |

Approvals can always be listed and read. Resolving one is a separate matter: it requires an approval authorizer, which the standalone daemon installs only under `--standalone-operator`. Without that flag, `POST /api/v1/action-approvals/{id}/decisions` returns `501 Not Implemented` and the capability advertises inspection only.

> **Standalone approvals do not attribute.** Under `--standalone-operator` the action policy's approver list contains exactly one principal, `user:local`, and every approval recorded in that deployment carries the same identity regardless of who actually decided. The records are auditable but they do not identify a person. See [Security](security.md).

Approvals appear in the terminal client's **Approvals** destination.

## Credentials at Execution Time

Skills that need credentials never receive credential values through the API or through prompts. A Skill definition names an opaque credential reference; the value is resolved by a worker at execution time, for the exact action call that carries the reference.

Secret values do not enter durable state, capability responses, prompts, or the terminal client. What crosses into the authoring and proposal layers is the reference and its display label, never the value. See [Configuration](configuration.md) for how references are declared and resolved.

## The Bundled Skill Definitions

Four canonical Skill definitions ship inside the binary and can be exported as YAML manifests.

| Field | Value |
|---|---|
| Source | Governed source access |
| Document | Document handling |
| Outreach | Governed external outreach |
| Delivery | Delivery to an external destination |

```bash
openseal skill manifest <skill-id> --output ./outreach.yaml
openseal skill validate ./outreach.yaml
```

> **The daemon registers one of the four, not all four.** At startup it registers the outreach Skill definition and verifies that any already-persisted copy matches it byte for byte, failing startup if it does not. The source, document, and delivery definitions are exportable through the CLI but are not registered in the catalog by the daemon — installing them is the operator's decision.

## ClawHub Skill Distribution

ClawHub is the registry from which Skills are discovered, verified, and installed. The daemon wires it at startup against the default registry and a skills directory, and the terminal client surfaces it as the **Marketplace** destination.

| Field | Value |
|---|---|
| Default registry | `https://clawhub.ai` |
| Registry override | `CLAWHUB_REGISTRY` |
| Skills directory | `OPENSEAL_SKILLS_DIR`, defaulting to `skills/` beside the daemon configuration file |

### Catalog and Lifecycle Operations

Catalog operations inspect a reference before anything is installed: fetch its metadata, list its versions, read an individual file from it, and verify it. Installation compiles the source and records provenance, including the source digest that ties the installed Skill back to the exact bytes that produced it.

| Operation | Description |
|---|---|
| Catalog read | Fetch metadata, list versions, read a file, verify a reference |
| Install | Compile the source and record provenance including the source digest |
| Verify installed | Re-check installed files against their recorded digest |
| Pin and unpin | Hold an installed Skill at a version, or release it |
| Update | Update one installed Skill, or all of them in a batch |
| Uninstall | Remove an installed Skill |

Read operations are always available once ClawHub is wired. The mutation operations — install, update, pin, unpin, uninstall — are filtered out of the advertised capability unless the host has explicitly granted mutation authority, which the standalone daemon does only under `--standalone-operator`.

### Canonical Error Codes

ClawHub is the only subsystem in OpenSeal with a canonical error-code mapping. Failures are classified into a stable, secret-free code that appears in the JSON body alongside the HTTP status.

| Code | HTTP Status | Description |
|---|---|---|
| `pinned` | 409 | The installed Skill is pinned and cannot be changed |
| `locally_modified` | 409 | The installed files no longer match their recorded digest |
| `ambiguous_reference` | 409 | The reference matches more than one Skill |
| `verification_failed` | 422 | Signature or digest verification did not pass |
| `not_found` | 404 | The reference does not resolve |
| `canceled` | 502 | The operation was canceled or timed out |
| `unavailable` | 502 | Any other registry or lifecycle failure |

`unavailable` is the classifier's default branch, so an unrecognized failure is reported as `unavailable` rather than misclassified as something specific. Client code should treat the code as the stable signal and the message as human-readable text.

## Governed Outreach

Outreach is external communication carried out under policy, scoped to a project. Threads are created against a project, messages are added to a thread, and delivery of an individual message is a separate, separately governed step.

Outreach is advertised only when the configured store implements the project, source monitor, outreach, and outreach action contracts together. Thread creation additionally requires a Skill catalog, and delivery additionally requires a wired dispatcher.

> **Delivery is refused for a scope whose source policy does not enable outreach,** even under `--standalone-operator`. Enabling outreach is a property of the source policy attached to that scope, not of the operator flag. See [Configuration](configuration.md).

## Next Steps

- Declare credential references and source policies in [Configuration](configuration.md).
- Understand what the operator flag does and does not establish in [Security](security.md).
- Generate a whole set of Agents, Teams, and bindings from a prompt in [Workforces](workforces.md).
