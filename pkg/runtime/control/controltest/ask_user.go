// Package controltest holds test fixtures for the control tools, built only
// on the exported API of control and lower packages: a foreground
// conversation Run that asks the person a question, and ask_user calls on a
// claimed Run. Runtime's tests and control's external tests share them.
package controltest

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

// Store is what a clarification fixture needs: the kernel, conversations
// and conversation tasks.
type Store interface {
	models.KernelStore
	models.ConversationStore
	models.ConversationTaskStore
}

// ForEachStore runs test on a fresh SQLite file store.
func ForEachStore(t *testing.T, test func(*testing.T, Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.New(filepath.Join(t.TempDir(), "task-clarification.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
}

// AskUserQuestion is the question the ask_user fixtures ask by default.
const AskUserQuestion = "Which page should I research?"

// ForegroundClarification is an Agent chat whose user message scheduled a
// foreground conversation Run.
type ForegroundClarification struct {
	Store        Store
	Service      *conversation.Service
	Scheduler    *conversationrun.Scheduler
	Conversation *models.Conversation
	Trigger      *models.ChannelMessage
	Run          *models.AgentRun
	Turn         *models.AgentTurn
	Asked        string
}

// NewForegroundClarification posts the user message (in a thread when
// threaded) and schedules its foreground Run.
func NewForegroundClarification(t *testing.T, store Store, threaded bool) *ForegroundClarification {
	t.Helper()
	f := &ForegroundClarification{Store: store, Service: conversation.NewService(store)}
	var err error
	conversationtest.SeedSpace(t, store.(models.SpaceStore), models.Scope{Kind: "organization", ID: "foreground-clarification"})
	f.Conversation, _, err = f.Service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: "organization", ID: "foreground-clarification"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "browser-agent"},
		Title: "Browser research", IdempotencyKey: "browser-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Scheduler, err = conversationrun.NewScheduler(store, store, conversationrun.SchedulerConfig{MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.Trigger = f.Post(t, conversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"},
		Intent: models.MessageIntentQuestion, Content: "Research the requested page.", RequiresResponse: true, StartThread: threaded, IdempotencyKey: "request"})
	result, _, err := f.Scheduler.ScheduleMessage(t.Context(), f.Conversation.Scope, f.Conversation.ID, f.Trigger.ID)
	if err != nil || result == nil || result.Run == nil {
		t.Fatalf("schedule foreground: %#v %v", result, err)
	}
	f.Run = result.Run
	return f
}

// ClaimedForegroundRun schedules a chat message and claims its foreground Run.
func ClaimedForegroundRun(t *testing.T, store Store) *ForegroundClarification {
	t.Helper()
	f := NewForegroundClarification(t, store, false)
	f.Run = ClaimRun(t, store, f.Run, "foreground-worker")
	f.Asked = AskUserQuestion
	return f
}

// Post posts req to the chat's channel at its current revision.
func (f *ForegroundClarification) Post(t *testing.T, req conversation.PostChannelMessageRequest) *models.ChannelMessage {
	t.Helper()
	current, err := f.Service.GetConversation(t.Context(), f.Conversation.Scope, f.Conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Scope, req.ConversationID, req.ExpectedRevision = current.Scope, current.ID, current.Revision
	req.Audience = models.ConversationAudience{Kind: models.ConversationAudienceChannel}
	result, err := f.Service.PostChannelMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

// Ask parks the foreground Run on an ask_user call (wakeType user_message),
// or on another event wait that is not a question.
func (f *ForegroundClarification) Ask(t *testing.T, question, wakeType string) {
	t.Helper()
	claimed := ClaimRun(t, f.Store, f.Run, "foreground-worker")
	before, _ := f.Store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.Run.Scope, ConversationID: f.Conversation.ID, Limit: 100})
	if wakeType == "user_message" {
		parked, call := AskUserParked(t, f.Store, claimed, "foreground-worker", question)
		f.Run, f.Turn, f.Asked = parked, call.Step, question
		return
	}
	parked, _, err := activity.NewRunActivityService(f.Store, f.Store).TransitionRun(t.Context(), claimed.Scope, claimed.ID, activity.RunTransitionRequest{
		ExpectedRevision: claimed.Revision, Status: models.AgentRunStatusWaitingForEvent, LeaseOwner: "foreground-worker",
		WakeCondition: &models.WakeCondition{Type: wakeType, Reference: f.Conversation.ID}, AppliedTurn: claimed.LastAppliedTurn + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Run, f.Turn, f.Asked = parked, nil, question
	after, _ := f.Store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.Run.Scope, ConversationID: f.Conversation.ID, Limit: 100})
	if len(before) != len(after) {
		t.Fatal("a non-question wait posted a message")
	}
}

