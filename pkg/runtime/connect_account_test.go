package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const connectAccountTestUser = "7"

// personActionCatalog binds an OAuth action of release-agent whose google
// slot is per_person.
func personActionCatalog(t *testing.T) (*skill.Catalog, Scope) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "mail", Version: "1.0.0", Name: "Mail", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://mail.invalid"},
		Actions: map[string]skill.Action{"send": {
			Name: "send", Description: "Send a message", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelWrite,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"to": map[string]interface{}{"type": "string"}}, "required": []interface{}{"to"}},
			Credentials: []skill.CredentialRequirement{{Name: "google", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{Provider: "google", Subject: "user", Scopes: []string{"mail.send"}}}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 3}, Idempotency: skill.IdempotencySupported,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "mail-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: "mail", SkillVersion: "1.0.0", AllowedActions: []string{"send"}, MaximumRisk: skill.RiskLevelWrite,
		Credentials: map[string]skill.CredentialReference{"google": {Kind: "oauth2", ID: skill.PersonCredentialReferenceID}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}

// claimedPersonChatRun claims a 1:1 chat run of release-agent acting for
// actingUser.
func claimedPersonChatRun(t *testing.T, store *sqlite.Store, scope Scope, now time.Time, actingUser string) *AgentRun {
	t.Helper()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "send mail",
		Source: RunSourceObjective, Kind: RunKindConversation, ActingUser: actingUser,
		Context: map[string]interface{}{conversationRunContextConversationID: "conversation-1", conversationRunContextTriggerID: "message-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return claimed
}

// proposePersonAction proposes send as the governed tool call of run loop
// step stepID, as the loop does for the Run's lease owner.
func proposePersonAction(t *testing.T, store *sqlite.Store, catalog *skill.Catalog, scope Scope, run *AgentRun, now time.Time, stepID, key string, policy ActionPolicyEvaluator) *ActionProposalResult {
	t.Helper()
	if policy == nil {
		policy = ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
			return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
		})
	}
	coordinator := NewActionCoordinator(store, store, catalog, policy)
	coordinator.now = func() time.Time { return now }
	proposal, err := coordinator.Propose(context.Background(), ProposeActionRequest{
		Scope: scope, RunID: run.ID, TurnID: stepID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		Origin: ActionOriginLoop, Execution: ActionExecutionLoop, Summary: "Send a message to ana@example.com",
		SkillID: "mail", SkillVersion: "1.0.0", Action: "send", Arguments: map[string]interface{}{"to": "ana@example.com"}, IdempotencyKey: key,
		ExternalOperation: &ExternalOperationIdentity{Resource: "mailbox-ana", Operation: "send"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

// executePersonAction executes the loop call inline for the Run's lease
// owner, as the governed executor does.
func executePersonAction(t *testing.T, worker *ActionWorker, scope Scope, proposal *ActionProposalResult) *ActionExecutionResult {
	t.Helper()
	result, err := worker.ExecuteOwnLoopCall(context.Background(), scope, proposal.Call.ID, "agent-worker")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type availabilityFunc func(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error)

func (f availabilityFunc) CheckCredentialAvailability(ctx context.Context, req CredentialAvailabilityRequest) (CredentialAvailability, error) {
	return f(ctx, req)
}

func TestActionWorkerFailsPersonActionWithoutPerson(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, "")
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	dispatched := false
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"google": "token"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatched = true
		return map[string]interface{}{}, nil
	}))
	worker.setCredentialAvailabilityChecker(availabilityFunc(func(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error) {
		t.Fatal("a run without a person is refused before the host check")
		return CredentialAvailability{}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	if dispatched || result.Call.Status != ActionCallStatusFailed || result.Call.ErrorCode != CredentialErrorPersonUnavailable ||
		!strings.Contains(result.Call.Error, "uses each person's own account, and no person asked for this run") {
		t.Fatalf("call = %#v dispatched=%v", result.Call, dispatched)
	}
	requests, err := store.ListSkillSetupRequests(context.Background(), scope, "release-agent", "conversation-1")
	if err != nil || len(requests) != 0 {
		t.Fatalf("no person must not create a card: %#v %v", requests, err)
	}
	stored, _ := store.GetAgentRun(context.Background(), scope, proposal.Call.RunID)
	if stored.WakeCondition != nil && stored.WakeCondition.Type == ConnectAccountWakeType {
		t.Fatalf("run parked without a card: %#v", stored.WakeCondition)
	}
}

func TestActionWorkerParksPersonGapOnCardAndRetries(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	if proposal.Call.ExternalOperationDigest == "" {
		t.Fatal("test needs an external operation")
	}
	connected := false
	var checks []CredentialAvailabilityRequest
	var resolutions []CredentialResolutionRequest
	dispatches := 0
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(_ context.Context, req CredentialResolutionRequest) (map[string]string, error) {
		resolutions = append(resolutions, req)
		return map[string]string{"google": "token"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return map[string]interface{}{"sent": true}, nil
	}))
	worker.setCredentialAvailabilityChecker(availabilityFunc(func(_ context.Context, req CredentialAvailabilityRequest) (CredentialAvailability, error) {
		checks = append(checks, req)
		if connected {
			return CredentialAvailability{}, nil
		}
		return CredentialAvailability{Gaps: map[string]CredentialGap{"google": CredentialGapPersonSignIn}}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	call := result.Call
	if dispatches != 0 || call.Status != ActionCallStatusFailed || call.FailurePhase != ActionFailureBeforeDispatch ||
		!strings.HasPrefix(call.Error, "connect_account_required: ") || call.ExternalOperationDigest != "" || !isParkedConnectAccountCall(call) {
		t.Fatalf("parked call = %#v", call)
	}
	if len(checks) != 1 || checks[0].ActingUser != connectAccountTestUser || checks[0].Umbrella != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}) ||
		checks[0].BindingID != "mail-binding" || checks[0].References["google"].ID != skill.PersonCredentialReferenceID || checks[0].Requirements["google"].OAuth2 == nil {
		t.Fatalf("availability request = %#v", checks)
	}
	card, err := store.GetSkillSetupRequest(context.Background(), scope, "connect-account:"+call.ID)
	if err != nil || card == nil {
		t.Fatalf("card = %#v, %v", card, err)
	}
	if card.Kind != SkillSetupKindConnectAccount || card.Mode != "per_person" || card.RequestedFor != connectAccountTestUser || card.Status != "pending" ||
		card.ConversationID != "conversation-1" || card.RunID != run.ID || len(card.Slots) != 1 || card.Slots[0].Name != "google" ||
		card.Reason != "This Agent uses each person's own Google account." {
		t.Fatalf("card = %#v", card)
	}
	// The worker leaves the Run to its loop, which parks it on the card at
	// step end.
	stored, err := store.GetAgentRun(context.Background(), scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != AgentRunStatusRunning || stored.FailureExplanation != nil {
		t.Fatalf("a parked call changed its Run: %s %#v", stored.Status, stored.FailureExplanation)
	}
	parker := newGovernedParker(store, nil, nil)
	wake, summary, err := parker.governedCardWait(context.Background(), stored, call)
	if err != nil || wake == nil || wake.Type != ConnectAccountWakeType || wake.Reference != card.ID || summary != connectAccountWaitSummary(card) {
		t.Fatalf("card wait = %#v %q %v", wake, summary, err)
	}

	// The person connects; the run resumes and the model asks again in a new
	// step for the same external operation.
	connected = true
	card.Status, card.ResolvedBy = "resolved", connectAccountTestUser
	card.ResolvedBindingID, card.ResolvedBindingRevision = "mail-binding", 1
	card.Revision++
	if err := store.SaveSkillSetupRequest(context.Background(), card, card.Revision-1); err != nil {
		t.Fatal(err)
	}
	if wake, _, err := parker.governedCardWait(context.Background(), stored, call); err != nil || wake != nil {
		t.Fatalf("a resolved card still parks: %#v %v", wake, err)
	}
	retry := proposePersonAction(t, store, catalog, scope, stored, now, "step-2", "send-1-retry", nil)
	if !retry.Created || retry.Call.ID == call.ID || retry.Call.Status != ActionCallStatusReady || retry.Call.DuplicateOfActionCallID != "" {
		t.Fatalf("retry not admitted: %#v", retry.Call)
	}
	result = executePersonAction(t, worker, scope, retry)
	if result.Call.ID != retry.Call.ID || result.Call.Status != ActionCallStatusSucceeded || dispatches != 1 {
		t.Fatalf("retry = %#v dispatches=%d", result.Call, dispatches)
	}
	last := resolutions[len(resolutions)-1]
	if last.ActingUser != connectAccountTestUser || last.Umbrella.ID != "release-agent" || last.Requirements["google"].OAuth2 == nil {
		t.Fatalf("resolution request = %#v", last)
	}
}

// The same external operation of a parked call is admitted again in another
// run: its digest was released when it parked.
func TestParkedCallReleasesExternalOperation(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"google": "token"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	}))
	worker.setCredentialAvailabilityChecker(availabilityFunc(func(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error) {
		return CredentialAvailability{Gaps: map[string]CredentialGap{"google": CredentialGapPersonSignIn}}, nil
	}))
	executePersonAction(t, worker, scope, proposal)
	digest := proposal.Call.ExternalOperationDigest
	if prior, err := store.GetActionCallByExternalOperation(context.Background(), scope, digest); prior != nil || err == nil {
		t.Fatalf("parked call still holds its external operation: %#v %v", prior, err)
	}
}

func TestActionWorkerSharedGapAsksManagers(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"google": "token"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		t.Fatal("a shared gap must not dispatch")
		return nil, nil
	}))
	worker.setCredentialAvailabilityChecker(availabilityFunc(func(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error) {
		return CredentialAvailability{Gaps: map[string]CredentialGap{"google": CredentialGapSharedSignIn}}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	card, err := store.GetSkillSetupRequest(context.Background(), scope, "connect-account:"+result.Call.ID)
	if err != nil || card == nil || card.Mode != "shared" || card.RequestedFor != "" ||
		card.Reason != "The Google account shared with this Agent is no longer available." {
		t.Fatalf("shared card = %#v, %v", card, err)
	}
	if summary := connectAccountWaitSummary(card); summary != "Waiting for someone who manages this Agent to connect Google" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestActionWorkerNoPersonGapFailsWithoutCard(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	worker := NewActionWorker(store, catalog, nil, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		t.Fatal("a no_person gap must not dispatch")
		return nil, nil
	}))
	worker.setCredentialAvailabilityChecker(availabilityFunc(func(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error) {
		return CredentialAvailability{Gaps: map[string]CredentialGap{"google": CredentialGapNoPerson}}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	if result.Call.ErrorCode != CredentialErrorPersonUnavailable || isParkedConnectAccountCall(result.Call) {
		t.Fatalf("call = %#v", result.Call)
	}
	requests, _ := store.ListSkillSetupRequests(context.Background(), scope, "release-agent", "conversation-1")
	if len(requests) != 0 {
		t.Fatalf("no_person created a card: %#v", requests)
	}
}

func TestPersonActionApprovalOnlyByActingUser(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}, {Type: "user", ID: "9"}}, ApprovalTTL: time.Hour}, nil
	}))
	if proposal.Approval == nil || len(proposal.Approval.EligibleApprovers) != 1 || proposal.Approval.EligibleApprovers[0] != (ApprovalPrincipal{Type: "user", ID: connectAccountTestUser}) {
		t.Fatalf("approval = %#v", proposal.Approval)
	}
}

func TestConnectAppKeyMatchesHostKey(t *testing.T) {
	if key := ConnectAppKey(ConnectAppTarget{CatalogSlug: "linear"}); key != "linear" {
		t.Fatalf("catalog key = %q", key)
	}
	key := ConnectAppKey(ConnectAppTarget{Address: "https://mcp.example.com/sse"})
	if !strings.HasPrefix(key, "custom:") || len(key) != len("custom:")+24 {
		t.Fatalf("custom key = %q", key)
	}
}

// A request_setup receipt of the previous bundled version still verifies a
// parked setup task.
func TestTaskSetupActionMatchesPreviousBundledVersion(t *testing.T) {
	run := &AgentRun{Scope: Scope{Kind: "tenant", ID: "one"}, ID: "run"}
	call := &ActionCall{Scope: run.Scope, RunID: run.ID, DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: "1.6.0", Action: SkillActionRequestSetup}
	if !taskSetupActionMatches(call, run, "agent") {
		t.Fatal("a 1.6.0 request_setup receipt no longer verifies")
	}
}
