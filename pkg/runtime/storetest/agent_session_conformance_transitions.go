package storetest

import (
	"errors"
	"sync"

	"github.com/axiom-studio/openseal/pkg/models"
)

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
