package conversationrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// Final answer delivery of chat Runs (run-loop section 05 §8, CONV-08), on
// the SQLite store (Postgres with OPENSEAL_TEST_POSTGRES_DSN).

const finalDeliveryWorker = "final-worker"

type finalDeliveryStore interface {
	conversationtest.ThreadStartStore
	models.AgentSessionStore
	models.ArtifactStore
}

// fakeActionCalls is the governed call store the handler reads; tool
// entries reference its calls by id.
type fakeActionCalls struct {
	mu    sync.Mutex
	calls map[string]*models.ActionCall
}

func (a *fakeActionCalls) GetActionCall(_ context.Context, _ models.Scope, id string) (*models.ActionCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call, ok := a.calls[id]
	if !ok {
		return nil, models.ErrActionNotFound
	}
	copied := *call
	return &copied, nil
}

type finalDeliveryFixture struct {
	t            *testing.T
	ctx          context.Context
	store        finalDeliveryStore
	service      *runtimeconversation.ConversationService
	conversation *models.Conversation
	trigger      *models.ChannelMessage
	run          *models.AgentRun
	session      *models.AgentSession
	runner       *ConversationRunTurnRunner
	actions      *fakeActionCalls
	handler      *conversationFinalHandler
	content      *visualTestContent
	now          time.Time
	steps        int
}

type finalDeliveryOptions struct {
	team          bool
	responseMode  string
	participation bool // RequireParticipationOptIn with participation disabled
	coordinator   func(*testing.T, *runtimeconversation.ConversationService) *runtimeconversation.ConversationCoordinator
	visualChecks  func(*models.AgentRun) presentation.ValidateOptions
}

func forFinalDeliveryStores(t *testing.T, options finalDeliveryOptions, test func(*testing.T, *finalDeliveryFixture)) {
	t.Helper()
	conversationtest.ForThreadStartStores(t, func(t *testing.T, base conversationtest.ThreadStartStore, _ func(*testing.T) conversationtest.ThreadStartStore) {
		test(t, newFinalDeliveryFixture(t, base, options))
	})
}

