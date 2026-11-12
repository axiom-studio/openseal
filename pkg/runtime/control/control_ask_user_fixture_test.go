// Copies of the ask_user call and foreground clarification fixtures of
// runtime's tests, on exported API, for the external ask_user tests.

package control_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// copied from conversation_task_clarification_test.go
func forConversationTaskClarificationStores(t *testing.T, test func(*testing.T, conversationTaskClarificationStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "task-clarification.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
}

// copied from conversation_task_clarification_test.go
type conversationTaskClarificationStore interface {
	models.KernelStore
	models.ConversationStore
	models.ConversationTaskStore
}

// copied from ask_user_call_fixture_test.go
// claimedForegroundRun schedules a chat message and claims its foreground Run.
func claimedForegroundRun(t *testing.T, store conversationTaskClarificationStore) *foregroundClarificationFixture {
	t.Helper()
	f := newForegroundClarificationFixture(t, store, false)
	f.run = claimAskUserRun(t, store, f.run, "foreground-worker")
	f.asked = askUserTestQuestion
	return f
}

// copied from ask_user_call_fixture_test.go
// newAskUserCall records an ask_user call on claimed, a Run leased by worker.
func newAskUserCall(t *testing.T, store askUserTestStore, claimed *models.AgentRun, worker, question string) *askUserCall {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"question": question})
	return newAskUserCallWithArguments(t, store, claimed, worker, arguments)
}

// copied from ask_user_call_fixture_test.go
const askUserTestQuestion = "Which page should I research?"

// copied from conversation_foreground_clarification_test.go
// parkedQuestionKey is the idempotency key of the question run is parked on,
// "" when it is not parked.
func parkedQuestionKey(run *models.AgentRun) string {
	if run == nil || run.WakeCondition == nil {
		return ""
	}
	return conversation.ConversationClarificationKey(run.ID, run.WakeCondition.Reference)
}

// copied from ask_user_call_fixture_test.go
// askUserParked settles an ask_user call of a claimed Run and parks it.
func askUserParked(t *testing.T, store askUserTestStore, claimed *models.AgentRun, worker, question string) (*models.AgentRun, *askUserCall) {
	t.Helper()
	call := newAskUserCall(t, store, claimed, worker, question)
	result := call.settle(t)
	if result.Kind != control.ControlParked || result.Park == nil {
		t.Fatalf("ask_user did not park: %#v", result)
	}
	return call.park(t, result.Park), call
}

// copied from ask_user_call_fixture_test.go
// newAskUserCallWithArguments records an ask_user call with raw arguments.
func newAskUserCallWithArguments(t *testing.T, store askUserTestStore, claimed *models.AgentRun, worker string, arguments json.RawMessage) *askUserCall {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	sessions := askUserSessions(t, store)
	lease := models.AgentRunLeaseGuard{WorkerID: worker, Now: now}
	var session *models.AgentSession
	if claimed.SessionID == "" {
		opened, err := sessions.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{
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
	assistant := &models.SessionEntry{
		ID: models.SessionEntryID(session.ID, models.AssistantEntryKey(stepID)), RunID: claimed.ID, StepID: stepID, Step: sequence,
		Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{ID: callID, Name: string(models.ControlToolAskUser), Arguments: arguments}},
		},
	}
	tool := &models.SessionEntry{
		ID: models.SessionEntryID(session.ID, models.ToolEntryKey(stepID, callID)), RunID: claimed.ID, StepID: stepID, Step: sequence,
		Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: callID,
		Tool: &models.ToolEntry{Name: string(models.ControlToolAskUser), Arguments: arguments, SideEffect: models.ToolSideEffectWrite},
	}
	appended, err := sessions.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: claimed.Scope, SessionID: session.ID, RunID: claimed.ID, Lease: &lease,
		Entries: []*models.SessionEntry{assistant, tool}, Now: now,
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
		step:  &models.AgentTurn{ID: stepID, RunID: run.ID, Sequence: sequence, Status: models.AgentTurnStatusCompleted},
		entry: appended.Entries[1],
		exec:  &control.ControlExecutor{Portfolio: store, Sessions: sessions, Conversations: conversation.NewConversationService(store)},
	}
}

// copied from ask_user_call_fixture_test.go
func askUserResultValue(t *testing.T, entry *models.SessionEntry) map[string]interface{} {
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

// copied from conversation_foreground_clarification_test.go
type foregroundClarificationFixture struct {
	store        conversationTaskClarificationStore
	service      *conversation.ConversationService
	scheduler    *conversationrun.ConversationRunScheduler
	conversation *models.Conversation
	trigger      *models.ChannelMessage
	run          *models.AgentRun
	turn         *models.AgentTurn
	asked        string
}

// copied from conversation_foreground_clarification_test.go
func (f *foregroundClarificationFixture) post(t *testing.T, req conversation.PostChannelMessageRequest) *models.ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Scope, req.ConversationID, req.ExpectedRevision = current.Scope, current.ID, current.Revision
	req.Audience = models.ConversationAudience{Kind: models.ConversationAudienceChannel}
	result, err := f.service.PostChannelMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

// copied from conversation_foreground_clarification_test.go
func (f *foregroundClarificationFixture) question(t *testing.T) *models.ChannelMessage {
	t.Helper()
	if _, err := f.scheduler.ReconcileScope(t.Context(), f.run.Scope); err != nil {
		t.Fatal(err)
	}
	question, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), f.run.Scope, f.conversation.ID, parkedQuestionKey(f.run))
	if err != nil || question == nil || question.Intent != models.MessageIntentQuestion || !question.RequiresResponse || question.ReplyToMessageID != f.trigger.ID ||
		question.Content != f.asked || len(question.References) != 1 || question.References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: f.run.ID}) {
		t.Fatalf("foreground question = %#v %v", question, err)
	}
	return question
}

