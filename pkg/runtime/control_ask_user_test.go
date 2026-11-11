package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type askUserTestStore interface {
	KernelStore
	ConversationStore
}

// askUserCall is one claimed Run with a finished, applied step whose assistant
// entry called ask_user (entry pending), ready to settle.
type askUserCall struct {
	store   askUserTestStore
	worker  string
	run     *AgentRun // the claimed Run, LastAppliedTurn = the step
	session *AgentSession
	step    *AgentTurn
	entry   *SessionEntry
	exec    *ControlExecutor
}

func askUserSessions(t *testing.T, store askUserTestStore) AgentSessionStore {
	t.Helper()
	sessions, ok := store.(AgentSessionStore)
	if !ok {
		t.Fatalf("%T is not a session store", store)
	}
	return sessions
}

// newAskUserCall records an ask_user call on claimed, a Run leased by worker.
func newAskUserCall(t *testing.T, store askUserTestStore, claimed *AgentRun, worker, question string) *askUserCall {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"question": question})
	return newAskUserCallWithArguments(t, store, claimed, worker, arguments)
}

// newAskUserCallWithArguments records an ask_user call with raw arguments.
func newAskUserCallWithArguments(t *testing.T, store askUserTestStore, claimed *AgentRun, worker string, arguments json.RawMessage) *askUserCall {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	sessions := askUserSessions(t, store)
	lease := AgentRunLeaseGuard{WorkerID: worker, Now: now}
	var session *AgentSession
	if claimed.SessionID == "" {
		opened, err := sessions.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
			Scope: claimed.Scope, OwnerRunID: claimed.ID, AgentID: claimed.AssignedAgentID, Lease: lease, Now: now,
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		session = opened.Session
	} else {
		session = openAskUserSession(t, sessions, claimed, lease)
	}
	sequence := claimed.LastAppliedTurn + 1
	stepID := fmt.Sprintf("%s-step-%d", claimed.ID, sequence)
	callID := fmt.Sprintf("call-%d", sequence)
	assistant := &SessionEntry{
		ID: SessionEntryID(session.ID, AssistantEntryKey(stepID)), RunID: claimed.ID, StepID: stepID, Step: sequence,
		Type: SessionEntryAssistant, Assistant: &AssistantEntry{
			Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopToolCalls,
			ToolCalls: []ModelToolCall{{ID: callID, Name: string(ControlToolAskUser), Arguments: arguments}},
		},
	}
	tool := &SessionEntry{
		ID: SessionEntryID(session.ID, ToolEntryKey(stepID, callID)), RunID: claimed.ID, StepID: stepID, Step: sequence,
		Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: callID,
		Tool: &ToolEntry{Name: string(ControlToolAskUser), Arguments: arguments, SideEffect: ToolSideEffectWrite},
	}
	appended, err := sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: claimed.Scope, SessionID: session.ID, RunID: claimed.ID, Lease: &lease,
		Entries: []*SessionEntry{assistant, tool}, Now: now,
	})
	if err != nil {
		t.Fatalf("append ask_user step: %v", err)
	}
	run, err := store.GetAgentRun(ctx, claimed.Scope, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.LastAppliedTurn = sequence // FinishTurn + applyStep, as the loop does before settle
	return &askUserCall{
		store: store, worker: worker, run: run, session: session,
		step:  &AgentTurn{ID: stepID, RunID: run.ID, Sequence: sequence, Status: AgentTurnStatusCompleted},
		entry: appended.Entries[1],
		exec:  &ControlExecutor{Portfolio: store, Sessions: sessions, Conversations: NewConversationService(store)},
	}
}

