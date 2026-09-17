# OpenSeal desktop

The React and Tauri app includes Home, Agents, Work, Reviews, Teams, and Settings. It owns a
local Go daemon and persists workspace data in the OS application-data directory.
The complete product is still in development: advanced placement and credential
mapping, full team management,
broader approval workflows, advanced channel workflows, and marketplace remain unfinished.

## Proposal review

Home displays generated agents as a structured proposal with expandable instructions,
assumptions, validation, requirements, and typed follow-up questions. Refinement and
retry actions require capabilities for the current proposal revision. Cardinality
hints explain valid answers; conflict refresh preserves an unchanged question's
draft, and stale responses cannot replace the active proposal.

Installation uses distinct **Check installation**, owner review checkbox,
**Approve installation**, and **Install and activate** or **Install without
activating** steps. The final action follows the proposal's activation intent;
unknown intent blocks installation. Capability revision, candidate digest, and
eligible evaluation requirements bind approval to the current proposal. The
receipt states the activation result, receives focus, and offers **View agents**.
For an installed inactive proposal, **Review activation** appears when the
workspace authorizes it for that revision. Preparation creates a deterministic
child proposal for the same resources without another model-generation request;
the original installation receipt is unchanged. The child persists across reloads.
Inherited request and assumption text is explicitly marked as original installation
history. Continue through **Check activation**, the owner review checkbox,
**Approve activation**, and **Activate resources**. The **Activated** heading
receives focus on success. An uncertain preparation request can be retried with
the same idempotency key without duplicating resources.

## Teams

Open **Teams** from navigation, the command menu, or Ctrl/Cmd+5 to inspect the
scoped team catalog when available; status mutations require update capability. Teams sort by name; search locally
across names, purposes, and role names and filter by Active, Draft, Paused, or
Archived status. Expand a team to inspect version/revision, roster, roles, member
bounds, required skills, and operating principles. Team deployment status does
not imply member availability. Available agent records link to their inspector;
missing members retain an explicit unavailable-details label.

**Refresh teams** preserves the prior data on failure and explains it may be
stale. Loading has named busy-group semantics, and empty, filtered, unavailable,
and retry feedback remain explicit. Team definition updates and manual delegation-request management remain unfinished; reviewed roster
editing is supported.

The full 53-browser-test suite passed in about 1.2 minutes before the final
loading-role correction. Final frontend build and Linux native team list/details
smoke passed with synthetic fixtures, alongside the earlier lifecycle. All six
focused tests passed in 8.2 seconds, including delayed-response accessibility
and real scoped SQLite list/reload. Final build, formatting, diff, and native-smoke
Python syntax checks passed. `teams-verdict.md` clears the loading-accessibility
finding with a bounded ship disposition and no regression in refreshed captures. No new design tokens or
ignores were added; this is not whole-app completion and the comp gate stays open.

## Team channels

Open Channels in selected-team details to browse owner-scoped conversations.
The lazy list shows 20 channels plus a lookahead and polls every five seconds.
**Create channel** accepts a title up to 60 characters. Conversation views validate
owner and identity, show the latest 50 messages chronologically, and offer older
sequence-cursor history and return to latest. Sender, time, audience, and reply
metadata accompany inert wrapped text. Archived channels remain readable; posting
requires the relevant capabilities. Refresh failure retains prior content with
stale-data guidance.

Post or reply as user `local-operator`. Replies preserve the original message's
audience and have separate per-message device drafts. The exact pending body/key
is saved before sending; storage failure sends nothing. Uncertain delivery offers
explicit same-request replay, including after reopening. Conflicts preserve the
draft and refresh state without automatic retry; late callbacks are ignored.
Posting records conversation only: it does not start agents, resolve agent
requests, or approve actions.

The desktop host restricts conversation creation/posting to its local scope and
pinned user sender through `SetDesktopConversationScope`; these checks are not a
general participant ACL system. Existing standalone behavior is unchanged.
The messages API exposes `beforeSequence`; see `docs/api.md`.

Five focused channel tests passed in 11 seconds, including real-daemon creation,
post, reply, and reload. Go server/commands/runtime suites and targeted channel
server race tests passed. Production build, formatting, diff, Python syntax, and
direct token checks passed without ignores. Full Linux native smoke passed channel
creation/posting alongside the prior lifecycle. All 81 browser tests passed in
2.2 minutes on the final implementation. `team-channels-finish.md` ships the bounded surface without
material findings. Desktop/mobile/native captures are scrolled team/conversation
samples, not full-page coverage. Automatic agent participation,
other-participant receipt UI, reference kinds beyond work/artifacts/agent requests/approvals, marketplace, and platform delivery remain
unfinished; live-provider quality and other operating systems are unverified.
The comp gate stays open and this does not certify the whole application.

## Use a channel message as team work

With create-team capability, **Use as team work** opens an inline review in the
existing Team work panel, including for readable archived channels. It starts
nothing automatically. Only the selected message's exact text is proposed as the
goal; the rest of the conversation, attachments, and source-reference metadata
are excluded. An existing different goal requires **Replace draft and review**.
Acceptance preserves the selected lead, saves the draft, and focuses its editor.
**Keep draft and return to message** preserves the draft and restores source focus.

Pending/busy work cannot be replaced; an uncertain pending request still has its
same-key retry. For messages over 16,000 characters, **Write a shorter goal**
dismisses review and focuses the editor while preserving current goal and lead;
there is no silent truncation. Selecting another team clears the transient
handoff. Explicit **Start team work** still checks active team/all members/lead
and the canonical request journal. The original channel remains unchanged.