// copied from conversation_foreground_clarification_test.go
func (f *foregroundClarificationFixture) current(t *testing.T) *models.AgentRun {
	t.Helper()
	run, err := f.store.GetAgentRun(t.Context(), f.run.Scope, f.run.ID)
	if err != nil || run == nil {
		t.Fatalf("read foreground: %#v %v", run, err)
	}
	return run
}

// copied from conversation_foreground_clarification_test.go
func newForegroundClarificationFixture(t *testing.T, store conversationTaskClarificationStore, threaded bool) *foregroundClarificationFixture {
	t.Helper()
	f := &foregroundClarificationFixture{store: store, service: conversation.NewConversationService(store)}
	var err error
	conversationtest.SeedSpace(t, store.(models.SpaceStore), models.Scope{Kind: "organization", ID: "foreground-clarification"})
	f.conversation, _, err = f.service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: "organization", ID: "foreground-clarification"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "browser-agent"},
		Title: "Browser research", IdempotencyKey: "browser-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler, err = conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.trigger = f.post(t, conversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"},
		Intent: models.MessageIntentQuestion, Content: "Research the requested page.", RequiresResponse: true, StartThread: threaded, IdempotencyKey: "request"})
	result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.conversation.Scope, f.conversation.ID, f.trigger.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", result, err)
	}
	f.run = result.Run
	return f
}

// copied from ask_user_call_fixture_test.go
func claimAskUserRun(t *testing.T, store askUserTestStore, run *models.AgentRun, worker string) *models.AgentRun {
	t.Helper()
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{
		Scope: run.Scope, Kind: run.Kind, WorkerID: worker, Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim %s: %#v %v", run.ID, claimed, err)
	}
	return claimed
}

// copied from ask_user_call_fixture_test.go
type askUserTestStore interface {
	models.KernelStore
	models.ConversationStore
}

// copied from ask_user_call_fixture_test.go
// askUserCall is one claimed Run with a finished, applied step whose assistant
// entry called ask_user (entry pending), ready to settle.
type askUserCall struct {
	store   askUserTestStore
	worker  string
	run     *models.AgentRun // the claimed Run, LastAppliedTurn = the step
	session *models.AgentSession
	step    *models.AgentTurn
	entry   *models.SessionEntry
	exec    *control.ControlExecutor
}

// copied from ask_user_call_fixture_test.go
func (c *askUserCall) context() agentrun.ControlContext {
	return control.NewControlContext(agentrun.StepToolsInput{
		Run: c.run, Step: c.step, Session: c.session, WorkerID: c.worker,
		Binding: &agentrun.TurnRunnerBinding{Authority: agentrun.ControlAuthority{CanAskUser: true}},
	}, c.entry)
}

// copied from ask_user_call_fixture_test.go
func (c *askUserCall) settle(t *testing.T) control.ControlResult {
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

// copied from ask_user_call_fixture_test.go
// park applies the settle's park the way the loop does.
func (c *askUserCall) park(t *testing.T, park *agentrun.LoopPark) *models.AgentRun {
	t.Helper()
	current, err := c.store.GetAgentRun(t.Context(), c.run.Scope, c.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked, _, err := activity.NewRunActivityService(c.store, c.store).TransitionRun(t.Context(), c.run.Scope, c.run.ID, activity.RunTransitionRequest{
		ExpectedRevision: current.Revision, Status: park.Status, WakeCondition: park.Wake, LeaseOwner: c.worker,
		AppliedTurn: c.step.Sequence, EventType: "run.waiting_for_user", Summary: park.Summary,
	})
	if err != nil {
		t.Fatalf("park on ask_user: %v", err)
	}
	return parked
}

// copied from ask_user_call_fixture_test.go
// resume claims the Run again and resumes the ask_user entry.
func (c *askUserCall) resume(t *testing.T) (control.ControlResume, *models.AgentRun) {
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

// copied from ask_user_call_fixture_test.go
func askUserSessions(t *testing.T, store askUserTestStore) models.AgentSessionStore {
	t.Helper()
	sessions, ok := store.(models.AgentSessionStore)
	if !ok {
		t.Fatalf("%T is not a session store", store)
	}
	return sessions
}

// copied from ask_user_call_fixture_test.go
// openAskUserSession gets the Run's bound session, opening a conversation
// lane session on its first claim.
func openAskUserSession(t *testing.T, sessions models.AgentSessionStore, run *models.AgentRun, lease models.AgentRunLeaseGuard) *models.AgentSession {
	t.Helper()
	session, err := sessions.GetAgentSession(t.Context(), run.Scope, run.SessionID)
	if err == nil {
		return session
	}
	if !errors.Is(err, models.ErrAgentSessionNotFound) {
		t.Fatalf("get session: %v", err)
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	thread, _ := run.Context["threadRootMessageId"].(string)
	for _, root := range []string{"", thread} {
		opened, err := sessions.OpenConversationAgentSession(t.Context(), models.OpenConversationAgentSessionRequest{
			Scope: run.Scope, ConversationID: conversationID, ThreadRootID: root, AgentID: run.AssignedAgentID,
			RunID: run.ID, Lease: lease, Now: lease.Now,
		})
		if err == nil {
			return opened.Session
		}
		if !errors.Is(err, models.ErrSessionMoved) {
			t.Fatalf("open conversation session: %v", err)
		}
	}
	t.Fatalf("no session for run %s", run.ID)
	return nil
}