func newFinalDeliveryFixture(t *testing.T, base conversationtest.ThreadStartStore, options finalDeliveryOptions) *finalDeliveryFixture {
	t.Helper()
	store, ok := base.(finalDeliveryStore)
	if !ok {
		t.Fatalf("%T is not an agent session store", base)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := models.Scope{Kind: "tenant", ID: "final-" + uuid.NewString()[:8]}
	service := runtimeconversation.NewConversationService(store)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	if options.team {
		owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-1"}
	}
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Final answers", IdempotencyKey: "final-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-1"}, Intent: models.MessageIntentQuestion,
		Content: "Can you pull the quarterly report?", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		ResponseMode: options.responseMode, IdempotencyKey: "trigger",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := posted.Message
	if conversation, err = service.GetConversation(ctx, scope, conversation.ID); err != nil {
		t.Fatal(err)
	}
	request := runtimeconversation.ConversationAgentRunRequest(conversation, trigger)
	if options.team {
		request.AssignedAgentID = "agent-1"
		request.ConcurrencyKey = models.ConversationRunLane(conversation.ID, "", "agent-1")
		request.SessionID = models.ConversationSessionID(scope, request.ConcurrencyKey)
		request.IdempotencyKey = "member-" + trigger.ID
	}
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, request)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: finalDeliveryWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	opened, err := store.OpenConversationAgentSession(ctx, models.OpenConversationAgentSessionRequest{
		Scope: scope, ConversationID: conversation.ID, AgentID: "agent-1", RunID: run.ID,
		Lease: models.AgentRunLeaseGuard{WorkerID: finalDeliveryWorker, Now: now}, Now: now,
	})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	coordinator := newTestCoordinator
	if options.coordinator != nil {
		coordinator = options.coordinator
	}
	content := &visualTestContent{}
	runner, err := NewConversationRunTurnRunner(store, coordinator(t, service), ConversationRunTurnRunnerConfig{
		AttachmentContent: content, RequireParticipationOptIn: options.participation, VisualChecks: options.visualChecks,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := &fakeActionCalls{calls: map[string]*models.ActionCall{}}
	return &finalDeliveryFixture{
		t: t, ctx: ctx, store: store, service: service, conversation: conversation, trigger: trigger, run: run,
		session: opened.Session, runner: runner, actions: actions, content: content, now: now,
		handler: newConversationFinalHandler(runner, store, actions, store),
	}
}

func (f *finalDeliveryFixture) lease() *models.AgentRunLeaseGuard {
	return &models.AgentRunLeaseGuard{WorkerID: finalDeliveryWorker, Now: f.now}
}

func (f *finalDeliveryFixture) currentRun() *models.AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.run.Scope, f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// mutateRun rewrites the Run outside the loop (a follow-up delivery, the
// failure transition).
func (f *finalDeliveryFixture) mutateRun(mutate func(*models.AgentRun)) {
	f.t.Helper()
	current := f.currentRun()
	updated := models.CloneAgentRun(current)
	mutate(updated)
	updated.Revision++
	updated.UpdatedAt = f.now
	event := &models.ActivityEvent{
		ID: "mutate-" + uuid.NewString()[:8], Scope: current.Scope, EventType: "run.updated", Severity: models.ActivitySeverityInfo,
		RunID: current.ID, Actor: models.ActivityActor{Type: "worker", ID: finalDeliveryWorker}, Summary: "Run updated",
		Visibility: models.ActivityVisibilityScope, CreatedAt: f.now,
	}
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, event, nil); err != nil {
		f.t.Fatal(err)
	}
}

// step appends one assistant entry with these tool calls as pending entries
// and returns the entries by call id.
func (f *finalDeliveryFixture) step(calls ...agentrun.LoopToolCall) (string, map[string]*models.SessionEntry) {
	f.t.Helper()
	f.steps++
	stepID := "step-" + string(rune('0'+f.steps))
	assistant := &models.SessionEntry{
		ID: models.SessionEntryID(f.session.ID, models.AssistantEntryKey(stepID)), RunID: f.run.ID, StepID: stepID, Step: int64(f.steps),
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls},
	}
	batch := []*models.SessionEntry{assistant}
	for _, call := range calls {
		assistant.Assistant.ToolCalls = append(assistant.Assistant.ToolCalls, models.ModelToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		batch = append(batch, &models.SessionEntry{
			ID: models.SessionEntryID(f.session.ID, models.ToolEntryKey(stepID, call.ID)), RunID: f.run.ID, StepID: stepID, Step: int64(f.steps),
			Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: call.ID,
			Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: models.ToolSideEffectWrite},
		})
	}
	appended, err := f.store.AppendAgentSessionEntries(f.ctx, models.AppendAgentSessionEntriesRequest{
		Scope: f.run.Scope, SessionID: f.session.ID, RunID: f.run.ID, Lease: f.lease(), Entries: batch, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("append step: %v", err)
	}
	entries := map[string]*models.SessionEntry{}
	for _, entry := range appended.Entries {
		if entry.Type == models.SessionEntryTool {
			entries[entry.ToolCallID] = entry
		}
	}
	return stepID, entries
}

func (f *finalDeliveryFixture) settle(entry *models.SessionEntry, to models.ToolEntryState, actionCallID string, text string) {
	f.t.Helper()
	status, isError := models.ToolResultSucceeded, false
	if to == models.ToolEntryError {
		status, isError = models.ToolResultFailed, true
	}
	_, err := f.store.TransitionToolEntry(f.ctx, models.ToolEntryTransitionRequest{
		Scope: f.run.Scope, EntryID: entry.ID, RunID: f.run.ID, Lease: *f.lease(),
		From: models.ToolEntryPending, To: to, ActionCallID: actionCallID,
		Result: &models.ToolResult{Status: status, IsError: isError, Content: []models.ContentPart{storetest.TextPart(text)}}, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("settle %s: %v", entry.ID, err)
	}
}

// governed records a governed tool call of the Run with this status and output.
func (f *finalDeliveryFixture) governed(callID string, status models.ActionCallStatus, output map[string]interface{}) string {
	f.t.Helper()
	_, entries := f.step(agentrun.LoopToolCall{ID: callID, Name: "reports__export", Arguments: json.RawMessage(`{}`)})
	actionID := "action-" + callID
	f.actions.mu.Lock()
	f.actions.calls[actionID] = &models.ActionCall{ID: actionID, Scope: f.run.Scope, RunID: f.run.ID, Status: status, Output: output}
	f.actions.mu.Unlock()
	state := models.ToolEntryCompleted
	if status != models.ActionCallStatusSucceeded {
		state = models.ToolEntryError
	}
	f.settle(entries[callID], state, actionID, "done")
	return actionID
}

// finalEntry appends a pending turn_final_answer entry and returns the
// FinalAnswer the control section would hand the loop for it.
func (f *finalDeliveryFixture) finalEntry(answer string, output map[string]interface{}) (agentrun.FinalAnswer, *models.SessionEntry) {
	f.t.Helper()
	callID := "final-" + uuid.NewString()[:6]
	stepID, entries := f.step(agentrun.LoopToolCall{ID: callID, Name: string(models.ControlToolFinalAnswer), Arguments: json.RawMessage(`{}`)})
	if output == nil {
		output = map[string]interface{}{FinalOutputSummaryKey: answer}
	}
	entry := entries[callID]
	return agentrun.FinalAnswer{StepID: stepID, ToolEntryID: entry.ID, ToolCallID: callID, Answer: answer, Output: output}, entry
}

func (f *finalDeliveryFixture) deliver(answer agentrun.FinalAnswer) (agentrun.FinalDeliveryResult, error) {
	f.t.Helper()
	run := f.currentRun()
	return f.handler.Deliver(f.ctx, agentrun.FinalDelivery{Run: run, Answer: answer, Guard: models.ReplyRunGuard{
		Scope: run.Scope, RunID: run.ID, WorkerID: finalDeliveryWorker, ConcurrencyKey: run.ConcurrencyKey,
		InterventionCount: len(run.PendingInterventions),
	}})
}

func (f *finalDeliveryFixture) mustDeliver(answer agentrun.FinalAnswer, kind agentrun.FinalDeliveryKind) agentrun.FinalDeliveryResult {
	f.t.Helper()
	result, err := f.deliver(answer)
	if err != nil {
		f.t.Fatalf("deliver: %v", err)
	}
	if result.Kind != kind {
		f.t.Fatalf("deliver kind = %q (%#v), want %q", result.Kind, result, kind)
	}
	return result
}

// answers are the Agent's messages after the trigger.
func (f *finalDeliveryFixture) answers() []*models.ChannelMessage {
	f.t.Helper()
	messages, err := f.service.ListChannelMessages(f.ctx, models.ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	var answers []*models.ChannelMessage
	for _, message := range messages {
		if message.Sender.Type == models.ConversationParticipantAgent {
			answers = append(answers, message)
		}
	}
	return answers
}

func (f *finalDeliveryFixture) followUp(key, content string) *models.ChannelMessage {
	f.t.Helper()
	current, err := f.service.GetConversation(f.ctx, f.run.Scope, f.conversation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.service.PostChannelMessage(f.ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.run.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "user-1"}, Intent: models.MessageIntentQuestion,
		Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	message := posted.Message
	f.mutateRun(func(run *models.AgentRun) {
		run.PendingInterventions = append(run.PendingInterventions, models.AgentRunIntervention{
			ID: models.ConversationFollowUpInterventionID(run.Scope, message.ID), Actor: models.ActivityActor{Type: "user", ID: "user-1"},
			Instruction: content, CreatedAt: f.now,
			ConversationMessage: &models.ConversationMessageDelivery{ConversationID: f.conversation.ID, MessageID: message.ID, Sequence: message.Sequence, Sender: message.Sender},
		})
		run.ConsumedInterventions = len(run.PendingInterventions)
	})
	return message
}

func finalDeliveryVisual() *presentation.Visual {
	return &presentation.Visual{Title: "Quarterly revenue", Components: []presentation.Component{
		{ID: "revenue", Type: "stat_row", Summary: "Revenue grew 12%.", StatRow: &presentation.StatRow{Stats: []presentation.Stat{
			{Label: "Revenue", Value: 1200000, Format: presentation.StatCurrency, Currency: "USD"},
		}}},
	}}
}

// A final whose files name no saved file (a path, or a failed tool's file) goes
// back to the model: posting it would silently attach nothing.
func TestFinalAnswerSendsBackFilesThatAreNotSavedFileIDs(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		ref := func(id string, version int) map[string]interface{} {
			return map[string]interface{}{"id": id, "version": version}
		}
		f.governed("export", models.ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": "report-pdf", "version": 1, "path": "docs/report.pdf"}}})
		f.governed("failed", models.ActionCallStatusFailed, map[string]interface{}{"artifactRefs": []interface{}{ref("failed-file", 1)}})
		for _, files := range [][]interface{}{{"docs/draft.pdf"}, {"docs/report.pdf", "failed-file"}} {
			answer, _ := f.finalEntry("Here is the report.", map[string]interface{}{FinalOutputSummaryKey: "Here is the report.", runtimeconversation.FinalOutputFilesKey: files})
			result := f.mustDeliver(answer, agentrun.FinalDeliveryRetryFinal)
			if !strings.Contains(result.RejectionText, files[len(files)-1].(string)) || !strings.Contains(result.RejectionText, "docs/report.pdf") {
				t.Fatalf("files %v: rejection = %q", files, result.RejectionText)
			}
		}
		if answers := f.answers(); len(answers) != 0 {
			t.Fatalf("posted %#v", answers)
		}
	})
}

func TestFinalAnswerPostsThreadReplyWithRunAndArtifactRefs(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		ref := func(id string, version int) map[string]interface{} {
			return map[string]interface{}{"id": id, "version": version}
		}
		f.governed("export", models.ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{ref("report-pdf", 1), ref("report-csv", 2)}})
		f.governed("again", models.ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{ref("report-pdf", 1), ref(strings.Repeat("x", 257), 1), ref("zero", 0)}})
		f.governed("failed", models.ActionCallStatusFailed, map[string]interface{}{"artifactRefs": []interface{}{ref("failed-file", 1)}})
		// The answer hands over the files it names; the Run's others stay in the Library.
		answer, _ := f.finalEntry("Here is the quarterly report.", map[string]interface{}{
			FinalOutputSummaryKey: "Here is the quarterly report.", runtimeconversation.FinalOutputFilesKey: []interface{}{"report-pdf", "report-csv"}})
		result := f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 {
			t.Fatalf("answers = %#v", answers)
		}
		posted := answers[0]
		want := []models.ConversationReference{
			{Kind: models.ConversationReferenceRun, ID: f.run.ID},
			{Kind: models.ConversationReferenceArtifact, ID: "report-pdf", Version: 1},
			{Kind: models.ConversationReferenceArtifact, ID: "report-csv", Version: 2},
		}
		if posted.Content != "Here is the quarterly report." || posted.Intent != models.MessageIntentAnswer || posted.Sender.ID != "agent-1" ||
			posted.ReplyToMessageID != f.trigger.ID || posted.ResolvesMessageID != f.trigger.ID || posted.BroadcastToChannel ||
			posted.IdempotencyKey != models.ConversationRunResponseKey(f.run, f.trigger.ID) || len(posted.References) != len(want) {
			t.Fatalf("posted = %#v", posted)
		}
		for i := range want {
			if posted.References[i] != want[i] {
				t.Fatalf("references = %#v, want %#v", posted.References, want)
			}
		}
		if result.Output["conversationId"] != f.conversation.ID || result.Output["triggerMessageId"] != f.trigger.ID ||
			result.Output["messageId"] != posted.ID || result.Output["replayed"] != false || result.Output["summary"] != posted.Content {
			t.Fatalf("output = %#v", result.Output)
		}
		if _, ok := result.Output[agentrun.FinalOutputBroadcastKey]; ok {
			t.Fatalf("broadcastToChannel present without broadcast: %#v", result.Output)
		}
		// A re-delivery after a crash returns the stored reply.
		replay := f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		if replay.Output["messageId"] != posted.ID || replay.Output["replayed"] != true || len(f.answers()) != 1 {
			t.Fatalf("replay = %#v, answers = %d", replay.Output, len(f.answers()))
		}
	})
}

