package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// steeringRun stores run r1 (bound to a conversation session) with the given
// interventions and cursor, and returns the stored Run and session.
func steeringRun(t *testing.T, store *sqlite.SQLiteStore, consumed int, interventions ...AgentRunIntervention) (*AgentRun, *AgentSession) {
	t.Helper()
	session := openTestConversationSession(t, store, "r1")
	run := getStoredRun(t, store, testSessionScope, "r1")
	run.PendingInterventions = interventions
	run.ConsumedInterventions = consumed
	putTestRun(t, store, run)
	return testStoredSessionRun(t, store, "r1"), session
}

func addSteeringIntervention(t *testing.T, store *sqlite.SQLiteStore, intervention AgentRunIntervention) *AgentRun {
	t.Helper()
	run := getStoredRun(t, store, testSessionScope, "r1")
	run.PendingInterventions = append(run.PendingInterventions, intervention)
	putTestRun(t, store, run)
	return testStoredSessionRun(t, store, "r1")
}

func steeringStep() *AgentTurn { return &AgentTurn{ID: "step-3", Sequence: 3} }

func steeringInput(run *AgentRun, session *AgentSession) StepInput {
	return StepInput{Run: run, Step: steeringStep(), Session: session, WorkerID: "w1", Mode: StepModeNormal}
}

func steeringEntries(t *testing.T, store *sqlite.SQLiteStore, sessionID string) []*SessionEntry {
	t.Helper()
	entries, err := store.ListAgentSessionEntries(context.Background(), SessionEntryFilter{Scope: testSessionScope, SessionID: sessionID})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	return entries
}

type steeringPreparer struct {
	calls int
	plans []SteeringPlan
	fn    func(in StepInput, plan SteeringPlan) *StepContextAppend
}

func (p *steeringPreparer) BeforeStep(_ context.Context, in StepInput, plan SteeringPlan) (*StepContextAppend, error) {
	p.calls++
	p.plans = append(p.plans, plan)
	if p.fn == nil {
		return nil, nil
	}
	return p.fn(in, plan), nil
}

func mixedSteeringPrefix() []AgentRunIntervention {
	return []AgentRunIntervention{
		{ID: "old", Instruction: "already consumed"},
		{ID: "card", Instruction: "card settled", ActionCallID: "call-1"},
		{ID: "answer", Instruction: "yes", ConversationAnswer: &ConversationAnswerReceipt{AnswerMessageID: "m-answer"}},
		{ID: "msg", Instruction: "also check the invoice", ConversationMessage: &ConversationMessageDelivery{
			ConversationID: "c1", MessageID: "m7", Sequence: 7, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "u1"},
		}},
		{ID: "steer", Instruction: "use the staging project", Actor: ActivityActor{Type: "user", ID: "u2"}},
		{ID: "setup", Instruction: "setup done", Actor: ActivityActor{Type: "system", ID: "kernel"},
			SkillSetupResolution: &SkillSetupResolutionReceipt{RequestID: "req-1", Status: "completed"}},
	}
}

