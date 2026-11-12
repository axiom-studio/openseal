package runloop

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// steeringRun stores run r1 (bound to a conversation session) with the given
// interventions and cursor, and returns the stored Run and session.
func steeringRun(t *testing.T, store *sqlite.Store, consumed int, interventions ...models.AgentRunIntervention) (*models.AgentRun, *models.AgentSession) {
	t.Helper()
	session := openLoopConversationSession(t, store, "r1")
	run := getLoopRun(t, store, loopSessionScope, "r1")
	run.PendingInterventions = interventions
	run.ConsumedInterventions = consumed
	putLoopRun(t, store, run)
	return loopStoredSessionRun(t, store, "r1"), session
}

func addSteeringIntervention(t *testing.T, store *sqlite.Store, intervention models.AgentRunIntervention) *models.AgentRun {
	t.Helper()
	run := getLoopRun(t, store, loopSessionScope, "r1")
	run.PendingInterventions = append(run.PendingInterventions, intervention)
	putLoopRun(t, store, run)
	return loopStoredSessionRun(t, store, "r1")
}

func steeringStep() *models.AgentTurn { return &models.AgentTurn{ID: "step-3", Sequence: 3} }

func steeringInput(run *models.AgentRun, session *models.AgentSession) agentrun.StepInput {
	return agentrun.StepInput{Run: run, Step: steeringStep(), Session: session, WorkerID: "w1", Mode: agentrun.StepModeNormal}
}

func steeringEntries(t *testing.T, store *sqlite.Store, sessionID string) []*models.SessionEntry {
	t.Helper()
	entries, err := store.ListAgentSessionEntries(context.Background(), models.SessionEntryFilter{Scope: loopSessionScope, SessionID: sessionID})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	return entries
}

type steeringPreparer struct {
	calls int
	plans []agentrun.SteeringPlan
	fn    func(in agentrun.StepInput, plan agentrun.SteeringPlan) *agentrun.StepContextAppend
}

func (p *steeringPreparer) BeforeStep(_ context.Context, in agentrun.StepInput, plan agentrun.SteeringPlan) (*agentrun.StepContextAppend, error) {
	p.calls++
	p.plans = append(p.plans, plan)
	if p.fn == nil {
		return nil, nil
	}
	return p.fn(in, plan), nil
}

func mixedSteeringPrefix() []models.AgentRunIntervention {
	return []models.AgentRunIntervention{
		{ID: "old", Instruction: "already consumed"},
		{ID: "card", Instruction: "card settled", ActionCallID: "call-1"},
		{ID: "answer", Instruction: "yes", ConversationAnswer: &models.ConversationAnswerReceipt{AnswerMessageID: "m-answer"}},
		{ID: "msg", Instruction: "also check the invoice", ConversationMessage: &models.ConversationMessageDelivery{
			ConversationID: "c1", MessageID: "m7", Sequence: 7, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "u1"},
		}},
		{ID: "steer", Instruction: "use the staging project", Actor: models.ActivityActor{Type: "user", ID: "u2"}},
		{ID: "setup", Instruction: "setup done", Actor: models.ActivityActor{Type: "system", ID: "kernel"},
			SkillSetupResolution: &models.SkillSetupResolutionReceipt{RequestID: "req-1", Status: "completed"}},
	}
}