Fourteen focused tests passed in 33.4 seconds and the full 90-test suite passed in
2.4 minutes before the narrow oversized-message recovery correction. After that
correction, eight Team work tests passed in 22.4 seconds, including the new
oversized regression, and one capture test passed in 4.3 seconds. The full
91-test suite was not rerun. Final production build, formatting, diff, and Python
checks passed; the direct token check was empty before the correction, with no
subsequent CSS change or ignores. Final Linux native smoke executed the actual
channel → review → lead → provider task → saved result flow with a synthetic
provider. `channel-work-verdict.md` clears the sole recovery finding in
`channel-work-finish.md` with a bounded ship disposition. Desktop/mobile samples
show long-message recovery; native shows normal handoff, all intentionally scrolled.
This is explicit user handoff, not automatic channel participation or general
work/artifact reference navigation. Existing broader limits and the open comp
gate remain; live-model quality, other platforms, and whole-app completion are
not certified.

## Linked-message context

**Original message**, **Resolved message**, and **Replaced message** open the
exact related message through a scoped read, even outside the visible 50-message
window. Sender, time, audience, and full inert text appear in a local context
panel. The history position and message composer remain unchanged, preserving
drafts. A loaded heading or error receives focus; **Close message** restores the
invoking control. Loading, failure, and retry states are explicit. Exact identity,
conversation, sequence, content, sender/audience, and date validation reject
mismatched data, while closed/unmounted panels ignore late reads.

Eleven focused tests passed in 24.1 seconds. Linux native smoke passed the actual
new-reply/open/close assertions, and production build, formatting, Python syntax,
diff, and direct token checks passed without ignores. All 87 browser tests passed
in 2.4 minutes on final source, and the final production build passed. Desktop/
mobile captures regenerated by that suite were reopened and remained valid. `channel-context-finish.md` ships the bounded
extension without material findings. Desktop/mobile/native captures are scrolled
context samples, not full-page coverage. This supports message relationships only;
recursive message navigation is not implemented here. Work/artifact reference
arrays now have separate bounded support described below. Automatic participation, other-participant receipt UI, marketplace,
platform delivery, and live-provider quality remain unfinished or unverified.
The global comp gate remains open; this is not overall application certification.

## Work, artifact, request, and approval references

Channel reference arrays support **Work**, **Artifact**, **Agent request**, and
**Approval** records. Work
uses an exact scoped read, validates canonical identity/goal/owner/status/revision,
and shows its goal/status inline. **Open work inspector** is a separate action,
with invoking focus retained for closing; a late read never opens work itself.

Artifact references pin positive versions. Omitted/zero versions explicitly mean
latest, resolved to a validated positive version before content preview/save.
The existing bounded UTF-8/checksum/native-save path is reused, with separate
get/download capability checks and no new content transport. Unsupported reference
kinds, invalid records, and versioned Work references are labeled unavailable.
Close restores reference-button focus; pending reads are ignored after unmount,
and conversation position/drafts remain unchanged.

`channel-references-finish.md` ships the bounded surface without material findings.
The native Linux smoke passed exact artifact-v1 preview while v2 existed,
canonical Work opening, close-focus restoration, and the prior lifecycle using a
synthetic provider. Final production build, formatting, Python syntax, diff, and
direct token checks passed without ignores. A separate existing roster-feedback
race was corrected to focus after DOM commit and accepted in a source-only review
addendum; native reference verification does not establish that roster fix.
All 94 browser tests passed in 2.8 minutes on final source, including the formerly
failing real active-team roster-feedback focus case. Required desktop/mobile/
native captures are valid intentionally scrolled samples, not full-page coverage.
Objective/project/activity/external/control/session references,
automatic participation, other-participant receipt UI, marketplace, and platform delivery remain
open. Live quality and other operating systems are unverified; the comp gate
remains open and the whole app is not certified.

The request/approval extension performs exact scoped reads: request → source run,
or approval → matching action → run. **Open request details** and **Open approval
review** retain the exact linked ID in the inspector, including historical
checkpoints while a newer approval is current. Get-only request access works
without exposing list controls. Pending approvals explain reviewing consequences
before deciding; recorded outcomes invite inspection rather than another decision.
Mismatches are recoverable, closed/unmounted reads stop before further fetches,
and opening/closing preserves invoking focus and channel drafts. Reading a link
neither approves an action nor changes a request.

The full 98-browser-test suite passed in 2.8 minutes before final copy/assertion
verification; all four focused cases passed in 9.3 seconds after the final copy.
Those cases cover exact get-only request/draft focus, historical approval binding,
mismatch recovery and closed-late reads, and pending review consent. Final
production build, formatting, Python syntax, and diff checks passed. The previously
clean stylesheet token check remains applicable because this extension adds no
CSS/tokens/ignores; no detector was rerun. Final Linux native smoke passed a real
saved-approval link read, exact inspector, and close-focus restoration using a
synthetic fixture. `channel-decision-links-finish.md` ships the bounded extension
without material findings. All three final desktop/mobile/native captures are
valid intentionally scrolled samples. Other reference kinds, automatic
participation, other-participant receipt UI, marketplace, platform delivery, live quality, and
other operating systems remain unfinished or unverified; the comp gate is open.

## Personal channel reading position

A selected channel shows the saved reading position for user `local-operator`,
refreshed every ten seconds. Opening the channel never marks messages read.
**Mark read through message N** explicitly includes that message and all earlier
messages. Receipt capability is independent of posting: a read-only channel can
still support personal read-position controls.

**Start at unread messages** loads ascending history after the saved cursor in
50-message windows plus lookahead, with an unread divider and older/newer/latest
controls. Repeating the current navigation action forces a fresh read instead of
waiting for polling. Posting returns to latest. Invalid message pages and cursor
identity/sequence failures disable marking. Uncertain/conflicting updates require
**Check saved read position** via GET before another write, with no automatic
replay. The client checks channel, scope, participant, monotonic revision, and
read/delivery sequences; completion feedback receives focus.

