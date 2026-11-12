package storetest

import (
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

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