func TestSteeringMixedPrefixConsumesEachKindOnce(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, session := steeringRun(t, store, 1, mixedSteeringPrefix()...)
	preparer := &steeringPreparer{}
	result, err := appendStepContext(context.Background(), store, steeringInput(run, session), []agentrun.StepPreparer{preparer}, loopSessionNow)
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
	if message.User.Origin != models.UserEntryChannelMessage || message.SourceKey != models.ChannelMessageSourceKey("m7") ||
		message.User.MessageID != "m7" || message.User.MessageSequence != 7 || message.User.InterventionID != "msg" ||
		message.User.Author != (models.SessionAuthor{Type: "user", ID: "u1"}) || message.User.Content[0].Text != "also check the invoice" {
		t.Fatalf("message entry = %+v / %+v", message, message.User)
	}
	if steer.User.Origin != models.UserEntryIntervention || steer.SourceKey != models.InterventionSourceKey("steer") ||
		steer.ID != models.SessionEntryID(session.ID, models.InterventionSourceKey("steer")) || steer.Step != 3 ||
		steer.User.Author != (models.SessionAuthor{Type: "user", ID: "u2"}) {
		t.Fatalf("steer entry = %+v / %+v", steer, steer.User)
	}
	if setup.User.Author.Type != "service" || len(setup.User.Content) != 2 || setup.User.Content[1].Type != models.ContentPartJSON {
		t.Fatalf("setup entry = %+v", setup.User)
	}
	after := loopStoredSessionRun(t, store, "r1")
	if after.ConsumedInterventions != 6 || after.Revision != run.Revision+1 {
		t.Fatalf("run cursor = %d revision %d, want 6 and %d", after.ConsumedInterventions, after.Revision, run.Revision+1)
	}

	// The next step sees nothing to consume and writes nothing.
	result, err = appendStepContext(context.Background(), store, steeringInput(after, session), []agentrun.StepPreparer{preparer}, loopSessionNow)
	if err != nil || result.Append != nil || result.CursorMoved || len(result.Steering.Prefix) != 0 {
		t.Fatalf("second step = %+v, %v", result, err)
	}
	if got := len(steeringEntries(t, store, session.ID)); got != 3 {
		t.Fatalf("entries after second step = %d", got)
	}
}

func TestSteeringMessageCoveredByCatchUpIsAppendedOnce(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, session := steeringRun(t, store, 0, mixedSteeringPrefix()[3])
	catchUp := &steeringPreparer{fn: func(in agentrun.StepInput, plan agentrun.SteeringPlan) *agentrun.StepContextAppend {
		expected := in.Session.ChannelWatermark
		return &agentrun.StepContextAppend{
			Entries: []*models.SessionEntry{{
				ID: models.SessionEntryID(in.Session.ID, models.ChannelMessageSourceKey("m7")), Type: models.SessionEntryUser, SourceKey: models.ChannelMessageSourceKey("m7"),
				User: &models.UserEntry{Origin: models.UserEntryChannelMessage, MessageID: "m7", MessageSequence: 7,
					Author: models.SessionAuthor{Type: "user", ID: "u1"}, Content: []models.ContentPart{storetest.TextPart("[u1] also check the invoice")}},
			}},
			ExpectedChannelWatermark: &expected, ChannelWatermark: plan.ThroughMessageSequence,
		}
	}}
	result, err := appendStepContext(context.Background(), store, steeringInput(run, session), []agentrun.StepPreparer{catchUp}, loopSessionNow)
	if err != nil {
		t.Fatalf("append step context: %v", err)
	}
	entries := steeringEntries(t, store, session.ID)
	if len(entries) != 1 || entries[0].User.Content[0].Text != "[u1] also check the invoice" {
		t.Fatalf("entries = %+v, want only the catch-up entry", entries)
	}
	if result.Session.ChannelWatermark != 7 || loopStoredSessionRun(t, store, "r1").ConsumedInterventions != 1 {
		t.Fatalf("watermark %d cursor %d", result.Session.ChannelWatermark, loopStoredSessionRun(t, store, "r1").ConsumedInterventions)
	}
}

