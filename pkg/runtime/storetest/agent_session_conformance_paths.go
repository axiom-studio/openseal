package storetest

import (
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

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
