package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// Cross-backend conformance suite of AgentSessionStore (run-loop section 01
// §10, SPEC §4.2/§4.6/§4.7). Each backend runs it from its own test:
// TestSQLiteAgentSessionStore and TestPostgresAgentSessionStore. The writer-model subtests of section 01 (2,
// 19, 30 and the promotion half of 31) are gone (A2); promotion is tested by
// store-8.

// AgentSessionContractStore is a backend under test. Runs are written with
// CreateAgentRun and leased with ClaimNextAgentRun.
type AgentSessionContractStore interface {
	models.AgentSessionStore
	models.PortfolioStore
	models.RunActivityStore
	models.AgentRunScheduleStore
}

// CreateRunSessionInOwnTx runs a backend's in-transaction
// createRunAgentSessionTx helper in a transaction of its own (forks).
type CreateRunSessionInOwnTx func(ctx context.Context, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error)

// SessionWorker is the worker that leases the fixture's Runs.
const SessionWorker = "session-worker"

// SessionFixture writes Runs, sessions and entries to one store in one
// scope at one clock. Tests outside the suite build their own with T, Ctx,
// Store, Scope and Now set.
type SessionFixture struct {
	T     *testing.T
	Ctx   context.Context
	Store AgentSessionContractStore
	// createRunSession is set only by the conformance suite, for fork.
	createRunSession CreateRunSessionInOwnTx
	Scope            models.Scope
	Now              time.Time
}

// RunAgentSessionContract runs every subtest of the suite on a fresh store
// from open; open also returns the backend's in-transaction run-session
// create, which the fork subtests call.
func RunAgentSessionContract(t *testing.T, open func(t *testing.T) (AgentSessionContractStore, CreateRunSessionInOwnTx)) {
	t.Helper()
	for _, sub := range agentSessionConformanceSubtests {
		sub := sub
		t.Run(sub.name, func(t *testing.T) {
			store, createRunSession := open(t)
			f := &SessionFixture{
				T:                t,
				Ctx:              context.Background(),
				Store:            store,
				createRunSession: createRunSession,
				Scope:            models.Scope{Kind: "tenant", ID: "session-" + uuid.NewString()[:8]},
				Now:              time.Now().UTC().Truncate(time.Millisecond),
			}
			sub.run(f)
		})
	}
}

var agentSessionConformanceSubtests = []struct {
	name string
	run  func(*SessionFixture)
}{
	{"OpenConversationCreatesOnceAndSeedsFlag", conformOpenConversationCreatesOnce},
	{"OpenConversationRequiresLease", conformOpenConversationRequiresLease},
	{"TeamMembersGetSeparateSessions", conformTeamMembersGetSeparateSessions},
	{"AppendAssignsDenseSequences", conformAppendAssignsDenseSequences},
	{"AppendFencedByLease", conformAppendFencedByLease},
	{"AppendReplaysSameBatch", conformAppendReplaysSameBatch},
	{"AppendSkipsExistingSourceKey", conformAppendSkipsExistingSourceKey},
	{"AppendWatermarkIsMonotonic", conformAppendWatermarkIsMonotonic},
	{"AppendEventsAreAtomic", conformAppendEventsAreAtomic},
	{"AppendToolEntriesNeedTheirAssistant", conformAppendToolEntriesNeedTheirAssistant},
	{"AppendRejectsOversize", conformAppendRejectsOversize},
	{"ModelChangeUpdatesSessionModel", conformModelChangeUpdatesSessionModel},
	{"TransitionHappyPath", conformTransitionHappyPath},
	{"TransitionCASOneWinner", conformTransitionCASOneWinner},
	{"TransitionRejectsInvalidEdges", conformTransitionRejectsInvalidEdges},
	{"TransitionResultRules", conformTransitionResultRules},
	{"TransitionFencedByEntryRun", conformTransitionFencedByEntryRun},
	{"LoadPathNoCompaction", conformLoadPathNoCompaction},
	{"LoadPathFromLatestCompaction", conformLoadPathFromLatestCompaction},
	{"LoadPathCompactionKeepsNothing", conformLoadPathCompactionKeepsNothing},
	{"LoadPathAcrossFork", conformLoadPathAcrossFork},
	{"LoadPathCompactionInParentAboveFork", conformLoadPathCompactionInParentAboveFork},
	{"LoadPathChildCompactionKeepsParentEntries", conformLoadPathChildCompactionKeepsParentEntries},
	{"CompactionFirstKeptMustBeOnPathAndNotTool", conformCompactionFirstKeptMustBeOnPathAndNotTool},
	{"CreateRunSessionForksAtParentLeaf", conformCreateRunSessionForksAtParentLeaf},
	{"CreateRunSessionIsIdempotent", conformCreateRunSessionIsIdempotent},
	{"ForkDepthLimit", conformForkDepthLimit},
	{"ListOpenToolEntriesByRun", conformListOpenToolEntriesByRun},
	{"ListByActionCallID", conformListByActionCallID},
	{"ParkedRunResumesAfterOtherRunAppended", conformParkedRunResumesAfterOtherRunAppended},
	{"CompactionCannotDropAssistantWithOpenToolEntry", conformCompactionCannotDropAssistantWithOpenToolEntry},
	{"LoadPathConsistentUnderConcurrentCompaction", conformLoadPathConsistentUnderConcurrentCompaction},
	{"SixteenConcurrentTransitionsOneBatch", conformSixteenConcurrentTransitionsOneBatch},
	{"WatermarkCASConflict", conformWatermarkCASConflict},
	{"CursorAdvancesWithAppend", conformCursorAdvancesWithAppend},
	{"SessionMovedRejected", conformSessionMovedRejected},
	{"TerminalRunNoteWithoutLease", conformTerminalRunNoteWithoutLease},
}

// ---------------------------------------------------------------------------
// Fixture helpers

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
	id := uuid.NewString()
	run := &models.AgentRun{
		ID: id, Kind: models.RunKindAgentWork, Scope: f.Scope, RootRunID: id,
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: agentID}, AssignedAgentID: agentID,
		Goal: "work on c1", Source: models.RunSourceObjective, Status: models.AgentRunStatusQueued,
		AvailableAt: f.Now, QueueEnteredAt: f.Now, Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now,
		SessionID: sessionID, Engine: "loop", // runtime.RunEngineLoop: a Run the run loop drives
	}
	if err := f.Store.CreateAgentRun(f.Ctx, run); err != nil {
		f.T.Fatalf("create run: %v", err)
	}
	return run
}

// leasedRun stores a Run bound to sessionID and claims it under
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

// updateRun rewrites the Run outside any lease (park, resume, finish, rebind).
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

// conversationRun claims a Run of the c1 lane of agentID and opens its session.
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

// ---------------------------------------------------------------------------
// Open / create

func conformOpenConversationCreatesOnce(f *SessionFixture) {
	run := f.LeasedRun("agent-a", f.laneSessionID("agent-a"))
	first, err := f.Store.OpenConversationAgentSession(f.Ctx, f.openRequest(run.ID, "agent-a"))
	if err != nil || !first.Created {
		f.T.Fatalf("first open = %+v, %v", first, err)
	}
	s := first.Session
	if s.ID != f.laneSessionID("agent-a") || s.Kind != models.AgentSessionKindConversation || s.LaneKey != models.ConversationRunLane("c1", "", "agent-a") ||
		s.ConversationID != "c1" || s.AgentID != "agent-a" || s.RootSessionID != s.ID || s.Depth != 0 || s.NextSequence != 1 || s.ChannelWatermark != 0 {
		f.T.Fatalf("new session = %+v", s)
	}
	again, err := f.Store.OpenConversationAgentSession(f.Ctx, f.openRequest(run.ID, "agent-a"))
	if err != nil || again.Created || again.Session.ID != s.ID {
		f.T.Fatalf("second open = %+v, %v", again, err)
	}
	// The next Run of the lane opens the same session and does not seed it.
	next := f.LeasedRun("agent-a", s.ID)
	third, err := f.Store.OpenConversationAgentSession(f.Ctx, f.openRequest(next.ID, "agent-a"))
	if err != nil || third.Created || third.Session.ID != s.ID {
		f.T.Fatalf("next lane run open = %+v, %v", third, err)
	}
}

