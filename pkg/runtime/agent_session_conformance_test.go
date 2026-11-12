package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Cross-backend conformance suite of AgentSessionStore (run-loop section 01
// §10, SPEC §4.2/§4.6/§4.7). Each backend runs it from its own test:
// TestMemoryAgentSessionStore, TestSQLiteAgentSessionStore and
// TestPostgresAgentSessionStore. The writer-model subtests of section 01 (2,
// 19, 30 and the promotion half of 31) are gone (A2); promotion is tested by
// store-8.

// agentSessionConformanceStore is a backend under test. Runs are created and
// leased through the normal PortfolioService + ClaimNextAgentRun path.
type agentSessionConformanceStore interface {
	AgentSessionStore
	PortfolioStore
	RunActivityStore
	AgentRunScheduleStore
	// conformanceCreateRunAgentSessionTx runs the backend's in-transaction
	// createRunAgentSessionTx helper in a transaction of its own (forks).
	conformanceCreateRunAgentSessionTx(ctx context.Context, req CreateRunAgentSessionTxRequest) (*OpenAgentSessionResult, error)
}

const conformanceWorker = "session-worker"

type sessionConformance struct {
	t     *testing.T
	ctx   context.Context
	store agentSessionConformanceStore
	scope Scope
	now   time.Time
}

func runAgentSessionStoreConformance(t *testing.T, open func(t *testing.T) agentSessionConformanceStore) {
	t.Helper()
	for _, sub := range agentSessionConformanceSubtests {
		sub := sub
		t.Run(sub.name, func(t *testing.T) {
			f := &sessionConformance{
				t:     t,
				ctx:   context.Background(),
				store: open(t),
				scope: Scope{Kind: "tenant", ID: "session-" + uuid.NewString()[:8]},
				now:   time.Now().UTC().Truncate(time.Millisecond),
			}
			sub.run(f)
		})
	}
}

var agentSessionConformanceSubtests = []struct {
	name string
	run  func(*sessionConformance)
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

func (f *sessionConformance) lease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: conformanceWorker, Now: f.now}
}

func (f *sessionConformance) leasePtr() *AgentRunLeaseGuard {
	lease := f.lease()
	return &lease
}

func (f *sessionConformance) expiredLease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: conformanceWorker, Now: f.now.Add(2 * time.Hour)}
}

func (f *sessionConformance) laneSessionID(agentID string) string {
	return ConversationAgentSessionID(f.scope, ConversationRunLane("c1", "", agentID))
}

// createRun stores a queued Run bound to sessionID (may be "").
func (f *sessionConformance) createRun(agentID, sessionID string) *AgentRun {
	f.t.Helper()
	portfolio := NewPortfolioService(f.store)
	portfolio.now = func() time.Time { return f.now }
	run, err := portfolio.CreateAgentRun(f.ctx, CreateAgentRunRequest{
		Scope: f.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID}, AssignedAgentID: agentID,
		Goal: "work on c1", Source: RunSourceObjective, SessionID: sessionID,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	return run
}

// leasedRun stores a Run bound to sessionID and claims it under
// conformanceWorker. Only one Run is ever queued at a time, so the claim
// takes this one.
func (f *sessionConformance) leasedRun(agentID, sessionID string) *AgentRun {
	f.t.Helper()
	created := f.createRun(agentID, sessionID)
	run, err := f.store.ClaimNextAgentRun(f.ctx, AgentRunClaim{
		Scope: f.scope, WorkerID: conformanceWorker, Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || run == nil || run.ID != created.ID {
		f.t.Fatalf("claim = %#v, %v (want %s)", run, err, created.ID)
	}
	return run
}

func (f *sessionConformance) run(runID string) *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.scope, runID)
	if err != nil {
		f.t.Fatalf("get run %s: %v", runID, err)
	}
	return run
}

// updateRun rewrites the Run outside any lease (park, resume, finish, rebind).
func (f *sessionConformance) updateRun(runID string, mutate func(*AgentRun)) *AgentRun {
	f.t.Helper()
	current := f.run(runID)
	next := cloneAgentRun(current)
	mutate(next)
	next.Revision = current.Revision + 1
	next.UpdatedAt = f.now
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: f.scope, EventType: "run.status_changed", Severity: ActivitySeverityInfo, AgentID: current.AssignedAgentID,
		RunID: runID, Actor: ActivityActor{Type: "worker", ID: conformanceWorker}, Summary: "Run changed", Visibility: ActivityVisibilityScope, CreatedAt: f.now,
	}
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, next, current.Revision, event, nil); err != nil {
		f.t.Fatalf("update run %s: %v", runID, err)
	}
	return f.run(runID)
}

func (f *sessionConformance) setStatus(runID string, status AgentRunStatus) *AgentRun {
	return f.updateRun(runID, func(run *AgentRun) {
		run.Status = status
		switch {
		case status == AgentRunStatusRunning:
			expires := f.now.Add(time.Hour)
			run.LeaseOwner, run.LeaseExpiresAt = conformanceWorker, &expires
		default:
			run.LeaseOwner, run.LeaseExpiresAt = "", nil
		}
		if isTerminalAgentRunStatus(status) {
			completed := f.now
			run.CompletedAt = &completed
		}
	})
}

// conversationRun claims a Run of the c1 lane of agentID and opens its session.
func (f *sessionConformance) conversationRun(agentID string) (*AgentRun, *AgentSession) {
	f.t.Helper()
	run := f.leasedRun(agentID, f.laneSessionID(agentID))
	session := f.open(run.ID, agentID)
	return run, session
}

func (f *sessionConformance) openRequest(runID, agentID string) OpenConversationAgentSessionRequest {
	return OpenConversationAgentSessionRequest{
		Scope: f.scope, ConversationID: "c1", AgentID: agentID, RunID: runID, Lease: f.lease(), Now: f.now,
	}
}

func (f *sessionConformance) open(runID, agentID string) *AgentSession {
	f.t.Helper()
	opened, err := f.store.OpenConversationAgentSession(f.ctx, f.openRequest(runID, agentID))
	if err != nil {
		f.t.Fatalf("open session for %s: %v", runID, err)
	}
	return opened.Session
}

func (f *sessionConformance) appendReq(sessionID, runID string, entries ...*SessionEntry) AppendAgentSessionEntriesRequest {
	return AppendAgentSessionEntriesRequest{
		Scope: f.scope, SessionID: sessionID, RunID: runID, Lease: f.leasePtr(), Entries: entries, Now: f.now,
	}
}

func (f *sessionConformance) append(sessionID, runID string, entries ...*SessionEntry) (*AppendAgentSessionEntriesResult, error) {
	return f.store.AppendAgentSessionEntries(f.ctx, f.appendReq(sessionID, runID, entries...))
}

func (f *sessionConformance) mustAppend(sessionID, runID string, entries ...*SessionEntry) *AppendAgentSessionEntriesResult {
	f.t.Helper()
	result, err := f.append(sessionID, runID, entries...)
	if err != nil {
		f.t.Fatalf("append: %v", err)
	}
	return result
}

func (f *sessionConformance) transitionReq(entryID, runID string, from, to ToolEntryState) ToolEntryTransitionRequest {
	req := ToolEntryTransitionRequest{
		Scope: f.scope, EntryID: entryID, RunID: runID, Lease: f.lease(), From: from, To: to, Now: f.now,
	}
	switch {
	case to.Terminal():
		req.Result = conformanceResult(to)
	case to.awaiting():
		req.Wait = &ToolEntryWait{ApprovalID: "ap1"}
	}
	return req
}

func (f *sessionConformance) transition(entryID, runID string, from, to ToolEntryState) (*ToolEntryTransitionResult, error) {
	return f.store.TransitionToolEntry(f.ctx, f.transitionReq(entryID, runID, from, to))
}

func (f *sessionConformance) mustTransition(entryID, runID string, from, to ToolEntryState) *SessionEntry {
	f.t.Helper()
	result, err := f.transition(entryID, runID, from, to)
	if err != nil {
		f.t.Fatalf("transition %s %s->%s: %v", entryID, from, to, err)
	}
	return result.Entry
}