// openAskUserSession gets the Run's bound session, opening a conversation
// lane session on its first claim.
func openAskUserSession(t *testing.T, sessions AgentSessionStore, run *AgentRun, lease AgentRunLeaseGuard) *AgentSession {
	t.Helper()
	session, err := sessions.GetAgentSession(t.Context(), run.Scope, run.SessionID)
	if err == nil {
		return session
	}
	if !errors.Is(err, ErrAgentSessionNotFound) {
		t.Fatalf("get session: %v", err)
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	thread, _ := run.Context["threadRootMessageId"].(string)
	for _, root := range []string{"", thread} {
		opened, err := sessions.OpenConversationAgentSession(t.Context(), OpenConversationAgentSessionRequest{
			Scope: run.Scope, ConversationID: conversationID, ThreadRootID: root, AgentID: run.AssignedAgentID,
			RunID: run.ID, Lease: lease, Now: lease.Now,
		})
		if err == nil {
			return opened.Session
		}
		if !errors.Is(err, ErrSessionMoved) {
			t.Fatalf("open conversation session: %v", err)
		}
	}
	t.Fatalf("no session for run %s", run.ID)
	return nil
}

func (c *askUserCall) context() ControlContext {
	return NewControlContext(StepToolsInput{
		Run: c.run, Step: c.step, Session: c.session, WorkerID: c.worker,
		Binding: &TurnRunnerBinding{Authority: ControlAuthority{CanAskUser: true}},
	}, c.entry)
}

func (c *askUserCall) settle(t *testing.T) ControlResult {
	t.Helper()
	result, err := c.exec.Settle(t.Context(), c.context())
	if err != nil {
		t.Fatalf("settle ask_user: %v", err)
	}
	if result.Entry != nil {
		c.entry = result.Entry
	}
	return result
}

// park applies the settle's park the way the loop does.
func (c *askUserCall) park(t *testing.T, park *LoopPark) *AgentRun {
	t.Helper()
	current, err := c.store.GetAgentRun(t.Context(), c.run.Scope, c.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked, _, err := NewRunActivityService(c.store, c.store).TransitionRun(t.Context(), c.run.Scope, c.run.ID, RunTransitionRequest{
		ExpectedRevision: current.Revision, Status: park.Status, WakeCondition: park.Wake, LeaseOwner: c.worker,
		AppliedTurn: c.step.Sequence, EventType: "run.waiting_for_user", Summary: park.Summary,
	})
	if err != nil {
		t.Fatalf("park on ask_user: %v", err)
	}
	return parked
}

// askUserParked settles an ask_user call of a claimed Run and parks it.
func askUserParked(t *testing.T, store askUserTestStore, claimed *AgentRun, worker, question string) (*AgentRun, *askUserCall) {
	t.Helper()
	call := newAskUserCall(t, store, claimed, worker, question)
	result := call.settle(t)
	if result.Kind != ControlParked || result.Park == nil {
		t.Fatalf("ask_user did not park: %#v", result)
	}
	return call.park(t, result.Park), call
}

// resume claims the Run again and resumes the ask_user entry.
func (c *askUserCall) resume(t *testing.T) (ControlResume, *AgentRun) {
	t.Helper()
	claimed := claimAskUserRun(t, c.store, c.run, c.worker)
	entry, err := askUserSessions(t, c.store).GetSessionEntry(t.Context(), claimed.Scope, c.entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.run, c.entry = claimed, entry
	resumed, err := c.exec.Resume(t.Context(), c.context())
	if err != nil {
		t.Fatalf("resume ask_user: %v", err)
	}
	if resumed.Resolved && resumed.Result.Entry != nil {
		c.entry = resumed.Result.Entry
	}
	return resumed, claimed
}

func claimAskUserRun(t *testing.T, store askUserTestStore, run *AgentRun, worker string) *AgentRun {
	t.Helper()
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: run.Scope, Kind: run.Kind, WorkerID: worker, Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim %s: %#v %v", run.ID, claimed, err)
	}
	return claimed
}

func askUserResultValue(t *testing.T, entry *SessionEntry) map[string]interface{} {
	t.Helper()
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil || len(entry.Tool.Result.Content) != 1 {
		t.Fatalf("entry has no result: %#v", entry)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(entry.Tool.Result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// claimedForegroundRun schedules a chat message and claims its foreground Run.
func claimedForegroundRun(t *testing.T, store conversationTaskClarificationStore) *foregroundClarificationFixture {
	t.Helper()
	f := newForegroundClarificationFixture(t, store, false)
	f.run = claimAskUserRun(t, store, f.run, "foreground-worker")
	f.asked = askUserTestQuestion
	return f
}

const askUserTestQuestion = "Which page should I research?"

func TestAskUserPostsQuestionAndParks(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		call := newAskUserCall(t, store, f.run, "foreground-worker", "  "+askUserTestQuestion+"  ")
		result := call.settle(t)
		if result.Kind != ControlParked || result.Park.Status != AgentRunStatusWaitingForEvent ||
			result.Park.Wake.Type != "user_message" || result.Park.Wake.Reference != call.entry.ID {
			t.Fatalf("park = %#v", result)
		}
		if call.entry.State != ToolEntryAwaitingEvent || call.entry.Tool.Wait == nil || call.entry.Tool.Wait.EventWaitID != call.entry.ID {
			t.Fatalf("entry = %#v", call.entry)
		}
		key := "conversation-clarification:" + f.run.ID + ":" + call.entry.ID
		meta, err := DecodeControlEntryMeta(call.entry)
		if err != nil || meta == nil || meta.Kind != ControlToolAskUser || meta.QuestionMessageID != stableConversationID(f.run.Scope, key, "message") {
			t.Fatalf("meta = %#v %v", meta, err)
		}
		question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, key)
		if err != nil || question == nil || question.ID != meta.QuestionMessageID || question.Content != askUserTestQuestion ||
			question.Intent != MessageIntentQuestion || !question.RequiresResponse || question.ReplyToMessageID != f.trigger.ID ||
			question.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: f.run.AssignedAgentID}) ||
			len(question.References) != 1 || question.References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: f.run.ID}) {
			t.Fatalf("question = %#v %v", question, err)
		}
		parked := call.park(t, result.Park)
		if !isConversationQuestionWait(parked) || clarificationQuestionKey(parked) != key {
			t.Fatalf("parked run is not a question wait: %#v", parked)
		}
		// The reconciler replays the same message, never a second one.
		if _, err := f.scheduler.ReconcileScope(t.Context(), f.run.Scope); err != nil {
			t.Fatal(err)
		}
		messages, _ := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID, Limit: 100})
		if len(messages) != 2 {
			t.Fatalf("messages = %d, want trigger + question", len(messages))
		}
	})
}