func conformOpenConversationRequiresLease(f *SessionFixture) {
	run := f.LeasedRun("agent-a", f.laneSessionID("agent-a"))
	foreign := f.openRequest(run.ID, "agent-a")
	foreign.Lease.WorkerID = "another-worker"
	_, err := f.Store.OpenConversationAgentSession(f.Ctx, foreign)
	ExpectErr(f.T, "open with a foreign lease", err, models.ErrLeaseLost)
	expired := f.openRequest(run.ID, "agent-a")
	expired.Lease = f.ExpiredLease()
	_, err = f.Store.OpenConversationAgentSession(f.Ctx, expired)
	ExpectErr(f.T, "open with an expired lease", err, models.ErrLeaseLost)
	_, err = f.Store.GetAgentSession(f.Ctx, f.Scope, f.laneSessionID("agent-a"))
	ExpectErr(f.T, "session after a refused open", err, models.ErrAgentSessionNotFound)
}

func conformTeamMembersGetSeparateSessions(f *SessionFixture) {
	_, a := f.ConversationRun("agent-a")
	_, b := f.ConversationRun("agent-b")
	if a.ID == b.ID || a.LaneKey == b.LaneKey || a.AgentID != "agent-a" || b.AgentID != "agent-b" {
		f.T.Fatalf("team member sessions: a=%+v b=%+v", a, b)
	}
}

// ---------------------------------------------------------------------------
// Append

func conformAppendAssignsDenseSequences(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, UserSessionEntry("u1", "a"), UserSessionEntry("u2", "b"))
	f.MustAppend(session.ID, run.ID, UserSessionEntry("u3", "c"))
	result := f.MustAppend(session.ID, run.ID, AssistantSessionEntry("a1", "s1", "c1", "c2"),
		ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending), ToolSessionEntry("e2", "s1", "c2", models.ToolEntryCompleted))
	if result.Session.NextSequence != 7 {
		f.T.Fatalf("NextSequence = %d, want 7", result.Session.NextSequence)
	}
	for i, entry := range result.Entries {
		if entry.Sequence != int64(4+i) || entry.Revision != 1 || entry.Tokens <= 0 || entry.SessionID != session.ID || entry.RunID != run.ID || entry.Scope != f.Scope {
			f.T.Fatalf("entry %d = %+v", i, entry)
		}
	}
	if done := f.Entry("e2"); done.Tool.CompletedAt == nil || done.State != models.ToolEntryCompleted {
		f.T.Fatalf("terminal appended entry = %+v", done.Tool)
	}
	if got := f.Session(session.ID).NextSequence; got != 7 {
		f.T.Fatalf("stored NextSequence = %d, want 7", got)
	}
	if ids := PathEntryIDs(f.Path(session.ID)); ids != "u1,u2,u3,a1,e1,e2" {
		f.T.Fatalf("path = %s", ids)
	}
}

func conformAppendFencedByLease(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	before := f.ActivityCount(run.ID)
	zero := int64(0)
	for name, lease := range map[string]models.AgentRunLeaseGuard{
		"foreign": {WorkerID: "another-worker", Now: f.Now},
		"expired": f.ExpiredLease(),
	} {
		lease := lease
		req := f.AppendRequest(session.ID, run.ID, UserSessionEntry("u-"+name, "x"))
		req.Lease = &lease
		req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 9
		req.Events = []*models.ActivityEvent{f.Event(run.ID, "Working")}
		_, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
		ExpectErr(f.T, name+" lease append", err, models.ErrLeaseLost)
		f.AssertNoEntry("u-" + name)
	}
	stored := f.Session(session.ID)
	if stored.NextSequence != 1 || stored.ChannelWatermark != 0 {
		f.T.Fatalf("session after fenced appends = %+v", stored)
	}
	if after := f.ActivityCount(run.ID); after != before {
		f.T.Fatalf("fenced appends wrote %d events", after-before)
	}
}

func conformAppendReplaysSameBatch(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	batch := []*models.SessionEntry{AssistantSessionEntry("a1", "s1", "c1"), ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending)}
	f.MustAppend(session.ID, run.ID, batch...)
	replay := f.MustAppend(session.ID, run.ID, batch...)
	if !replay.Replayed || len(replay.Entries) != 2 || replay.Entries[0].Sequence != 1 || replay.Session.NextSequence != 3 {
		f.T.Fatalf("replay = %+v", replay)
	}
	if got := f.Session(session.ID).NextSequence; got != 3 {
		f.T.Fatalf("replay wrote rows: NextSequence = %d", got)
	}
	// One id reused in another session conflicts.
	other, otherSession := f.ConversationRun("agent-b")
	_, err := f.append(otherSession.ID, other.ID, UserSessionEntry("a1", "x"))
	ExpectErr(f.T, "id reused in another session", err, models.ErrAgentSessionConflict)
	if got := f.Session(otherSession.ID).NextSequence; got != 1 {
		f.T.Fatalf("conflicting append wrote rows")
	}
}

