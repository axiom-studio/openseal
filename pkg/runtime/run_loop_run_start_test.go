package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// runStartActions serves ListActionCalls from a fixed set; every other
// ActionStore method is unused by the preparer.
type runStartActions struct {
	ActionStore
	lister fakeActionCallLister
}

func (a *runStartActions) ListActionCalls(ctx context.Context, filter ActionFilter) ([]*ActionCall, error) {
	return a.lister.ListActionCalls(ctx, filter)
}

func TestRunStartNoteIdempotent(t *testing.T) {
	// Governed-history notes: rate-limited source and pending setup.
	g := newGuardFixture(t)
	g.rateLimitedLightpanda("read-429")
	g.add("setup-1", SkillManagementSkillID, agentrun.SkillActionRequestSetup, ActionCallStatusSucceeded, map[string]interface{}{
		"setupRequest": map[string]interface{}{"id": "form-1", "status": "pending", "runId": g.run.ID, "actionCallId": "setup-1"},
	}, "", nil)
	actions := &runStartActions{}
	for _, call := range g.calls {
		actions.lister.calls = append(actions.lister.calls, call)
	}
	session := &AgentSession{ID: g.run.SessionID, Scope: g.run.Scope}
	step := &AgentTurn{ID: "step-9", Scope: g.run.Scope, RunID: g.run.ID, Sequence: 9}
	prepare := func(run *AgentRun, path *AgentSessionPath) []*SessionEntry {
		t.Helper()
		out, err := newRunStartPreparer(newTestKernelStore(t), actions).BeforeStep(context.Background(),
			agentrun.StepInput{Run: run, Step: step, Session: session, Path: path}, agentrun.SteeringPlan{})
		if err != nil {
			t.Fatalf("before step: %v", err)
		}
		if out == nil {
			return nil
		}
		return out.Entries
	}
	notes := prepare(g.run, g.path)
	if len(notes) != 2 || notes[0].Note.Kind != NoteRateLimitedSource || notes[1].Note.Kind != NoteSetupPending {
		t.Fatalf("governed notes = %+v", notes)
	}
	if notes[0].SourceKey != RateLimitedSourceKey(g.run.ID, "read-429") || notes[0].Note.Text != LiveBrowserRateLimitedError {
		t.Fatalf("rate-limited note = %+v", notes[0].Note)
	}
	// The setup note is the same entry the governed executor appends.
	setupEntry := g.path.Entries[len(g.path.Entries)-1]
	executorNote := PendingSkillSetupNote(g.run, session.ID, setupEntry, g.calls["setup-1"], nil)
	if executorNote == nil || notes[1].ID != executorNote.ID || notes[1].SourceKey != executorNote.SourceKey ||
		notes[1].Note.Text != executorNote.Note.Text || string(notes[1].Note.Data) != string(executorNote.Note.Data) {
		t.Fatalf("setup note %+v differs from executor note %+v", notes[1], executorNote)
	}
	if calls := actions.lister.pages; calls != 1 {
		t.Fatalf("history pages read = %d", calls)
	}

	// Once on the path, neither comes again.
	withNotes := &AgentSessionPath{Entries: append(append([]*SessionEntry{}, g.path.Entries...), notes...)}
	if got := prepare(g.run, withNotes); len(got) != 0 {
		t.Fatalf("notes repeated: %+v", got)
	}
	// A step that ran no governed call since the newest assistant entry reads
	// no history.
	pages := actions.lister.pages
	quiet := &AgentSessionPath{Entries: append(append([]*SessionEntry{}, g.path.Entries...),
		&SessionEntry{ID: "assistant-10", RunID: g.run.ID, Type: SessionEntryAssistant})}
	if got := prepare(g.run, quiet); len(got) != 0 || actions.lister.pages != pages {
		t.Fatalf("quiet step: notes %+v, pages %d -> %d", got, pages, actions.lister.pages)
	}
	// Background task Runs park at the final answer instead of a setup note.
	task := cloneAgentRun(g.run)
	task.Kind = RunKindAgentWork
	task.Context = map[string]interface{}{ConversationTaskContextKey: map[string]interface{}{"taskId": "t1"}}
	if got := prepare(task, g.path); len(got) != 1 || got[0].Note.Kind != NoteRateLimitedSource {
		t.Fatalf("task Run notes = %+v", got)
	}
	// A newer Lightpanda success clears the rate limit.
	g.add("read-ok", LightpandaBrowserSkillID, "lightpanda-read", ActionCallStatusSucceeded, nil, "", nil)
	actions.lister.calls = append(actions.lister.calls, g.calls["read-ok"])
	if got := prepare(g.run, g.path); len(got) != 1 || got[0].Note.Kind != NoteSetupPending {
		t.Fatalf("after success notes = %+v", got)
	}
}
