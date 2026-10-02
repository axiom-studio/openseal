# Scheduled agent tasks

Chat exposes scheduled work through the bundled `openseal.runbooks` Skill:

- `create_task` saves an active Objective and an Objective-owned routine activation with a self-contained goal, explicit six-field cron, IANA timezone, and optional occurrence limit.
- `list` pages through the current Agent or Team's routines and returns their exact IDs, revisions, schedules and reporting destination.
- `set_status` pauses, resumes or permanently retires future occurrences using a revision check. It does not cancel an execution already in progress; use Run controls for that.
- `start` runs an existing activation on demand. `replace_schedule` creates a new generation after retirement, retaining work, authority, budget and reporting.

Scheduled tasks use the existing persisted activation cursor, replica-safe scheduler, deterministic occurrence Run IDs, concurrency admission and terminal reporting. The execution method is `task` rather than an immutable Runbook graph. Each occurrence is ordinary hosted Agent work, resolving the current deployment and its current tool bindings and approval requirements. Scheduling never copies credentials or confers future action authority.

The kernel derives the owner, tenant, assigned Agent, reporting conversation and originating user message from the durable chat Run. Creation requires a user message visible to that channel and cannot be invoked recursively from background work. Callers cannot choose a foreign tenant, Agent or reporting chat. Task results and failures appear in the original conversation; retries do not duplicate results. Archived or unavailable destinations and inactive deployments suspend execution.

The create and lifecycle operations retain normal write-risk policy and approval evaluation. Their action receipts confirm saved work; they do not assert future reads or writes succeeded. Exact action idempotency and lifecycle action receipts recover partially completed retries, while stale revisions from different calls remain conflicts.

Chat-authored tasks have an independent lifecycle from an Agent's embedded Runbook. Agent definition amendments and workforce projection replacement preserve those tasks. Persistence uses the existing tenant-filtered Objective, Runbook activation, Run and conversation stores; no new tables or schema migration are needed.

A bounded browser test can request: "Every minute in Asia/Kolkata, use the browser to fetch the current public forecast and post a sourced summary here. Run exactly twice, then stop." Verify two scheduled executions, two resulting messages, and retirement at the occurrence ceiling. Use a temporary conversation rather than altering an existing user's routine.