func conformAppendSkipsExistingSourceKey(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	first := UserSessionEntry("u1", "hi")
	first.SourceKey = models.ChannelMessageSourceKey("m1")
	zero, three := int64(0), int64(3)
	req := f.AppendRequest(session.ID, run.ID, first)
	req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 3
	if _, err := f.Store.AppendAgentSessionEntries(f.Ctx, req); err != nil {
		f.T.Fatal(err)
	}
	second := UserSessionEntry("u2", "hi again")
	second.SourceKey = models.ChannelMessageSourceKey("m1")
	req = f.AppendRequest(session.ID, run.ID, second, UserSessionEntry("u3", "new"))
	req.ExpectedChannelWatermark, req.ChannelWatermark = &three, 5
	result, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
	if err != nil {
		f.T.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "u2" || EntryIDs(result.Entries) != "u3" || result.Entries[0].Sequence != 2 {
		f.T.Fatalf("append with a dup source key = %+v", result)
	}
	if result.Session.ChannelWatermark != 5 || f.Session(session.ID).ChannelWatermark != 5 {
		f.T.Fatalf("watermark did not advance: %d", result.Session.ChannelWatermark)
	}
	f.AssertNoEntry("u2")
}

func conformAppendWatermarkIsMonotonic(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	zero, ten := int64(0), int64(10)
	req := f.AppendRequest(session.ID, run.ID)
	req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 10
	if _, err := f.Store.AppendAgentSessionEntries(f.Ctx, req); err != nil {
		f.T.Fatal(err)
	}
	req.ExpectedChannelWatermark, req.ChannelWatermark = &ten, 7
	_, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
	ExpectErr(f.T, "watermark moving backwards", err, models.ErrInvalidSessionEntry)
	if got := f.Session(session.ID).ChannelWatermark; got != 10 {
		f.T.Fatalf("watermark = %d, want 10", got)
	}
}

func conformAppendEventsAreAtomic(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	before := f.ActivityCount(run.ID)
	bad := f.Event(run.ID, "")
	req := f.AppendRequest(session.ID, run.ID, UserSessionEntry("u1", "x"))
	req.Events = []*models.ActivityEvent{f.Event(run.ID, "fine"), bad}
	_, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
	ExpectErr(f.T, "append with an invalid event", err, models.ErrInvalidSessionEntry)
	f.AssertNoEntry("u1")
	if after := f.ActivityCount(run.ID); after != before {
		f.T.Fatalf("failed append wrote %d events", after-before)
	}
	req.Events = []*models.ActivityEvent{f.Event(run.ID, "Checking the repo")}
	result, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
	if err != nil || len(result.Events) != 1 || result.Events[0].Sequence <= 0 || len(result.Entries) != 1 {
		f.T.Fatalf("append with an event = %+v, %v", result, err)
	}
	if after := f.ActivityCount(run.ID); after != before+1 {
		f.T.Fatalf("events after append = %d, want %d", after, before+1)
	}
}

func conformAppendToolEntriesNeedTheirAssistant(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	cases := map[string][]*models.SessionEntry{
		"alone":        {ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending)},
		"unknown call": {AssistantSessionEntry("a1", "s1", "c1"), ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending), ToolSessionEntry("e2", "s1", "c9", models.ToolEntryPending)},
		"missing tool": {AssistantSessionEntry("a1", "s1", "c1", "c2"), ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending)},
	}
	for name, entries := range cases {
		_, err := f.append(session.ID, run.ID, entries...)
		ExpectErr(f.T, name, err, models.ErrInvalidSessionEntry)
	}
	if got := f.Session(session.ID).NextSequence; got != 1 {
		f.T.Fatalf("rejected appends wrote rows")
	}
}

func conformAppendRejectsOversize(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	bigResult := ToolSessionEntry("e1", "s1", "c1", models.ToolEntryCompleted)
	bigResult.Tool.Result.Content = []models.ContentPart{TextPart(strings.Repeat("r", models.MaxToolResultContentBytes+1))}

	bigArgs := AssistantSessionEntry("a2", "s2", "c2")
	bigArgs.Assistant.ToolCalls[0].Arguments = json.RawMessage(`{"x":"` + strings.Repeat("a", models.MaxToolArgumentsBytes+1-len(`{"x":""}`)) + `"}`)
	if len(bigArgs.Assistant.ToolCalls[0].Arguments) != models.MaxToolArgumentsBytes+1 {
		f.T.Fatalf("arguments fixture is %d bytes", len(bigArgs.Assistant.ToolCalls[0].Arguments))
	}

	nativeEntry := func(id string, bytes int) *models.SessionEntry {
		entry := AssistantSessionEntry(id, "step-"+id)
		entry.Assistant.Native = &models.AssistantNative{Anthropic: []json.RawMessage{json.RawMessage(`"` + strings.Repeat("n", bytes) + `"`)}}
		return entry
	}
	many := make([]*models.SessionEntry, models.MaxSessionAppendEntries+1)
	for i := range many {
		many[i] = UserSessionEntry(fmt.Sprintf("m%d", i), "x")
	}
	var heavy []*models.SessionEntry
	for i := 0; i < 5; i++ {
		heavy = append(heavy, nativeEntry(fmt.Sprintf("h%d", i), models.MaxSessionEntryBytes-(256<<10)))
	}
	cases := map[string][]*models.SessionEntry{
		"tool result": {AssistantSessionEntry("a1", "s1", "c1"), bigResult},
		"arguments":   {bigArgs, ToolSessionEntry("e2", "s2", "c2", models.ToolEntryPending)},
		"entry":       {nativeEntry("big", models.MaxSessionEntryBytes+1)},
		"entries":     many,
		"batch":       heavy,
	}
	for name, entries := range cases {
		_, err := f.append(session.ID, run.ID, entries...)
		ExpectErr(f.T, name, err, models.ErrSessionEntryTooLarge)
	}
	if got := f.Session(session.ID).NextSequence; got != 1 {
		f.T.Fatalf("rejected appends wrote rows")
	}
}

func conformModelChangeUpdatesSessionModel(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	change := &models.SessionEntry{ID: "mc1", Type: models.SessionEntryModelChange, ModelChange: &models.ModelChangeEntry{Provider: "anthropic", Model: "claude-x"}}
	result := f.MustAppend(session.ID, run.ID, change)
	if result.Session.ModelProvider != "anthropic" || result.Session.Model != "claude-x" {
		f.T.Fatalf("session after model_change = %+v", result.Session)
	}
	second := &models.SessionEntry{ID: "mc2", Type: models.SessionEntryModelChange, ModelChange: &models.ModelChangeEntry{
		FromProvider: "anthropic", FromModel: "claude-x", Provider: "openai-responses", Model: "gpt-y",
	}}
	f.MustAppend(session.ID, run.ID, UserSessionEntry("u1", "x"), second)
	if stored := f.Session(session.ID); stored.ModelProvider != "openai-responses" || stored.Model != "gpt-y" {
		f.T.Fatalf("stored session model = %s/%s", stored.ModelProvider, stored.Model)
	}
}

// ---------------------------------------------------------------------------
// Transitions

// RunWithPendingCall leases a conversation Run whose session holds an
// assistant call with a pending tool entry.
func (f *SessionFixture) RunWithPendingCall() (*models.AgentRun, *models.AgentSession) {
	f.T.Helper()
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, AssistantSessionEntry("a1", "s1", "c1"), ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending))
	return run, session
}

func conformTransitionHappyPath(f *SessionFixture) {
	run, _ := f.RunWithPendingCall()
	running := f.MustTransition("e1", run.ID, models.ToolEntryPending, models.ToolEntryRunning)
	if running.State != models.ToolEntryRunning || running.Revision != 2 || running.Tool.StartedAt == nil || running.Tool.CompletedAt != nil {
		f.T.Fatalf("running entry = %+v / %+v", running, running.Tool)
	}
	req := f.TransitionRequest("e1", run.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	req.ExpectedRevision = 2
	req.Kernel = &models.ToolEntryKernelPatch{ResultDigest: "digest-1"}
	result, err := f.Store.TransitionToolEntry(f.Ctx, req)
	if err != nil {
		f.T.Fatal(err)
	}
	done := f.Entry("e1")
	if done.State != models.ToolEntryCompleted || done.Revision != 3 || done.Tool.StartedAt == nil || done.Tool.CompletedAt == nil ||
		done.Tool.Result == nil || done.Tool.Result.Status != models.ToolResultSucceeded || done.Tool.ResultDigest != "digest-1" || result.Entry.Revision != 3 {
		f.T.Fatalf("completed entry = %+v / %+v", done, done.Tool)
	}
}

func conformTransitionCASOneWinner(f *SessionFixture) {
	run, _ := f.RunWithPendingCall()
	f.MustTransition("e1", run.ID, models.ToolEntryPending, models.ToolEntryRunning)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.transition("e1", run.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, models.ErrToolEntryStateConflict):
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		f.T.Fatalf("concurrent completes = %v", errs)
	}
	if entry := f.Entry("e1"); entry.Revision != 3 || entry.State != models.ToolEntryCompleted {
		f.T.Fatalf("entry after race = %+v", entry)
	}
	// A stale expected revision loses even when the state matches.
	f.MustAppend(f.Run(run.ID).SessionID, run.ID, AssistantSessionEntry("a2", "s2", "c2"), ToolSessionEntry("e2", "s2", "c2", models.ToolEntryPending))
	stale := f.TransitionRequest("e2", run.ID, models.ToolEntryPending, models.ToolEntryRunning)
	stale.ExpectedRevision = 2
	_, err := f.Store.TransitionToolEntry(f.Ctx, stale)
	ExpectErr(f.T, "stale expected revision", err, models.ErrToolEntryStateConflict)
}