The full 101-test browser suite passed in 2.8 minutes before final message-page
validation/navigation corrections and the fourth read-state test. All 13 focused
read-state/channel tests passed in 26.6 seconds after the review fix; final frontend
build, backend rebuild, Go server/runtime suites and race checks passed. Final
Linux native smoke passed repeated unread navigation and a real mark-through-3
PUT followed by GET refresh/focus, alongside the previous lifecycle. An initial
pre-feature roster assertion required waiting for the refreshed parent label in
the harness; no product roster change was made. `channel-read-verdict.md` clears
the sole same-page navigation finding from `channel-read-finish.md` with a bounded
ship disposition. Desktop/mobile/native captures are valid scrolled samples.
No new CSS, tokens, ignores, or extra detector run were introduced.

This supports personal position in the selected channel and unread counts in its
team’s channel list. A cross-team unread inbox, other-participant receipt UI, and
automatic viewport tracking remain open,
alongside automatic participation, remaining reference kinds, marketplace, and
platform delivery. Synthetic-provider evidence does not establish live quality,
other operating systems, or whole-app completion; the comp gate stays open.

## Channel-list unread counts

When the host offers the `channels` capability’s `receipts` operation, channel
buttons append a positive **N unread** count based on `lastSequence` minus the
local-operator’s saved `readSequence`. These are reading-position counts, not
viewport tracking. Zero omits the label without asserting that everything was
seen; an older host’s missing projection never produces a fabricated count.
Opening a channel still does not mark it read.

The existing scoped list request includes the reader and returns an optional
`readPosition` for each row. Reader identity, safe sequence values, and revision
are checked. Channel and cursor revisions merge independently, preventing a late
list response from undoing newer metadata or a saved reading position. Explicit
marking updates the visible list immediately. Owner/status filters and pagination
remain unchanged. This uses one HTTP list request with a bounded server cursor
lookup per listed row; it introduces no database batch API or scalability claim.

Final frontend build, backend rebuild, and server race checks passed. Five initial
focused tests passed in 13.2 seconds. The last full browser run finished with 102
passes and one stale test assertion expecting an archived channel button without
its new unread label. After that test-only correction, all six recovery-focused
tests passed in 17.7 seconds, covering the read-state/list cases and the complete
real-daemon team lifecycle through reload, rename, archive, restore, and posting.
Earlier readiness/name test assumptions were also corrected. The full 103-test
suite was not rerun after the final assertion correction, so no clean full-suite
pass is claimed. Product source remained unchanged throughout final verification.
Linux native smoke passed a real three-unread label, explicit marking, immediate
label removal, and the prior lifecycle. Synthetic fixtures do not establish live
provider quality or other operating-system support.

The bounded review in `.impeccable/review/channel-unread-list-finish.md` accepts
the surface without material findings. Desktop/mobile/native captures are
intentionally scrolled channel-list samples. No CSS, tokens, or ignores changed.
Cross-team unread discovery, other-participant receipt views, and automatic
viewport reading remain unfinished; the global comp gate remains open.

## Channel settings and availability

Channels default to **Active channels**, with Archived and All filters. Changing
the filter clears selection; a successful rename/archive/restore keeps the
inspected channel and updates list metadata without regressing revision.
Management requires update capability. Rename drafts persist with their base
name, and a remotely changed name requires checking/review before overwrite.
Archive and restore have explicit consequence confirmations; a newer revision
invalidates confirmation, and cancel restores focus. Archive keeps messages and
drafts but stops new posts; restore re-enables posting. Neither cancels team tasks.

The exact pending change is saved before PATCH; storage failure sends nothing.
Uncertain delivery or a conflict requires **Check saved channel**, which reads
current state rather than replaying the edit. Recovery persists across reopening,
validates ID/owner/revision, and can confirm the desired state without claiming
that this client caused it. Late unmounted callbacks are ignored.

The previously declared `PATCH /conversations/{id}` route now calls the existing
revision-guarded core update service, rejects empty patches, and applies the
configured desktop scope guard to updates as well as creation/posting. See
`docs/api.md`. This is not a general participant ACL system.

All 85 browser tests passed in 2.5 minutes; nine focused tests passed in 17.5
seconds, including real-daemon rename, archive, reload/history, restore, and post.
Final Linux native full smoke passed rename/archive/disabled posting/restore.
Go server/commands/runtime/client suites and channel server race tests passed.
Production build, formatting, diff, Python syntax, and direct stylesheet token
checks passed without ignores. `channel-settings-finish.md` ships the bounded
surface without material findings. Desktop/mobile/native captures are scrolled
settings samples, not full-page coverage. Single-line title height and wrapping
toolbars use existing field tokens. Automatic participation, other-participant receipt UI,
reference kinds beyond work/artifacts/agent requests/approvals, marketplace, and platform delivery remain incomplete;
live-provider quality and other operating systems remain unverified. The comp
gate is open, and no whole-app certification is claimed.

## Start team work

Expand **Team work** in team details, choose an active member as lead, and enter
the goal. New work requires an active team with no pending activation and active
agents across the entire assigned roster. The host verifies team ownership,
membership, lead assignment, and its own bounded budget. Only an explicitly
capable desktop host advertises `agent-runs.create-team`.

Goal and lead drafts persist on the device. The request identity is saved before
POST; local storage failure sends nothing. An uncertain response freezes the
submitted fields and preserves its key across reload for explicit retry. Exact
accepted requests are recovered read-only before mutable admission checks, while
changed-body reuse of a key is rejected. Definitive 400/401/403/404/422 responses
retain an editable draft. Leaving the component prevents late inspector opening.

Team-owner history shows 20 newest-first runs with one lookahead, polls every
three seconds while open, and retains loaded runs when refresh fails. Rows open
the Work inspector; child runs are labeled **Delegated run**. The lead may delegate
under the team's acceptance and review policies. Clarification can be inspected and guided through the lead as described below;
direct manual request management remains unavailable.