func conformanceResult(state ToolEntryState) *ToolResult {
	switch state {
	case ToolEntryCompleted:
		return &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{testTextPart("done")}}
	case ToolEntryInterrupted:
		return &ToolResult{Status: ToolResultInterrupted, IsError: true, Content: []ContentPart{testTextPart("interrupted")}}
	default:
		return &ToolResult{Status: ToolResultFailed, IsError: true, Content: []ContentPart{testTextPart("failed")}}
	}
}

func (f *sessionConformance) session(sessionID string) *AgentSession {
	f.t.Helper()
	session, err := f.store.GetAgentSession(f.ctx, f.scope, sessionID)
	if err != nil {
		f.t.Fatalf("get session: %v", err)
	}
	return session
}

func (f *sessionConformance) entry(entryID string) *SessionEntry {
	f.t.Helper()
	entry, err := f.store.GetSessionEntry(f.ctx, f.scope, entryID)
	if err != nil {
		f.t.Fatalf("get entry %s: %v", entryID, err)
	}
	return entry
}

func (f *sessionConformance) assertNoEntry(entryID string) {
	f.t.Helper()
	if _, err := f.store.GetSessionEntry(f.ctx, f.scope, entryID); !errors.Is(err, ErrSessionEntryNotFound) {
		f.t.Fatalf("entry %s exists or errs: %v", entryID, err)
	}
}

func (f *sessionConformance) path(sessionID string) *AgentSessionPath {
	f.t.Helper()
	path, err := f.store.LoadAgentSessionPath(f.ctx, f.scope, sessionID)
	if err != nil {
		f.t.Fatalf("load path: %v", err)
	}
	return path
}

func (f *sessionConformance) activityCount(runID string) int {
	f.t.Helper()
	events, err := f.store.ListActivity(f.ctx, ActivityFilter{Scope: f.scope, RunID: runID, Limit: 500})
	if err != nil {
		f.t.Fatalf("list activity: %v", err)
	}
	return len(events)
}

func (f *sessionConformance) event(runID, summary string) *ActivityEvent {
	return &ActivityEvent{
		ID: uuid.NewString(), Scope: f.scope, EventType: "turn.narration", Severity: ActivitySeverityInfo, AgentID: "agent-a",
		RunID: runID, Actor: ActivityActor{Type: "worker", ID: conformanceWorker}, Summary: summary, Visibility: ActivityVisibilityScope, CreatedAt: f.now,
	}
}

// fork runs the backend's in-transaction run-session create.
func (f *sessionConformance) fork(ownerRunID, parentSessionID string, forkSequence int64, initial ...*SessionEntry) (*OpenAgentSessionResult, error) {
	return f.store.conformanceCreateRunAgentSessionTx(f.ctx, CreateRunAgentSessionTxRequest{
		Scope: f.scope, OwnerRunID: ownerRunID, AgentID: "agent-a", ParentSessionID: parentSessionID,
		ForkSequence: forkSequence, InitialEntries: initial, Now: f.now,
	})
}

// forkedChild claims a child Run and forks its session from parent at forkSequence.
func (f *sessionConformance) forkedChild(parentSessionID string, forkSequence int64) (*AgentRun, *AgentSession) {
	f.t.Helper()
	child := f.leasedRun("agent-a", "")
	result, err := f.fork(child.ID, parentSessionID, forkSequence)
	if err != nil || !result.Created {
		f.t.Fatalf("fork = %+v, %v", result, err)
	}
	return f.run(child.ID), result.Session
}

func withTokens(entry *SessionEntry, tokens int64) *SessionEntry {
	entry.Tokens = tokens
	return entry
}

func compactionEntry(id, firstKept string) *SessionEntry {
	return &SessionEntry{ID: id, Type: SessionEntryCompaction, Compaction: &CompactionEntry{
		Summary: "summary " + id, FirstKeptEntryID: firstKept, Trigger: CompactionTriggerThreshold, Provider: "anthropic", Model: "claude-x",
	}}
}

func goalEntry(id, ownerRunID string) *SessionEntry {
	return &SessionEntry{ID: id, Type: SessionEntryUser, SourceKey: GoalSourceKey(ownerRunID), User: &UserEntry{
		Origin: UserEntryGoal, GoalKind: "task", Author: SessionAuthor{Type: "service", ID: "kernel"}, Content: []ContentPart{testTextPart("do the task")},
	}}
}

func noteEntry(id string, kind NoteKind) *SessionEntry {
	return &SessionEntry{ID: id, Type: SessionEntryNote, Note: &NoteEntry{Kind: kind, Text: "note " + id}}
}

func expectErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s = %v, want %v", what, err, want)
	}
}

func idsOf(entries []*SessionEntry) string {
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return strings.Join(ids, ",")
}

// ---------------------------------------------------------------------------
// Open / create

func conformOpenConversationCreatesOnce(f *sessionConformance) {
	run := f.leasedRun("agent-a", f.laneSessionID("agent-a"))
	first, err := f.store.OpenConversationAgentSession(f.ctx, f.openRequest(run.ID, "agent-a"))
	if err != nil || !first.Created {
		f.t.Fatalf("first open = %+v, %v", first, err)
	}
	s := first.Session
	if s.ID != f.laneSessionID("agent-a") || s.Kind != AgentSessionKindConversation || s.LaneKey != ConversationRunLane("c1", "", "agent-a") ||
		s.ConversationID != "c1" || s.AgentID != "agent-a" || s.RootSessionID != s.ID || s.Depth != 0 || s.NextSequence != 1 || s.ChannelWatermark != 0 {
		f.t.Fatalf("new session = %+v", s)
	}
	again, err := f.store.OpenConversationAgentSession(f.ctx, f.openRequest(run.ID, "agent-a"))
	if err != nil || again.Created || again.Session.ID != s.ID {
		f.t.Fatalf("second open = %+v, %v", again, err)
	}
	// The next Run of the lane opens the same session and does not seed it.
	next := f.leasedRun("agent-a", s.ID)
	third, err := f.store.OpenConversationAgentSession(f.ctx, f.openRequest(next.ID, "agent-a"))
	if err != nil || third.Created || third.Session.ID != s.ID {
		f.t.Fatalf("next lane run open = %+v, %v", third, err)
	}
}

func conformOpenConversationRequiresLease(f *sessionConformance) {
	run := f.leasedRun("agent-a", f.laneSessionID("agent-a"))
	foreign := f.openRequest(run.ID, "agent-a")
	foreign.Lease.WorkerID = "another-worker"
	_, err := f.store.OpenConversationAgentSession(f.ctx, foreign)
	expectErr(f.t, "open with a foreign lease", err, ErrLeaseLost)
	expired := f.openRequest(run.ID, "agent-a")
	expired.Lease = f.expiredLease()
	_, err = f.store.OpenConversationAgentSession(f.ctx, expired)
	expectErr(f.t, "open with an expired lease", err, ErrLeaseLost)
	_, err = f.store.GetAgentSession(f.ctx, f.scope, f.laneSessionID("agent-a"))
	expectErr(f.t, "session after a refused open", err, ErrAgentSessionNotFound)
}

func conformTeamMembersGetSeparateSessions(f *sessionConformance) {
	_, a := f.conversationRun("agent-a")
	_, b := f.conversationRun("agent-b")
	if a.ID == b.ID || a.LaneKey == b.LaneKey || a.AgentID != "agent-a" || b.AgentID != "agent-b" {
		f.t.Fatalf("team member sessions: a=%+v b=%+v", a, b)
	}
}

// ---------------------------------------------------------------------------
// Append