func TestBroadcastToChannelFlagPosted(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("Shipped to everyone.", map[string]interface{}{FinalOutputSummaryKey: "Shipped to everyone.", agentrun.FinalOutputBroadcastKey: true})
		result := f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || !answers[0].BroadcastToChannel || result.Output[agentrun.FinalOutputBroadcastKey] != true {
			t.Fatalf("answers = %#v, output = %#v", answers, result.Output)
		}
	})
}

func TestSilentOnlyForSpoken(t *testing.T) {
	silent := map[string]interface{}{agentrun.FinalOutputSilentKey: true}
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("", silent)
		result := f.mustDeliver(answer, agentrun.FinalDeliveryRetryFinal)
		if result.RejectionText != conversationSilentNotSpokenText || len(f.answers()) != 0 {
			t.Fatalf("result = %#v, answers = %d", result, len(f.answers()))
		}
	})
	forFinalDeliveryStores(t, finalDeliveryOptions{responseMode: "spoken"}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("", silent)
		result := f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		if result.Output[agentrun.FinalOutputSilentKey] != true || result.Output["conversationId"] != f.conversation.ID ||
			result.Output["triggerMessageId"] != f.trigger.ID || len(f.answers()) != 0 {
			t.Fatalf("output = %#v, answers = %d", result.Output, len(f.answers()))
		}
	})
}

