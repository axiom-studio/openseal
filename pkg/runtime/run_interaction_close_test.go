package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// interactionCallsStore adds recorded interaction calls to a store's listing.
type interactionCallsStore struct {
	*sqlite.Store
	calls []*ActionCall
}

func (s interactionCallsStore) ListActionCalls(ctx context.Context, filter ActionFilter) ([]*ActionCall, error) {
	calls, err := s.Store.ListActionCalls(ctx, filter)
	if err != nil {
		return nil, err
	}
	for _, call := range s.calls {
		if call.Scope == filter.Scope && call.RunID == filter.RunID {
			calls = append(calls, call)
		}
	}
	return calls, nil
}

func interactionCall(scope Scope, runID, id, action, key, requestID string) *ActionCall {
	return &ActionCall{ID: id, Scope: scope, RunID: runID, DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion,
		Action: action, Status: ActionCallStatusSucceeded, Output: map[string]interface{}{key: map[string]interface{}{"id": requestID}}}
}

// Dev run e655b041 was canceled while its credential request was still
// pending, and the card stayed in the chat. Ending a Run closes the in-chat
// requests it left open.
func TestCanceledRunDismissesItsOpenInteractionRequests(t *testing.T) {
	ctx, store, catalog, dispatcher, scope := skillbindingtest.CredentialRequestDispatchFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, agentrun.SkillActionRequestCredential, map[string]interface{}{
		"kind": "payment_card", "reason": "Add your card so I can pay 319.00 INR on amazon.in"})
	run := f.run
	executed := f.execute(NewActionWorker(store, catalog, nil, dispatcher), call.ID)
	card, _ := executed.Call.Output["credentialRequest"].(map[string]interface{})
	requestID, _ := card["id"].(string)
	if executed.Call.Status != ActionCallStatusSucceeded || requestID == "" {
		t.Fatalf("credential action: %#v", executed.Call)
	}
	// The loop parks the Run on the card at step end.
	f.park(AgentRunStatusWaitingForEvent, &WakeCondition{Type: skillbinding.CredentialRequestWakeType, Reference: requestID})
	parked := f.current()
	// A pending Skill setup request of the same Run is closed too.
	setup := &SkillSetupRequest{ID: "setup-1", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: run.ID,
		ActionCallID: "setup-call", Kind: "configure", SkillID: "skill-x", SkillVersion: "1.0.0", SkillName: "X", Reason: "Connect X",
		Status: "pending", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.SaveSkillSetupRequest(ctx, setup, 0); err != nil {
		t.Fatal(err)
	}
	commands := agentrun.NewRunCommandService(interactionCallsStore{store, []*ActionCall{interactionCall(scope, run.ID, "setup-call", agentrun.SkillActionRequestSetup, "setupRequest", "setup-1")}})
	canceled, err := commands.CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{Scope: scope, RunID: run.ID, ExpectedRevision: parked.Revision,
		Kind: agentrun.AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "user-1"}})
	if err != nil || canceled.Run.Status != AgentRunStatusCanceled {
		t.Fatalf("cancel: %#v %v", canceled, err)
	}
	requests, err := store.ListCredentialRequests(ctx, scope, "agent", "chat")
	if err != nil || len(requests) != 1 || requests[0].Status != CredentialRequestStatusDismissed || requests[0].ResolvedBy != agentrun.TerminalRunInteractionCloser {
		t.Fatalf("credential requests after cancel: %#v %v", requests, err)
	}
	closed, err := store.GetSkillSetupRequest(ctx, scope, "setup-1")
	if err != nil || closed.Status != "dismissed" || closed.ResolvedBy != agentrun.TerminalRunInteractionCloser {
		t.Fatalf("setup request after cancel: %#v %v", closed, err)
	}
	// Reconciling the terminal Run again is a no-op.
	if err := commands.CascadeTerminalRun(ctx, canceled.Run); err != nil {
		t.Fatal(err)
	}
}

// A completed foreground reply may leave a Skill setup form for the user to
// finish later; only its credential requests (which resume nothing now) close.
func TestCompletedRunKeepsSetupFormsButClosesCredentialRequests(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusFailed} {
		t.Run(string(status), func(t *testing.T) { testFinishedRunKeepsSetupForms(t, status) })
	}
}

// A Run that failed after its setup form went up leaves the form open too.
func testFinishedRunKeepsSetupForms(t *testing.T, status AgentRunStatus) {
	ctx, store, _, _, scope := skillbindingtest.CredentialRequestDispatchFixture(t)
	now := time.Now().UTC()
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "Reply in the chat", Source: RunSourceChat})
	if err != nil {
		t.Fatal(err)
	}
	run := cloneAgentRun(created)
	run.Status = status
	run.Revision++
	if _, err := store.UpdateAgentRunWithEvent(ctx, run, created.Revision, &ActivityEvent{
		ID: "run-finished", Scope: scope, EventType: "run.updated", Severity: ActivitySeverityInfo, AgentID: "agent", RunID: run.ID,
		Actor: ActivityActor{Type: "worker", ID: "worker"}, Summary: "Run finished", Visibility: ActivityVisibilityScope, CreatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	credential := &CredentialRequest{ID: "credential-1", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: run.ID, ActionCallID: "c1",
		Kind: CredentialRequestKindPaymentCard, Reason: "Add your card", Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveCredentialRequest(ctx, credential, 0); err != nil {
		t.Fatal(err)
	}
	setup := &SkillSetupRequest{ID: "setup-2", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: run.ID,
		ActionCallID: "c2", Kind: "configure", SkillID: "skill-x", SkillVersion: "1.0.0", SkillName: "X", Reason: "Connect X",
		Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveSkillSetupRequest(ctx, setup, 0); err != nil {
		t.Fatal(err)
	}
	calls := interactionCallsStore{store, []*ActionCall{
		interactionCall(scope, run.ID, "c1", agentrun.SkillActionRequestCredential, "credentialRequest", "credential-1"),
		interactionCall(scope, run.ID, "c2", agentrun.SkillActionRequestSetup, "setupRequest", "setup-2")}}
	if err := agentrun.NewRunCommandService(calls).CascadeTerminalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetCredentialRequest(ctx, scope, "credential-1"); got.Status != CredentialRequestStatusDismissed {
		t.Fatalf("credential request = %#v", got)
	}
	if got, _ := store.GetSkillSetupRequest(ctx, scope, "setup-2"); got.Status != "pending" {
		t.Fatalf("setup request = %#v", got)
	}
}