func conformAppendAssignsDenseSequences(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, testUserEntry("u1", "a"), testUserEntry("u2", "b"))
	f.mustAppend(session.ID, run.ID, testUserEntry("u3", "c"))
	result := f.mustAppend(session.ID, run.ID, testAssistantEntry("a1", "s1", "c1", "c2"),
		testToolEntry("e1", "s1", "c1", ToolEntryPending), testToolEntry("e2", "s1", "c2", ToolEntryCompleted))
	if result.Session.NextSequence != 7 {
		f.t.Fatalf("NextSequence = %d, want 7", result.Session.NextSequence)
	}
	for i, entry := range result.Entries {
		if entry.Sequence != int64(4+i) || entry.Revision != 1 || entry.Tokens <= 0 || entry.SessionID != session.ID || entry.RunID != run.ID || entry.Scope != f.scope {
			f.t.Fatalf("entry %d = %+v", i, entry)
		}
	}
	if done := f.entry("e2"); done.Tool.CompletedAt == nil || done.State != ToolEntryCompleted {
		f.t.Fatalf("terminal appended entry = %+v", done.Tool)
	}
	if got := f.session(session.ID).NextSequence; got != 7 {
		f.t.Fatalf("stored NextSequence = %d, want 7", got)
	}
	if ids := pathIDs(f.path(session.ID)); ids != "u1,u2,u3,a1,e1,e2" {
		f.t.Fatalf("path = %s", ids)
	}
}

func conformAppendFencedByLease(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	before := f.activityCount(run.ID)
	zero := int64(0)
	for name, lease := range map[string]AgentRunLeaseGuard{
		"foreign": {WorkerID: "another-worker", Now: f.now},
		"expired": f.expiredLease(),
	} {
		lease := lease
		req := f.appendReq(session.ID, run.ID, testUserEntry("u-"+name, "x"))
		req.Lease = &lease
		req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 9
		req.Events = []*ActivityEvent{f.event(run.ID, "Working")}
		_, err := f.store.AppendAgentSessionEntries(f.ctx, req)
		expectErr(f.t, name+" lease append", err, ErrLeaseLost)
		f.assertNoEntry("u-" + name)
	}
	stored := f.session(session.ID)
	if stored.NextSequence != 1 || stored.ChannelWatermark != 0 {
		f.t.Fatalf("session after fenced appends = %+v", stored)
	}
	if after := f.activityCount(run.ID); after != before {
		f.t.Fatalf("fenced appends wrote %d events", after-before)
	}
}

func conformAppendReplaysSameBatch(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	batch := []*SessionEntry{testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", ToolEntryPending)}
	f.mustAppend(session.ID, run.ID, batch...)
	replay := f.mustAppend(session.ID, run.ID, batch...)
	if !replay.Replayed || len(replay.Entries) != 2 || replay.Entries[0].Sequence != 1 || replay.Session.NextSequence != 3 {
		f.t.Fatalf("replay = %+v", replay)
	}
	if got := f.session(session.ID).NextSequence; got != 3 {
		f.t.Fatalf("replay wrote rows: NextSequence = %d", got)
	}
	// One id reused in another session conflicts.
	other, otherSession := f.conversationRun("agent-b")
	_, err := f.append(otherSession.ID, other.ID, testUserEntry("a1", "x"))
	expectErr(f.t, "id reused in another session", err, ErrAgentSessionConflict)
	if got := f.session(otherSession.ID).NextSequence; got != 1 {
		f.t.Fatalf("conflicting append wrote rows")
	}
}

func conformAppendSkipsExistingSourceKey(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	first := testUserEntry("u1", "hi")
	first.SourceKey = ChannelMessageSourceKey("m1")
	zero, three := int64(0), int64(3)
	req := f.appendReq(session.ID, run.ID, first)
	req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 3
	if _, err := f.store.AppendAgentSessionEntries(f.ctx, req); err != nil {
		f.t.Fatal(err)
	}
	second := testUserEntry("u2", "hi again")
	second.SourceKey = ChannelMessageSourceKey("m1")
	req = f.appendReq(session.ID, run.ID, second, testUserEntry("u3", "new"))
	req.ExpectedChannelWatermark, req.ChannelWatermark = &three, 5
	result, err := f.store.AppendAgentSessionEntries(f.ctx, req)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "u2" || idsOf(result.Entries) != "u3" || result.Entries[0].Sequence != 2 {
		f.t.Fatalf("append with a dup source key = %+v", result)
	}
	if result.Session.ChannelWatermark != 5 || f.session(session.ID).ChannelWatermark != 5 {
		f.t.Fatalf("watermark did not advance: %d", result.Session.ChannelWatermark)
	}
	f.assertNoEntry("u2")
}

func conformAppendWatermarkIsMonotonic(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	zero, ten := int64(0), int64(10)
	req := f.appendReq(session.ID, run.ID)
	req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 10
	if _, err := f.store.AppendAgentSessionEntries(f.ctx, req); err != nil {
		f.t.Fatal(err)
	}
	req.ExpectedChannelWatermark, req.ChannelWatermark = &ten, 7
	_, err := f.store.AppendAgentSessionEntries(f.ctx, req)
	expectErr(f.t, "watermark moving backwards", err, ErrInvalidSessionEntry)
	if got := f.session(session.ID).ChannelWatermark; got != 10 {
		f.t.Fatalf("watermark = %d, want 10", got)
	}
}

func conformAppendEventsAreAtomic(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	before := f.activityCount(run.ID)
	bad := f.event(run.ID, "")
	req := f.appendReq(session.ID, run.ID, testUserEntry("u1", "x"))
	req.Events = []*ActivityEvent{f.event(run.ID, "fine"), bad}
	_, err := f.store.AppendAgentSessionEntries(f.ctx, req)
	expectErr(f.t, "append with an invalid event", err, ErrInvalidSessionEntry)
	f.assertNoEntry("u1")
	if after := f.activityCount(run.ID); after != before {
		f.t.Fatalf("failed append wrote %d events", after-before)
	}
	req.Events = []*ActivityEvent{f.event(run.ID, "Checking the repo")}
	result, err := f.store.AppendAgentSessionEntries(f.ctx, req)
	if err != nil || len(result.Events) != 1 || result.Events[0].Sequence <= 0 || len(result.Entries) != 1 {
		f.t.Fatalf("append with an event = %+v, %v", result, err)
	}
	if after := f.activityCount(run.ID); after != before+1 {
		f.t.Fatalf("events after append = %d, want %d", after, before+1)
	}
}

func conformAppendToolEntriesNeedTheirAssistant(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	cases := map[string][]*SessionEntry{
		"alone":        {testToolEntry("e1", "s1", "c1", ToolEntryPending)},
		"unknown call": {testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", ToolEntryPending), testToolEntry("e2", "s1", "c9", ToolEntryPending)},
		"missing tool": {testAssistantEntry("a1", "s1", "c1", "c2"), testToolEntry("e1", "s1", "c1", ToolEntryPending)},
	}
	for name, entries := range cases {
		_, err := f.append(session.ID, run.ID, entries...)
		expectErr(f.t, name, err, ErrInvalidSessionEntry)
	}
	if got := f.session(session.ID).NextSequence; got != 1 {
		f.t.Fatalf("rejected appends wrote rows")
	}
}

func conformAppendRejectsOversize(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	bigResult := testToolEntry("e1", "s1", "c1", ToolEntryCompleted)
	bigResult.Tool.Result.Content = []ContentPart{testTextPart(strings.Repeat("r", MaxToolResultContentBytes+1))}

	bigArgs := testAssistantEntry("a2", "s2", "c2")
	bigArgs.Assistant.ToolCalls[0].Arguments = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxToolArgumentsBytes+1-len(`{"x":""}`)) + `"}`)
	if len(bigArgs.Assistant.ToolCalls[0].Arguments) != MaxToolArgumentsBytes+1 {
		f.t.Fatalf("arguments fixture is %d bytes", len(bigArgs.Assistant.ToolCalls[0].Arguments))
	}

	nativeEntry := func(id string, bytes int) *SessionEntry {
		entry := testAssistantEntry(id, "step-"+id)
		entry.Assistant.Native = &AssistantNative{Anthropic: []json.RawMessage{json.RawMessage(`"` + strings.Repeat("n", bytes) + `"`)}}
		return entry
	}
	many := make([]*SessionEntry, MaxSessionAppendEntries+1)
	for i := range many {
		many[i] = testUserEntry(fmt.Sprintf("m%d", i), "x")
	}
	var heavy []*SessionEntry
	for i := 0; i < 5; i++ {
		heavy = append(heavy, nativeEntry(fmt.Sprintf("h%d", i), MaxSessionEntryBytes-(256<<10)))
	}
	cases := map[string][]*SessionEntry{
		"tool result": {testAssistantEntry("a1", "s1", "c1"), bigResult},
		"arguments":   {bigArgs, testToolEntry("e2", "s2", "c2", ToolEntryPending)},
		"entry":       {nativeEntry("big", MaxSessionEntryBytes+1)},
		"entries":     many,
		"batch":       heavy,
	}
	for name, entries := range cases {
		_, err := f.append(session.ID, run.ID, entries...)
		expectErr(f.t, name, err, ErrSessionEntryTooLarge)
	}
	if got := f.session(session.ID).NextSequence; got != 1 {
		f.t.Fatalf("rejected appends wrote rows")
	}
}