Full Go suites and targeted race tests passed for daemon, server, commands, and
runtime; build, formatting, diff, and token checks passed without ignores. A real
delegation test passed lead → recipient acceptance → child completion → lead
resumption using the advertised minimum child budget. Linux native team start and
result smoke passed alongside the prior lifecycle. All 70 browser tests passed in 2.1 minutes, including actual delegated root/child
completion and exact-request recovery returning the same run after team pause
while a fresh request remains blocked. `team-work-finish.md` gives the bounded addition
a ship disposition without material findings. Required desktop/mobile captures
and the supplemental scrolled native sample were reopened with the correct final
acceptance-rule copy. The native sample does not establish full-page coverage.
Evidence uses a synthetic provider; live commercial quality and other platforms
are not certified. Automatic channel participation, marketplace, platform delivery, and broader app
scope remain unfinished; the comp gate remains open.

## Edit a team roster

Expand a team and choose **Edit roster**. Select installed agents meeting each
role's exact required definitions and all required skills, add/remove assignments,
and optionally set a name used within the team. Role minimum/maximum counts and
one role per agent are enforced in the editor; paused/inactive agents remain
eligible when their definition/skills qualify and their status is explicit.
Replacing an agent preserves the assignment ID. Membership changes neither
activate agents nor cancel running work; the kernel remains authoritative.

Review the before/after list, supply a reason, and check the review consent before
**Save roster**. Drafts are saved on this device; a storage failure is disclosed.
Draft/eligibility/revision changes invalidate consent. **Review latest team**
compares the base, draft, and current saved roster, preserves unrelated updates,
and requires **Keep my draft** or **Keep saved assignment** for each conflicting
assignment. Those choices prepare a new draft for review, not an automatic save.
Lost/ambiguous responses require latest-state checking and never automatically
replay the mutation. Editing requires update and agent-list capabilities, the
local workspace, a nonarchived team, and no pending installation activation.
Saved drafts remain visible if editing becomes unavailable. Separate parent
component identities prevent duplicated status/editor controls.

All 66 browser tests passed in about 1.6 minutes, along with 11 targeted tests,
production build, formatting, and diff checks. The real-daemon integration uses
`seed_roster_agent.go` to install an eligible paused substitute, then replaces the
original agent through the UI. It verifies assignment identity, team status and
restrictions, unchanged states for both agents, the audit reason/local-operator,
and reload persistence. Full Linux native smoke passed a reviewed alias save
alongside the previous lifecycle. `team-roster-finish.md` independently gives the
bounded roster extension a ship disposition with no material findings. Required
desktop/mobile captures and the supplemental scrolled native sample were inspected;
the latter is not full-page native coverage. No tokens or ignores were added.
Synthetic-provider/alias evidence does not certify live-model quality or other
operating systems. Direct manual request management, advanced channel workflows, marketplace, and broader app
scope remain unfinished. The comp gate remains open, without approved comp or
code-first approval; this milestone does not certify the whole application.

## Installed-team controls

For an installed team with update capability, **Pause team**, **Resume team**,
**Archive team**, and **Restore team** require an explicit confirmation and reason.
Restore returns an archived team to paused. These controls change team coordination
availability; they do not cancel runs or pause member agents. The existing scoped
PUT preserves deployment fields and submits the expected revision and user
`local-operator` actor. Read-only capabilities expose no mutation controls.

A revision change invalidates the pending action. Lost responses or failed conflict
refreshes require **Check saved team state** before choosing another action; no
mutation is replayed automatically. The reason is retained within the open
component, not promised across reopening. A selected team remains visible after
its status no longer matches the filter, with an explanatory note; explicit
search/filter changes clear selection. Catalog merges preserve newer revisions.

An inactive installation offers **Review team activation**, loading its original
proposal from the stored change-set ID even after browser local storage is cleared.
It cannot be directly resumed, and responses arriving after unmount are ignored.
**Team change history** lazily reads the activations audit endpoint, newest revision
first, displays 20 entries initially, and offers **Show older changes** and retry.

All 62 browser tests passed in about 1.6 minutes, including five controls tests,
real-daemon audit transitions for all four states, and original-proposal recovery.
Linux native pause/resume with recorded reasons and the previous lifecycle passed.
Final build, formatting, diff, and Python syntax checks passed. The bounded
`team-controls-finish.md` review shipped without findings; shared-field alignment
was corrected before final desktop/mobile/native captures. No backend changes,
new design tokens, or ignores were introduced. Team definition editing,
manual delegation-request management and advanced channel workflows remain unfinished; the comp gate stays open.

## Create and review a team

Choose **Create team** on Teams to open the focused Home composer in team mode.
That mode is saved in the version-1 draft. Submission prefixes the request with
`Create one team.` and prepares one team with new agents for review; it does not
install immediately.

Review roles and assignments, member bounds, required definitions/skills, role
skill permissions, approval boundaries, shared context, delegation, coordination,
operating principles, and objective templates. Proposed agent instructions appear
below the team. Missing roles/approval boundaries or unmatched assignments block
approval and installation in the interface; kernel validation remains authoritative.
Continue through installation checks, the explicit reviewed checkbox, approval,
and installation. The receipt offers **View teams** and member inspection.
An inactive team's later activation retains the same team/agent IDs and roster,
without another model call. Its review explicitly activates existing resources.