func TestReactionOnlyCompletionPostsNoMessage(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		args, _ := json.Marshal(runtimeconversation.TurnReaction{MessageID: f.trigger.ID, Emoji: "👍"})
		_, entries := f.step(agentrun.LoopToolCall{ID: "react", Name: string(models.ControlToolReact), Arguments: args})
		f.settle(entries["react"], models.ToolEntryCompleted, "", `{"status":"added"}`)
		// The control section's reactions-only final.
		reacted, _ := f.finalEntry("", map[string]interface{}{agentrun.FinalOutputReactedKey: true})
		result := f.mustDeliver(reacted, agentrun.FinalDeliveryComplete)
		if result.Output[agentrun.FinalOutputReactedKey] != true || result.Output["triggerMessageId"] != f.trigger.ID {
			t.Fatalf("output = %#v", result.Output)
		}
		// An answer that only repeats the emoji says nothing more.
		echo, _ := f.finalEntry(" 👍️ ", nil)
		if result := f.mustDeliver(echo, agentrun.FinalDeliveryComplete); result.Output[agentrun.FinalOutputReactedKey] != true {
			t.Fatalf("echo output = %#v", result.Output)
		}
		if answers := f.answers(); len(answers) != 0 {
			t.Fatalf("a reaction-only reply posted %#v", answers)
		}
		// Words beyond the emoji are a reply.
		words, _ := f.finalEntry("👍 On it, report coming up.", nil)
		f.mustDeliver(words, agentrun.FinalDeliveryComplete)
		if answers := f.answers(); len(answers) != 1 {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestEmptyAnswerRetriesOnceThenFails(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		first, entry := f.finalEntry("  ", nil)
		result := f.mustDeliver(first, agentrun.FinalDeliveryRetryFinal)
		if result.RejectionText != conversationEmptyAnswerText {
			t.Fatalf("result = %#v", result)
		}
		// The loop records the rejection on the final entry (rejectDeliveredFinal).
		f.settle(entry, models.ToolEntryError, "", result.RejectionText)
		second, _ := f.finalEntry("", nil)
		result = f.mustDeliver(second, agentrun.FinalDeliveryFail)
		if result.Failure == nil || result.Failure.Message != conversationEmptyAnswerFailure || result.Failure.Code != conversationEmptyAnswerCode {
			t.Fatalf("failure = %#v", result.Failure)
		}
		if len(f.answers()) != 0 {
			t.Fatalf("an empty answer was posted")
		}
	})
}

