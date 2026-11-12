package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// proposePersonAction proposes send as the governed tool call of run loop
// step stepID, as the loop does for the Run's lease owner.
func proposePersonAction(t *testing.T, store *sqlite.Store, catalog *skill.Catalog, scope models.Scope, run *models.AgentRun, now time.Time, stepID, key string, policy action.ActionPolicyEvaluator) *models.ActionProposalResult {
	t.Helper()
	if policy == nil {
		policy = action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
			return action.ActionPolicyDecision{Disposition: action.ActionDispositionAllow}, nil
		})
	}
	coordinator := action.NewActionCoordinator(store, store, catalog, policy, nil, models.WithClock(func() time.Time { return now }))
	proposal, err := coordinator.Propose(context.Background(), action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, TurnID: stepID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop, Summary: "Send a message to ana@example.com",
		SkillID: "mail", SkillVersion: "1.0.0", Action: "send", Arguments: map[string]interface{}{"to": "ana@example.com"}, IdempotencyKey: key,
		ExternalOperation: &activity.ExternalOperationIdentity{Resource: "mailbox-ana", Operation: "send"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

// executePersonAction executes the loop call inline for the Run's lease
// owner, as the governed executor does.
func executePersonAction(t *testing.T, worker *action.ActionWorker, scope models.Scope, proposal *models.ActionProposalResult) *models.ActionExecutionResult {
	t.Helper()
	result, err := worker.ExecuteOwnLoopCall(context.Background(), scope, proposal.Call.ID, "agent-worker")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestActionWorkerFailsPersonActionWithoutPerson(t *testing.T) {
	store := kerneltest.NewStore(t)
	now := time.Now().UTC()
	catalog, scope := actiontest.PersonCatalog(t)
	run := actiontest.ClaimedPersonChatRun(t, store, scope, now, "")
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	dispatched := false
	worker := action.NewActionWorker(store, catalog, action.CredentialResolverFunc(func(context.Context, action.CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"google": "token"}, nil
	}), action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		dispatched = true
		return map[string]interface{}{}, nil
	}))
	worker.SetCredentialAvailabilityChecker(actiontest.AvailabilityFunc(func(context.Context, action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
		t.Fatal("a run without a person is refused before the host check")
		return action.CredentialAvailability{}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	if dispatched || result.Call.Status != models.ActionCallStatusFailed || result.Call.ErrorCode != skillbinding.CredentialErrorPersonUnavailable ||
		!strings.Contains(result.Call.Error, "uses each person's own account, and no person asked for this run") {
		t.Fatalf("call = %#v dispatched=%v", result.Call, dispatched)
	}
	requests, err := store.ListSkillSetupRequests(context.Background(), scope, "release-agent", "conversation-1")
	if err != nil || len(requests) != 0 {
		t.Fatalf("no person must not create a card: %#v %v", requests, err)
	}
	stored, _ := store.GetAgentRun(context.Background(), scope, proposal.Call.RunID)
	if stored.WakeCondition != nil && stored.WakeCondition.Type == models.ConnectAccountWakeType {
		t.Fatalf("run parked without a card: %#v", stored.WakeCondition)
	}
}

// The same external operation of a parked call is admitted again in another
// run: its digest was released when it parked.
func TestParkedCallReleasesExternalOperation(t *testing.T) {
	store := kerneltest.NewStore(t)
	now := time.Now().UTC()
	catalog, scope := actiontest.PersonCatalog(t)
	run := actiontest.ClaimedPersonChatRun(t, store, scope, now, actiontest.ActingUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	worker := action.NewActionWorker(store, catalog, action.CredentialResolverFunc(func(context.Context, action.CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"google": "token"}, nil
	}), action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	}))
	worker.SetCredentialAvailabilityChecker(actiontest.AvailabilityFunc(func(context.Context, action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
		return action.CredentialAvailability{Gaps: map[string]action.CredentialGap{"google": action.CredentialGapPersonSignIn}}, nil
	}))
	executePersonAction(t, worker, scope, proposal)
	digest := proposal.Call.ExternalOperationDigest
	if prior, err := store.GetActionCallByExternalOperation(context.Background(), scope, digest); prior != nil || err == nil {
		t.Fatalf("parked call still holds its external operation: %#v %v", prior, err)
	}
}

func TestActionWorkerSharedGapAsksManagers(t *testing.T) {
	store := kerneltest.NewStore(t)
	now := time.Now().UTC()
	catalog, scope := actiontest.PersonCatalog(t)
	run := actiontest.ClaimedPersonChatRun(t, store, scope, now, actiontest.ActingUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	worker := action.NewActionWorker(store, catalog, action.CredentialResolverFunc(func(context.Context, action.CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"google": "token"}, nil
	}), action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		t.Fatal("a shared gap must not dispatch")
		return nil, nil
	}))
	worker.SetCredentialAvailabilityChecker(actiontest.AvailabilityFunc(func(context.Context, action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
		return action.CredentialAvailability{Gaps: map[string]action.CredentialGap{"google": action.CredentialGapSharedSignIn}}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	card, err := store.GetSkillSetupRequest(context.Background(), scope, "connect-account:"+result.Call.ID)
	if err != nil || card == nil || card.Mode != "shared" || card.RequestedFor != "" ||
		card.Reason != "The Google account shared with this Agent is no longer available." {
		t.Fatalf("shared card = %#v, %v", card, err)
	}
	if summary := skillbinding.ConnectAccountWaitSummary(card); summary != "Waiting for someone who manages this Agent to connect Google" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestActionWorkerNoPersonGapFailsWithoutCard(t *testing.T) {
	store := kerneltest.NewStore(t)
	now := time.Now().UTC()
	catalog, scope := actiontest.PersonCatalog(t)
	run := actiontest.ClaimedPersonChatRun(t, store, scope, now, actiontest.ActingUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", nil)
	worker := action.NewActionWorker(store, catalog, nil, action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		t.Fatal("a no_person gap must not dispatch")
		return nil, nil
	}))
	worker.SetCredentialAvailabilityChecker(actiontest.AvailabilityFunc(func(context.Context, action.CredentialAvailabilityRequest) (action.CredentialAvailability, error) {
		return action.CredentialAvailability{Gaps: map[string]action.CredentialGap{"google": action.CredentialGapNoPerson}}, nil
	}))
	result := executePersonAction(t, worker, scope, proposal)
	if result.Call.ErrorCode != skillbinding.CredentialErrorPersonUnavailable || skillbinding.IsParkedConnectAccountCall(result.Call) {
		t.Fatalf("call = %#v", result.Call)
	}
	requests, _ := store.ListSkillSetupRequests(context.Background(), scope, "release-agent", "conversation-1")
	if len(requests) != 0 {
		t.Fatalf("no_person created a card: %#v", requests)
	}
}

func TestPersonActionApprovalOnlyByActingUser(t *testing.T) {
	store := kerneltest.NewStore(t)
	now := time.Now().UTC()
	catalog, scope := actiontest.PersonCatalog(t)
	run := actiontest.ClaimedPersonChatRun(t, store, scope, now, actiontest.ActingUser)
	proposal := proposePersonAction(t, store, catalog, scope, run, now, "step-1", "send-1", action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionRequireApproval, EligibleApprovers: []models.ApprovalPrincipal{{Type: "role", ID: "operator"}, {Type: "user", ID: "9"}}, ApprovalTTL: time.Hour}, nil
	}))
	if proposal.Approval == nil || len(proposal.Approval.EligibleApprovers) != 1 || proposal.Approval.EligibleApprovers[0] != (models.ApprovalPrincipal{Type: "user", ID: actiontest.ActingUser}) {
		t.Fatalf("approval = %#v", proposal.Approval)
	}
}

func TestConnectAppKeyMatchesHostKey(t *testing.T) {
	if key := skillbinding.ConnectAppKey(models.ConnectAppTarget{CatalogSlug: "linear"}); key != "linear" {
		t.Fatalf("catalog key = %q", key)
	}
	key := skillbinding.ConnectAppKey(models.ConnectAppTarget{Address: "https://mcp.example.com/sse"})
	if !strings.HasPrefix(key, "custom:") || len(key) != len("custom:")+24 {
		t.Fatalf("custom key = %q", key)
	}
}

// A request_setup receipt of the previous bundled version still verifies a
// parked setup task.
func TestTaskSetupActionMatchesPreviousBundledVersion(t *testing.T) {
	run := &models.AgentRun{Scope: models.Scope{Kind: "tenant", ID: "one"}, ID: "run"}
	call := &models.ActionCall{Scope: run.Scope, RunID: run.ID, DeploymentID: "agent", SkillID: models.SkillManagementSkillID, SkillVersion: "1.6.0", Action: agentrun.SkillActionRequestSetup}
	if !skillbinding.TaskSetupActionMatches(call, run, "agent") {
		t.Fatal("a 1.6.0 request_setup receipt no longer verifies")
	}
}
