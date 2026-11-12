package runtime

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
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// Final answer delivery of chat Runs (run-loop section 05 §8, CONV-08), on
// the SQLite store (Postgres with OPENSEAL_TEST_POSTGRES_DSN).

const finalDeliveryWorker = "final-worker"

type finalDeliveryStore interface {
	conversationThreadStartTestStore
	AgentSessionStore
	ArtifactStore
}

// fakeActionCalls is the governed call store the handler reads; tool
// entries reference its calls by id.
type fakeActionCalls struct {
	mu    sync.Mutex
	calls map[string]*ActionCall
}

func (a *fakeActionCalls) GetActionCall(_ context.Context, _ Scope, id string) (*ActionCall, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call, ok := a.calls[id]
	if !ok {
		return nil, ErrActionNotFound
	}
	copied := *call
	return &copied, nil
}

type finalDeliveryFixture struct {
	t            *testing.T
	ctx          context.Context
	store        finalDeliveryStore
	service      *ConversationService
	conversation *Conversation
	trigger      *ChannelMessage
	run          *AgentRun
	session      *AgentSession
	runner       *ConversationRunTurnRunner
	actions      *fakeActionCalls
	handler      *conversationFinalHandler
	content      *answerVisualContent
	now          time.Time
	steps        int
}

type finalDeliveryOptions struct {
	team          bool
	responseMode  string
	participation bool // RequireParticipationOptIn with participation disabled
	coordinator   func(*testing.T, *ConversationService) *ConversationCoordinator
	visualChecks  func(*AgentRun) presentation.ValidateOptions
}

func forFinalDeliveryStores(t *testing.T, options finalDeliveryOptions, test func(*testing.T, *finalDeliveryFixture)) {
	t.Helper()
	forConversationThreadStartStores(t, func(t *testing.T, base conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
		test(t, newFinalDeliveryFixture(t, base, options))
	})
}

