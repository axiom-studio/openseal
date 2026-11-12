package storetest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// Lease is SessionWorker's lease guard at the fixture's clock.
func (f *SessionFixture) Lease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: SessionWorker, Now: f.Now}
}

func (f *SessionFixture) leasePtr() *models.AgentRunLeaseGuard {
	lease := f.Lease()
	return &lease
}

// ExpiredLease is SessionWorker's lease guard two hours on, after the
// fixture's hour-long leases expired.
func (f *SessionFixture) ExpiredLease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: SessionWorker, Now: f.Now.Add(2 * time.Hour)}
}

func (f *SessionFixture) laneSessionID(agentID string) string {
	return models.ConversationAgentSessionID(f.Scope, models.ConversationRunLane("c1", "", agentID))
}

// createRun stores a queued Run bound to sessionID (may be "").
func (f *SessionFixture) createRun(agentID, sessionID string) *models.AgentRun {
	f.T.Helper()
	run := QueuedAgentRun(f.Scope, agentID, "work on c1", f.Now)
	run.SessionID = sessionID
	if err := f.Store.CreateAgentRun(f.Ctx, run); err != nil {
		f.T.Fatalf("create run: %v", err)
	}
	return run
}

// LeasedRun stores a Run bound to sessionID and claims it under
// SessionWorker. Only one Run is ever queued at a time, so the claim
// takes this one.
func (f *SessionFixture) LeasedRun(agentID, sessionID string) *models.AgentRun {
	f.T.Helper()
	created := f.createRun(agentID, sessionID)
	run, err := f.Store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{
		Scope: f.Scope, WorkerID: SessionWorker, Now: f.Now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != created.ID {
		f.T.Fatalf("claim = %#v, %v (want %s)", run, err, created.ID)
	}
	return run
}

// Run reads a Run that must exist.
func (f *SessionFixture) Run(runID string) *models.AgentRun {
	f.T.Helper()
	run, err := f.Store.GetAgentRun(f.Ctx, f.Scope, runID)
	if err != nil {
		f.T.Fatalf("get run %s: %v", runID, err)
	}
	return run
}

// UpdateRun rewrites the Run outside any lease (park, resume, finish, rebind).
func (f *SessionFixture) UpdateRun(runID string, mutate func(*models.AgentRun)) *models.AgentRun {
	f.T.Helper()
	current := f.Run(runID)
	next := models.CloneAgentRun(current)
	mutate(next)
	next.Revision = current.Revision + 1
	next.UpdatedAt = f.Now
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: f.Scope, EventType: "run.status_changed", Severity: models.ActivitySeverityInfo, AgentID: current.AssignedAgentID,
		RunID: runID, Actor: models.ActivityActor{Type: "worker", ID: SessionWorker}, Summary: "Run changed", Visibility: models.ActivityVisibilityScope, CreatedAt: f.Now,
	}
	if _, err := f.Store.UpdateAgentRunWithEvent(f.Ctx, next, current.Revision, event, nil); err != nil {
		f.T.Fatalf("update run %s: %v", runID, err)
	}
	return f.Run(runID)
}

func (f *SessionFixture) setStatus(runID string, status models.AgentRunStatus) *models.AgentRun {
	return f.UpdateRun(runID, func(run *models.AgentRun) {
		run.Status = status
		switch {
		case status == models.AgentRunStatusRunning:
			expires := f.Now.Add(time.Hour)
			run.LeaseOwner, run.LeaseExpiresAt = SessionWorker, &expires
		default:
			run.LeaseOwner, run.LeaseExpiresAt = "", nil
		}
		if models.IsTerminalAgentRunStatus(status) {
			completed := f.Now
			run.CompletedAt = &completed
		}
	})
}

// ConversationRun claims a Run of the c1 lane of agentID and opens its session.
func (f *SessionFixture) ConversationRun(agentID string) (*models.AgentRun, *models.AgentSession) {
	f.T.Helper()
	run := f.LeasedRun(agentID, f.laneSessionID(agentID))
	session := f.open(run.ID, agentID)
	return run, session
}