func conformTransitionRejectsInvalidEdges(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, AssistantSessionEntry("a1", "s1", "c1", "c2", "c3"),
		ToolSessionEntry("done", "s1", "c1", models.ToolEntryCompleted), ToolSessionEntry("open", "s1", "c2", models.ToolEntryPending),
		ToolSessionEntry("child", "s1", "c3", models.ToolEntryPending))
	f.MustTransition("child", run.ID, models.ToolEntryPending, models.ToolEntryAwaitingChild)
	for _, tc := range []struct {
		id       string
		from, to models.ToolEntryState
	}{
		{"done", models.ToolEntryCompleted, models.ToolEntryRunning},
		{"open", models.ToolEntryPending, models.ToolEntryPending},
		{"child", models.ToolEntryAwaitingChild, models.ToolEntryRunning},
	} {
		_, err := f.transition(tc.id, run.ID, tc.from, tc.to)
		ExpectErr(f.T, string(tc.from)+"->"+string(tc.to), err, models.ErrInvalidToolEntryTransition)
	}
	if f.Entry("open").Revision != 1 || f.Entry("child").Revision != 2 || f.Entry("done").Revision != 1 {
		f.T.Fatalf("rejected transitions wrote rows")
	}
}

func conformTransitionResultRules(f *SessionFixture) {
	run, _ := f.RunWithPendingCall()
	f.MustTransition("e1", run.ID, models.ToolEntryPending, models.ToolEntryRunning)
	noResult := f.TransitionRequest("e1", run.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	noResult.Result = nil
	nonTerminal := f.TransitionRequest("e1", run.ID, models.ToolEntryRunning, models.ToolEntryAwaitingEvent)
	nonTerminal.Result = ToolResultFor(models.ToolEntryError)
	disagree := f.TransitionRequest("e1", run.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	disagree.Result = &models.ToolResult{Status: models.ToolResultSucceeded, IsError: true, Content: []models.ContentPart{TextPart("x")}}
	succeededError := f.TransitionRequest("e1", run.ID, models.ToolEntryRunning, models.ToolEntryError)
	succeededError.Result = &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{TextPart("x")}}
	for name, req := range map[string]models.ToolEntryTransitionRequest{
		"terminal without result": noResult, "result on non-terminal": nonTerminal,
		"isError disagrees": disagree, "succeeded as error": succeededError,
	} {
		_, err := f.Store.TransitionToolEntry(f.Ctx, req)
		ExpectErr(f.T, name, err, models.ErrInvalidToolEntryTransition)
	}
	if entry := f.Entry("e1"); entry.State != models.ToolEntryRunning || entry.Revision != 2 {
		f.T.Fatalf("rejected transitions wrote rows: %+v", entry)
	}
}

func conformTransitionFencedByEntryRun(f *SessionFixture) {
	run, session := f.RunWithPendingCall()
	other := f.LeasedRun("agent-a", session.ID)
	_, err := f.transition("e1", other.ID, models.ToolEntryPending, models.ToolEntryRunning)
	ExpectErr(f.T, "transition by another run", err, models.ErrToolEntryNotOwned)
	req := f.TransitionRequest("e1", run.ID, models.ToolEntryPending, models.ToolEntryRunning)
	req.Lease = f.ExpiredLease()
	_, err = f.Store.TransitionToolEntry(f.Ctx, req)
	ExpectErr(f.T, "transition with an expired lease", err, models.ErrLeaseLost)
	if entry := f.Entry("e1"); entry.State != models.ToolEntryPending || entry.Revision != 1 {
		f.T.Fatalf("fenced transitions wrote rows: %+v", entry)
	}
}

// ---------------------------------------------------------------------------
// Paths

func conformLoadPathNoCompaction(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, WithTokens(UserSessionEntry("u1", "a"), 3))
	f.MustAppend(session.ID, run.ID, WithTokens(AssistantSessionEntry("a1", "s1", "c1"), 5), WithTokens(ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending), 7))
	f.MustAppend(session.ID, run.ID, WithTokens(UserSessionEntry("u2", "b"), 11))
	path := f.Path(session.ID)
	if PathEntryIDs(path) != "u1,a1,e1,u2" || path.Compaction != nil || path.Tokens != 26 || len(path.Chain) != 1 ||
		path.Session.ID != session.ID || path.Chain[0].ID != session.ID {
		f.T.Fatalf("path = %s tokens=%d chain=%d compaction=%v", PathEntryIDs(path), path.Tokens, len(path.Chain), path.Compaction)
	}
	for i, entry := range path.Entries {
		if entry.Sequence != int64(i+1) {
			f.T.Fatalf("path entry %d has sequence %d", i, entry.Sequence)
		}
	}
}

func conformLoadPathFromLatestCompaction(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, WithTokens(UserSessionEntry("u1", "a"), 3), WithTokens(UserSessionEntry("u2", "b"), 5))
	f.MustAppend(session.ID, run.ID, WithTokens(compactionEntry("k1", "u2"), 2))
	f.MustAppend(session.ID, run.ID, WithTokens(UserSessionEntry("u3", "c"), 7), WithTokens(UserSessionEntry("u4", "d"), 11))
	f.MustAppend(session.ID, run.ID, WithTokens(compactionEntry("k2", "u3"), 13))
	f.MustAppend(session.ID, run.ID, WithTokens(UserSessionEntry("u5", "e"), 17))
	path := f.Path(session.ID)
	if path.Compaction == nil || path.Compaction.ID != "k2" || PathEntryIDs(path) != "u3,u4,u5" || path.Tokens != 13+7+11+17 {
		f.T.Fatalf("path = %s compaction=%v tokens=%d", PathEntryIDs(path), path.Compaction, path.Tokens)
	}
	for _, entry := range path.Entries {
		if entry.Type == models.SessionEntryCompaction {
			f.T.Fatalf("path contains compaction %s", entry.ID)
		}
	}
}

func conformLoadPathCompactionKeepsNothing(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, WithTokens(UserSessionEntry("u1", "a"), 3), WithTokens(compactionEntry("k1", ""), 2))
	f.MustAppend(session.ID, run.ID, WithTokens(UserSessionEntry("u2", "b"), 5))
	path := f.Path(session.ID)
	if path.Compaction == nil || path.Compaction.ID != "k1" || PathEntryIDs(path) != "u2" || path.Tokens != 7 {
		f.T.Fatalf("path = %s tokens=%d", PathEntryIDs(path), path.Tokens)
	}
}

func (f *SessionFixture) parentWith(ids ...string) (*models.AgentRun, *models.AgentSession) {
	f.T.Helper()
	run, session := f.ConversationRun("agent-a")
	entries := make([]*models.SessionEntry, len(ids))
	for i, id := range ids {
		entries[i] = UserSessionEntry(id, id)
	}
	f.MustAppend(session.ID, run.ID, entries...)
	return run, session
}