func TestAskUserAnswerResumesWithAnswerAndConsumesIntervention(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		parked, call := askUserParked(t, store, f.run, "foreground-worker", askUserTestQuestion)
		f.run = parked
		question := f.question(t)

		// Not answered yet: the resume re-parks on the same entry.
		answer := f.post(t, PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer,
			Content: "The pricing page.", ResolvesMessageID: question.ID, IdempotencyKey: "answer"})
		resumed, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
		if err != nil || resumed == nil || resumed.Run.ID != f.run.ID || resumed.Run.Status != AgentRunStatusQueued {
			t.Fatalf("answer did not wake the Run: %#v %v", resumed, err)
		}
		receipt := resumed.Run.PendingInterventions[len(resumed.Run.PendingInterventions)-1].ConversationAnswer
		if receipt == nil || receipt.QuestionTurnID != call.step.ID || receipt.QuestionTurnSequence != call.step.Sequence ||
			receipt.QuestionMessageID != question.ID || receipt.AnswerMessageID != answer.ID {
			t.Fatalf("receipt not bound to the ask_user step: %#v", receipt)
		}
		result, claimed := call.resume(t)
		if !result.Resolved || result.Result.Kind != ControlContinue || call.entry.State != ToolEntryCompleted {
			t.Fatalf("resume = %#v", result)
		}
		value := askUserResultValue(t, call.entry)
		if value["status"] != "answered" || value["answer"] != "The pricing page." || value["messageId"] != answer.ID {
			t.Fatalf("result = %#v", value)
		}
		// The answer is the tool result; steering consumes it without a user entry.
		steering, err := consumeSteering(claimed, &AgentTurn{ID: "next", RunID: claimed.ID, Sequence: call.step.Sequence + 1}, call.session)
		if err != nil || len(steering.Entries) != 0 || steering.Through == nil || *steering.Through != len(claimed.PendingInterventions) {
			t.Fatalf("steering = %#v %v", steering, err)
		}

		// The same question again is answered from the session.
		again := newAskUserCall(t, store, claimed, "foreground-worker", askUserTestQuestion)
		refused := again.settle(t)
		value = askUserResultValue(t, refused.Entry)
		if refused.Kind != ControlContinue || refused.Entry.State != ToolEntryError || value["error"] != "already_asked" ||
			!strings.Contains(value["message"].(string), "The pricing page.") {
			t.Fatalf("repeat = %#v %#v", refused, value)
		}
	})
}

func TestAskUserResumeWithoutAnswerRepostsAndReparks(t *testing.T) {
	store := newTestKernelStore(t)
	f := claimedForegroundRun(t, store)
	call := newAskUserCall(t, store, f.run, "foreground-worker", askUserTestQuestion)
	// Crash after the entry transition, before the post and the park: the
	// next claim resumes the awaiting entry.
	cc := call.context()
	cc.Arguments = &askUserArgs{Question: askUserTestQuestion}
	entry, err := call.exec.transitionControlEntry(t.Context(), cc, ToolEntryAwaitingEvent, &ToolEntryWait{EventWaitID: call.entry.ID, Reason: askUserWaitReason}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call.entry = entry
	resumed, err := call.exec.Resume(t.Context(), call.context())
	if err != nil || resumed.Resolved || resumed.Park == nil || resumed.Park.Wake.Reference != entry.ID {
		t.Fatalf("resume = %#v %v", resumed, err)
	}
	key := conversationClarificationKey(f.run.ID, entry.ID)
	question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, key)
	if err != nil || question == nil || question.Content != askUserTestQuestion {
		t.Fatalf("question not posted on resume: %#v %v", question, err)
	}
}