func conformModelChangeUpdatesSessionModel(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	change := &SessionEntry{ID: "mc1", Type: SessionEntryModelChange, ModelChange: &ModelChangeEntry{Provider: "anthropic", Model: "claude-x"}}
	result := f.mustAppend(session.ID, run.ID, change)
	if result.Session.ModelProvider != "anthropic" || result.Session.Model != "claude-x" {
		f.t.Fatalf("session after model_change = %+v", result.Session)
	}
	second := &SessionEntry{ID: "mc2", Type: SessionEntryModelChange, ModelChange: &ModelChangeEntry{
		FromProvider: "anthropic", FromModel: "claude-x", Provider: "openai-responses", Model: "gpt-y",
	}}
	f.mustAppend(session.ID, run.ID, testUserEntry("u1", "x"), second)
	if stored := f.session(session.ID); stored.ModelProvider != "openai-responses" || stored.Model != "gpt-y" {
		f.t.Fatalf("stored session model = %s/%s", stored.ModelProvider, stored.Model)
	}
}

// ---------------------------------------------------------------------------
// Transitions

func (f *sessionConformance) runWithPendingCall() (*AgentRun, *AgentSession) {
	f.t.Helper()
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", ToolEntryPending))
	return run, session
}

func conformTransitionHappyPath(f *sessionConformance) {
	run, _ := f.runWithPendingCall()
	running := f.mustTransition("e1", run.ID, ToolEntryPending, ToolEntryRunning)
	if running.State != ToolEntryRunning || running.Revision != 2 || running.Tool.StartedAt == nil || running.Tool.CompletedAt != nil {
		f.t.Fatalf("running entry = %+v / %+v", running, running.Tool)
	}
	req := f.transitionReq("e1", run.ID, ToolEntryRunning, ToolEntryCompleted)
	req.ExpectedRevision = 2
	req.Kernel = &ToolEntryKernelPatch{ResultDigest: "digest-1"}
	result, err := f.store.TransitionToolEntry(f.ctx, req)
	if err != nil {
		f.t.Fatal(err)
	}
	done := f.entry("e1")
	if done.State != ToolEntryCompleted || done.Revision != 3 || done.Tool.StartedAt == nil || done.Tool.CompletedAt == nil ||
		done.Tool.Result == nil || done.Tool.Result.Status != ToolResultSucceeded || done.Tool.ResultDigest != "digest-1" || result.Entry.Revision != 3 {
		f.t.Fatalf("completed entry = %+v / %+v", done, done.Tool)
	}
}

func conformTransitionCASOneWinner(f *sessionConformance) {
	run, _ := f.runWithPendingCall()
	f.mustTransition("e1", run.ID, ToolEntryPending, ToolEntryRunning)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.transition("e1", run.ID, ToolEntryRunning, ToolEntryCompleted)
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrToolEntryStateConflict):
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		f.t.Fatalf("concurrent completes = %v", errs)
	}
	if entry := f.entry("e1"); entry.Revision != 3 || entry.State != ToolEntryCompleted {
		f.t.Fatalf("entry after race = %+v", entry)
	}
	// A stale expected revision loses even when the state matches.
	f.mustAppend(f.run(run.ID).SessionID, run.ID, testAssistantEntry("a2", "s2", "c2"), testToolEntry("e2", "s2", "c2", ToolEntryPending))
	stale := f.transitionReq("e2", run.ID, ToolEntryPending, ToolEntryRunning)
	stale.ExpectedRevision = 2
	_, err := f.store.TransitionToolEntry(f.ctx, stale)
	expectErr(f.t, "stale expected revision", err, ErrToolEntryStateConflict)
}

func conformTransitionRejectsInvalidEdges(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, testAssistantEntry("a1", "s1", "c1", "c2", "c3"),
		testToolEntry("done", "s1", "c1", ToolEntryCompleted), testToolEntry("open", "s1", "c2", ToolEntryPending),
		testToolEntry("child", "s1", "c3", ToolEntryPending))
	f.mustTransition("child", run.ID, ToolEntryPending, ToolEntryAwaitingChild)
	for _, tc := range []struct {
		id       string
		from, to ToolEntryState
	}{
		{"done", ToolEntryCompleted, ToolEntryRunning},
		{"open", ToolEntryPending, ToolEntryPending},
		{"child", ToolEntryAwaitingChild, ToolEntryRunning},
	} {
		_, err := f.transition(tc.id, run.ID, tc.from, tc.to)
		expectErr(f.t, string(tc.from)+"->"+string(tc.to), err, ErrInvalidToolEntryTransition)
	}
	if f.entry("open").Revision != 1 || f.entry("child").Revision != 2 || f.entry("done").Revision != 1 {
		f.t.Fatalf("rejected transitions wrote rows")
	}
}

func conformTransitionResultRules(f *sessionConformance) {
	run, _ := f.runWithPendingCall()
	f.mustTransition("e1", run.ID, ToolEntryPending, ToolEntryRunning)
	noResult := f.transitionReq("e1", run.ID, ToolEntryRunning, ToolEntryCompleted)
	noResult.Result = nil
	nonTerminal := f.transitionReq("e1", run.ID, ToolEntryRunning, ToolEntryAwaitingEvent)
	nonTerminal.Result = conformanceResult(ToolEntryError)
	disagree := f.transitionReq("e1", run.ID, ToolEntryRunning, ToolEntryCompleted)
	disagree.Result = &ToolResult{Status: ToolResultSucceeded, IsError: true, Content: []ContentPart{testTextPart("x")}}
	succeededError := f.transitionReq("e1", run.ID, ToolEntryRunning, ToolEntryError)
	succeededError.Result = &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{testTextPart("x")}}
	for name, req := range map[string]ToolEntryTransitionRequest{
		"terminal without result": noResult, "result on non-terminal": nonTerminal,
		"isError disagrees": disagree, "succeeded as error": succeededError,
	} {
		_, err := f.store.TransitionToolEntry(f.ctx, req)
		expectErr(f.t, name, err, ErrInvalidToolEntryTransition)
	}
	if entry := f.entry("e1"); entry.State != ToolEntryRunning || entry.Revision != 2 {
		f.t.Fatalf("rejected transitions wrote rows: %+v", entry)
	}
}

func conformTransitionFencedByEntryRun(f *sessionConformance) {
	run, session := f.runWithPendingCall()
	other := f.leasedRun("agent-a", session.ID)
	_, err := f.transition("e1", other.ID, ToolEntryPending, ToolEntryRunning)
	expectErr(f.t, "transition by another run", err, ErrToolEntryNotOwned)
	req := f.transitionReq("e1", run.ID, ToolEntryPending, ToolEntryRunning)
	req.Lease = f.expiredLease()
	_, err = f.store.TransitionToolEntry(f.ctx, req)
	expectErr(f.t, "transition with an expired lease", err, ErrLeaseLost)
	if entry := f.entry("e1"); entry.State != ToolEntryPending || entry.Revision != 1 {
		f.t.Fatalf("fenced transitions wrote rows: %+v", entry)
	}
}

// ---------------------------------------------------------------------------
// Paths