func TestSteeringMixedPrefixConsumesEachKindOnce(t *testing.T) {
	store := newTestKernelStore(t)
	run, session := steeringRun(t, store, 1, mixedSteeringPrefix()...)
	preparer := &steeringPreparer{}
	result, err := appendStepContext(context.Background(), store, steeringInput(run, session), []StepPreparer{preparer}, testSessionNow)
	if err != nil {
		t.Fatalf("append step context: %v", err)
	}
	if len(preparer.plans) != 1 || len(preparer.plans[0].Prefix) != 5 || preparer.plans[0].ThroughMessageSequence != 7 {
		t.Fatalf("preparer plans = %+v, want the 5 unconsumed interventions through message 7", preparer.plans)
	}
	if !result.CursorMoved {
		t.Fatal("cursor move not reported")
	}
	entries := steeringEntries(t, store, session.ID)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want message, steer and setup only", len(entries))
	}
	message, steer, setup := entries[0], entries[1], entries[2]
	if message.User.Origin != UserEntryChannelMessage || message.SourceKey != ChannelMessageSourceKey("m7") ||
		message.User.MessageID != "m7" || message.User.MessageSequence != 7 || message.User.InterventionID != "msg" ||
		message.User.Author != (SessionAuthor{Type: "user", ID: "u1"}) || message.User.Content[0].Text != "also check the invoice" {
		t.Fatalf("message entry = %+v / %+v", message, message.User)
	}
	if steer.User.Origin != UserEntryIntervention || steer.SourceKey != InterventionSourceKey("steer") ||
		steer.ID != SessionEntryID(session.ID, InterventionSourceKey("steer")) || steer.Step != 3 ||
		steer.User.Author != (SessionAuthor{Type: "user", ID: "u2"}) {
		t.Fatalf("steer entry = %+v / %+v", steer, steer.User)
	}
	if setup.User.Author.Type != "service" || len(setup.User.Content) != 2 || setup.User.Content[1].Type != ContentPartJSON {
		t.Fatalf("setup entry = %+v", setup.User)
	}
	after := testStoredSessionRun(t, store, "r1")
	if after.ConsumedInterventions != 6 || after.Revision != run.Revision+1 {
		t.Fatalf("run cursor = %d revision %d, want 6 and %d", after.ConsumedInterventions, after.Revision, run.Revision+1)
	}

	// The next step sees nothing to consume and writes nothing.
	result, err = appendStepContext(context.Background(), store, steeringInput(after, session), []StepPreparer{preparer}, testSessionNow)
	if err != nil || result.Append != nil || result.CursorMoved || len(result.Steering.Prefix) != 0 {
		t.Fatalf("second step = %+v, %v", result, err)
	}
	if got := len(steeringEntries(t, store, session.ID)); got != 3 {
		t.Fatalf("entries after second step = %d", got)
	}
}

func TestSteeringMessageCoveredByCatchUpIsAppendedOnce(t *testing.T) {
	store := newTestKernelStore(t)
	run, session := steeringRun(t, store, 0, mixedSteeringPrefix()[3])
	catchUp := &steeringPreparer{fn: func(in StepInput, plan SteeringPlan) *StepContextAppend {
		expected := in.Session.ChannelWatermark
		return &StepContextAppend{
			Entries: []*SessionEntry{{
				ID: SessionEntryID(in.Session.ID, ChannelMessageSourceKey("m7")), Type: SessionEntryUser, SourceKey: ChannelMessageSourceKey("m7"),
				User: &UserEntry{Origin: UserEntryChannelMessage, MessageID: "m7", MessageSequence: 7,
					Author: SessionAuthor{Type: "user", ID: "u1"}, Content: []ContentPart{storetest.TextPart("[u1] also check the invoice")}},
			}},
			ExpectedChannelWatermark: &expected, ChannelWatermark: plan.ThroughMessageSequence,
		}
	}}
	result, err := appendStepContext(context.Background(), store, steeringInput(run, session), []StepPreparer{catchUp}, testSessionNow)
	if err != nil {
		t.Fatalf("append step context: %v", err)
	}
	entries := steeringEntries(t, store, session.ID)
	if len(entries) != 1 || entries[0].User.Content[0].Text != "[u1] also check the invoice" {
		t.Fatalf("entries = %+v, want only the catch-up entry", entries)
	}
	if result.Session.ChannelWatermark != 7 || testStoredSessionRun(t, store, "r1").ConsumedInterventions != 1 {
		t.Fatalf("watermark %d cursor %d", result.Session.ChannelWatermark, testStoredSessionRun(t, store, "r1").ConsumedInterventions)
	}
}