func TestVisualStoredPerFollowUp(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		output := func(answer string) map[string]interface{} {
			return map[string]interface{}{FinalOutputSummaryKey: answer, agentrun.FinalOutputVisualKey: finalDeliveryVisual()}
		}
		first, _ := f.finalEntry("Revenue is up.", output("Revenue is up."))
		result := f.mustDeliver(first, agentrun.FinalDeliveryComplete)
		if _, ok := result.Output[agentrun.FinalOutputVisualKey]; ok {
			t.Fatalf("the visual stayed in the Run output: %#v", result.Output)
		}
		firstID := answerVisualArtifactID(f.run.ID, f.trigger.ID, "")
		follow := f.followUp("follow-up", "And last quarter?")
		second, _ := f.finalEntry("Last quarter was flat.", output("Last quarter was flat."))
		f.mustDeliver(second, agentrun.FinalDeliveryComplete)
		secondID := answerVisualArtifactID(f.run.ID, f.trigger.ID, "followup:"+follow.ID)
		answers := f.answers()
		if len(answers) != 2 || firstID == secondID {
			t.Fatalf("answers = %#v", answers)
		}
		for i, id := range []string{firstID, secondID} {
			refs := answers[i].References
			if len(refs) != 2 || refs[1] != (models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: id, Version: 1}) {
				t.Fatalf("answer %d references = %#v, want visual %s", i, refs, id)
			}
			artifact, err := f.store.GetArtifact(f.ctx, f.run.Scope, id, 1)
			if err != nil || artifact.Type != presentation.ArtifactType || artifact.Provenance.RunID != f.run.ID {
				t.Fatalf("visual %s = %#v, %v", id, artifact, err)
			}
		}
		if answers[0].IdempotencyKey == answers[1].IdempotencyKey {
			t.Fatalf("the follow-up answer reused the first answer's key")
		}
	})
}