func conformLoadPathNoCompaction(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, withTokens(testUserEntry("u1", "a"), 3))
	f.mustAppend(session.ID, run.ID, withTokens(testAssistantEntry("a1", "s1", "c1"), 5), withTokens(testToolEntry("e1", "s1", "c1", ToolEntryPending), 7))
	f.mustAppend(session.ID, run.ID, withTokens(testUserEntry("u2", "b"), 11))
	path := f.path(session.ID)
	if pathIDs(path) != "u1,a1,e1,u2" || path.Compaction != nil || path.Tokens != 26 || len(path.Chain) != 1 ||
		path.Session.ID != session.ID || path.Chain[0].ID != session.ID {
		f.t.Fatalf("path = %s tokens=%d chain=%d compaction=%v", pathIDs(path), path.Tokens, len(path.Chain), path.Compaction)
	}
	for i, entry := range path.Entries {
		if entry.Sequence != int64(i+1) {
			f.t.Fatalf("path entry %d has sequence %d", i, entry.Sequence)
		}
	}
}

func conformLoadPathFromLatestCompaction(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, withTokens(testUserEntry("u1", "a"), 3), withTokens(testUserEntry("u2", "b"), 5))
	f.mustAppend(session.ID, run.ID, withTokens(compactionEntry("k1", "u2"), 2))
	f.mustAppend(session.ID, run.ID, withTokens(testUserEntry("u3", "c"), 7), withTokens(testUserEntry("u4", "d"), 11))
	f.mustAppend(session.ID, run.ID, withTokens(compactionEntry("k2", "u3"), 13))
	f.mustAppend(session.ID, run.ID, withTokens(testUserEntry("u5", "e"), 17))
	path := f.path(session.ID)
	if path.Compaction == nil || path.Compaction.ID != "k2" || pathIDs(path) != "u3,u4,u5" || path.Tokens != 13+7+11+17 {
		f.t.Fatalf("path = %s compaction=%v tokens=%d", pathIDs(path), path.Compaction, path.Tokens)
	}
	for _, entry := range path.Entries {
		if entry.Type == SessionEntryCompaction {
			f.t.Fatalf("path contains compaction %s", entry.ID)
		}
	}
}

func conformLoadPathCompactionKeepsNothing(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, withTokens(testUserEntry("u1", "a"), 3), withTokens(compactionEntry("k1", ""), 2))
	f.mustAppend(session.ID, run.ID, withTokens(testUserEntry("u2", "b"), 5))
	path := f.path(session.ID)
	if path.Compaction == nil || path.Compaction.ID != "k1" || pathIDs(path) != "u2" || path.Tokens != 7 {
		f.t.Fatalf("path = %s tokens=%d", pathIDs(path), path.Tokens)
	}
}

func (f *sessionConformance) parentWith(ids ...string) (*AgentRun, *AgentSession) {
	f.t.Helper()
	run, session := f.conversationRun("agent-a")
	entries := make([]*SessionEntry, len(ids))
	for i, id := range ids {
		entries[i] = testUserEntry(id, id)
	}
	f.mustAppend(session.ID, run.ID, entries...)
	return run, session
}

func conformLoadPathAcrossFork(f *sessionConformance) {
	parentRun, parent := f.parentWith("p1", "p2", "p3")
	child, childSession := f.forkedChild(parent.ID, 2)
	f.mustAppend(childSession.ID, child.ID, testUserEntry("c1", "child"))
	f.mustAppend(parent.ID, parentRun.ID, testUserEntry("p4", "later"))
	path := f.path(childSession.ID)
	if pathIDs(path) != "p1,p2,c1" || len(path.Chain) != 2 || path.Chain[0].ID != parent.ID || path.Chain[1].ID != childSession.ID {
		f.t.Fatalf("child path = %s chain=%d", pathIDs(path), len(path.Chain))
	}
	if ids := pathIDs(f.path(parent.ID)); ids != "p1,p2,p3,p4" {
		f.t.Fatalf("parent path = %s", ids)
	}
}

func conformLoadPathCompactionInParentAboveFork(f *sessionConformance) {
	parentRun, parent := f.parentWith("p1", "p2")
	f.mustAppend(parent.ID, parentRun.ID, compactionEntry("k1", "p2"), testUserEntry("p3", "p3"))
	_, childSession := f.forkedChild(parent.ID, 4)
	// A parent compaction after the fork point is not on the child's path.
	f.mustAppend(parent.ID, parentRun.ID, compactionEntry("k2", "p3"))
	child := f.run(childSession.OwnerRunID)
	f.mustAppend(childSession.ID, child.ID, testUserEntry("c1", "child"))
	path := f.path(childSession.ID)
	if path.Compaction == nil || path.Compaction.ID != "k1" || pathIDs(path) != "p2,p3,c1" {
		f.t.Fatalf("child path = %s compaction=%v", pathIDs(path), path.Compaction)
	}
	if parentPath := f.path(parent.ID); parentPath.Compaction.ID != "k2" || pathIDs(parentPath) != "p3" {
		f.t.Fatalf("parent path = %s", pathIDs(parentPath))
	}
}

func conformLoadPathChildCompactionKeepsParentEntries(f *sessionConformance) {
	_, parent := f.parentWith("p1", "p2", "p3")
	child, childSession := f.forkedChild(parent.ID, 3)
	f.mustAppend(childSession.ID, child.ID, testUserEntry("c1", "child"))
	f.mustAppend(childSession.ID, child.ID, compactionEntry("ck", "p2"), testUserEntry("c2", "after"))
	path := f.path(childSession.ID)
	if path.Compaction == nil || path.Compaction.ID != "ck" || pathIDs(path) != "p2,p3,c1,c2" {
		f.t.Fatalf("child path = %s compaction=%v", pathIDs(path), path.Compaction)
	}
}

func conformCompactionFirstKeptMustBeOnPathAndNotTool(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, testUserEntry("u1", "a"), testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", ToolEntryCompleted))
	otherRun, other := f.conversationRun("agent-b")
	f.mustAppend(other.ID, otherRun.ID, testUserEntry("elsewhere", "x"))
	for name, batch := range map[string][]*SessionEntry{
		"off path":             {compactionEntry("k1", "elsewhere")},
		"after the compaction": {compactionEntry("k2", "u9"), testUserEntry("u9", "later")},
		"tool entry":           {compactionEntry("k3", "e1")},
		"unknown":              {compactionEntry("k4", "missing")},
	} {
		_, err := f.append(session.ID, run.ID, batch...)
		expectErr(f.t, name, err, ErrInvalidSessionEntry)
	}
	if got := f.session(session.ID).NextSequence; got != 4 {
		f.t.Fatalf("rejected compactions wrote rows")
	}
	f.mustAppend(session.ID, run.ID, compactionEntry("k5", "a1"))
}

// ---------------------------------------------------------------------------
// Run sessions and forks