All 57 browser tests passed in about 1.4 minutes before the final activation-copy
fix; final build passed. All three focused tests passed in 10.1 seconds after the copy fix, with exact
assertions and refreshed desktop/mobile activation captures. `teams-creation-verdict.md` clears the activation-wording finding with a bounded
ship disposition and four valid refreshed captures without regression. Final
formatting, diff, and Python syntax checks passed. Linux native
team proposal, approval, installation, and member inspection passed on a fresh
retry following an initial RemoteDisconnected transport failure; no root cause
is claimed. The native proposal capture covers the first viewport only. Initial
invalid synthetic model fixtures were corrected without backend changes. Team
definition updates, manual delegation-request management, and broader channel workflows remain
unfinished. No new tokens, ignores, or detector rerun were introduced; the comp
gate stays open and this does not certify the whole app.

## Task artifacts

Expand **Artifacts** in Work details to load files recorded by that task,
including immutable earlier versions. The scoped list validates producer-run
provenance and shows 20 records per page with one lookahead, version,
classification, bytes, media type, and producer. Loading, empty, unavailable,
refresh, and error states remain explicit.

**Preview text** accepts supported UTF-8 text up to 256 KiB and displays it
inertly, including HTML source. Successful loading focuses the requested
version's preview. Image previews are not implemented. **Save as…** supports up
to 100 MiB. Native export pins artifact identity/version, fetches authoritative
metadata, verifies byte size and SHA-256, writes a private temporary file in the
chosen directory, then atomically persists it. Integrity failure preserves an
existing destination. The native picker permits renaming and reports cancellation
without claiming a save. Allowed punctuation and Unicode IDs are URL encoded
without changing the requested identity. Browser downloads preserve binary bytes
through the Vite proxy and validate the same metadata; feedback says Download
started, not saved. Saving does not grant an agent general workspace file access.

Artifacts evidence: all 44 browser tests passed in about one minute, including
exact-version preview focus and real-daemon encoded IDs. Eight Rust unit tests
passed; one optional real-daemon integration test was ignored, while Linux native
smoke supplies actual integration. The final native run passed preview, native
Save As cancellation, renamed binary byte verification, and the prior lifecycle
after the encoding and focus fixes. Native/frontend builds, formatting, and diff
checks passed. The bounded `artifacts-verdict.md` clears the original preview-focus
finding in `artifacts-finish.md`. Desktop/mobile/native captures use supplied
fixtures; that milestone did not verify model-produced files. The generated-file
integration described below now verifies synthetic provider production, while
live commercial-model quality and external actions remain unverified.
Native evidence covers Linux only. No new design tokens or ignores were added,
and the separate comp gate remains open.

## Generated text files

Ask an active agent for plain text, Markdown, CSV, or JSON. The model proposes
`runOutput.generatedFiles` entries containing `name`, `mediaType`, and `text`.
At most eight files and 256 KiB total UTF-8 text are allowed, within the provider
turn's output-token budget. The host validates portable unique filenames without
paths, supported types, text, and JSON syntax. It rejects model-authored
`artifactRefs` and forged metadata. IDs derive from scope/run/turn/index; content,
digests, provenance, and internal classification remain host-owned.

The accepted turn is saved durably before idempotent private-content and catalog
publication, which precedes the completed-run transition. Run output replaces
file bodies with `artifactRefs`; the accepted turn retains drafts for recovery.
Partial publication failure pauses the run with a storage error and activity.
Fix storage access, then **Resume** to publish from the saved turn without another
model call or duplicate artifacts. Cancellation before provider return discards
files. Output accepted before cancellation may already be partially published;
there is no all-or-nothing batch guarantee.

All 44 browser tests passed in about 1.1 minutes, including a synthetic provider
through the real daemon to generated Markdown preview/exact download and canceled
work with zero artifacts. Linux native smoke passed generated Markdown across
close/reopen and focused preview, alongside the previous binary Save As fixture
flow. Runtime/daemon/commands and server race suites passed. SQLite recovery tests
cover reopening, Resume, partial-publication deduplication, foreign scope,
malformed/oversized/duplicate files, forged metadata/references, and cancellation.
Frontend/backend builds, the final backend rebuild, formatting, and diff checks
passed.
The bounded composer-help review shipped with no new style tokens or ignores.

These results verify synthetic-provider integration, not live commercial-model
quality. Image input/previews, reading the local filesystem, Office/PDF generation,
other operating systems, and external actions are not established by this feature.
The overall application remains in development and the comp gate remains open.

## Reviews

Open **Reviews** from the sidebar, command menu, or Ctrl/Cmd+4. Its pending dot
means at least one scoped pending review exists; it is not a count. **Needs
review** lists oldest pending requests first. Approved, Changes requested,
Rejected, Expired, and Canceled history lists newest requests first, not newest
decisions. Pages show 25 records with one lookahead. Offset pages are live and
may shift as reviews resolve. Installation proposals remain in Home.

Opening a review directly loads its task even outside the recent 100, rejects
late requests, focuses details, and restores the originating row on close. The
selected approval ID stays fixed through run updates, keeping saved checkpoints
inspectable. Decisions are read-only unless the checkpoint is currently awaiting
review for that run. Refreshing a saved decision describes the recorded outcome
without prompting another decision, and repeated polling does not invent changes.
The approval-list API validates `created_asc`/`created_desc`; memory ordering uses
timestamp/ID tie breaks aligned with SQL. See `docs/api.md`.

## Review an action

Work details show the exact approval checkpoint and its bound action: policy,
risk, effects, inputs, preview, skill/action/version, deadline, timeout behavior,
and any recorded reviewer guidance. Review the details, check the reviewed box,
then choose **Approve action**, **Reject action**, or **Request changes** and
confirm its stated consequence. Changes require guidance. Expired, ineligible,
read-only, nonwaiting, loading, and error states cannot submit a decision.
**Refresh approval** recovers current state. Changed details invalidate review;
uncertain delivery retains the same request identity for an unchanged decision.
Approval records permission, not proof that the action executed.