func TestSteeringCursorCommitsWithEntries(t *testing.T) {
	store := newTestKernelStore(t)
	run, session := steeringRun(t, store, 0, AgentRunIntervention{ID: "steer", Instruction: "stop after the draft"})
	stale := int64(99)
	moving := &steeringPreparer{fn: func(StepInput, SteeringPlan) *StepContextAppend {
		return &StepContextAppend{ExpectedChannelWatermark: &stale, ChannelWatermark: 100}
	}}
	_, err := appendStepContext(context.Background(), store, steeringInput(run, session), []StepPreparer{moving}, testSessionNow)
	if !errors.Is(err, ErrChannelWatermarkMoved) {
		t.Fatalf("append with a moving watermark = %v, want ErrChannelWatermarkMoved", err)
	}
	if moving.calls != stepContextAttempts {
		t.Fatalf("preparer ran %d times, want %d", moving.calls, stepContextAttempts)
	}
	if got := testStoredSessionRun(t, store, "r1"); got.ConsumedInterventions != 0 || got.Revision != run.Revision {
		t.Fatalf("failed append moved the cursor: %d (revision %d)", got.ConsumedInterventions, got.Revision)
	}
	if got := len(steeringEntries(t, store, session.ID)); got != 0 {
		t.Fatalf("failed append wrote %d entries", got)
	}

	// The watermark settles on the second try: entries and cursor land together.
	tries := 0
	settling := &steeringPreparer{fn: func(in StepInput, _ SteeringPlan) *StepContextAppend {
		tries++
		if tries == 1 {
			return &StepContextAppend{ExpectedChannelWatermark: &stale, ChannelWatermark: 100}
		}
		expected := in.Session.ChannelWatermark
		return &StepContextAppend{ExpectedChannelWatermark: &expected, ChannelWatermark: 5}
	}}
	result, err := appendStepContext(context.Background(), store, steeringInput(run, session), []StepPreparer{settling}, testSessionNow)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if tries != 2 || len(result.Append.Entries) != 1 || result.Session.ChannelWatermark != 5 {
		t.Fatalf("tries %d result %+v", tries, result.Append)
	}
	if got := testStoredSessionRun(t, store, "r1"); got.ConsumedInterventions != 1 || got.Revision != run.Revision+1 {
		t.Fatalf("cursor %d revision %d after commit", got.ConsumedInterventions, got.Revision)
	}
}

func TestSteeringReplayAfterCrashAppendsOnce(t *testing.T) {
	store := newTestKernelStore(t)
	run, session := steeringRun(t, store, 0,
		AgentRunIntervention{ID: "a", Instruction: "first"},
		AgentRunIntervention{ID: "b", Instruction: "second"})
	in := steeringInput(run, session)
	if _, err := appendStepContext(context.Background(), store, in, nil, testSessionNow); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// The worker crashed after the commit and retries with the Run it had read.
	result, err := appendStepContext(context.Background(), store, in, nil, testSessionNow)
	if err != nil {
		t.Fatalf("replayed append: %v", err)
	}
	if !result.Append.Replayed || !result.CursorMoved {
		t.Fatalf("replay = %+v, want a replay that reports the stale cursor", result.Append)
	}
	if got := len(steeringEntries(t, store, session.ID)); got != 2 {
		t.Fatalf("entries after replay = %d, want 2", got)
	}
	reloaded := testStoredSessionRun(t, store, "r1")
	if reloaded.ConsumedInterventions != 2 || reloaded.Revision != run.Revision+1 {
		t.Fatalf("cursor %d revision %d after replay", reloaded.ConsumedInterventions, reloaded.Revision)
	}

	// A new intervention after the reload is the only thing the next step adds.
	reloaded = addSteeringIntervention(t, store, AgentRunIntervention{ID: "c", Instruction: "third"})
	if _, err := appendStepContext(context.Background(), store, steeringInput(reloaded, session), nil, testSessionNow); err != nil {
		t.Fatalf("next step: %v", err)
	}
	entries := steeringEntries(t, store, session.ID)
	if len(entries) != 3 || entries[2].User.InterventionID != "c" {
		t.Fatalf("entries = %d, last %+v", len(entries), entries[len(entries)-1].User)
	}
	if got := testStoredSessionRun(t, store, "r1").ConsumedInterventions; got != 3 {
		t.Fatalf("cursor = %d, want 3", got)
	}
}