func (f *SessionFixture) openRequest(runID, agentID string) models.OpenConversationAgentSessionRequest {
	return models.OpenConversationAgentSessionRequest{
		Scope: f.Scope, ConversationID: "c1", AgentID: agentID, RunID: runID, Lease: f.Lease(), Now: f.Now,
	}
}

func (f *SessionFixture) open(runID, agentID string) *models.AgentSession {
	f.T.Helper()
	opened, err := f.Store.OpenConversationAgentSession(f.Ctx, f.openRequest(runID, agentID))
	if err != nil {
		f.T.Fatalf("open session for %s: %v", runID, err)
	}
	return opened.Session
}

// AppendRequest appends entries to the session under the Run's lease.
func (f *SessionFixture) AppendRequest(sessionID, runID string, entries ...*models.SessionEntry) models.AppendAgentSessionEntriesRequest {
	return models.AppendAgentSessionEntriesRequest{
		Scope: f.Scope, SessionID: sessionID, RunID: runID, Lease: f.leasePtr(), Entries: entries, Now: f.Now,
	}
}

func (f *SessionFixture) append(sessionID, runID string, entries ...*models.SessionEntry) (*models.AppendAgentSessionEntriesResult, error) {
	return f.Store.AppendAgentSessionEntries(f.Ctx, f.AppendRequest(sessionID, runID, entries...))
}

// MustAppend appends entries and fails the test on error.
func (f *SessionFixture) MustAppend(sessionID, runID string, entries ...*models.SessionEntry) *models.AppendAgentSessionEntriesResult {
	f.T.Helper()
	result, err := f.append(sessionID, runID, entries...)
	if err != nil {
		f.T.Fatalf("append: %v", err)
	}
	return result
}

// TransitionRequest moves a tool entry from one state to another under the
// Run's lease.
func (f *SessionFixture) TransitionRequest(entryID, runID string, from, to models.ToolEntryState) models.ToolEntryTransitionRequest {
	req := models.ToolEntryTransitionRequest{
		Scope: f.Scope, EntryID: entryID, RunID: runID, Lease: f.Lease(), From: from, To: to, Now: f.Now,
	}
	switch {
	case to.Terminal():
		req.Result = ToolResultFor(to)
	case to.Awaiting():
		req.Wait = &models.ToolEntryWait{ApprovalID: "ap1"}
	}
	return req
}

func (f *SessionFixture) transition(entryID, runID string, from, to models.ToolEntryState) (*models.ToolEntryTransitionResult, error) {
	return f.Store.TransitionToolEntry(f.Ctx, f.TransitionRequest(entryID, runID, from, to))
}

// MustTransition transitions a tool entry and fails the test on error.
func (f *SessionFixture) MustTransition(entryID, runID string, from, to models.ToolEntryState) *models.SessionEntry {
	f.T.Helper()
	result, err := f.transition(entryID, runID, from, to)
	if err != nil {
		f.T.Fatalf("transition %s %s->%s: %v", entryID, from, to, err)
	}
	return result.Entry
}

// ToolResultFor is the result a tool entry carries into a terminal state.
func ToolResultFor(state models.ToolEntryState) *models.ToolResult {
	switch state {
	case models.ToolEntryCompleted:
		return &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{TextPart("done")}}
	case models.ToolEntryInterrupted:
		return &models.ToolResult{Status: models.ToolResultInterrupted, IsError: true, Content: []models.ContentPart{TextPart("interrupted")}}
	default:
		return &models.ToolResult{Status: models.ToolResultFailed, IsError: true, Content: []models.ContentPart{TextPart("failed")}}
	}
}

// Session reads a session that must exist.
func (f *SessionFixture) Session(sessionID string) *models.AgentSession {
	f.T.Helper()
	session, err := f.Store.GetAgentSession(f.Ctx, f.Scope, sessionID)
	if err != nil {
		f.T.Fatalf("get session: %v", err)
	}
	return session
}

// Entry reads an entry that must exist.
func (f *SessionFixture) Entry(entryID string) *models.SessionEntry {
	f.T.Helper()
	entry, err := f.Store.GetSessionEntry(f.Ctx, f.Scope, entryID)
	if err != nil {
		f.T.Fatalf("get entry %s: %v", entryID, err)
	}
	return entry
}