`--desktop-operator` installs `HostDesktopApprovalAuthorizer` for authenticated
loopback requests within the local scope. The principal is pinned to user
`local-operator`; eligibility requires that user or the default `operator` role.
Arbitrary reviewer impersonation is rejected, including on replay. Approval
status filtering accepts `changes_requested`.

## Work Requests

Work now has **Tasks** and **Requests** views. Requests discovers agent-to-agent
requests across the scoped workspace, defaulting to **Clarification requested**.
Filters cover all eight canonical request states plus All requests. Pages show
20 rows with one lookahead, poll every five seconds, and provide manual refresh.
Previously loaded rows remain visible with stale-data guidance after refresh
failure. Capabilities separately gate listing, participant naming, and inspection;
agent/team names fall back to IDs when catalog details are unavailable.

Opening a request loads and validates its parent task, ignores late responses,
and pins that exact request in the Collaboration inspector even outside the
parent's first request page. **Show all requests for this task** returns to its
outgoing list. **Guide the lead** uses the existing persisted user-intervention
path; it does not directly manage an agent's request. Home **View all** and
successful agent-task creation explicitly select Tasks; general Work navigation
restores the current Tasks/Requests view.

The bounded `request-inbox-verdict.md` clears the task-intent navigation finding
from `request-inbox-finish.md`. Seven focused inbox/real-agent tests passed before
a subsequent artifact-preview focus correction; preview focus now runs after its
DOM commit. Final production build, formatting, diff, and direct token checks
passed without ignores. Final Linux native smoke passed after the artifact-focus
correction. All 77 browser tests passed in 2.1 minutes, including real artifact
preview focus and team clarification through Work Requests. The final production
build also passed; these results follow the last implementation correction.
Desktop/mobile/native Requests captures were verified; initial native navigation
verification passed before the final focus fix. Direct manual request management,
advanced channel workflows, marketplace, platform delivery, and live-provider quality remain
unverified or unfinished. The global comp gate remains open.

## Browse work

The Work page searches task descriptions and statuses across the entire scoped
workspace before pagination, using case-insensitive literal matching. Results
are explicitly newest first. Filter by All work, In progress, Completed, Failed,
Paused, or Canceled. Search/filter changes return to the first page. Previous and
Next controls show 25 records at a time; a one-record lookahead determines whether
another page is available. Home also requests newest-first creation order.

View changes discard stale requests. Paging/retry feedback receives focus, and
background refresh preserves rows in the current view. Empty, unavailable, and
recoverable error states provide appropriate guidance. Offset pages reflect live
data and may shift as new tasks arrive.

The API accepts a validated order and a `q` query capped at 1,000 bytes. Memory and
SQLite share scoped filtering; PostgreSQL uses parameterized literal `strpos`
matching. SQLite already scans scoped rows; no scalability benchmark is claimed.
See `docs/api.md` for the API contract.

## Inspect collaboration and guide the lead

Open **Delegation and requests** in Work details to inspect that run's outgoing
requests; it starts expanded when opening a run waiting for another agent. The
scoped list loads lazily, shows 20 requests with one lookahead, and polls every
three seconds while open. It shows clarification, distinct latest response,
completion/review summaries, resolution, and review disagreement. Identical
question/response text is shown once. Agent names and **This team** identify
participants, with original IDs in tooltips. Parent/delegated-work links open the
matching run and ignore late responses after leaving.

For a recipient's clarification question, **Guide the lead** opens and focuses
the existing guidance form. Request context is prefilled only when the form has
no existing draft or pending submission. Save sends a user run intervention using
persisted request identity and revision checks; it does not directly answer the
request, approve it, or impersonate the lead. The lead subsequently responds to
the existing canonical request. Paused tasks retain guidance until **Resume**.
Saved guidance is context, not a claim that the instruction was followed.

All 73 browser tests passed in 2.5 minutes; three focused tests passed in 8.9
seconds after the final participant-name change. Production build, formatting,
diff, Python syntax, and token checks passed without ignores. The real-daemon
integration verifies recipient question → user guidance during a held lead turn →
stale output discarded → lead clarifies the same request → recipient acceptance →
child completion → root completion, retaining exact replay-after-pause coverage.
Full Linux native smoke passed a two-agent team's clarification, pause, guidance,
resume, and result. There were no Go changes in this milestone. The independent
`work-collaboration-finish.md` review shipped the bounded addition without material
findings. Inspector-top and lower-guidance desktop/mobile captures, plus the
supplemental native sample, were opened and verified. These synthetic-provider
results do not establish live commercial quality or other-platform behavior.
Direct manual request management, advanced channel workflows,
marketplace, and platform delivery remain unfinished. The comp gate stays open.

## Task guidance

Choose **Add guidance** in Work details to add an instruction to a nonterminal
run when the workspace permits intervention. The form accepts 4,000 characters.
Paused work stays paused; sleeping work wakes to consider the instruction.
Guidance does not approve or cancel an action review or bypass dependency waits.
Already-started actions may finish.

The draft and pending UUID are saved locally before POST. Uncertain delivery
makes the text read-only and offers **Check saved guidance** and explicit **Retry
guidance** with the same identity, including after reopening. Revision conflicts
refresh current state and preserve the draft without automatic resubmission.
Definitive client errors say the guidance was not saved. Ended tasks and unavailable
capabilities retain unsent text read-only for copying. Success closes the form and
focuses feedback. Saved guidance history records context, not proof of compliance.

The API accepts an optional UUID `interventionId` and up to 16,000 UTF-8 bytes.
A matching saved ID is bound to its instruction and actor and is recognized before
revision checking; another payload/actor cannot reuse it. Actual turn invocations
persist their input intervention IDs. If guidance changes before an old turn is
applied, its proposals/output are suppressed, including after persistence/recovery;
usage is retained and work continues within budget without requesting the old
action again. Already-applied/started actions and partially published artifacts
remain; guidance is not rollback.