func conformLoadPathAcrossFork(f *SessionFixture) {
	parentRun, parent := f.parentWith("p1", "p2", "p3")
	child, childSession := f.forkedChild(parent.ID, 2)
	f.MustAppend(childSession.ID, child.ID, UserSessionEntry("c1", "child"))
	f.MustAppend(parent.ID, parentRun.ID, UserSessionEntry("p4", "later"))
	path := f.Path(childSession.ID)
	if PathEntryIDs(path) != "p1,p2,c1" || len(path.Chain) != 2 || path.Chain[0].ID != parent.ID || path.Chain[1].ID != childSession.ID {
		f.T.Fatalf("child path = %s chain=%d", PathEntryIDs(path), len(path.Chain))
	}
	if ids := PathEntryIDs(f.Path(parent.ID)); ids != "p1,p2,p3,p4" {
		f.T.Fatalf("parent path = %s", ids)
	}
}

func conformLoadPathCompactionInParentAboveFork(f *SessionFixture) {
	parentRun, parent := f.parentWith("p1", "p2")
	f.MustAppend(parent.ID, parentRun.ID, compactionEntry("k1", "p2"), UserSessionEntry("p3", "p3"))
	_, childSession := f.forkedChild(parent.ID, 4)
	// A parent compaction after the fork point is not on the child's path.
	f.MustAppend(parent.ID, parentRun.ID, compactionEntry("k2", "p3"))
	child := f.Run(childSession.OwnerRunID)
	f.MustAppend(childSession.ID, child.ID, UserSessionEntry("c1", "child"))
	path := f.Path(childSession.ID)
	if path.Compaction == nil || path.Compaction.ID != "k1" || PathEntryIDs(path) != "p2,p3,c1" {
		f.T.Fatalf("child path = %s compaction=%v", PathEntryIDs(path), path.Compaction)
	}
	if parentPath := f.Path(parent.ID); parentPath.Compaction.ID != "k2" || PathEntryIDs(parentPath) != "p3" {
		f.T.Fatalf("parent path = %s", PathEntryIDs(parentPath))
	}
}

func conformLoadPathChildCompactionKeepsParentEntries(f *SessionFixture) {
	_, parent := f.parentWith("p1", "p2", "p3")
	child, childSession := f.forkedChild(parent.ID, 3)
	f.MustAppend(childSession.ID, child.ID, UserSessionEntry("c1", "child"))
	f.MustAppend(childSession.ID, child.ID, compactionEntry("ck", "p2"), UserSessionEntry("c2", "after"))
	path := f.Path(childSession.ID)
	if path.Compaction == nil || path.Compaction.ID != "ck" || PathEntryIDs(path) != "p2,p3,c1,c2" {
		f.T.Fatalf("child path = %s compaction=%v", PathEntryIDs(path), path.Compaction)
	}
}

func conformCompactionFirstKeptMustBeOnPathAndNotTool(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, UserSessionEntry("u1", "a"), AssistantSessionEntry("a1", "s1", "c1"), ToolSessionEntry("e1", "s1", "c1", models.ToolEntryCompleted))
	otherRun, other := f.ConversationRun("agent-b")
	f.MustAppend(other.ID, otherRun.ID, UserSessionEntry("elsewhere", "x"))
	for name, batch := range map[string][]*models.SessionEntry{
		"off path":             {compactionEntry("k1", "elsewhere")},
		"after the compaction": {compactionEntry("k2", "u9"), UserSessionEntry("u9", "later")},
		"tool entry":           {compactionEntry("k3", "e1")},
		"unknown":              {compactionEntry("k4", "missing")},
	} {
		_, err := f.append(session.ID, run.ID, batch...)
		ExpectErr(f.T, name, err, models.ErrInvalidSessionEntry)
	}
	if got := f.Session(session.ID).NextSequence; got != 4 {
		f.T.Fatalf("rejected compactions wrote rows")
	}
	f.MustAppend(session.ID, run.ID, compactionEntry("k5", "a1"))
}

// ---------------------------------------------------------------------------
// Run sessions and forks

func conformCreateRunSessionForksAtParentLeaf(f *SessionFixture) {
	// Parentless at first claim, through the public method.
	bg := f.LeasedRun("agent-a", "")
	created, err := f.Store.CreateRunAgentSession(f.Ctx, models.CreateRunAgentSessionRequest{
		Scope: f.Scope, OwnerRunID: bg.ID, AgentID: "agent-a", Lease: f.Lease(), InitialEntries: []*models.SessionEntry{GoalSessionEntry("g0", bg.ID)}, Now: f.Now,
	})
	if err != nil || !created.Created {
		f.T.Fatalf("create = %+v, %v", created, err)
	}
	s := created.Session
	if s.ID != models.RunAgentSessionID(f.Scope, bg.ID) || s.Kind != models.AgentSessionKindRun || s.OwnerRunID != bg.ID || s.Depth != 0 ||
		s.RootSessionID != s.ID || s.ParentSessionID != "" || s.NextSequence != 2 {
		f.T.Fatalf("run session = %+v", s)
	}
	if got := f.Run(bg.ID); got.SessionID != s.ID {
		f.T.Fatalf("run binding = %q", got.SessionID)
	}

	// Fork at the parent leaf.
	_, parent := f.parentWith("p1", "p2", "p3")
	leaf := f.Session(parent.ID).NextSequence - 1
	child := f.LeasedRun("agent-a", "")
	forked, err := f.fork(child.ID, parent.ID, leaf, GoalSessionEntry("g1", child.ID))
	if err != nil || !forked.Created {
		f.T.Fatalf("fork = %+v, %v", forked, err)
	}
	c := forked.Session
	if c.Depth != 1 || c.RootSessionID != parent.ID || c.ParentSessionID != parent.ID || c.ForkEntryID != "p3" || c.ForkSequence != 3 ||
		c.NextSequence != 2 || c.OwnerRunID != child.ID || c.ConversationID != "c1" || c.Kind != models.AgentSessionKindRun {
		f.T.Fatalf("forked session = %+v", c)
	}
	if got := f.Run(child.ID); got.SessionID != c.ID {
		f.T.Fatalf("child binding = %q", got.SessionID)
	}
	if goal := f.Entry("g1"); goal.SessionID != c.ID || goal.Sequence != 1 || goal.RunID != child.ID {
		f.T.Fatalf("initial entry = %+v", goal)
	}
	if ids := PathEntryIDs(f.Path(c.ID)); ids != "p1,p2,p3,g1" {
		f.T.Fatalf("child path = %s", ids)
	}
}

func conformCreateRunSessionIsIdempotent(f *SessionFixture) {
	_, parent := f.parentWith("p1")
	_, otherParent := f.ConversationRun("agent-b")
	child := f.LeasedRun("agent-a", "")
	first, err := f.fork(child.ID, parent.ID, 1, GoalSessionEntry("g1", child.ID))
	if err != nil || !first.Created {
		f.T.Fatalf("fork = %+v, %v", first, err)
	}
	revision := f.Run(child.ID).Revision
	again, err := f.fork(child.ID, parent.ID, 1, GoalSessionEntry("g1", child.ID))
	if err != nil || again.Created || again.Session.ID != first.Session.ID || again.Session.NextSequence != 2 {
		f.T.Fatalf("fork replay = %+v, %v", again, err)
	}
	if f.Run(child.ID).Revision != revision {
		f.T.Fatalf("fork replay rewrote the run")
	}
	_, err = f.fork(child.ID, otherParent.ID, 0)
	ExpectErr(f.T, "fork under another parent", err, models.ErrAgentSessionConflict)

	bg := f.LeasedRun("agent-a", "")
	req := models.CreateRunAgentSessionRequest{Scope: f.Scope, OwnerRunID: bg.ID, AgentID: "agent-a", Lease: f.Lease(), Now: f.Now}
	if created, err := f.Store.CreateRunAgentSession(f.Ctx, req); err != nil || !created.Created {
		f.T.Fatalf("create = %+v, %v", created, err)
	}
	if replay, err := f.Store.CreateRunAgentSession(f.Ctx, req); err != nil || replay.Created {
		f.T.Fatalf("create replay = %+v, %v", replay, err)
	}
}

