package runtime

import (
	"context"
	"testing"
	"time"
)

// interactionCallsStore adds recorded interaction calls to a store's listing.
type interactionCallsStore struct {
	*MemoryStore
	calls []*ActionCall
}

func (s interactionCallsStore) ListActionCalls(ctx context.Context, filter ActionFilter) ([]*ActionCall, error) {
	calls, err := s.MemoryStore.ListActionCalls(ctx, filter)
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
	return &ActionCall{ID: id, Scope: scope, RunID: runID, DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion,
		Action: action, Status: ActionCallStatusSucceeded, Output: map[string]interface{}{key: map[string]interface{}{"id": requestID}}}
}

// Dev run e655b041 was canceled while its credential request was still
// pending, and the card stayed in the chat. Ending a Run closes the in-chat
// requests it left open.
func TestCanceledRunDismissesItsOpenInteractionRequests(t *testing.T) {
	ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
	run := proposeInteraction(t, ctx, store, catalog, scope, SkillActionRequestCredential, map[string]interface{}{
		"kind": "payment_card", "reason": "Add your card so I can pay 319.00 INR on amazon.in"})
	if _, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	parked, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || parked.Status != AgentRunStatusWaitingForEvent {
		t.Fatalf("run not waiting: %#v %v", parked, err)
	}
	// A pending Skill setup request of the same Run is closed too.
	setup := &SkillSetupRequest{ID: "setup-1", Scope: scope, DeploymentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: run.ID,
		ActionCallID: "setup-call", Kind: "configure", SkillID: "skill-x", SkillVersion: "1.0.0", SkillName: "X", Reason: "Connect X",
		Status: "pending", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.SaveSkillSetupRequest(ctx, setup, 0); err != nil {
		t.Fatal(err)
	}
	commands := NewRunCommandService(interactionCallsStore{store, []*ActionCall{interactionCall(scope, run.ID, "setup-call", SkillActionRequestSetup, "setupRequest", "setup-1")}})
	canceled, err := commands.CommandAgentRun(ctx, AgentRunCommandRequest{Scope: scope, RunID: run.ID, ExpectedRevision: parked.Revision,
		Kind: AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "user-1"}})
	if err != nil || canceled.Run.Status != AgentRunStatusCanceled {
		t.Fatalf("cancel: %#v %v", canceled, err)
	}
	requests, err := store.ListCredentialRequests(ctx, scope, "agent", "chat")
	if err != nil || len(requests) != 1 || requests[0].Status != CredentialRequestStatusDismissed || requests[0].ResolvedBy != TerminalRunInteractionCloser {
		t.Fatalf("credential requests after cancel: %#v %v", requests, err)
	}
	closed, err := store.GetSkillSetupRequest(ctx, scope, "setup-1")
	if err != nil || closed.Status != "dismissed" || closed.ResolvedBy != TerminalRunInteractionCloser {
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
	ctx, store, _, _, scope := credentialRequestDispatchFixture(t)
	run := &AgentRun{ID: "run-done", Scope: scope, Status: AgentRunStatusCompleted}
	now := time.Now().UTC()
	credential := &CredentialRequest{ID: "credential-1", Scope: scope, DeploymentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: run.ID, ActionCallID: "c1",
		Kind: CredentialRequestKindPaymentCard, Reason: "Add your card", Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveCredentialRequest(ctx, credential, 0); err != nil {
		t.Fatal(err)
	}
	setup := &SkillSetupRequest{ID: "setup-2", Scope: scope, DeploymentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: run.ID,
		ActionCallID: "c2", Kind: "configure", SkillID: "skill-x", SkillVersion: "1.0.0", SkillName: "X", Reason: "Connect X",
		Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveSkillSetupRequest(ctx, setup, 0); err != nil {
		t.Fatal(err)
	}
	calls := interactionCallsStore{store, []*ActionCall{
		interactionCall(scope, run.ID, "c1", SkillActionRequestCredential, "credentialRequest", "credential-1"),
		interactionCall(scope, run.ID, "c2", SkillActionRequestSetup, "setupRequest", "setup-2")}}
	if err := closeTerminalRunInteractionRequests(ctx, calls, run); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetCredentialRequest(ctx, scope, "credential-1"); got.Status != CredentialRequestStatusDismissed {
		t.Fatalf("credential request = %#v", got)
	}
	if got, _ := store.GetSkillSetupRequest(ctx, scope, "setup-2"); got.Status != "pending" {
		t.Fatalf("setup request = %#v", got)
	}
}