All 47 browser tests passed in about 1.1 minutes before the last two recovery-copy
fixes; four targeted guidance tests passed in 8.8 seconds after the fixes. Final
frontend build, backend rebuild, formatting/diff checks, and runtime/server/daemon/
commands race suites passed. Linux native smoke verified a held provider response,
saved guidance, and the subsequent turn receiving the instruction. The first
native attempt lacked the Vite development listener; restarting that prerequisite
resolved it. The bounded guidance verdict clears both original recovery-copy
findings. Tests use synthetic provider responses, not live quality or external
actions; other operating systems and overall app completion remain unverified.
No new style tokens or ignores were introduced; the comp gate remains open.

## Task execution and results

With `--desktop-operator` and a valid provider configuration, the daemon installs
`ProviderTurnHost` and a dispatcher for root work in the configured workspace scope.
Tasks reuse the workspace provider through an OpenAI-compatible chat-completions
API with structured `submit_agent_turn` output. The kernel retains turn compilation,
action, skill, and completion checks. Actor identity and budgets are host-owned;
renderer-supplied execution context, checkpoints, and policy claims are rejected.

Desktop tasks are bounded to 16 turns, 24 attempts, 128,000 total tokens, 32,000
output tokens, 16 actions, and 15 minutes. Provider HTTP requests have a 90-second
timeout and a 4 MiB response bound, reject redirects, and keep private provider
errors out of user-facing responses. Native workspace operations and file access,
media, and deployment-specific model credentials are unavailable and fail closed.
The UI explains its task limits and lack of file access. External-action and tool
workflows have not been validated.

Work details show the first nonblank reply, answer, or summary as readable text,
with **Copy result** and success or manual-copy fallback feedback. **Structured
output** reveals the complete output. Long text wraps inside the inspector, and
completed, failed, canceled, and ongoing runs have appropriate empty-result copy.
Saved tasks and results survive reopening the workspace.

Expand **Activity history** to lazily load scoped daemon events for the selected
run, newest first, with recorded summaries and local timestamps. **Load earlier
activity** follows cursor pages and keeps older entries visible when the run
updates. **Refresh history** explicitly returns to the latest events. Empty,
loading, and recoverable error feedback is inline. Stale responses cannot populate
another run, and distinct inspector component identities prevent duplicated
Result content when selection changes.

The inspector supports **Pause work** for kernel-supported nonterminal states and
**Resume** for paused work, subject to scoped capabilities. **Cancel work** opens
an inline confirmation explaining that saved results remain and external actions
already started may finish. The focused confirmation includes that description
for screen readers; **Go back** restores focus to the cancel action. Revision
changes invalidate confirmation. Commands use expected revisions and a submission
lock; conflicts fetch current state without replaying the command. Same-ID and
monotonic revision checks prevent late command/poll responses from replacing the
selected run or rolling its state back.

Work status explains the actual queued, active, waiting, paused, failed, or
canceled state. Only a sleeping run with the `hosted-turn-retry` wake reference
is described as waiting for provider retry; it shows a valid local scheduled
date/time and **Open provider settings**. Budget guidance distinguishes the last
admission check from current availability, shows required/remaining capacity and
its dimension, and explains that exhausted capacity can include reservations.
Kernel-authorized **Resume** remains available to recheck capacity; the app cannot
increase the run budget.

For failed or canceled agent work, **Review a new attempt** is available when
creation is authorized. It prepares the previous goal in Home and selects the
previous agent only if still active; otherwise the user must choose an agent.
Focus moves to the composer, and nothing runs until explicit submission. The
original run is unchanged. A different unfinished draft requires **Replace draft
and review** or **Keep draft**. The confirmation has an accessible explanation;
keeping the draft preserves its contents and returns focus to the review action.
The prepared draft persists across native app reopening. Prompt, composer mode,
and assigned-agent ID are saved atomically in a version 1 local draft record.
Reopening restores that context without submitting. Missing or malformed records
fall back to the legacy saved prompt in agent-creation mode. A missing or inactive
saved agent remains explicitly unavailable; work submission and its keyboard
shortcut are blocked until an active agent is chosen. Choosing another agent
preserves the text and saves the new assignment.

If a parent run becomes terminal while its owned turn is in flight, the turn
coordinator closes that turn as canceled even when its context is canceled. It
retains turn usage while discarding late output, actions, and checkpoints; those
late results cannot be applied to the terminal run. Memory and SQLite regressions
cover this behavior. No external actions were performed in the supplied tests.

## Run

