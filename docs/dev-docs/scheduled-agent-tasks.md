# Scheduled agent tasks

Chat exposes routines through the bundled `openseal.runbooks` Skill, version 1.9.0. A routine runs on a schedule or when something happens:

- `create_task` saves an active Objective and an Objective-owned routine activation with a self-contained goal and a `trigger` object holding exactly one of `schedule` or `event`. `trigger.schedule` takes an explicit six-field `cron`, an IANA `timezone` and an optional `maximumOccurrences`. `trigger.event` takes a `kind`, a `source` from `event_sources`, an optional `filter`, `calendar` timing for calendar routines, `includeBots`, `maxPerHour` (default 20) and an optional `maximumOccurrences`. The 1.8.0 top-level `cron`, `timezone` and `maximumOccurrences` are gone.
- `event_sources` lists what the current Agent or Team can listen to: its Slack and Telegram connections with their channels, GitHub repositories with `routines` callback subscriptions, the person's own Google sign-in for Gmail and Calendar, and a new webhook address when the host can derive hook secrets. A source with `available: false` needs the person first, for example their Google sign-in.
- `send_test_event` sends a test event to an event routine so the person sees one run. Only the routine's creator can call it, at most 5 times per routine per hour, and test runs do not count toward the hourly limit or `maximumOccurrences`.
- `list` pages through the current Agent or Team's routines and returns their exact IDs, revisions, triggers and reporting destination.
- `set_status` pauses, resumes or permanently retires future occurrences using a revision check. It does not cancel an execution already in progress; use Run controls for that. Resuming clears a system pause reason.
- `set_script` makes a script routine run its chat file's current version (see below).
- `start` runs an existing activation on demand. `replace_schedule` creates a new generation of a scheduled routine after retirement, retaining work, authority, budget and reporting; it refuses event routines.

Scheduled routines use the existing persisted activation cursor, replica-safe scheduler, deterministic occurrence Run IDs, concurrency admission and terminal reporting. The execution method is `task` rather than an immutable Runbook graph. Each occurrence is ordinary hosted Agent work, resolving the current deployment and its current tool bindings and approval requirements. Scheduling never copies credentials or confers future action authority.

The kernel derives the owner, tenant, assigned Agent, reporting conversation and originating user message from the durable chat Run. Creation requires a user message visible to that channel and cannot be invoked recursively from background work. Callers cannot choose a foreign tenant, Agent or reporting chat. Task results and failures appear in the original conversation; retries do not duplicate results. Archived or unavailable destinations and inactive deployments suspend execution.

The create and lifecycle operations retain normal write-risk policy and approval evaluation. Their action receipts confirm saved work; they do not assert future reads or writes succeeded. Exact action idempotency and lifecycle action receipts recover partially completed retries, while stale revisions from different calls remain conflicts.

Chat-authored tasks have an independent lifecycle from an Agent's embedded Runbook. Agent definition amendments and workforce projection replacement preserve those tasks. Agent definitions themselves declare schedule triggers only; event and channel triggers on definitions are gone.

## Routines that start on an event

An event routine is the same activation with `Trigger{Kind: event}` and `ScheduledAgentTask.Event` set. `create_task` resolves the model's `source` through the same catalog as `event_sources` for this run's owner and acting user and never trusts the string itself. It also requires:

- a signed-in person (`run.ActingUser`), who becomes `Task.CreatedBy`;
- for Slack and Telegram, a `channel` or `chat` condition (`equals` or `one_of`) naming only conversations that are active endpoints of this Agent or Team on that connection;
- for Gmail and Calendar, the person's own Google sign-in, not a shared link;
- for a webhook, `source: "new"`; the kernel creates the routine's hook before the activation commits;
- that every message-sending grant is limited to the person (`toOwnerOnly`) or to named destinations, since anyone who can trigger the routine controls part of its input.

The approval preview carries `event: {kind, sourceLabel, when, whoCanTrigger, filter, maxPerHour, maximumOccurrences, calendar, webhook}` next to `schedule`. `when` and `whoCanTrigger` are plain sentences from `DescribeRoutineEventTrigger` and `DescribeRoutineEventTriggerAudience`.

Each occurrence is an ordinary Run with `Source: event` and a fresh session seeded with the brief and one entry holding the batch of events as outside data. Event Runs always use manual approval mode, so only the brief's grants run without asking. An occurrence that changed nothing may finish with `silent: true`; then nothing is posted, in HelloSeal or in a mirrored Slack or Telegram chat. At most one run per routine is active at a time, at most `maxPerHour` batches of up to 50 events start per hour, and events that arrive meanwhile go into the next batch.

A routine can be paused by the system with a `pausedReason`: `needs_sign_in` (the Google sign-in is gone or revoked), `source_removed` (the connection, channel or GitHub registration is gone) or `unverified` (the Agent, chat or request message is gone).