func conformCreateRunSessionForksAtParentLeaf(f *sessionConformance) {
	// Parentless at first claim, through the public method.
	bg := f.leasedRun("agent-a", "")
	created, err := f.store.CreateRunAgentSession(f.ctx, CreateRunAgentSessionRequest{
		Scope: f.scope, OwnerRunID: bg.ID, AgentID: "agent-a", Lease: f.lease(), InitialEntries: []*SessionEntry{goalEntry("g0", bg.ID)}, Now: f.now,
	})
	if err != nil || !created.Created {
		f.t.Fatalf("create = %+v, %v", created, err)
	}
	s := created.Session
	if s.ID != RunAgentSessionID(f.scope, bg.ID) || s.Kind != AgentSessionKindRun || s.OwnerRunID != bg.ID || s.Depth != 0 ||
		s.RootSessionID != s.ID || s.ParentSessionID != "" || s.NextSequence != 2 {
		f.t.Fatalf("run session = %+v", s)
	}
	if got := f.run(bg.ID); got.SessionID != s.ID {
		f.t.Fatalf("run binding = %q", got.SessionID)
	}

	// Fork at the parent leaf.
	_, parent := f.parentWith("p1", "p2", "p3")
	leaf := f.session(parent.ID).NextSequence - 1
	child := f.leasedRun("agent-a", "")
	forked, err := f.fork(child.ID, parent.ID, leaf, goalEntry("g1", child.ID))
	if err != nil || !forked.Created {
		f.t.Fatalf("fork = %+v, %v", forked, err)
	}
	c := forked.Session
	if c.Depth != 1 || c.RootSessionID != parent.ID || c.ParentSessionID != parent.ID || c.ForkEntryID != "p3" || c.ForkSequence != 3 ||
		c.NextSequence != 2 || c.OwnerRunID != child.ID || c.ConversationID != "c1" || c.Kind != AgentSessionKindRun {
		f.t.Fatalf("forked session = %+v", c)
	}
	if got := f.run(child.ID); got.SessionID != c.ID {
		f.t.Fatalf("child binding = %q", got.SessionID)
	}
	if goal := f.entry("g1"); goal.SessionID != c.ID || goal.Sequence != 1 || goal.RunID != child.ID {
		f.t.Fatalf("initial entry = %+v", goal)
	}
	if ids := pathIDs(f.path(c.ID)); ids != "p1,p2,p3,g1" {
		f.t.Fatalf("child path = %s", ids)
	}
}

func conformCreateRunSessionIsIdempotent(f *sessionConformance) {
	_, parent := f.parentWith("p1")
	_, otherParent := f.conversationRun("agent-b")
	child := f.leasedRun("agent-a", "")
	first, err := f.fork(child.ID, parent.ID, 1, goalEntry("g1", child.ID))
	if err != nil || !first.Created {
		f.t.Fatalf("fork = %+v, %v", first, err)
	}
	revision := f.run(child.ID).Revision
	again, err := f.fork(child.ID, parent.ID, 1, goalEntry("g1", child.ID))
	if err != nil || again.Created || again.Session.ID != first.Session.ID || again.Session.NextSequence != 2 {
		f.t.Fatalf("fork replay = %+v, %v", again, err)
	}
	if f.run(child.ID).Revision != revision {
		f.t.Fatalf("fork replay rewrote the run")
	}
	_, err = f.fork(child.ID, otherParent.ID, 0)
	expectErr(f.t, "fork under another parent", err, ErrAgentSessionConflict)

	bg := f.leasedRun("agent-a", "")
	req := CreateRunAgentSessionRequest{Scope: f.scope, OwnerRunID: bg.ID, AgentID: "agent-a", Lease: f.lease(), Now: f.now}
	if created, err := f.store.CreateRunAgentSession(f.ctx, req); err != nil || !created.Created {
		f.t.Fatalf("create = %+v, %v", created, err)
	}
	if replay, err := f.store.CreateRunAgentSession(f.ctx, req); err != nil || replay.Created {
		f.t.Fatalf("create replay = %+v, %v", replay, err)
	}
}

func conformForkDepthLimit(f *sessionConformance) {
	_, parent := f.conversationRun("agent-a")
	for depth := 1; depth <= MaxAgentSessionDepth+1; depth++ {
		child := f.createRun("agent-a", "")
		result, err := f.fork(child.ID, parent.ID, 0)
		if depth > MaxAgentSessionDepth {
			expectErr(f.t, fmt.Sprintf("fork at depth %d", depth), err, ErrAgentSessionForkTooDeep)
			return
		}
		if err != nil || result.Session.Depth != depth {
			f.t.Fatalf("fork at depth %d = %+v, %v", depth, result, err)
		}
		parent = result.Session
	}
}

// ---------------------------------------------------------------------------
// Lists

func conformListOpenToolEntriesByRun(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, testAssistantEntry("a1", "s1", "c1", "c2", "c3", "c4"),
		testToolEntry("e1", "s1", "c1", ToolEntryPending), testToolEntry("e2", "s1", "c2", ToolEntryPending),
		testToolEntry("e3", "s1", "c3", ToolEntryCompleted), testToolEntry("e4", "s1", "c4", ToolEntryPending))
	f.mustTransition("e2", run.ID, ToolEntryPending, ToolEntryAwaitingApproval)
	f.mustTransition("e4", run.ID, ToolEntryPending, ToolEntryError)
	other := f.leasedRun("agent-a", session.ID)
	f.mustAppend(session.ID, other.ID, testAssistantEntry("b1", "s2", "d1"), testToolEntry("f1", "s2", "d1", ToolEntryPending))

	open, err := f.store.ListAgentSessionEntries(f.ctx, SessionEntryFilter{Scope: f.scope, RunID: run.ID, States: OpenToolEntryStates})
	if err != nil || idsOf(open) != "e1,e2" {
		f.t.Fatalf("open entries of run = %s, %v", idsOf(open), err)
	}
	awaiting, err := f.store.ListAgentSessionEntries(f.ctx, SessionEntryFilter{Scope: f.scope, SessionID: session.ID, States: []ToolEntryState{ToolEntryAwaitingApproval}})
	if err != nil || idsOf(awaiting) != "e2" || awaiting[0].Tool.Wait == nil {
		f.t.Fatalf("awaiting entries = %s, %v", idsOf(awaiting), err)
	}
	all, err := f.store.ListAgentSessionEntries(f.ctx, SessionEntryFilter{Scope: f.scope, SessionID: session.ID, States: OpenToolEntryStates, Descending: true, Limit: 2})
	if err != nil || idsOf(all) != "f1,e2" {
		f.t.Fatalf("open entries of session, newest first = %s, %v", idsOf(all), err)
	}
}

func conformListByActionCallID(f *sessionConformance) {
	run, _ := f.runWithPendingCall()
	req := f.transitionReq("e1", run.ID, ToolEntryPending, ToolEntryRunning)
	req.ActionCallID = "ac-1"
	if _, err := f.store.TransitionToolEntry(f.ctx, req); err != nil {
		f.t.Fatal(err)
	}
	found, err := f.store.ListAgentSessionEntries(f.ctx, SessionEntryFilter{Scope: f.scope, ActionCallID: "ac-1"})
	if err != nil || idsOf(found) != "e1" || found[0].ActionCallID != "ac-1" {
		f.t.Fatalf("list by action call = %s, %v", idsOf(found), err)
	}
	none, err := f.store.ListAgentSessionEntries(f.ctx, SessionEntryFilter{Scope: f.scope, ActionCallID: "ac-2"})
	if err != nil || len(none) != 0 {
		f.t.Fatalf("list by unknown action call = %s, %v", idsOf(none), err)
	}
	// The id is set once.
	other := f.transitionReq("e1", run.ID, ToolEntryRunning, ToolEntryCompleted)
	other.ActionCallID = "ac-2"
	_, err = f.store.TransitionToolEntry(f.ctx, other)
	expectErr(f.t, "second action call id", err, ErrInvalidToolEntryTransition)
}

// ---------------------------------------------------------------------------
// Lane interleaving, compaction bound, concurrency (SPEC §4.2, §4.6, §4.7)