Install Go, Rust, Node.js, pnpm, and the platform-specific
[Tauri prerequisites](https://v2.tauri.app/start/prerequisites/), then run:

```sh
make desktop-install
make desktop-dev
```

Use `make desktop-build` to build the native app. Release installers and signing
have not been validated across platforms. Rust changes require rebuilding and
restarting the native app; frontend hot reload cannot add native commands.

For browser development, build the Go binary with `make build`, then run
`pnpm --dir desktop dev`. This uses a separate `desktop/.dev-workspace`.

## Model provider

Open **Settings → Model provider**, enter your API base URL, model name, and API
key, then choose **Save and connect**. Saving reconnects the local workspace;
manual YAML editing and reopening the app are unnecessary. Provider access is
checked when creating an agent, not when saving configuration.

Keys are written to private files under the workspace `.secrets` directory
(owner-only permissions on Unix). Configuration contains a credential reference;
saved keys are never returned to the frontend. Leave the key field blank to keep
the saved key. Changing provider origins requires entering a key again.
Existing environment-variable and file credential references are supported.

## Native process ownership

Each launch uses an ephemeral loopback port and a fresh bearer token held in
native memory. The bridge permits only local `/api/v1/` requests, disables
redirects and proxies, and bounds response sizes. Unix shutdown allows five
seconds for the owned daemon to flush before killing and reaping it. Windows
currently uses process termination; graceful Windows shutdown remains unfinished.

## Verification

```sh
make desktop-host-test
pnpm --dir desktop build
pnpm --dir desktop test
```

Tests use isolated workspaces and synthetic credentials. They exercise the real
Go daemon, secure credential persistence, reconnect, navigation, and accessibility;
they do not validate access to a live model provider. The Linux native smoke
script additionally requires tauri-driver, WebKitWebDriver, Xvfb, a built debug
app, and the development server.

The Linux native smoke test verifies native provider saving and reconnecting,
proposal generation, inactive installation, activation preparation, activation
checking, explicit approval, activation of the same resources, Agents navigation,
owner-only key storage, native window closure with owned-daemon
exit, and reopening the same workspace with the agent and settings retained and
the key hidden. It also executes a task, inspects its readable result, and confirms
the task/result persist after reopening. It uses a synthetic local model; live
provider quality and external-action/tool workflows remain unverified. It uses direct
`WM_DELETE_WINDOW` events because Xvfb has no window manager. macOS and Windows
lifecycle behavior still need platform testing.

With the development server running, execute the native test from the repository
root (requires `xdotool` and `libX11` in addition to the tools above):

```sh
cargo build --locked --manifest-path desktop/src-tauri/Cargo.toml
xvfb-run -a dbus-run-session python3 desktop/tests/native_smoke.py
```

Current milestone evidence: all 21 browser tests passed, including real provider
HTTP 429 → scheduled retry → pause, new-attempt draft protection without mutation,
and accessibility checks. The frontend build and diff checks passed. No backend
changes were made for this milestone; earlier runtime/server race tests and
backend build evidence remain recorded in the preceding milestone. Linux native
smoke passed cancellation, review of a new attempt, focused composer, and
close/reopen with that draft retained, alongside the existing installation,
activation, task, result, and shutdown flow. Status/draft desktop and mobile
captures received bounded review acceptance. All provider responses are synthetic;
live quality and external-action/tool workflows remain unverified. These checks
do not certify the complete app or other operating systems. The comp gate remains
open.

Activity history is a frontend-only milestone. Its frontend build and all 23 browser tests passed, including real-daemon activity
history and the single-Result regression; the diff check passed. The bounded history surface received a ship verdict in
`.impeccable/review/work-history-finish.md`; duplicate Result content is resolved
and desktop/mobile captures show the history controls. No new native-history
test has been run. The direct design-system check is empty without
suppressions, and all existing normative tokens are preserved.

Draft recovery adds no CSS or backend changes. Its two targeted browser tests,
frontend build, and Linux native smoke passed. The native check reopens a prepared
attempt with work mode, Native analyst assignment, retained text, and submission
available. All 25 browser tests passed in 34.2 seconds and the diff check passed.
The bounded draft-recovery review has a ship disposition with no material defects;
its pending native note is superseded by the separately confirmed smoke result.

Work browsing evidence: runtime/server suites, isolated real PostgreSQL integration,
and initial backend/frontend builds passed. The temporary PostgreSQL instance was
stopped and cleaned up. All 27 browser tests passed in 39.5 seconds, including real-daemon search/status
filtering. Linux native smoke passed search, empty-result recovery, and finding
the persisted completed task after reopening. The bounded reviewer accepted the
unavailable-copy correction with a ship disposition. A subsequent direct refresh
for an inspected run outside the recent 100 preserves identity/revision guards;
all five targeted tests passed in 9.8 seconds, including older-inspector refresh
and controls; the final frontend build and diff check passed. No CSS changes, new style tokens, or suppressions were introduced.

Action approval evidence: authority tests cover all three decisions, replay, and
impersonation; seven initial browser tests passed with an actual Go fixture.
Linux native smoke approved a synthetic uninstalled-skill checkpoint through the
UI and verified saved status `approved` and decision maker `local-operator`.
`desktop/tests/seed_approval.go` seeds only an isolated SQLite canonical transaction
for that synthetic checkpoint. No live external skill or operation was executed.
The bounded reviewer accepted the error-state disabling fix. The final frontend
build and all 37 browser tests passed in 51.9 seconds, including nine focused
approval cases and the real-daemon decision case. Server/commands/runtime race
tests, final diff check, and direct token check passed; native saved-decision
verification also passed. Existing tokens are reused without ignores. See
`docs/api.md` for decision semantics and host authority.

Reviews evidence: all 40 browser tests passed in 56.8 seconds, including real-daemon
pending review, approval, history, and reload. Linux native Reviews approval and
saved-history verification passed; runtime/server race tests passed. The bounded
review has a ship disposition without issues. After the final saved-history refresh
copy/signature correction, all 13 focused tests passed in 16.4 seconds, including
saved-history refresh focus; the final frontend build also passed.
No CSS/new token/ignore changes were introduced. Fixtures use a synthetic
uninstalled skill; no external actions were performed.

## Installation authority

The daemon now has an explicit `--desktop-operator` mode for local workforce
installation. It requires a loopback listener, a nonempty `OPENSEAL_API_TOKEN`,
and a local workspace scope. Host policy requires an owner decision tied to the
exact proposal before apply; renderer-supplied policy claims cannot remove that
requirement. Revision, candidate digest, credential/placement readiness, and
atomic persistence checks remain in the kernel. Existing standalone mode retains
its previous permissions.

Native, Rust development, and Vite launchers enable `--desktop-operator`. The UI
connects host policy checks, explicit owner approval, installation, and later
activation of installed inactive resources through a separate child proposal. Advanced
placement and credential mapping remain incomplete. Server integration tests cover
policy → approval → apply, retry replay, forged roles, stale digests, foreign
workspace scopes, and incomplete placement. No live model or external action is
part of those tests.