**Hooks.** A webhook routine's address is `/orchestrator/agent/kernel/v1/hooks/{hookId}` on the host. Senders sign the body with the routine's secret in `X-HelloSeal-Signature` (`t=<unix>,v1=<hex HMAC-SHA256 of t.body>`) or GitHub's `X-Hub-Signature-256`, so a GitHub repository webhook can target a routine directly. A private path-token address exists for tools that cannot sign; it is off by default. The secret is derived by the host from a platform key and the hook's revision and is never stored. Only the creator can read or rotate it.

**Google sync.** Gmail and Calendar are polled by the host, not pushed: one leader-gated sync loop per person and Google account with at least one active routine, Gmail through `users.history.list` and Calendar through `events.list` with a sync token. Calendar `starts_in` and `ended` routines fire from stored rows that the routine scheduler tick picks up.

The matcher, dispatcher, hook ingress, Google loop and their load limits are described in [Event routines](event-routines.md).

**Binding upgrade.** 1.9.0 changes `create_task`'s input schema, so moving existing `openseal.runbooks` 1.8.0 bindings needs one platform-operator approval of 1.8.0→1.9.0 in the Skill update queue. The upgrade planner's `GrantActions` adds `event_sources` and `send_test_event` to each moved binding without restoring actions the owner removed. Until the approval, a 1.8.0 binding has no working routine actions; existing routines keep running. `openseal.agents` 1.6.0 (where `configure_channel` no longer takes `trigger`) moves the same way.

**Storage.** Event routines add Postgres migration 80 (`routine_events`, `routine_event_batches`, `hooks` and the activation index columns `event_kind` and `event_source_ref`), with matching SQLite and memory stores. Migrations 79 and 81 drop the old event-source and event-wait tables.

## Routines that run a saved script

Work that is the same steps with new data each time can run a saved script instead of re-planning with the model. The Agent writes and tests the script in the chat's sandbox with `workspace.run_command`, then proposes `create_task` with `script: {path, command, timeoutSeconds?}`. The path is a normalized chat file path of plain characters; the command runs only that file (`python3 <path>`, `node <path>` or `bash <path>`, then plain arguments without quotes or shell characters); the timeout is at most 600 seconds (default 300).

When the proposal is made, the kernel reads the chat's files (`WithChatFileContent`, through `chatfs`) and writes `script.digest` (the file's content hash) and `script.filesVersion` (the immutable chat files snapshot holding it) into the arguments, discarding any model-supplied values. The approval preview carries `script: {path, digest, filesVersion, command, timeoutSeconds}`, which the brief card shows as its **Runs** line, and approval stores exactly that pin on `ScheduledAgentTask.Script`. Later edits to the file never change what the routine runs. `Engine.RoutineScriptStatus` reports `changed` (and `missing`) when the chat file's current digest differs from the pin; `set_script {activationId, expectedRevision}` re-pins to the current version and, like `create_task`, is always approved with a preview carrying `script`, `previousScript`, title, goal, schedule or event and grants. Keeping the approved version needs no call.

Approving a script's version (applying `create_task` or `set_script`) sets `script.approvedAt`; it is never proposed and is not part of the approved arguments. `RoutineScriptStatus.pinned` and `set_script`'s `previousScript` carry it, and a conversation's routine projection carries `script: {path}`.

An occurrence is an ordinary agent run in its chat: the model runs the script with `workspace.run_command`, as it would in the chat, then reports and may take more steps (for example granted posting actions). There is no separate execution path. Two kernel guards keep it to the approved script:

- **Pin.** The pin applies to every model step of the occurrence's Run, whose Run context carries the pin it started with (`routineScript`). The `routine_script` system section tells the model to run exactly the approved command first. Each `workspace_run_command` host tool call that invokes the file carries the pin as `HostToolCall.CommandPins [{path, digest, filesVersion, command, timeoutSeconds}]`, and the host attaches it with `workspace.PinRunCommand`: the host-only argument `expectedDigest {path, digest, filesVersion}`, which is not in the model's input schema. A host honoring `expectedDigest` restores the file from snapshot `filesVersion` when the working copy differs, after checking its digest, and refuses to run otherwise (result `refused`); the result echoes `expectedDigest` when it ran. The host reports what the pinned command did (`workspace.RecordPinnedCommand`: `{command, workingDirectory?, path, digest, exitCode, timedOut, refused, outputTail}`); the kernel validates it against the pin and stores it on that call's tool entry in the Run's session log (`ToolEntry.PinnedRun`).
- **Completion check.** A final answer of a Run whose tool entries show no clean run of the approved command, from the chat files root, at the pinned digest, exiting 0, does not complete it. The kernel records a final failure of kind `routine_script` (code `routine_script_failed`; "The routine's script did not run: …", "… exited with status N: …" with the last output lines, "… timed out after N seconds: …", or "… could not run: …"), and the model, in a tool-free explanation step with the script failure instruction, explains it in place of its report. The Run fails with that explanation.

A bounded browser test can request: "Every minute in Asia/Kolkata, use the browser to fetch the current public forecast and post a sourced summary here. Run exactly twice, then stop." Verify two scheduled executions, two resulting messages, and retirement at the occurrence ceiling. Use a temporary conversation rather than altering an existing user's routine.
