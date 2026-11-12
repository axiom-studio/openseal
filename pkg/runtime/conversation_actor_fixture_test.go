package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The external conversation run tests (actor authority, clarifications and
// Team approval replacement) park Runs with control's ask_user tool, so they
// import runtime and cannot use its in-package test helpers. These are
// copies of the helpers they need from runtime's tests; the ask_user ones
// belong to control's tests and move to a control test-support package when
// control is extracted. Change the copies together with the originals.

// Copied from conversation_run_test.go
func mustConversationRunScheduler(t *testing.T, store *sqlite.Store) *runtime.ConversationRunScheduler {
	t.Helper()
	scheduler, err := runtime.NewConversationRunScheduler(store, store, runtime.ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

// Copied from control_ask_user_test.go
// askUserParked settles an ask_user call of a claimed Run and parks it.
func askUserParked(t *testing.T, store askUserTestStore, claimed *models.AgentRun, worker, question string) (*models.AgentRun, *askUserCall) {
	t.Helper()
	call := newAskUserCall(t, store, claimed, worker, question)
	result := call.settle(t)
	if result.Kind != runtime.ControlParked || result.Park == nil {
		t.Fatalf("ask_user did not park: %#v", result)
	}
	return call.park(t, result.Park), call
}

// Copied from control_ask_user_test.go
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

// Copied from conversation_task_clarification_test.go
// clarificationQuestionKeyOf is the idempotency key of the question a Run
// waiting on an ask_user entry posted: the ask_user settle and the question
// reconciler post under it, so either replays the other. The tests spell it
// out to pin that key.
func clarificationQuestionKeyOf(run *models.AgentRun) string {
	if run == nil || run.WakeCondition == nil {
		return ""
	}
	return "conversation-clarification:" + run.ID + ":" + run.WakeCondition.Reference
}

// Copied from conversation_follow_up_test.go
// runHasFollowUp reports whether messageID was delivered into run as a
// follow-up.
func runHasFollowUp(run *models.AgentRun, messageID string) bool {
	return slices.ContainsFunc(models.ConversationRunFollowUps(run), func(delivery models.ConversationMessageDelivery) bool {
		return delivery.MessageID == messageID
	})
}

// Copied from run_cancellation_test.go
func cancellationActionCatalog(t *testing.T) (*skill.Catalog, models.Scope) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "cancellation"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "forum", Version: "1.0.0", Name: "Forum", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://forum.invalid"},
		Actions: map[string]skill.Action{"reply": {
			Name: "reply", Description: "Post a reviewed reply", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"body": map[string]interface{}{"type": "string"}}, "required": []interface{}{"body"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencyRequired,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "forum-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-agent",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"reply"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}

// Copied from control_ask_user_test.go
type askUserTestStore interface {
	models.KernelStore
	models.ConversationStore
}

// Copied from conversation_run_test.go
func postConversationRunTestMessage(
	t *testing.T,
	service *runtimeconversation.ConversationService,
	conversation *models.Conversation,
	senderType models.ConversationParticipantType,
	intent models.ConversationMessageIntent,
	content string,
	key string,
) *models.ChannelMessage {
	t.Helper()
	result, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: senderType, ID: "sender-" + string(senderType)},
		Intent: intent, Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

// Copied from control_ask_user_test.go
// askUserCall is one claimed Run with a finished, applied step whose assistant
// entry called ask_user (entry pending), ready to settle.
type askUserCall struct {
	store   askUserTestStore
	worker  string
	run     *models.AgentRun // the claimed Run, LastAppliedTurn = the step
	session *models.AgentSession
	step    *models.AgentTurn
	entry   *models.SessionEntry
	exec    *runtime.ControlExecutor
}

// Copied from control_ask_user_test.go
func (c *askUserCall) context() agentrun.ControlContext {
	return runtime.NewControlContext(agentrun.StepToolsInput{
		Run: c.run, Step: c.step, Session: c.session, WorkerID: c.worker,
		Binding: &agentrun.TurnRunnerBinding{Authority: agentrun.ControlAuthority{CanAskUser: true}},
	}, c.entry)
}

// Copied from control_ask_user_test.go
func (c *askUserCall) settle(t *testing.T) runtime.ControlResult {
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

// Copied from control_ask_user_test.go
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

// Copied from control_ask_user_test.go
// newAskUserCall records an ask_user call on claimed, a Run leased by worker.
func newAskUserCall(t *testing.T, store askUserTestStore, claimed *models.AgentRun, worker, question string) *askUserCall {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"question": question})
	return newAskUserCallWithArguments(t, store, claimed, worker, arguments)
}

// Copied from control_ask_user_test.go
func askUserSessions(t *testing.T, store askUserTestStore) models.AgentSessionStore {
	t.Helper()
	sessions, ok := store.(models.AgentSessionStore)
	if !ok {
		t.Fatalf("%T is not a session store", store)
	}
	return sessions
}

// Copied from control_ask_user_test.go
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
		exec:  &runtime.ControlExecutor{Portfolio: store, Sessions: sessions, Conversations: runtimeconversation.NewConversationService(store)},
	}
}

// Copied from control_ask_user_test.go
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