// ParkedQuestionKey is the idempotency key of the question run is parked on,
// "" when it is not parked.
func ParkedQuestionKey(run *models.AgentRun) string {
	if run == nil || run.WakeCondition == nil {
		return ""
	}
	return conversation.ClarificationKey(run.ID, run.WakeCondition.Reference)
}

// Question reconciles the scope and returns the question the Run is parked
// on, failing unless it replies to the trigger with the asked text.
func (f *ForegroundClarification) Question(t *testing.T) *models.ChannelMessage {
	t.Helper()
	if _, err := f.Scheduler.ReconcileScope(t.Context(), f.Run.Scope); err != nil {
		t.Fatal(err)
	}
	question, err := f.Store.FindChannelMessageByIdempotencyKey(t.Context(), f.Run.Scope, f.Conversation.ID, ParkedQuestionKey(f.Run))
	if err != nil || question == nil || question.Intent != models.MessageIntentQuestion || !question.RequiresResponse || question.ReplyToMessageID != f.Trigger.ID ||
		question.Content != f.Asked || len(question.References) != 1 || question.References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: f.Run.ID}) {
		t.Fatalf("foreground question = %#v %v", question, err)
	}
	return question
}

// Current reads the foreground Run.
func (f *ForegroundClarification) Current(t *testing.T) *models.AgentRun {
	t.Helper()
	run, err := f.Store.GetAgentRun(t.Context(), f.Run.Scope, f.Run.ID)
	if err != nil || run == nil {
		t.Fatalf("read foreground: %#v %v", run, err)
	}
	return run
}

// AssertOnlyRun fails when the scope holds any Run but the foreground one,
// or the foreground Run became a conversation task.
func (f *ForegroundClarification) AssertOnlyRun(t *testing.T) {
	t.Helper()
	runs, err := f.Store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: f.Run.Scope, Limit: 100})
	if err != nil || len(runs) != 1 || runs[0].ID != f.Run.ID || runs[0].Context[models.ConversationTaskContextKey] != nil {
		t.Fatalf("clarification forked or promoted foreground work: %#v %v", runs, err)
	}
}

// AskUserStore is what an ask_user call needs.
type AskUserStore interface {
	models.KernelStore
	models.ConversationStore
}

// AskUserCall is one claimed Run with a finished, applied step whose assistant
// entry called ask_user (entry pending), ready to settle.
type AskUserCall struct {
	Store   AskUserStore
	Worker  string
	Run     *models.AgentRun // the claimed Run, LastAppliedTurn = the step
	Session *models.AgentSession
	Step    *models.AgentTurn
	Entry   *models.SessionEntry
	Exec    *control.Executor
}

// NewAskUserCall records an ask_user call on claimed, a Run leased by worker.
func NewAskUserCall(t *testing.T, store AskUserStore, claimed *models.AgentRun, worker, question string) *AskUserCall {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"question": question})
	return NewAskUserCallWithArguments(t, store, claimed, worker, arguments)
}

// NewAskUserCallWithArguments records an ask_user call with raw arguments.
func NewAskUserCallWithArguments(t *testing.T, store AskUserStore, claimed *models.AgentRun, worker string, arguments json.RawMessage) *AskUserCall {
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
	return &AskUserCall{
		Store: store, Worker: worker, Run: run, Session: session,
		Step:  &models.AgentTurn{ID: stepID, RunID: run.ID, Sequence: sequence, Status: models.AgentTurnStatusCompleted},
		Entry: appended.Entries[1],
		Exec:  &control.Executor{Portfolio: store, Sessions: sessions, Conversations: conversation.NewService(store)},
	}
}