func conformForkDepthLimit(f *SessionFixture) {
	_, parent := f.ConversationRun("agent-a")
	for depth := 1; depth <= models.MaxAgentSessionDepth+1; depth++ {
		child := f.createRun("agent-a", "")
		result, err := f.fork(child.ID, parent.ID, 0)
		if depth > models.MaxAgentSessionDepth {
			ExpectErr(f.T, fmt.Sprintf("fork at depth %d", depth), err, models.ErrAgentSessionForkTooDeep)
			return
		}
		if err != nil || result.Session.Depth != depth {
			f.T.Fatalf("fork at depth %d = %+v, %v", depth, result, err)
		}
		parent = result.Session
	}
}

// ---------------------------------------------------------------------------
// Lists

func conformListOpenToolEntriesByRun(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, AssistantSessionEntry("a1", "s1", "c1", "c2", "c3", "c4"),
		ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending), ToolSessionEntry("e2", "s1", "c2", models.ToolEntryPending),
		ToolSessionEntry("e3", "s1", "c3", models.ToolEntryCompleted), ToolSessionEntry("e4", "s1", "c4", models.ToolEntryPending))
	f.MustTransition("e2", run.ID, models.ToolEntryPending, models.ToolEntryAwaitingApproval)
	f.MustTransition("e4", run.ID, models.ToolEntryPending, models.ToolEntryError)
	other := f.LeasedRun("agent-a", session.ID)
	f.MustAppend(session.ID, other.ID, AssistantSessionEntry("b1", "s2", "d1"), ToolSessionEntry("f1", "s2", "d1", models.ToolEntryPending))

	open, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, RunID: run.ID, States: openSessionToolStates()})
	if err != nil || EntryIDs(open) != "e1,e2" {
		f.T.Fatalf("open entries of run = %s, %v", EntryIDs(open), err)
	}
	awaiting, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, SessionID: session.ID, States: []models.ToolEntryState{models.ToolEntryAwaitingApproval}})
	if err != nil || EntryIDs(awaiting) != "e2" || awaiting[0].Tool.Wait == nil {
		f.T.Fatalf("awaiting entries = %s, %v", EntryIDs(awaiting), err)
	}
	all, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, SessionID: session.ID, States: openSessionToolStates(), Descending: true, Limit: 2})
	if err != nil || EntryIDs(all) != "f1,e2" {
		f.T.Fatalf("open entries of session, newest first = %s, %v", EntryIDs(all), err)
	}
}

func conformListByActionCallID(f *SessionFixture) {
	run, _ := f.RunWithPendingCall()
	req := f.TransitionRequest("e1", run.ID, models.ToolEntryPending, models.ToolEntryRunning)
	req.ActionCallID = "ac-1"
	if _, err := f.Store.TransitionToolEntry(f.Ctx, req); err != nil {
		f.T.Fatal(err)
	}
	found, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, ActionCallID: "ac-1"})
	if err != nil || EntryIDs(found) != "e1" || found[0].ActionCallID != "ac-1" {
		f.T.Fatalf("list by action call = %s, %v", EntryIDs(found), err)
	}
	none, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, ActionCallID: "ac-2"})
	if err != nil || len(none) != 0 {
		f.T.Fatalf("list by unknown action call = %s, %v", EntryIDs(none), err)
	}
	// The id is set once.
	other := f.TransitionRequest("e1", run.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	other.ActionCallID = "ac-2"
	_, err = f.Store.TransitionToolEntry(f.Ctx, other)
	ExpectErr(f.T, "second action call id", err, models.ErrInvalidToolEntryTransition)
}

// ---------------------------------------------------------------------------
// Lane interleaving, compaction bound, concurrency (SPEC §4.2, §4.6, §4.7)

func conformParkedRunResumesAfterOtherRunAppended(f *SessionFixture) {
	runA, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, runA.ID, UserSessionEntry("uA", "first"))
	f.MustAppend(session.ID, runA.ID, AssistantSessionEntry("aA1", "sA1", "cA"), ToolSessionEntry("eA", "sA1", "cA", models.ToolEntryPending))
	f.MustTransition("eA", runA.ID, models.ToolEntryPending, models.ToolEntryAwaitingApproval)
	f.setStatus(runA.ID, models.AgentRunStatusWaitingForApproval)

	// The parked Run holds no lease.
	_, err := f.append(session.ID, runA.ID, UserSessionEntry("too-early", "x"))
	ExpectErr(f.T, "append by the parked run", err, models.ErrLeaseLost)

	runB := f.LeasedRun("agent-a", session.ID)
	if again := f.open(runB.ID, "agent-a"); again.ID != session.ID {
		f.T.Fatalf("lane run opened %s", again.ID)
	}
	f.MustAppend(session.ID, runB.ID, UserSessionEntry("uB", "second"))
	f.MustAppend(session.ID, runB.ID, AssistantSessionEntry("aB", "sB"))
	f.setStatus(runB.ID, models.AgentRunStatusCompleted)

	f.setStatus(runA.ID, models.AgentRunStatusRunning)
	f.MustTransition("eA", runA.ID, models.ToolEntryAwaitingApproval, models.ToolEntryRunning)
	f.MustTransition("eA", runA.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	f.MustAppend(session.ID, runA.ID, AssistantSessionEntry("aA2", "sA2"))

	path := f.Path(session.ID)
	if ids := PathEntryIDs(path); ids != "uA,aA1,eA,uB,aB,aA2" {
		f.T.Fatalf("interleaved path = %s", ids)
	}
	runs := make([]string, len(path.Entries))
	for i, entry := range path.Entries {
		runs[i] = map[string]string{runA.ID: "A", runB.ID: "B"}[entry.RunID]
	}
	if got := strings.Join(runs, ""); got != "AAABBA" {
		f.T.Fatalf("path runs = %s", got)
	}
	if path.Entries[2].State != models.ToolEntryCompleted {
		f.T.Fatalf("resumed tool entry state = %s", path.Entries[2].State)
	}
}