// AssertNoEntry fails the test if the entry exists.
func (f *SessionFixture) AssertNoEntry(entryID string) {
	f.T.Helper()
	if _, err := f.Store.GetSessionEntry(f.Ctx, f.Scope, entryID); !errors.Is(err, models.ErrSessionEntryNotFound) {
		f.T.Fatalf("entry %s exists or errs: %v", entryID, err)
	}
}

// Path loads the session's path.
func (f *SessionFixture) Path(sessionID string) *models.AgentSessionPath {
	f.T.Helper()
	path, err := f.Store.LoadAgentSessionPath(f.Ctx, f.Scope, sessionID)
	if err != nil {
		f.T.Fatalf("load path: %v", err)
	}
	return path
}

// ActivityCount counts the Run's activity events.
func (f *SessionFixture) ActivityCount(runID string) int {
	f.T.Helper()
	events, err := f.Store.ListActivity(f.Ctx, models.ActivityFilter{Scope: f.Scope, RunID: runID, Limit: 500})
	if err != nil {
		f.T.Fatalf("list activity: %v", err)
	}
	return len(events)
}

// Event is an activity event of the Run with summary.
func (f *SessionFixture) Event(runID, summary string) *models.ActivityEvent {
	return &models.ActivityEvent{
		ID: uuid.NewString(), Scope: f.Scope, EventType: "turn.narration", Severity: models.ActivitySeverityInfo, AgentID: "agent-a",
		RunID: runID, Actor: models.ActivityActor{Type: "worker", ID: SessionWorker}, Summary: summary, Visibility: models.ActivityVisibilityScope, CreatedAt: f.Now,
	}
}

// fork runs the backend's in-transaction run-session create.
func (f *SessionFixture) fork(ownerRunID, parentSessionID string, forkSequence int64, initial ...*models.SessionEntry) (*models.OpenAgentSessionResult, error) {
	return f.createRunSession(f.Ctx, models.CreateRunAgentSessionTxRequest{
		Scope: f.Scope, OwnerRunID: ownerRunID, AgentID: "agent-a", ParentSessionID: parentSessionID,
		ForkSequence: forkSequence, InitialEntries: initial, Now: f.Now,
	})
}

// forkedChild claims a child Run and forks its session from parent at forkSequence.
func (f *SessionFixture) forkedChild(parentSessionID string, forkSequence int64) (*models.AgentRun, *models.AgentSession) {
	f.T.Helper()
	child := f.LeasedRun("agent-a", "")
	result, err := f.fork(child.ID, parentSessionID, forkSequence)
	if err != nil || !result.Created {
		f.T.Fatalf("fork = %+v, %v", result, err)
	}
	return f.Run(child.ID), result.Session
}

// WithTokens sets the entry's token count.
func WithTokens(entry *models.SessionEntry, tokens int64) *models.SessionEntry {
	entry.Tokens = tokens
	return entry
}

func compactionEntry(id, firstKept string) *models.SessionEntry {
	return &models.SessionEntry{ID: id, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntry{
		Summary: "summary " + id, FirstKeptEntryID: firstKept, Trigger: models.CompactionTriggerThreshold, Provider: "anthropic", Model: "claude-x",
	}}
}

// GoalSessionEntry is the user entry that gives ownerRunID its goal.
func GoalSessionEntry(id, ownerRunID string) *models.SessionEntry {
	return &models.SessionEntry{ID: id, Type: models.SessionEntryUser, SourceKey: models.GoalSourceKey(ownerRunID), User: &models.UserEntry{
		Origin: models.UserEntryGoal, GoalKind: "task", Author: models.SessionAuthor{Type: "service", ID: "kernel"}, Content: []models.ContentPart{TextPart("do the task")},
	}}
}

// NoteSessionEntry is a note entry of kind.
func NoteSessionEntry(id string, kind models.NoteKind) *models.SessionEntry {
	return &models.SessionEntry{ID: id, Type: models.SessionEntryNote, Note: &models.NoteEntry{Kind: kind, Text: "note " + id}}
}

// ExpectErr fails the test unless err is want.
func ExpectErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s = %v, want %v", what, err, want)
	}
}

// EntryIDs joins the entries' IDs with commas.
func EntryIDs(entries []*models.SessionEntry) string {
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return strings.Join(ids, ",")
}