func conformParkedRunResumesAfterOtherRunAppended(f *sessionConformance) {
	runA, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, runA.ID, testUserEntry("uA", "first"))
	f.mustAppend(session.ID, runA.ID, testAssistantEntry("aA1", "sA1", "cA"), testToolEntry("eA", "sA1", "cA", ToolEntryPending))
	f.mustTransition("eA", runA.ID, ToolEntryPending, ToolEntryAwaitingApproval)
	f.setStatus(runA.ID, AgentRunStatusWaitingForApproval)

	// The parked Run holds no lease.
	_, err := f.append(session.ID, runA.ID, testUserEntry("too-early", "x"))
	expectErr(f.t, "append by the parked run", err, ErrLeaseLost)

	runB := f.leasedRun("agent-a", session.ID)
	if again := f.open(runB.ID, "agent-a"); again.ID != session.ID {
		f.t.Fatalf("lane run opened %s", again.ID)
	}
	f.mustAppend(session.ID, runB.ID, testUserEntry("uB", "second"))
	f.mustAppend(session.ID, runB.ID, testAssistantEntry("aB", "sB"))
	f.setStatus(runB.ID, AgentRunStatusCompleted)

	f.setStatus(runA.ID, AgentRunStatusRunning)
	f.mustTransition("eA", runA.ID, ToolEntryAwaitingApproval, ToolEntryRunning)
	f.mustTransition("eA", runA.ID, ToolEntryRunning, ToolEntryCompleted)
	f.mustAppend(session.ID, runA.ID, testAssistantEntry("aA2", "sA2"))

	path := f.path(session.ID)
	if ids := pathIDs(path); ids != "uA,aA1,eA,uB,aB,aA2" {
		f.t.Fatalf("interleaved path = %s", ids)
	}
	runs := make([]string, len(path.Entries))
	for i, entry := range path.Entries {
		runs[i] = map[string]string{runA.ID: "A", runB.ID: "B"}[entry.RunID]
	}
	if got := strings.Join(runs, ""); got != "AAABBA" {
		f.t.Fatalf("path runs = %s", got)
	}
	if path.Entries[2].State != ToolEntryCompleted {
		f.t.Fatalf("resumed tool entry state = %s", path.Entries[2].State)
	}
}

func conformCompactionCannotDropAssistantWithOpenToolEntry(f *sessionConformance) {
	runA, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, runA.ID, testUserEntry("u1", "a"))
	f.mustAppend(session.ID, runA.ID, testAssistantEntry("a1", "s1", "c1"), testToolEntry("e1", "s1", "c1", ToolEntryPending))
	f.mustTransition("e1", runA.ID, ToolEntryPending, ToolEntryAwaitingApproval)
	f.setStatus(runA.ID, AgentRunStatusWaitingForApproval)

	// Another Run of the lane compacts: the open entry of Run A bounds the cut.
	runB := f.leasedRun("agent-a", session.ID)
	f.mustAppend(session.ID, runB.ID, testUserEntry("u2", "b"))
	_, err := f.append(session.ID, runB.ID, compactionEntry("k1", "u2"))
	expectErr(f.t, "cut past an open call", err, ErrInvalidSessionEntry)
	_, err = f.append(session.ID, runB.ID, compactionEntry("k2", ""))
	expectErr(f.t, "keep nothing over an open call", err, ErrInvalidSessionEntry)
	_, err = f.append(session.ID, runB.ID, compactionEntry("k3", "e1"))
	expectErr(f.t, "cut at the tool entry", err, ErrInvalidSessionEntry)
	f.mustAppend(session.ID, runB.ID, compactionEntry("k4", "a1"))
	path := f.path(session.ID)
	if path.Compaction == nil || path.Compaction.ID != "k4" || pathIDs(path) != "a1,e1,u2" {
		f.t.Fatalf("path after compaction = %s", pathIDs(path))
	}

	// Once the call settles, the cut may pass it.
	f.setStatus(runA.ID, AgentRunStatusRunning)
	f.mustTransition("e1", runA.ID, ToolEntryAwaitingApproval, ToolEntryRunning)
	f.mustTransition("e1", runA.ID, ToolEntryRunning, ToolEntryCompleted)
	f.mustAppend(session.ID, runB.ID, compactionEntry("k5", "u2"))
	if ids := pathIDs(f.path(session.ID)); ids != "u2" {
		f.t.Fatalf("path after the second compaction = %s", ids)
	}
}