func conformCompactionCannotDropAssistantWithOpenToolEntry(f *SessionFixture) {
	runA, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, runA.ID, UserSessionEntry("u1", "a"))
	f.MustAppend(session.ID, runA.ID, AssistantSessionEntry("a1", "s1", "c1"), ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending))
	f.MustTransition("e1", runA.ID, models.ToolEntryPending, models.ToolEntryAwaitingApproval)
	f.setStatus(runA.ID, models.AgentRunStatusWaitingForApproval)

	// Another Run of the lane compacts: the open entry of Run A bounds the cut.
	runB := f.LeasedRun("agent-a", session.ID)
	f.MustAppend(session.ID, runB.ID, UserSessionEntry("u2", "b"))
	_, err := f.append(session.ID, runB.ID, compactionEntry("k1", "u2"))
	ExpectErr(f.T, "cut past an open call", err, models.ErrInvalidSessionEntry)
	_, err = f.append(session.ID, runB.ID, compactionEntry("k2", ""))
	ExpectErr(f.T, "keep nothing over an open call", err, models.ErrInvalidSessionEntry)
	_, err = f.append(session.ID, runB.ID, compactionEntry("k3", "e1"))
	ExpectErr(f.T, "cut at the tool entry", err, models.ErrInvalidSessionEntry)
	f.MustAppend(session.ID, runB.ID, compactionEntry("k4", "a1"))
	path := f.Path(session.ID)
	if path.Compaction == nil || path.Compaction.ID != "k4" || PathEntryIDs(path) != "a1,e1,u2" {
		f.T.Fatalf("path after compaction = %s", PathEntryIDs(path))
	}

	// Once the call settles, the cut may pass it.
	f.setStatus(runA.ID, models.AgentRunStatusRunning)
	f.MustTransition("e1", runA.ID, models.ToolEntryAwaitingApproval, models.ToolEntryRunning)
	f.MustTransition("e1", runA.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	f.MustAppend(session.ID, runB.ID, compactionEntry("k5", "u2"))
	if ids := PathEntryIDs(f.Path(session.ID)); ids != "u2" {
		f.T.Fatalf("path after the second compaction = %s", ids)
	}
}

func conformLoadPathConsistentUnderConcurrentCompaction(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, UserSessionEntry("u0", "start"))
	const rounds = 25
	done := make(chan struct{})
	var writeErr error
	go func() {
		defer close(done)
		for i := 1; i <= rounds; i++ {
			id := fmt.Sprintf("u%d", i)
			if _, err := f.append(session.ID, run.ID, UserSessionEntry(id, id)); err != nil {
				writeErr = err
				return
			}
			if _, err := f.append(session.ID, run.ID, compactionEntry(fmt.Sprintf("k%d", i), id)); err != nil {
				writeErr = err
				return
			}
		}
	}()
	var wg sync.WaitGroup
	readErrs := make(chan error, 4)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				path, err := f.Store.LoadAgentSessionPath(f.Ctx, f.Scope, session.ID)
				if err == nil {
					err = checkConcurrentCompactionPath(path)
				}
				if err != nil {
					readErrs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	<-done
	close(readErrs)
	if writeErr != nil {
		f.T.Fatalf("writer: %v", writeErr)
	}
	for err := range readErrs {
		f.T.Fatalf("reader: %v", err)
	}
	final := f.Path(session.ID)
	if final.Compaction == nil || final.Compaction.ID != fmt.Sprintf("k%d", rounds) || PathEntryIDs(final) != fmt.Sprintf("u%d", rounds) {
		f.T.Fatalf("final path = %s", PathEntryIDs(final))
	}
}

// checkConcurrentCompactionPath checks one path of the u_i / k_i(keep u_i)
// writer: it starts at the compaction's kept entry, holds at most that entry
// and the next one, never holds compactions, and its tokens add up.
func checkConcurrentCompactionPath(path *models.AgentSessionPath) error {
	var tokens int64
	if path.Compaction != nil {
		tokens = path.Compaction.Tokens
		if len(path.Entries) == 0 || path.Entries[0].ID != path.Compaction.Compaction.FirstKeptEntryID {
			return fmt.Errorf("path %s does not start at %s's kept entry", PathEntryIDs(path), path.Compaction.ID)
		}
		if path.Compaction.Sequence <= path.Entries[0].Sequence {
			return fmt.Errorf("compaction %s is not after its kept entry", path.Compaction.ID)
		}
	}
	if len(path.Entries) == 0 || len(path.Entries) > 2 {
		return fmt.Errorf("path %s has %d entries", PathEntryIDs(path), len(path.Entries))
	}
	for i, entry := range path.Entries {
		if entry.Type == models.SessionEntryCompaction {
			return fmt.Errorf("path holds compaction %s", entry.ID)
		}
		if i > 0 && entry.Sequence <= path.Entries[i-1].Sequence {
			return fmt.Errorf("path %s is out of order", PathEntryIDs(path))
		}
		if entry.Sequence > path.Session.NextSequence-1 {
			return fmt.Errorf("entry %s is past the session leaf", entry.ID)
		}
		tokens += entry.Tokens
	}
	if tokens != path.Tokens {
		return fmt.Errorf("path tokens = %d, entries add up to %d", path.Tokens, tokens)
	}
	return nil
}

func conformSixteenConcurrentTransitionsOneBatch(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	const calls = 16
	callIDs := make([]string, calls)
	for i := range callIDs {
		callIDs[i] = fmt.Sprintf("c%02d", i)
	}
	batch := []*models.SessionEntry{AssistantSessionEntry("a1", "s1", callIDs...)}
	for i, callID := range callIDs {
		batch = append(batch, ToolSessionEntry(fmt.Sprintf("e%02d", i), "s1", callID, models.ToolEntryPending))
	}
	f.MustAppend(session.ID, run.ID, batch...)
	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("e%02d", i)
			if _, err := f.transition(id, run.ID, models.ToolEntryPending, models.ToolEntryRunning); err != nil {
				errs[i] = err
				return
			}
			req := f.TransitionRequest(id, run.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
			req.ExpectedRevision = 2
			req.Events = []*models.ActivityEvent{f.Event(run.ID, "Finished "+id)}
			_, errs[i] = f.Store.TransitionToolEntry(f.Ctx, req)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			f.T.Fatalf("transition %d: %v", i, err)
		}
	}
	open, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, SessionID: session.ID, States: openSessionToolStates()})
	if err != nil || len(open) != 0 {
		f.T.Fatalf("open entries after the batch = %s, %v", EntryIDs(open), err)
	}
	completed, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, StepID: "s1", States: []models.ToolEntryState{models.ToolEntryCompleted}})
	if err != nil || len(completed) != calls {
		f.T.Fatalf("completed entries = %d, %v", len(completed), err)
	}
	for _, entry := range completed {
		if entry.Revision != 3 {
			f.T.Fatalf("entry %s revision = %d", entry.ID, entry.Revision)
		}
	}
	if got := f.Session(session.ID).NextSequence; got != calls+2 {
		f.T.Fatalf("transitions changed NextSequence to %d", got)
	}
}

// ---------------------------------------------------------------------------
// Watermark, cursor, binding and lease-less notes

func conformWatermarkCASConflict(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	zero, ten := int64(0), int64(10)
	req := f.AppendRequest(session.ID, run.ID, UserSessionEntry("u1", "x"))
	req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 10
	if result, err := f.Store.AppendAgentSessionEntries(f.Ctx, req); err != nil || result.Session.ChannelWatermark != 10 {
		f.T.Fatalf("CAS append = %+v, %v", result, err)
	}
	stale := f.AppendRequest(session.ID, run.ID, UserSessionEntry("u2", "x"))
	stale.ExpectedChannelWatermark, stale.ChannelWatermark = &zero, 12
	stale.Events = []*models.ActivityEvent{f.Event(run.ID, "Caught up")}
	before := f.ActivityCount(run.ID)
	_, err := f.Store.AppendAgentSessionEntries(f.Ctx, stale)
	ExpectErr(f.T, "stale watermark", err, models.ErrChannelWatermarkMoved)
	f.AssertNoEntry("u2")
	if stored := f.Session(session.ID); stored.ChannelWatermark != 10 || stored.NextSequence != 2 {
		f.T.Fatalf("session after a stale CAS = %+v", stored)
	}
	if f.ActivityCount(run.ID) != before {
		f.T.Fatalf("stale CAS wrote events")
	}
	fresh := f.AppendRequest(session.ID, run.ID, UserSessionEntry("u2", "x"))
	fresh.ExpectedChannelWatermark, fresh.ChannelWatermark = &ten, 12
	if result, err := f.Store.AppendAgentSessionEntries(f.Ctx, fresh); err != nil || result.Session.ChannelWatermark != 12 {
		f.T.Fatalf("fresh CAS append = %+v, %v", result, err)
	}
}