func TestVisualFailingHostChecksRetriesOnceThenPostsWithout(t *testing.T) {
	refuse := func(*models.AgentRun) presentation.ValidateOptions {
		return presentation.ValidateOptions{Bindings: presentation.BindingResolverFunc(func(context.Context, presentation.Binding) error {
			return errors.New("not in this chat")
		})}
	}
	forFinalDeliveryStores(t, finalDeliveryOptions{visualChecks: refuse}, func(t *testing.T, f *finalDeliveryFixture) {
		bound := finalDeliveryVisual()
		bound.Components = append(bound.Components, presentation.Component{ID: "next", Type: "actions", Actions: []presentation.Action{
			{ID: "stop", Label: "Stop", Kind: presentation.ActionEntity, Command: "stop", Binding: &presentation.Binding{Kind: presentation.BindingRunbook, ID: "rb-1"}},
		}})
		first, entry := f.finalEntry("Revenue is up.", map[string]interface{}{FinalOutputSummaryKey: "Revenue is up.", agentrun.FinalOutputVisualKey: bound})
		result := f.mustDeliver(first, agentrun.FinalDeliveryRetryFinal)
		if !strings.HasPrefix(result.RejectionText, conversationVisualRejectedText) || len(f.answers()) != 0 {
			t.Fatalf("result = %#v", result)
		}
		f.settle(entry, models.ToolEntryError, "", result.RejectionText)
		second, _ := f.finalEntry("Revenue is up.", map[string]interface{}{FinalOutputSummaryKey: "Revenue is up.", agentrun.FinalOutputVisualKey: bound})
		f.mustDeliver(second, agentrun.FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || len(answers[0].References) != 1 {
			t.Fatalf("answers = %#v", answers)
		}
		events, err := f.store.ListActivity(f.ctx, models.ActivityFilter{Scope: f.run.Scope, RunID: f.run.ID, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		rejected := 0
		for _, event := range events {
			if event.EventType == answerVisualRejectedEvent {
				rejected++
			}
		}
		if rejected != 1 {
			t.Fatalf("visual_rejected events = %d", rejected)
		}
	})
}

func TestNoDeterministicOperationReply(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		f.governed("operation", models.ActionCallStatusSucceeded, map[string]interface{}{"summary": "Runbook operation completed", "resourceId": "rb-1"})
		answer, _ := f.finalEntry("I paused the weekly digest; it will not send on Monday.", nil)
		f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || answers[0].Content != "I paused the weekly digest; it will not send on Monday." {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestUnconsumedInterventionBlocksPostAndContinues(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("Here is the report.", nil)
		run := f.currentRun()
		delivery := agentrun.FinalDelivery{Run: run, Answer: answer, Guard: models.ReplyRunGuard{
			Scope: run.Scope, RunID: run.ID, WorkerID: finalDeliveryWorker, ConcurrencyKey: run.ConcurrencyKey,
			InterventionCount: len(run.PendingInterventions),
		}}
		// A message arrives after the loop's D8 check, before the post.
		f.followUp("late", "Actually, only Q3 please.")
		if _, err := f.handler.Deliver(f.ctx, delivery); !errors.Is(err, models.ErrSteeringArrived) {
			t.Fatalf("deliver = %v, want ErrSteeringArrived", err)
		}
		if len(f.answers()) != 0 {
			t.Fatalf("a superseded answer was posted")
		}
		// The next answer, which saw the message, posts under its own key.
		next, _ := f.finalEntry("Here is Q3.", nil)
		f.mustDeliver(next, agentrun.FinalDeliveryComplete)
		if answers := f.answers(); len(answers) != 1 || answers[0].Content != "Here is Q3." {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestTeamHandoffMentionsFromFinalAnswer(t *testing.T) {
	team := func(t *testing.T, service *runtimeconversation.ConversationService) *runtimeconversation.ConversationCoordinator {
		t.Helper()
		participants := runtimeconversation.ConversationParticipantSourceFunc(func(context.Context, runtimeconversation.ConversationParticipantQuery) ([]runtimeconversation.ConversationParticipantBinding, error) {
			return []runtimeconversation.ConversationParticipantBinding{
				{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent-1"}},
				{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent-2"}},
			}, nil
		})
		router := runtimeconversation.ConversationRouterFunc(func(context.Context, runtimeconversation.ConversationRouteRequest) (runtimeconversation.ConversationRouteDecision, error) {
			return runtimeconversation.ConversationRouteDecision{AgentID: "agent-1"}, nil
		})
		coordinator, err := runtimeconversation.NewConversationCoordinator(service, participants, router, runtimeconversation.DefaultConversationCoordinatorConfig())
		if err != nil {
			t.Fatal(err)
		}
		return coordinator
	}
	forFinalDeliveryStores(t, finalDeliveryOptions{team: true, coordinator: team}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("@agent-2 can you check the ad spend numbers?", nil)
		f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || len(answers[0].Mentions) != 1 || answers[0].Mentions[0].ID != "agent-2" {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestOTPAnswerIsPostedThenParks(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("What is the code the bank just sent you?", nil)
		answer.Park = &agentrun.FinalPark{Deliver: true, Park: agentrun.LoopPark{Status: models.AgentRunStatusWaitingForEvent, Wake: &models.WakeCondition{Type: "user_message"}}}
		result := f.mustDeliver(answer, agentrun.FinalDeliveryPark)
		answers := f.answers()
		if len(answers) != 1 || answers[0].Content != answer.Answer || result.Output["messageId"] != answers[0].ID {
			t.Fatalf("answers = %#v, output = %#v", answers, result.Output)
		}
	})
}

func TestFailureExplanationIsPostedAndFails(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		f.mutateRun(func(run *models.AgentRun) {
			run.FailureExplanation = &models.RunFailureExplanation{Kind: runtimeconversation.RunFailureKindExecution, Code: "execution_failed", Message: "The export service was unavailable."}
		})
		answer, _ := f.finalEntry("I couldn't export the report: the export service was down.",
			map[string]interface{}{FinalOutputSummaryKey: "x", agentrun.FinalOutputVisualKey: finalDeliveryVisual()})
		result := f.mustDeliver(answer, agentrun.FinalDeliveryFail)
		answers := f.answers()
		if result.Failure != nil || len(answers) != 1 || answers[0].Content != answer.Answer || len(answers[0].References) != 1 ||
			result.Output["summary"] != answer.Answer || result.Output["messageId"] != answers[0].ID {
			t.Fatalf("result = %#v, answers = %#v", result, answers)
		}
	})
	// An empty explanation posts the kernel's reply.
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		f.mutateRun(func(run *models.AgentRun) {
			run.FailureExplanation = &models.RunFailureExplanation{Kind: runtimeconversation.RunFailureKindExecution, Code: "execution_failed", Message: "The export service was unavailable."}
		})
		answer, _ := f.finalEntry("", map[string]interface{}{agentrun.FinalOutputSilentKey: true})
		f.mustDeliver(answer, agentrun.FinalDeliveryFail)
		answers := f.answers()
		if len(answers) != 1 || answers[0].Content != runtimeconversation.TerminalFailureReply("execution_failed") {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestParticipationStoppedPostsNothing(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{participation: true}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("Here is the report.", nil)
		result := f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		if result.Output["participationSkipped"] != true || len(f.answers()) != 0 {
			t.Fatalf("output = %#v, answers = %d", result.Output, len(f.answers()))
		}
	})
}

func TestKernelFinalWithoutAnswerKeepsItsOutput(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		output := map[string]interface{}{"approvalReplaced": true, "replacedByMessageId": "m-2"}
		result := f.mustDeliver(agentrun.FinalAnswer{StepID: "step-k", Output: output}, agentrun.FinalDeliveryComplete)
		if result.Output["approvalReplaced"] != true || len(f.answers()) != 0 {
			t.Fatalf("output = %#v, answers = %d", result.Output, len(f.answers()))
		}
	})
}
