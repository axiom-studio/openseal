package storetest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

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