func conformCursorAdvancesWithAppend(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.UpdateRun(run.ID, func(r *models.AgentRun) {
		for _, id := range []string{"iv1", "iv2"} {
			r.PendingInterventions = append(r.PendingInterventions, models.AgentRunIntervention{
				ID: id, Actor: models.ActivityActor{Type: "user", ID: "u1"}, Instruction: "steer " + id, CreatedAt: f.Now,
			})
		}
	})
	before := f.Run(run.ID)
	steer := &models.SessionEntry{ID: "s1", Type: models.SessionEntryUser, SourceKey: models.InterventionSourceKey("iv1"), User: &models.UserEntry{
		Origin: models.UserEntryIntervention, InterventionID: "iv1", Author: models.SessionAuthor{Type: "user", ID: "u1"}, Content: []models.ContentPart{TextPart("steer iv1")},
	}}
	one := 1
	req := f.AppendRequest(session.ID, run.ID, steer)
	req.ConsumeInterventionsThrough = &one
	if _, err := f.Store.AppendAgentSessionEntries(f.Ctx, req); err != nil {
		f.T.Fatal(err)
	}
	after := f.Run(run.ID)
	if after.ConsumedInterventions != 1 || after.Revision != before.Revision+1 {
		f.T.Fatalf("run after cursor append: consumed=%d revision=%d (was %d)", after.ConsumedInterventions, after.Revision, before.Revision)
	}
	// Cursor-only append.
	two := 2
	cursorOnly := f.AppendRequest(session.ID, run.ID)
	cursorOnly.ConsumeInterventionsThrough = &two
	if _, err := f.Store.AppendAgentSessionEntries(f.Ctx, cursorOnly); err != nil {
		f.T.Fatal(err)
	}
	if got := f.Run(run.ID); got.ConsumedInterventions != 2 || got.Revision != after.Revision+1 {
		f.T.Fatalf("cursor-only append: consumed=%d revision=%d", got.ConsumedInterventions, got.Revision)
	}
	revision := f.Run(run.ID).Revision
	// Backwards or past the end fails and writes nothing.
	for _, bad := range []int{1, 3} {
		bad := bad
		badReq := f.AppendRequest(session.ID, run.ID, UserSessionEntry(fmt.Sprintf("bad%d", bad), "x"))
		badReq.ConsumeInterventionsThrough = &bad
		_, err := f.Store.AppendAgentSessionEntries(f.Ctx, badReq)
		ExpectErr(f.T, fmt.Sprintf("cursor %d", bad), err, models.ErrInvalidSessionEntry)
		f.AssertNoEntry(fmt.Sprintf("bad%d", bad))
	}
	// The same value writes nothing on the Run row.
	if _, err := f.Store.AppendAgentSessionEntries(f.Ctx, cursorOnly); err != nil {
		f.T.Fatal(err)
	}
	if got := f.Run(run.ID); got.ConsumedInterventions != 2 || got.Revision != revision {
		f.T.Fatalf("unchanged cursor: consumed=%d revision=%d (was %d)", got.ConsumedInterventions, got.Revision, revision)
	}
}

func conformSessionMovedRejected(f *SessionFixture) {
	runA, session := f.ConversationRun("agent-a")
	// A Run bound to another session (here agent-b's lane) cannot open or
	// append to agent-a's, even under a valid lease.
	runB, sessionB := f.ConversationRun("agent-b")
	_, err := f.Store.OpenConversationAgentSession(f.Ctx, f.openRequest(runB.ID, "agent-a"))
	ExpectErr(f.T, "open another lane", err, models.ErrSessionMoved)
	_, err = f.append(session.ID, runB.ID, UserSessionEntry("x1", "x"))
	ExpectErr(f.T, "append to another session", err, models.ErrSessionMoved)
	f.AssertNoEntry("x1")
	_, err = f.Store.CreateRunAgentSession(f.Ctx, models.CreateRunAgentSessionRequest{
		Scope: f.Scope, OwnerRunID: runA.ID, AgentID: "agent-a", Lease: f.Lease(), Now: f.Now,
	})
	ExpectErr(f.T, "create a run session for a bound run", err, models.ErrSessionMoved)

	// A Run whose binding moved (promotion) gets ErrSessionMoved on its old session.
	_, childSession := f.forkedChild(session.ID, 0)
	f.UpdateRun(runA.ID, func(r *models.AgentRun) { r.SessionID = childSession.ID })
	_, err = f.append(session.ID, runA.ID, UserSessionEntry("x2", "x"))
	ExpectErr(f.T, "append after the binding moved", err, models.ErrSessionMoved)
	_, err = f.Store.OpenConversationAgentSession(f.Ctx, f.openRequest(runA.ID, "agent-a"))
	ExpectErr(f.T, "open after the binding moved", err, models.ErrSessionMoved)
	if got := f.Session(session.ID).NextSequence; got != 1 {
		f.T.Fatalf("moved appends wrote rows")
	}
	f.MustAppend(sessionB.ID, runB.ID, UserSessionEntry("b1", "own session"))
}

func conformTerminalRunNoteWithoutLease(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	note := NoteSessionEntry("n1", models.NoteRunFailed)
	note.SourceKey = models.FailureNoteSourceKey(run.ID, "s1")
	req := models.AppendAgentSessionEntriesRequest{Scope: f.Scope, SessionID: session.ID, RunID: run.ID, Entries: []*models.SessionEntry{note}, Now: f.Now}
	_, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
	ExpectErr(f.T, "lease-less note on a running run", err, models.ErrLeaseLost)
	f.setStatus(run.ID, models.AgentRunStatusFailed)

	userReq := req
	userReq.Entries = []*models.SessionEntry{UserSessionEntry("u1", "x")}
	_, err = f.Store.AppendAgentSessionEntries(f.Ctx, userReq)
	ExpectErr(f.T, "lease-less user entry", err, models.ErrInvalidSessionEntry)

	result, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
	if err != nil || EntryIDs(result.Entries) != "n1" || result.Entries[0].Sequence != 1 {
		f.T.Fatalf("lease-less note on a failed run = %+v, %v", result, err)
	}
	replay, err := f.Store.AppendAgentSessionEntries(f.Ctx, req)
	if err != nil || !replay.Replayed {
		f.T.Fatalf("lease-less note replay = %+v, %v", replay, err)
	}
	// The terminal Run is still fenced by its session binding.
	other := f.createRun("agent-a", "elsewhere")
	f.setStatus(other.ID, models.AgentRunStatusFailed)
	otherReq := req
	otherReq.RunID = other.ID
	otherReq.Entries = []*models.SessionEntry{NoteSessionEntry("n2", models.NoteRunFailed)}
	_, err = f.Store.AppendAgentSessionEntries(f.Ctx, otherReq)
	ExpectErr(f.T, "lease-less note into another session", err, models.ErrSessionMoved)
	f.AssertNoEntry("n2")
}

// openSessionToolStates lists the states a tool entry can still leave.
func openSessionToolStates() []models.ToolEntryState {
	var open []models.ToolEntryState
	for _, state := range models.ToolEntryStateValues() {
		if state.Open() {
			open = append(open, state)
		}
	}
	return open
}
