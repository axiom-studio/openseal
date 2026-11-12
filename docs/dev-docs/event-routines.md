# Event routines

An event routine is a routine that starts when something happens instead of on
a clock: a Slack or Telegram message in a named channel, a GitHub event, a new
email in the creator's Gmail, a Google Calendar event, or a request to the
routine's own webhook address. People create them in chat through
`openseal.runbooks` 1.9.0 (see [Scheduled agent tasks](scheduled-agent-tasks.md)).

This guide covers how events get from a source to a Run. The binding contract is
in the event-routines `SPEC.md`; this page names the code and the limits.

## Model

An event routine is an ordinary routine activation: a `RunbookActivation` with
`Task != nil`, `Trigger{Kind: event}` and `Task.Event` set to a
`RoutineEventTrigger` (`pkg/models/routine_event.go`). The trigger holds the
event kind, one `SourceRef`, an AND list of filter conditions, calendar timing
for calendar routines, `includeBots` for chat sources, `maxPerHour` (1 to 60,
default 20) and an optional `maximumOccurrences`. An event Task always has a
non-empty `CreatedBy`. Definition-backed activations are schedule only; Agent
definitions cannot declare event or channel triggers.

Every occurrence is a normal run-loop Run with `Source: event`, a fresh run
session and the routine's brief. There are no checkpoints, event waits or
definition entrypoints.

| Kind | SourceRef | Producer |
| --- | --- | --- |
| `slack.message`, `telegram.message` | `conversation:` + hash of deployment, binding, adapter and installation | conversation ingress (`routine_event_ingress.go`) |
| `github.event` | `callback:<registrationID>` | callback consumer `routines` |
| `gmail.message`, `calendar.event` | `google:<userID>:<googleSub>` | host Google sync loop (cortex) |
| `webhook` | `hook:<hookID>` | host hook ingress (cortex) |

Chat routines must name their channels or chats, and each must be an active
endpoint of the routine's Agent or Team. Direct messages never feed routines.
Bot messages are skipped unless `includeBots` is set. Gmail and Calendar
routines use only the creator's own Google sign-in.

## Matcher and index

Producers hand a `RoutineEvent` to `RoutineEventMatcher`
(`routine_event_matcher.go`). The matcher never starts a Run and never calls a
provider.

The matcher keeps an in-memory index (`routine_event_index.go`) of the
`(scope, kind, sourceRef)` keys that have at least one active event routine. It
is a map behind an `atomic.Pointer`, swapped copy-on-write per scope, so readers
take no lock. `Listening(scope, kind, sourceRef)` is just that lookup.
Producers call it before they build an event, and `OfferRoutineEvent` checks it
first, so an event with no bound routine costs one map lookup: no query, no
allocation (`BenchmarkRoutineEventOfferUnbound`,
`TestRoutineEventOfferUnboundAllocFree`).

The index is kept current by signals, not polling:

- `RunbookActivationService` refreshes the local scope after every committed
  activation write and calls the host's `RoutineIndexPublisher`.
- Other processes receive the signal and call `Engine.RefreshRoutineIndex(scope)`
  (one indexed query). Cortex publishes on the Valkey channel
  `openseal:routine-index:v1`.
- A full rebuild runs at start, when the host's subscription reconnects
  (`Engine.RebuildRoutineIndex`), and every 15 minutes as a backstop.

On an index hit, `OfferRoutineEvent` validates the event, loads the active
routines for that key (an indexed query), checks the owner, applies each
routine's filter and inserts a `pending` row in `routine_events`. The dedupe key
is `(scope, activationID, event.ID)`, so a repeated delivery is counted as a
duplicate. When a routine already has 500 waiting events, or the tenant has
20 000, the row is stored as `dropped` with no payload. Then the matcher nudges
the dispatcher through a buffered channel and returns.

Test events (`send_test_event`, or `POST …/test-events` in cortex) go to one
routine only, skip the filter, are limited to 5 per routine per hour, and only
the routine's creator can send them.

## Dispatcher and caps

`RoutineEventDispatcher` (`routine_event_dispatcher.go`) starts the Runs. Its
loop wakes on the matcher's nudge or a 10 second tick. The host runs it through
`Engine.RunRoutineEventDispatcher(ctx, interval, leads)` and passes its sweep
leader check, so only one process starts runs; a single-process host passes
`nil` (always leads). The standalone daemon does not start the loop yet. Cortex reads the interval from
`OPENSEAL_ROUTINE_EVENT_INTERVAL_SECONDS` (default 10).

For each routine with waiting events it:

1. Closes out routines that are no longer active (pending events are dropped).
2. Waits while the routine already has a non-terminal Run (one active run per
   routine, the scheduler's `backpressured` rule).
3. Skips the rest of the tenant's work for this pass when the tenant has 10
   event Runs not yet finished (`RoutineEventTenantRunCap`, option
   `WithRoutineEventTenantRunCap`).
4. Retires the routine when its real batches reach `maximumOccurrences`, and
   claims only test events while it has started `maxPerHour` real batches in
   the last 60 minutes.
5. Claims a batch of at most 50 events (test events form their own batches) and
   starts one Run through the shared `startActivationOccurrence` with the
   idempotency key `routine-event:<batchID>`.

Occurrence counts come from durable batch rows, so a crash cannot lose or
double an occurrence. Once an hour the dispatcher checks that each chat and
GitHub source still exists and pauses the routine with `source_removed` when
it does not. Once a day it prunes started and dropped event rows older than
7 days, and hooks whose routine is retired or gone.

The Run's session gets the brief plus one `routine_event` user entry
(`routine_event_seed.go`). Its text is fixed kernel wording that says the
events are outside data; the events themselves are one JSON part when they fit
64 KiB, or a chat file `routine-events/<batchID>.json` through the host's
`RoutineEventFileKeeper`. Event Runs always use manual approval mode, so only
the brief's grants run without asking. A routine occurrence may finish with
`silent: true` when it changed nothing; then nothing is posted.

## Hooks

A webhook routine gets a hook when it is created: ID
`hk_` + 26 base32 characters derived from the scope and activation ID, stored
in the kernel with a revision and a `pathTokenEnabled` flag. The kernel stores
no secret.

Cortex serves the public address
`/orchestrator/agent/kernel/v1/hooks/{hookId}` (and `/{hookId}/{token}`) in
`cmd/sentinel/hooks.go`:

- Per-hook (30 a minute, burst 60) and per-IP token buckets reject with `429`
  before the body is read or any lookup is made. A resolved hook is also
  limited per tenant (300 a minute).
- The body is capped at 256 KiB (180 KiB for non-JSON). A known hook is cached
  for 60 seconds.
- The secret is derived, never stored: `whsec_` + HMAC-SHA256 of the hook ID
  and revision under the platform key `OPENSEAL_HOOK_KEY` (at least 32 random
  bytes, from a Kubernetes Secret). Without the key, `SetHooksAvailable(false)`
  and webhook routines are not offered. Rotating the key breaks every hook.
- A request must carry `X-HelloSeal-Signature: t=<unix>,v1=<hex HMAC of t.body>`
  within 5 minutes, or GitHub's `X-Hub-Signature-256` over the body. The path
  token URL works only when the creator switched it on.
- Event IDs and summaries come only from signed material, so retries dedupe and
  unsigned headers are ignored.

Only the creator can read the secret, rotate it, switch the path token or send
test events; others who can manage the Agent see the address only. Audit
logging skips `/hooks/` bodies and masks path tokens.

## Google sync loop

Gmail and Calendar are polled, never pushed: no Pub/Sub, no watch channels, no
public Google endpoint. Cortex runs one loop (`cmd/sentinel/google_sync.go`)
on the sweep leader:

- **Reconcile, every 60 seconds:** pages
  `Engine.ListRoutineEventSubscriptions` for Gmail and Calendar routines and
  keeps one `google_sync_accounts` row per (tenant, scope, person, Google
  account), shared by all that person's routines. Accounts nobody needs are
  deleted. The person's sign-in is checked when a routine first appears and at
  most hourly after that.
- **Sync tick, every 5 seconds:** due accounts go through a pool of 8 workers,
  so at most 8 accounts sync at once. Gmail runs every 60 seconds after new mail
  and backs off to 5 minutes when idle; Calendar every 2 minutes, backing off to
  10. Every next time has jitter.
- **Gmail:** `users.history.list` since the stored history ID, at most 500
  messages a tick. A new account only records the current history ID; nothing
  older is sent. The sender goes in `from` only when DMARC passes aligned with
  the From domain; otherwise it goes in `fromUnverified`.
- **Calendar:** `events.list` with a sync token for created and updated events.
  A 48-hour window of upcoming events is stored as `google_calendar_fires` rows
  for `starts_in` and `ended` routines; the existing 30 second routine scheduler
  tick offers the due ones. A fire more than 10 minutes late is dropped.
- **Errors:** 429, 5xx and network errors back off that account only, from 1
  minute doubling to 30. A revoked or missing sign-in pauses the person's
  routines with `needs_sign_in` and the account stops syncing.

Both steps ask `Engine.RoutineEventListening` before building events.

## Load budget

Event routines must not slow the platform down:

- An event with no listening routine costs one map lookup.
- The index refreshes on activation-change signals; the full rebuild is a
  15 minute backstop.
- Nothing runs per routine: no goroutine, timer or Google watch. Google work is
  per account, at most 8 at a time.
- Fan-out is bounded: one active run per routine, `maxPerHour` batches of up to
  50 events, 10 event Runs per tenant, 500 waiting events per routine and
  20 000 per tenant (`TestRoutineEventBurstRespectsCaps`).
- Webhook floods are rejected before the body is read or any lookup is made
  (`TestHookIngressRateLimitedBeforeResolve`).
- Event and dedupe rows are pruned after 7 days; payloads over 64 KiB go to chat
  files.

`Engine.RoutineEventStats()` returns lock-free counters: events in, matched and
dropped per kind, match latency (index hits only), queue depth and runs started.
Cortex exports them as `sentinel_routine_events_total`,
`sentinel_routine_event_match_seconds`, `sentinel_routine_event_queue_depth`,
`sentinel_routine_event_runs_started_total`, and counts Google calls in
`sentinel_google_api_calls_total`.