// conflictingQuestionStore makes every question post lose the conversation
// revision race.
type conflictingQuestionStore struct {
	ConversationStore
}

func (s conflictingQuestionStore) CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	if strings.HasPrefix(record.Message.IdempotencyKey, conversationClarificationKeyPrefix) {
		return nil, ErrRevisionConflict
	}
	return s.ConversationStore.CommitChannelMessage(ctx, record)
}

func TestAskUserQuestionReconcilerRepostsFromEntry(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := claimedForegroundRun(t, store)
		call := newAskUserCall(t, store, f.run, "foreground-worker", askUserTestQuestion)
		call.exec.Conversations = NewConversationService(conflictingQuestionStore{store})
		result := call.settle(t)
		if result.Kind != ControlParked {
			t.Fatalf("settle = %#v", result)
		}
		key := conversationClarificationKey(f.run.ID, call.entry.ID)
		if q, _ := store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, key); q != nil {
			t.Fatal("question posted despite conflicts")
		}
		f.run = call.park(t, result.Park)
		if f.run.Output["summary"] != nil {
			t.Fatal("question text leaked into Run output")
		}
		question := f.question(t)
		if question.Content != askUserTestQuestion || question.ID != stableConversationID(f.run.Scope, key, "message") {
			t.Fatalf("reconciled question = %#v", question)
		}
	})
}

func TestAskUserAnswerFromOtherActorIgnored(t *testing.T) {
	store := newTestKernelStore(t)
	f := claimedForegroundRun(t, store)
	parked, call := askUserParked(t, store, f.run, "foreground-worker", askUserTestQuestion)
	f.run = parked
	question := f.question(t)
	foreign := f.post(t, PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "someone-else"},
		Intent: MessageIntentAnswer, Content: "Use the admin page.", ResolvesMessageID: question.ID, IdempotencyKey: "foreign-answer"})
	// An explicit reply to the question is a clarification attempt (handled),
	// but a foreign actor's answer wakes nothing.
	if result, _, err := f.scheduler.resumeConversationAnswer(t.Context(), f.conversation, foreign); err != nil || result != nil {
		t.Fatalf("foreign answer accepted: %#v %v", result, err)
	}
	current := f.current(t)
	if current.Status != AgentRunStatusWaitingForEvent || len(current.PendingInterventions) != 0 {
		t.Fatalf("foreign answer woke the Run: %#v", current)
	}
	if answer := askUserAnswer(current, call.entry, question.ID); answer != nil {
		t.Fatalf("foreign answer bound to the entry: %#v", answer)
	}
}

func TestAskUserWithoutConversationIsRefused(t *testing.T) {
	f := newControlFixture(t, []LoopToolCall{{ID: "ask", Name: string(ControlToolAskUser), Arguments: json.RawMessage(`{"question":"Which one?"}`)}})
	f.exec.handlers = nil // the real registry
	f.exec.Reporting = f.store
	result, err := f.exec.Settle(f.ctx, f.context("ask", ControlAuthority{CanAskUser: true}))
	if err != nil || result.Kind != ControlContinue || result.Entry.State != ToolEntryError {
		t.Fatalf("settle = %#v %v", result, err)
	}
	requireControlError(t, result.Entry.Tool.Result, ToolResultRefused, "cannot_post")
}

func TestAskUserArgumentsAreValidated(t *testing.T) {
	for _, raw := range []string{`{"question":"   "}`, `{"question":"x","extra":1}`, `{"question":"` + strings.Repeat("a", 4097) + `"}`} {
		if _, err := (askUserHandler{}).decode(json.RawMessage(raw)); err == nil {
			t.Fatalf("decode accepted %s", raw[:min(len(raw), 40)])
		}
	}
	value, err := (askUserHandler{}).decode(json.RawMessage(`{"question":"  Which one?  "}`))
	if err != nil || value.(*askUserArgs).Question != "Which one?" {
		t.Fatalf("decode = %#v %v", value, err)
	}
}