func newFinalDeliveryFixture(t *testing.T, base conversationThreadStartTestStore, options finalDeliveryOptions) *finalDeliveryFixture {
	t.Helper()
	store, ok := base.(finalDeliveryStore)
	if !ok {
		t.Fatalf("%T is not an agent session store", base)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "final-" + uuid.NewString()[:8]}
	service := NewConversationService(store)
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
	if options.team {
		owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-1"}
	}
	seedTestSpace(t, store.(SpaceStore), scope)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Final answers", IdempotencyKey: "final-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentQuestion,
		Content: "Can you pull the quarterly report?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		ResponseMode: options.responseMode, IdempotencyKey: "trigger",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := posted.Message
	if conversation, err = service.GetConversation(ctx, scope, conversation.ID); err != nil {
		t.Fatal(err)
	}
	request := conversationAgentRunRequest(conversation, trigger)
	if options.team {
		request.AssignedAgentID = "agent-1"
		request.ConcurrencyKey = ConversationRunLane(conversation.ID, "", "agent-1")
		request.SessionID = conversationSessionID(scope, request.ConcurrencyKey)
		request.IdempotencyKey = "member-" + trigger.ID
	}
	portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, request)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: finalDeliveryWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	opened, err := store.OpenConversationAgentSession(ctx, OpenConversationAgentSessionRequest{
		Scope: scope, ConversationID: conversation.ID, AgentID: "agent-1", RunID: run.ID,
		Lease: AgentRunLeaseGuard{WorkerID: finalDeliveryWorker, Now: now}, Now: now,
	})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	coordinator := conversationRunTestCoordinator
	if options.coordinator != nil {
		coordinator = options.coordinator
	}
	content := &answerVisualContent{}
	runner, err := NewConversationRunTurnRunner(store, coordinator(t, service), ConversationRunTurnRunnerConfig{
		AttachmentContent: content, RequireParticipationOptIn: options.participation, VisualChecks: options.visualChecks,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := &fakeActionCalls{calls: map[string]*ActionCall{}}
	return &finalDeliveryFixture{
		t: t, ctx: ctx, store: store, service: service, conversation: conversation, trigger: trigger, run: run,
		session: opened.Session, runner: runner, actions: actions, content: content, now: now,
		handler: newConversationFinalHandler(runner, store, actions, store),
	}
}

func (f *finalDeliveryFixture) lease() *AgentRunLeaseGuard {
	return &AgentRunLeaseGuard{WorkerID: finalDeliveryWorker, Now: f.now}
}

func (f *finalDeliveryFixture) currentRun() *AgentRun {
	f.t.Helper()
	run, err := f.store.GetAgentRun(f.ctx, f.run.Scope, f.run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// mutateRun rewrites the Run outside the loop (a follow-up delivery, the
// failure transition).
func (f *finalDeliveryFixture) mutateRun(mutate func(*AgentRun)) {
	f.t.Helper()
	current := f.currentRun()
	updated := cloneAgentRun(current)
	mutate(updated)
	updated.Revision++
	updated.UpdatedAt = f.now
	event := &ActivityEvent{
		ID: "mutate-" + uuid.NewString()[:8], Scope: current.Scope, EventType: "run.updated", Severity: ActivitySeverityInfo,
		RunID: current.ID, Actor: ActivityActor{Type: "worker", ID: finalDeliveryWorker}, Summary: "Run updated",
		Visibility: ActivityVisibilityScope, CreatedAt: f.now,
	}
	if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, event, nil); err != nil {
		f.t.Fatal(err)
	}
}

// step appends one assistant entry with these tool calls as pending entries
// and returns the entries by call id.
func (f *finalDeliveryFixture) step(calls ...LoopToolCall) (string, map[string]*SessionEntry) {
	f.t.Helper()
	f.steps++
	stepID := "step-" + string(rune('0'+f.steps))
	assistant := &SessionEntry{
		ID: SessionEntryID(f.session.ID, AssistantEntryKey(stepID)), RunID: f.run.ID, StepID: stepID, Step: int64(f.steps),
		Type: SessionEntryAssistant, Assistant: &AssistantEntry{Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopToolCalls},
	}
	batch := []*SessionEntry{assistant}
	for _, call := range calls {
		assistant.Assistant.ToolCalls = append(assistant.Assistant.ToolCalls, ModelToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		batch = append(batch, &SessionEntry{
			ID: SessionEntryID(f.session.ID, ToolEntryKey(stepID, call.ID)), RunID: f.run.ID, StepID: stepID, Step: int64(f.steps),
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: call.ID,
			Tool: &ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: ToolSideEffectWrite},
		})
	}
	appended, err := f.store.AppendAgentSessionEntries(f.ctx, AppendAgentSessionEntriesRequest{
		Scope: f.run.Scope, SessionID: f.session.ID, RunID: f.run.ID, Lease: f.lease(), Entries: batch, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("append step: %v", err)
	}
	entries := map[string]*SessionEntry{}
	for _, entry := range appended.Entries {
		if entry.Type == SessionEntryTool {
			entries[entry.ToolCallID] = entry
		}
	}
	return stepID, entries
}

func (f *finalDeliveryFixture) settle(entry *SessionEntry, to ToolEntryState, actionCallID string, text string) {
	f.t.Helper()
	status, isError := ToolResultSucceeded, false
	if to == ToolEntryError {
		status, isError = ToolResultFailed, true
	}
	_, err := f.store.TransitionToolEntry(f.ctx, ToolEntryTransitionRequest{
		Scope: f.run.Scope, EntryID: entry.ID, RunID: f.run.ID, Lease: *f.lease(),
		From: ToolEntryPending, To: to, ActionCallID: actionCallID,
		Result: &ToolResult{Status: status, IsError: isError, Content: []ContentPart{storetest.TextPart(text)}}, Now: f.now,
	})
	if err != nil {
		f.t.Fatalf("settle %s: %v", entry.ID, err)
	}
}

// governed records a governed tool call of the Run with this status and output.
func (f *finalDeliveryFixture) governed(callID string, status ActionCallStatus, output map[string]interface{}) string {
	f.t.Helper()
	_, entries := f.step(LoopToolCall{ID: callID, Name: "reports__export", Arguments: json.RawMessage(`{}`)})
	actionID := "action-" + callID
	f.actions.mu.Lock()
	f.actions.calls[actionID] = &ActionCall{ID: actionID, Scope: f.run.Scope, RunID: f.run.ID, Status: status, Output: output}
	f.actions.mu.Unlock()
	state := ToolEntryCompleted
	if status != ActionCallStatusSucceeded {
		state = ToolEntryError
	}
	f.settle(entries[callID], state, actionID, "done")
	return actionID
}

// finalEntry appends a pending turn_final_answer entry and returns the
// FinalAnswer the control section would hand the loop for it.
func (f *finalDeliveryFixture) finalEntry(answer string, output map[string]interface{}) (FinalAnswer, *SessionEntry) {
	f.t.Helper()
	callID := "final-" + uuid.NewString()[:6]
	stepID, entries := f.step(LoopToolCall{ID: callID, Name: string(ControlToolFinalAnswer), Arguments: json.RawMessage(`{}`)})
	if output == nil {
		output = map[string]interface{}{finalOutputSummaryKey: answer}
	}
	entry := entries[callID]
	return FinalAnswer{StepID: stepID, ToolEntryID: entry.ID, ToolCallID: callID, Answer: answer, Output: output}, entry
}

func (f *finalDeliveryFixture) deliver(answer FinalAnswer) (FinalDeliveryResult, error) {
	f.t.Helper()
	run := f.currentRun()
	return f.handler.Deliver(f.ctx, FinalDelivery{Run: run, Answer: answer, Guard: ReplyRunGuard{
		Scope: run.Scope, RunID: run.ID, WorkerID: finalDeliveryWorker, ConcurrencyKey: run.ConcurrencyKey,
		InterventionCount: len(run.PendingInterventions),
	}})
}

func (f *finalDeliveryFixture) mustDeliver(answer FinalAnswer, kind FinalDeliveryKind) FinalDeliveryResult {
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
func (f *finalDeliveryFixture) answers() []*ChannelMessage {
	f.t.Helper()
	messages, err := f.service.ListChannelMessages(f.ctx, ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	var answers []*ChannelMessage
	for _, message := range messages {
		if message.Sender.Type == ConversationParticipantAgent {
			answers = append(answers, message)
		}
	}
	return answers
}

func (f *finalDeliveryFixture) followUp(key, content string) *ChannelMessage {
	f.t.Helper()
	current, err := f.service.GetConversation(f.ctx, f.run.Scope, f.conversation.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	posted, err := f.service.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.run.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentQuestion,
		Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	message := posted.Message
	f.mutateRun(func(run *AgentRun) {
		run.PendingInterventions = append(run.PendingInterventions, AgentRunIntervention{
			ID: conversationFollowUpInterventionID(run.Scope, message.ID), Actor: ActivityActor{Type: "user", ID: "user-1"},
			Instruction: content, CreatedAt: f.now,
			ConversationMessage: &ConversationMessageDelivery{ConversationID: f.conversation.ID, MessageID: message.ID, Sequence: message.Sequence, Sender: message.Sender},
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
		f.governed("export", ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": "report-pdf", "version": 1, "path": "docs/report.pdf"}}})
		f.governed("failed", ActionCallStatusFailed, map[string]interface{}{"artifactRefs": []interface{}{ref("failed-file", 1)}})
		for _, files := range [][]interface{}{{"docs/draft.pdf"}, {"docs/report.pdf", "failed-file"}} {
			answer, _ := f.finalEntry("Here is the report.", map[string]interface{}{finalOutputSummaryKey: "Here is the report.", finalOutputFilesKey: files})
			result := f.mustDeliver(answer, FinalDeliveryRetryFinal)
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
		f.governed("export", ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{ref("report-pdf", 1), ref("report-csv", 2)}})
		f.governed("again", ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{ref("report-pdf", 1), ref(strings.Repeat("x", 257), 1), ref("zero", 0)}})
		f.governed("failed", ActionCallStatusFailed, map[string]interface{}{"artifactRefs": []interface{}{ref("failed-file", 1)}})
		// The answer hands over the files it names; the Run's others stay in the Library.
		answer, _ := f.finalEntry("Here is the quarterly report.", map[string]interface{}{
			finalOutputSummaryKey: "Here is the quarterly report.", finalOutputFilesKey: []interface{}{"report-pdf", "report-csv"}})
		result := f.mustDeliver(answer, FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 {
			t.Fatalf("answers = %#v", answers)
		}
		posted := answers[0]
		want := []ConversationReference{
			{Kind: ConversationReferenceRun, ID: f.run.ID},
			{Kind: ConversationReferenceArtifact, ID: "report-pdf", Version: 1},
			{Kind: ConversationReferenceArtifact, ID: "report-csv", Version: 2},
		}
		if posted.Content != "Here is the quarterly report." || posted.Intent != MessageIntentAnswer || posted.Sender.ID != "agent-1" ||
			posted.ReplyToMessageID != f.trigger.ID || posted.ResolvesMessageID != f.trigger.ID || posted.BroadcastToChannel ||
			posted.IdempotencyKey != conversationRunResponseKey(f.run, f.trigger.ID) || len(posted.References) != len(want) {
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
		if _, ok := result.Output[finalOutputBroadcastKey]; ok {
			t.Fatalf("broadcastToChannel present without broadcast: %#v", result.Output)
		}
		// A re-delivery after a crash returns the stored reply.
		replay := f.mustDeliver(answer, FinalDeliveryComplete)
		if replay.Output["messageId"] != posted.ID || replay.Output["replayed"] != true || len(f.answers()) != 1 {
			t.Fatalf("replay = %#v, answers = %d", replay.Output, len(f.answers()))
		}
	})
}

func TestBroadcastToChannelFlagPosted(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("Shipped to everyone.", map[string]interface{}{finalOutputSummaryKey: "Shipped to everyone.", finalOutputBroadcastKey: true})
		result := f.mustDeliver(answer, FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || !answers[0].BroadcastToChannel || result.Output[finalOutputBroadcastKey] != true {
			t.Fatalf("answers = %#v, output = %#v", answers, result.Output)
		}
	})
}

func TestSilentOnlyForSpoken(t *testing.T) {
	silent := map[string]interface{}{finalOutputSilentKey: true}
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("", silent)
		result := f.mustDeliver(answer, FinalDeliveryRetryFinal)
		if result.RejectionText != conversationSilentNotSpokenText || len(f.answers()) != 0 {
			t.Fatalf("result = %#v, answers = %d", result, len(f.answers()))
		}
	})
	forFinalDeliveryStores(t, finalDeliveryOptions{responseMode: "spoken"}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("", silent)
		result := f.mustDeliver(answer, FinalDeliveryComplete)
		if result.Output[finalOutputSilentKey] != true || result.Output["conversationId"] != f.conversation.ID ||
			result.Output["triggerMessageId"] != f.trigger.ID || len(f.answers()) != 0 {
			t.Fatalf("output = %#v, answers = %d", result.Output, len(f.answers()))
		}
	})
}

func TestReactionOnlyCompletionPostsNoMessage(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		args, _ := json.Marshal(TurnReaction{MessageID: f.trigger.ID, Emoji: "👍"})
		_, entries := f.step(LoopToolCall{ID: "react", Name: string(ControlToolReact), Arguments: args})
		f.settle(entries["react"], ToolEntryCompleted, "", `{"status":"added"}`)
		// The control section's reactions-only final.
		reacted, _ := f.finalEntry("", map[string]interface{}{finalOutputReactedKey: true})
		result := f.mustDeliver(reacted, FinalDeliveryComplete)
		if result.Output[finalOutputReactedKey] != true || result.Output["triggerMessageId"] != f.trigger.ID {
			t.Fatalf("output = %#v", result.Output)
		}
		// An answer that only repeats the emoji says nothing more.
		echo, _ := f.finalEntry(" 👍️ ", nil)
		if result := f.mustDeliver(echo, FinalDeliveryComplete); result.Output[finalOutputReactedKey] != true {
			t.Fatalf("echo output = %#v", result.Output)
		}
		if answers := f.answers(); len(answers) != 0 {
			t.Fatalf("a reaction-only reply posted %#v", answers)
		}
		// Words beyond the emoji are a reply.
		words, _ := f.finalEntry("👍 On it, report coming up.", nil)
		f.mustDeliver(words, FinalDeliveryComplete)
		if answers := f.answers(); len(answers) != 1 {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestEmptyAnswerRetriesOnceThenFails(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		first, entry := f.finalEntry("  ", nil)
		result := f.mustDeliver(first, FinalDeliveryRetryFinal)
		if result.RejectionText != conversationEmptyAnswerText {
			t.Fatalf("result = %#v", result)
		}
		// The loop records the rejection on the final entry (rejectDeliveredFinal).
		f.settle(entry, ToolEntryError, "", result.RejectionText)
		second, _ := f.finalEntry("", nil)
		result = f.mustDeliver(second, FinalDeliveryFail)
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
			return map[string]interface{}{finalOutputSummaryKey: answer, finalOutputVisualKey: finalDeliveryVisual()}
		}
		first, _ := f.finalEntry("Revenue is up.", output("Revenue is up."))
		result := f.mustDeliver(first, FinalDeliveryComplete)
		if _, ok := result.Output[finalOutputVisualKey]; ok {
			t.Fatalf("the visual stayed in the Run output: %#v", result.Output)
		}
		firstID := answerVisualArtifactID(f.run.ID, f.trigger.ID, "")
		follow := f.followUp("follow-up", "And last quarter?")
		second, _ := f.finalEntry("Last quarter was flat.", output("Last quarter was flat."))
		f.mustDeliver(second, FinalDeliveryComplete)
		secondID := answerVisualArtifactID(f.run.ID, f.trigger.ID, "followup:"+follow.ID)
		answers := f.answers()
		if len(answers) != 2 || firstID == secondID {
			t.Fatalf("answers = %#v", answers)
		}
		for i, id := range []string{firstID, secondID} {
			refs := answers[i].References
			if len(refs) != 2 || refs[1] != (ConversationReference{Kind: ConversationReferenceArtifact, ID: id, Version: 1}) {
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
	refuse := func(*AgentRun) presentation.ValidateOptions {
		return presentation.ValidateOptions{Bindings: presentation.BindingResolverFunc(func(context.Context, presentation.Binding) error {
			return errors.New("not in this chat")
		})}
	}
	forFinalDeliveryStores(t, finalDeliveryOptions{visualChecks: refuse}, func(t *testing.T, f *finalDeliveryFixture) {
		bound := finalDeliveryVisual()
		bound.Components = append(bound.Components, presentation.Component{ID: "next", Type: "actions", Actions: []presentation.Action{
			{ID: "stop", Label: "Stop", Kind: presentation.ActionEntity, Command: "stop", Binding: &presentation.Binding{Kind: presentation.BindingRunbook, ID: "rb-1"}},
		}})
		first, entry := f.finalEntry("Revenue is up.", map[string]interface{}{finalOutputSummaryKey: "Revenue is up.", finalOutputVisualKey: bound})
		result := f.mustDeliver(first, FinalDeliveryRetryFinal)
		if !strings.HasPrefix(result.RejectionText, conversationVisualRejectedText) || len(f.answers()) != 0 {
			t.Fatalf("result = %#v", result)
		}
		f.settle(entry, ToolEntryError, "", result.RejectionText)
		second, _ := f.finalEntry("Revenue is up.", map[string]interface{}{finalOutputSummaryKey: "Revenue is up.", finalOutputVisualKey: bound})
		f.mustDeliver(second, FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || len(answers[0].References) != 1 {
			t.Fatalf("answers = %#v", answers)
		}
		events, err := f.store.ListActivity(f.ctx, ActivityFilter{Scope: f.run.Scope, RunID: f.run.ID, Limit: 100})
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
		f.governed("operation", ActionCallStatusSucceeded, map[string]interface{}{"summary": "Runbook operation completed", "resourceId": "rb-1"})
		answer, _ := f.finalEntry("I paused the weekly digest; it will not send on Monday.", nil)
		f.mustDeliver(answer, FinalDeliveryComplete)
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
		delivery := FinalDelivery{Run: run, Answer: answer, Guard: ReplyRunGuard{
			Scope: run.Scope, RunID: run.ID, WorkerID: finalDeliveryWorker, ConcurrencyKey: run.ConcurrencyKey,
			InterventionCount: len(run.PendingInterventions),
		}}
		// A message arrives after the loop's D8 check, before the post.
		f.followUp("late", "Actually, only Q3 please.")
		if _, err := f.handler.Deliver(f.ctx, delivery); !errors.Is(err, ErrSteeringArrived) {
			t.Fatalf("deliver = %v, want ErrSteeringArrived", err)
		}
		if len(f.answers()) != 0 {
			t.Fatalf("a superseded answer was posted")
		}
		// The next answer, which saw the message, posts under its own key.
		next, _ := f.finalEntry("Here is Q3.", nil)
		f.mustDeliver(next, FinalDeliveryComplete)
		if answers := f.answers(); len(answers) != 1 || answers[0].Content != "Here is Q3." {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestTeamHandoffMentionsFromFinalAnswer(t *testing.T) {
	team := func(t *testing.T, service *ConversationService) *ConversationCoordinator {
		t.Helper()
		participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return []ConversationParticipantBinding{
				{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"}},
				{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-2"}},
			}, nil
		})
		router := ConversationRouterFunc(func(context.Context, ConversationRouteRequest) (ConversationRouteDecision, error) {
			return ConversationRouteDecision{AgentID: "agent-1"}, nil
		})
		coordinator, err := NewConversationCoordinator(service, participants, router, DefaultConversationCoordinatorConfig())
		if err != nil {
			t.Fatal(err)
		}
		return coordinator
	}
	forFinalDeliveryStores(t, finalDeliveryOptions{team: true, coordinator: team}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("@agent-2 can you check the ad spend numbers?", nil)
		f.mustDeliver(answer, FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || len(answers[0].Mentions) != 1 || answers[0].Mentions[0].ID != "agent-2" {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestOTPAnswerIsPostedThenParks(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("What is the code the bank just sent you?", nil)
		answer.Park = &FinalPark{Deliver: true, Park: LoopPark{Status: AgentRunStatusWaitingForEvent, Wake: &WakeCondition{Type: "user_message"}}}
		result := f.mustDeliver(answer, FinalDeliveryPark)
		answers := f.answers()
		if len(answers) != 1 || answers[0].Content != answer.Answer || result.Output["messageId"] != answers[0].ID {
			t.Fatalf("answers = %#v, output = %#v", answers, result.Output)
		}
	})
}

func TestFailureExplanationIsPostedAndFails(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		f.mutateRun(func(run *AgentRun) {
			run.FailureExplanation = &RunFailureExplanation{Kind: RunFailureKindExecution, Code: "execution_failed", Message: "The export service was unavailable."}
		})
		answer, _ := f.finalEntry("I couldn't export the report: the export service was down.",
			map[string]interface{}{finalOutputSummaryKey: "x", finalOutputVisualKey: finalDeliveryVisual()})
		result := f.mustDeliver(answer, FinalDeliveryFail)
		answers := f.answers()
		if result.Failure != nil || len(answers) != 1 || answers[0].Content != answer.Answer || len(answers[0].References) != 1 ||
			result.Output["summary"] != answer.Answer || result.Output["messageId"] != answers[0].ID {
			t.Fatalf("result = %#v, answers = %#v", result, answers)
		}
	})
	// An empty explanation posts the kernel's reply.
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		f.mutateRun(func(run *AgentRun) {
			run.FailureExplanation = &RunFailureExplanation{Kind: RunFailureKindExecution, Code: "execution_failed", Message: "The export service was unavailable."}
		})
		answer, _ := f.finalEntry("", map[string]interface{}{finalOutputSilentKey: true})
		f.mustDeliver(answer, FinalDeliveryFail)
		answers := f.answers()
		if len(answers) != 1 || answers[0].Content != TerminalFailureReply("execution_failed") {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestParticipationStoppedPostsNothing(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{participation: true}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("Here is the report.", nil)
		result := f.mustDeliver(answer, FinalDeliveryComplete)
		if result.Output["participationSkipped"] != true || len(f.answers()) != 0 {
			t.Fatalf("output = %#v, answers = %d", result.Output, len(f.answers()))
		}
	})
}

func TestKernelFinalWithoutAnswerKeepsItsOutput(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		output := map[string]interface{}{"approvalReplaced": true, "replacedByMessageId": "m-2"}
		result := f.mustDeliver(FinalAnswer{StepID: "step-k", Output: output}, FinalDeliveryComplete)
		if result.Output["approvalReplaced"] != true || len(f.answers()) != 0 {
			t.Fatalf("output = %#v, answers = %d", result.Output, len(f.answers()))
		}
	})
}