func askUserSessions(t *testing.T, store AskUserStore) models.AgentSessionStore {
	t.Helper()
	sessions, ok := store.(models.AgentSessionStore)
	if !ok {
		t.Fatalf("%T is not a session store", store)
	}
	return sessions
}

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

// Context is the control context of the call's entry, with ask_user offered.
func (c *AskUserCall) Context() agentrun.ControlContext {
	return control.NewContext(agentrun.StepToolsInput{
		Run: c.Run, Step: c.Step, Session: c.Session, WorkerID: c.Worker,
		Binding: &agentrun.TurnRunnerBinding{Authority: agentrun.ControlAuthority{CanAskUser: true}},
	}, c.Entry)
}

// Settle settles the ask_user entry and keeps the entry it leaves.
func (c *AskUserCall) Settle(t *testing.T) control.Result {
	t.Helper()
	result, err := c.Exec.Settle(t.Context(), c.Context())
	if err != nil {
		t.Fatalf("settle ask_user: %v", err)
	}
	if result.Entry != nil {
		c.Entry = result.Entry
	}
	return result
}

// Park applies the settle's park the way the loop does.
func (c *AskUserCall) Park(t *testing.T, park *agentrun.LoopPark) *models.AgentRun {
	t.Helper()
	current, err := c.Store.GetAgentRun(t.Context(), c.Run.Scope, c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked, _, err := activity.NewRunActivityService(c.Store, c.Store).TransitionRun(t.Context(), c.Run.Scope, c.Run.ID, activity.RunTransitionRequest{
		ExpectedRevision: current.Revision, Status: park.Status, WakeCondition: park.Wake, LeaseOwner: c.Worker,
		AppliedTurn: c.Step.Sequence, EventType: "run.waiting_for_user", Summary: park.Summary,
	})
	if err != nil {
		t.Fatalf("park on ask_user: %v", err)
	}
	return parked
}

// AskUserParked settles an ask_user call of a claimed Run and parks it.
func AskUserParked(t *testing.T, store AskUserStore, claimed *models.AgentRun, worker, question string) (*models.AgentRun, *AskUserCall) {
	t.Helper()
	call := NewAskUserCall(t, store, claimed, worker, question)
	result := call.Settle(t)
	if result.Kind != control.Parked || result.Park == nil {
		t.Fatalf("ask_user did not park: %#v", result)
	}
	return call.Park(t, result.Park), call
}

// Resume claims the Run again and resumes the ask_user entry.
func (c *AskUserCall) Resume(t *testing.T) (control.Resume, *models.AgentRun) {
	t.Helper()
	claimed := ClaimRun(t, c.Store, c.Run, c.Worker)
	entry, err := askUserSessions(t, c.Store).GetSessionEntry(t.Context(), claimed.Scope, c.Entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Run, c.Entry = claimed, entry
	resumed, err := c.Exec.Resume(t.Context(), c.Context())
	if err != nil {
		t.Fatalf("resume ask_user: %v", err)
	}
	if resumed.Resolved && resumed.Result.Entry != nil {
		c.Entry = resumed.Result.Entry
	}
	return resumed, claimed
}

// ClaimRun claims run for worker, failing when another Run is claimed.
func ClaimRun(t *testing.T, store AskUserStore, run *models.AgentRun, worker string) *models.AgentRun {
	t.Helper()
	claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{
		Scope: run.Scope, Kind: run.Kind, WorkerID: worker, Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim %s: %#v %v", run.ID, claimed, err)
	}
	return claimed
}

// AskUserResultValue decodes the JSON result of an ask_user entry.
func AskUserResultValue(t *testing.T, entry *models.SessionEntry) map[string]interface{} {
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