func conformLoadPathConsistentUnderConcurrentCompaction(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.mustAppend(session.ID, run.ID, testUserEntry("u0", "start"))
	const rounds = 25
	done := make(chan struct{})
	var writeErr error
	go func() {
		defer close(done)
		for i := 1; i <= rounds; i++ {
			id := fmt.Sprintf("u%d", i)
			if _, err := f.append(session.ID, run.ID, testUserEntry(id, id)); err != nil {
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
				path, err := f.store.LoadAgentSessionPath(f.ctx, f.scope, session.ID)
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
		f.t.Fatalf("writer: %v", writeErr)
	}
	for err := range readErrs {
		f.t.Fatalf("reader: %v", err)
	}
	final := f.path(session.ID)
	if final.Compaction == nil || final.Compaction.ID != fmt.Sprintf("k%d", rounds) || pathIDs(final) != fmt.Sprintf("u%d", rounds) {
		f.t.Fatalf("final path = %s", pathIDs(final))
	}
}

// checkConcurrentCompactionPath checks one path of the u_i / k_i(keep u_i)
// writer: it starts at the compaction's kept entry, holds at most that entry
// and the next one, never holds compactions, and its tokens add up.
func checkConcurrentCompactionPath(path *AgentSessionPath) error {
	var tokens int64
	if path.Compaction != nil {
		tokens = path.Compaction.Tokens
		if len(path.Entries) == 0 || path.Entries[0].ID != path.Compaction.Compaction.FirstKeptEntryID {
			return fmt.Errorf("path %s does not start at %s's kept entry", pathIDs(path), path.Compaction.ID)
		}
		if path.Compaction.Sequence <= path.Entries[0].Sequence {
			return fmt.Errorf("compaction %s is not after its kept entry", path.Compaction.ID)
		}
	}
	if len(path.Entries) == 0 || len(path.Entries) > 2 {
		return fmt.Errorf("path %s has %d entries", pathIDs(path), len(path.Entries))
	}
	for i, entry := range path.Entries {
		if entry.Type == SessionEntryCompaction {
			return fmt.Errorf("path holds compaction %s", entry.ID)
		}
		if i > 0 && entry.Sequence <= path.Entries[i-1].Sequence {
			return fmt.Errorf("path %s is out of order", pathIDs(path))
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

func conformSixteenConcurrentTransitionsOneBatch(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	const calls = 16
	callIDs := make([]string, calls)
	for i := range callIDs {
		callIDs[i] = fmt.Sprintf("c%02d", i)
	}
	batch := []*SessionEntry{testAssistantEntry("a1", "s1", callIDs...)}
	for i, callID := range callIDs {
		batch = append(batch, testToolEntry(fmt.Sprintf("e%02d", i), "s1", callID, ToolEntryPending))
	}
	f.mustAppend(session.ID, run.ID, batch...)
	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("e%02d", i)
			if _, err := f.transition(id, run.ID, ToolEntryPending, ToolEntryRunning); err != nil {
				errs[i] = err
				return
			}
			req := f.transitionReq(id, run.ID, ToolEntryRunning, ToolEntryCompleted)
			req.ExpectedRevision = 2
			req.Events = []*ActivityEvent{f.event(run.ID, "Finished "+id)}
			_, errs[i] = f.store.TransitionToolEntry(f.ctx, req)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			f.t.Fatalf("transition %d: %v", i, err)
		}
	}
	open, err := f.store.ListAgentSessionEntries(f.ctx, SessionEntryFilter{Scope: f.scope, SessionID: session.ID, States: OpenToolEntryStates})
	if err != nil || len(open) != 0 {
		f.t.Fatalf("open entries after the batch = %s, %v", idsOf(open), err)
	}
	completed, err := f.store.ListAgentSessionEntries(f.ctx, SessionEntryFilter{Scope: f.scope, StepID: "s1", States: []ToolEntryState{ToolEntryCompleted}})
	if err != nil || len(completed) != calls {
		f.t.Fatalf("completed entries = %d, %v", len(completed), err)
	}
	for _, entry := range completed {
		if entry.Revision != 3 {
			f.t.Fatalf("entry %s revision = %d", entry.ID, entry.Revision)
		}
	}
	if got := f.session(session.ID).NextSequence; got != calls+2 {
		f.t.Fatalf("transitions changed NextSequence to %d", got)
	}
}

// ---------------------------------------------------------------------------
// Watermark, cursor, binding and lease-less notes

func conformWatermarkCASConflict(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	zero, ten := int64(0), int64(10)
	req := f.appendReq(session.ID, run.ID, testUserEntry("u1", "x"))
	req.ExpectedChannelWatermark, req.ChannelWatermark = &zero, 10
	if result, err := f.store.AppendAgentSessionEntries(f.ctx, req); err != nil || result.Session.ChannelWatermark != 10 {
		f.t.Fatalf("CAS append = %+v, %v", result, err)
	}
	stale := f.appendReq(session.ID, run.ID, testUserEntry("u2", "x"))
	stale.ExpectedChannelWatermark, stale.ChannelWatermark = &zero, 12
	stale.Events = []*ActivityEvent{f.event(run.ID, "Caught up")}
	before := f.activityCount(run.ID)
	_, err := f.store.AppendAgentSessionEntries(f.ctx, stale)
	expectErr(f.t, "stale watermark", err, ErrChannelWatermarkMoved)
	f.assertNoEntry("u2")
	if stored := f.session(session.ID); stored.ChannelWatermark != 10 || stored.NextSequence != 2 {
		f.t.Fatalf("session after a stale CAS = %+v", stored)
	}
	if f.activityCount(run.ID) != before {
		f.t.Fatalf("stale CAS wrote events")
	}
	fresh := f.appendReq(session.ID, run.ID, testUserEntry("u2", "x"))
	fresh.ExpectedChannelWatermark, fresh.ChannelWatermark = &ten, 12
	if result, err := f.store.AppendAgentSessionEntries(f.ctx, fresh); err != nil || result.Session.ChannelWatermark != 12 {
		f.t.Fatalf("fresh CAS append = %+v, %v", result, err)
	}
}

func conformCursorAdvancesWithAppend(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	f.updateRun(run.ID, func(r *AgentRun) {
		for _, id := range []string{"iv1", "iv2"} {
			r.PendingInterventions = append(r.PendingInterventions, AgentRunIntervention{
				ID: id, Actor: ActivityActor{Type: "user", ID: "u1"}, Instruction: "steer " + id, CreatedAt: f.now,
			})
		}
	})
	before := f.run(run.ID)
	steer := &SessionEntry{ID: "s1", Type: SessionEntryUser, SourceKey: InterventionSourceKey("iv1"), User: &UserEntry{
		Origin: UserEntryIntervention, InterventionID: "iv1", Author: SessionAuthor{Type: "user", ID: "u1"}, Content: []ContentPart{testTextPart("steer iv1")},
	}}
	one := 1
	req := f.appendReq(session.ID, run.ID, steer)
	req.ConsumeInterventionsThrough = &one
	if _, err := f.store.AppendAgentSessionEntries(f.ctx, req); err != nil {
		f.t.Fatal(err)
	}
	after := f.run(run.ID)
	if after.ConsumedInterventions != 1 || after.Revision != before.Revision+1 {
		f.t.Fatalf("run after cursor append: consumed=%d revision=%d (was %d)", after.ConsumedInterventions, after.Revision, before.Revision)
	}
	// Cursor-only append.
	two := 2
	cursorOnly := f.appendReq(session.ID, run.ID)
	cursorOnly.ConsumeInterventionsThrough = &two
	if _, err := f.store.AppendAgentSessionEntries(f.ctx, cursorOnly); err != nil {
		f.t.Fatal(err)
	}
	if got := f.run(run.ID); got.ConsumedInterventions != 2 || got.Revision != after.Revision+1 {
		f.t.Fatalf("cursor-only append: consumed=%d revision=%d", got.ConsumedInterventions, got.Revision)
	}
	revision := f.run(run.ID).Revision
	// Backwards or past the end fails and writes nothing.
	for _, bad := range []int{1, 3} {
		bad := bad
		badReq := f.appendReq(session.ID, run.ID, testUserEntry(fmt.Sprintf("bad%d", bad), "x"))
		badReq.ConsumeInterventionsThrough = &bad
		_, err := f.store.AppendAgentSessionEntries(f.ctx, badReq)
		expectErr(f.t, fmt.Sprintf("cursor %d", bad), err, ErrInvalidSessionEntry)
		f.assertNoEntry(fmt.Sprintf("bad%d", bad))
	}
	// The same value writes nothing on the Run row.
	if _, err := f.store.AppendAgentSessionEntries(f.ctx, cursorOnly); err != nil {
		f.t.Fatal(err)
	}
	if got := f.run(run.ID); got.ConsumedInterventions != 2 || got.Revision != revision {
		f.t.Fatalf("unchanged cursor: consumed=%d revision=%d (was %d)", got.ConsumedInterventions, got.Revision, revision)
	}
}

func conformSessionMovedRejected(f *sessionConformance) {
	runA, session := f.conversationRun("agent-a")
	// A Run bound to another session (here agent-b's lane) cannot open or
	// append to agent-a's, even under a valid lease.
	runB, sessionB := f.conversationRun("agent-b")
	_, err := f.store.OpenConversationAgentSession(f.ctx, f.openRequest(runB.ID, "agent-a"))
	expectErr(f.t, "open another lane", err, ErrSessionMoved)
	_, err = f.append(session.ID, runB.ID, testUserEntry("x1", "x"))
	expectErr(f.t, "append to another session", err, ErrSessionMoved)
	f.assertNoEntry("x1")
	_, err = f.store.CreateRunAgentSession(f.ctx, CreateRunAgentSessionRequest{
		Scope: f.scope, OwnerRunID: runA.ID, AgentID: "agent-a", Lease: f.lease(), Now: f.now,
	})
	expectErr(f.t, "create a run session for a bound run", err, ErrSessionMoved)

	// A Run whose binding moved (promotion) gets ErrSessionMoved on its old session.
	_, childSession := f.forkedChild(session.ID, 0)
	f.updateRun(runA.ID, func(r *AgentRun) { r.SessionID = childSession.ID })
	_, err = f.append(session.ID, runA.ID, testUserEntry("x2", "x"))
	expectErr(f.t, "append after the binding moved", err, ErrSessionMoved)
	_, err = f.store.OpenConversationAgentSession(f.ctx, f.openRequest(runA.ID, "agent-a"))
	expectErr(f.t, "open after the binding moved", err, ErrSessionMoved)
	if got := f.session(session.ID).NextSequence; got != 1 {
		f.t.Fatalf("moved appends wrote rows")
	}
	f.mustAppend(sessionB.ID, runB.ID, testUserEntry("b1", "own session"))
}

func conformTerminalRunNoteWithoutLease(f *sessionConformance) {
	run, session := f.conversationRun("agent-a")
	note := noteEntry("n1", NoteRunFailed)
	note.SourceKey = FailureNoteSourceKey(run.ID, "s1")
	req := AppendAgentSessionEntriesRequest{Scope: f.scope, SessionID: session.ID, RunID: run.ID, Entries: []*SessionEntry{note}, Now: f.now}
	_, err := f.store.AppendAgentSessionEntries(f.ctx, req)
	expectErr(f.t, "lease-less note on a running run", err, ErrLeaseLost)
	f.setStatus(run.ID, AgentRunStatusFailed)

	userReq := req
	userReq.Entries = []*SessionEntry{testUserEntry("u1", "x")}
	_, err = f.store.AppendAgentSessionEntries(f.ctx, userReq)
	expectErr(f.t, "lease-less user entry", err, ErrInvalidSessionEntry)

	result, err := f.store.AppendAgentSessionEntries(f.ctx, req)
	if err != nil || idsOf(result.Entries) != "n1" || result.Entries[0].Sequence != 1 {
		f.t.Fatalf("lease-less note on a failed run = %+v, %v", result, err)
	}
	replay, err := f.store.AppendAgentSessionEntries(f.ctx, req)
	if err != nil || !replay.Replayed {
		f.t.Fatalf("lease-less note replay = %+v, %v", replay, err)
	}
	// The terminal Run is still fenced by its session binding.
	other := f.createRun("agent-a", "elsewhere")
	f.setStatus(other.ID, AgentRunStatusFailed)
	otherReq := req
	otherReq.RunID = other.ID
	otherReq.Entries = []*SessionEntry{noteEntry("n2", NoteRunFailed)}
	_, err = f.store.AppendAgentSessionEntries(f.ctx, otherReq)
	expectErr(f.t, "lease-less note into another session", err, ErrSessionMoved)
	f.assertNoEntry("n2")
}