func TestSteeringCursorCommitsWithEntries(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, session := steeringRun(t, store, 0, models.AgentRunIntervention{ID: "steer", Instruction: "stop after the draft"})
	stale := int64(99)
	moving := &steeringPreparer{fn: func(agentrun.StepInput, agentrun.SteeringPlan) *agentrun.StepContextAppend {
		return &agentrun.StepContextAppend{ExpectedChannelWatermark: &stale, ChannelWatermark: 100}
	}}
	_, err := appendStepContext(context.Background(), store, steeringInput(run, session), []agentrun.StepPreparer{moving}, loopSessionNow)
	if !errors.Is(err, models.ErrChannelWatermarkMoved) {
		t.Fatalf("append with a moving watermark = %v, want ErrChannelWatermarkMoved", err)
	}
	if moving.calls != stepContextAttempts {
		t.Fatalf("preparer ran %d times, want %d", moving.calls, stepContextAttempts)
	}
	if got := loopStoredSessionRun(t, store, "r1"); got.ConsumedInterventions != 0 || got.Revision != run.Revision {
		t.Fatalf("failed append moved the cursor: %d (revision %d)", got.ConsumedInterventions, got.Revision)
	}
	if got := len(steeringEntries(t, store, session.ID)); got != 0 {
		t.Fatalf("failed append wrote %d entries", got)
	}

	// The watermark settles on the second try: entries and cursor land together.
	tries := 0
	settling := &steeringPreparer{fn: func(in agentrun.StepInput, _ agentrun.SteeringPlan) *agentrun.StepContextAppend {
		tries++
		if tries == 1 {
			return &agentrun.StepContextAppend{ExpectedChannelWatermark: &stale, ChannelWatermark: 100}
		}
		expected := in.Session.ChannelWatermark
		return &agentrun.StepContextAppend{ExpectedChannelWatermark: &expected, ChannelWatermark: 5}
	}}
	result, err := appendStepContext(context.Background(), store, steeringInput(run, session), []agentrun.StepPreparer{settling}, loopSessionNow)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if tries != 2 || len(result.Append.Entries) != 1 || result.Session.ChannelWatermark != 5 {
		t.Fatalf("tries %d result %+v", tries, result.Append)
	}
	if got := loopStoredSessionRun(t, store, "r1"); got.ConsumedInterventions != 1 || got.Revision != run.Revision+1 {
		t.Fatalf("cursor %d revision %d after commit", got.ConsumedInterventions, got.Revision)
	}
}

func TestSteeringReplayAfterCrashAppendsOnce(t *testing.T) {
	store := kerneltest.NewStore(t)
	run, session := steeringRun(t, store, 0,
		models.AgentRunIntervention{ID: "a", Instruction: "first"},
		models.AgentRunIntervention{ID: "b", Instruction: "second"})
	in := steeringInput(run, session)
	if _, err := appendStepContext(context.Background(), store, in, nil, loopSessionNow); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// The worker crashed after the commit and retries with the Run it had read.
	result, err := appendStepContext(context.Background(), store, in, nil, loopSessionNow)
	if err != nil {
		t.Fatalf("replayed append: %v", err)
	}
	if !result.Append.Replayed || !result.CursorMoved {
		t.Fatalf("replay = %+v, want a replay that reports the stale cursor", result.Append)
	}
	if got := len(steeringEntries(t, store, session.ID)); got != 2 {
		t.Fatalf("entries after replay = %d, want 2", got)
	}
	reloaded := loopStoredSessionRun(t, store, "r1")
	if reloaded.ConsumedInterventions != 2 || reloaded.Revision != run.Revision+1 {
		t.Fatalf("cursor %d revision %d after replay", reloaded.ConsumedInterventions, reloaded.Revision)
	}

	// A new intervention after the reload is the only thing the next step adds.
	reloaded = addSteeringIntervention(t, store, models.AgentRunIntervention{ID: "c", Instruction: "third"})
	if _, err := appendStepContext(context.Background(), store, steeringInput(reloaded, session), nil, loopSessionNow); err != nil {
		t.Fatalf("next step: %v", err)
	}
	entries := steeringEntries(t, store, session.ID)
	if len(entries) != 3 || entries[2].User.InterventionID != "c" {
		t.Fatalf("entries = %d, last %+v", len(entries), entries[len(entries)-1].User)
	}
	if got := loopStoredSessionRun(t, store, "r1").ConsumedInterventions; got != 3 {
		t.Fatalf("cursor = %d, want 3", got)
	}
}
