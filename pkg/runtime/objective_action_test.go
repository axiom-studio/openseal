package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestObjectiveManagementSkillVersionsRunbookOnlyContract(t *testing.T) {
	definition := action.ObjectiveManagementSkill()
	if definition.Version != "1.2.0" {
		t.Fatalf("Objective management Skill version = %q", definition.Version)
	}
	for _, actionName := range []string{action.ObjectiveActionCreate, action.ObjectiveActionUpdate} {
		action := definition.Actions[actionName]
		properties, _ := action.InputSchema["properties"].(map[string]interface{})
		if _, exists := properties["cadence"]; exists {
			t.Fatalf("%s action still exposes Objective cadence", actionName)
		}
		if _, exists := properties["eventRules"]; exists {
			t.Fatalf("%s action still exposes Objective event rules", actionName)
		}
		executionPolicy, ok := properties["executionPolicy"].(map[string]interface{})
		if !ok || executionPolicy["additionalProperties"] != false {
			t.Fatalf("%s action does not expose a strict execution policy: %#v", actionName, executionPolicy)
		}
	}
}

func TestGovernedObjectiveCreateUsesExistingApprovalAndActionLifecycle(t *testing.T) {
	for _, testCase := range objectiveActionStores() {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			ctx := context.Background()
			scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
			owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}
			catalog := objectiveActionCatalog(t, store, scope, owner.ID)
			portfolio := runtimeportfolio.NewPortfolioService(store)
			run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, Goal: "Create release objective", Source: models.RunSourceChat})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil || claimed.ID != run.ID {
				t.Fatalf("claim = %#v, %v", claimed, err)
			}
			validator, err := action.NewObjectiveActionValidator(store)
			if err != nil {
				t.Fatal(err)
			}
			f := actiontest.NewLoopProposalFixture(t, store, claimed, "conversation-worker", time.Now().UTC())
			coordinator := action.NewActionCoordinator(store, store, catalog, action.NewDefaultActionPolicy(), []action.ActionProposalValidator{validator})
			proposal, err := coordinator.Propose(ctx, f.Request(action.ProposeActionRequest{
				DeploymentID: owner.ID,
				SkillID:      models.ObjectiveManagementSkillID, SkillVersion: action.ObjectiveManagementSkillVersion, Action: action.ObjectiveActionCreate,
				Arguments:      map[string]interface{}{"title": "Publish release notes", "goal": "Create and review release notes", "priority": 2},
				IdempotencyKey: "message-42:create-release-notes", Summary: "Create objective: Publish release notes", TurnID: "turn-42",
			}, "create-release-notes"))
			if err != nil {
				t.Fatal(err)
			}
			if proposal.Approval == nil || proposal.Call.Status != models.ActionCallStatusWaitingApproval || proposal.Call.TurnID != "turn-42" {
				t.Fatalf("proposal lifecycle = %#v", proposal)
			}
			preview := proposal.Approval.ProposedAction
			if preview["resourceType"] != "objective" || preview["operation"] != action.ObjectiveActionCreate || preview["resultingStatus"] != string(models.ObjectiveStatusDraft) || preview["owner"] == nil || preview["arguments"] != nil {
				t.Fatalf("typed approval preview = %#v", preview)
			}
			dispatcher, err := action.NewObjectiveActionDispatcher(store, nil)
			if err != nil {
				t.Fatal(err)
			}
			executed := approveAndExecuteLoopAction(t, store, f, proposal, action.NewActionWorker(store, catalog, nil, dispatcher), "decision-42")
			if executed.Call.Status != models.ActionCallStatusSucceeded || executed.Call.Output["resourceType"] != "objective" || executed.Call.Output["operation"] != action.ObjectiveActionCreate || executed.Call.Output["created"] != true {
				t.Fatalf("execution = %#v", executed)
			}
			objectives, err := portfolio.ListObjectives(ctx, models.ObjectiveFilter{Scope: scope, Owner: &owner})
			if err != nil || len(objectives) != 1 || objectives[0].Status != models.ObjectiveStatusDraft || objectives[0].Title != "Publish release notes" {
				t.Fatalf("objectives = %#v, %v", objectives, err)
			}
			bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, models.ObjectiveManagementSkillID, action.ObjectiveManagementSkillVersion, action.ObjectiveActionCreate)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments})
			if err != nil || replayed["created"] != false {
				t.Fatalf("idempotent replay = %#v, %v", replayed, err)
			}
		})
	}
}

func TestObjectiveActionsRejectInjectedOwnerForeignTargetAndStaleRevisionBeforeApproval(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "marketing"}
	foreignOwner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "finance"}
	catalog := objectiveActionCatalog(t, store, scope, owner.ID)
	portfolio := runtimeportfolio.NewPortfolioService(store)
	foreign, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{Scope: scope, Owner: foreignOwner, Title: "Close books", Goal: "Close monthly books"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: "marketing-lead", Goal: "Manage campaign objectives", Source: models.RunSourceChat})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	validator, _ := action.NewObjectiveActionValidator(store)
	coordinator := action.NewActionCoordinator(store, store, catalog, action.NewDefaultActionPolicy(), []action.ActionProposalValidator{validator})
	base := actiontest.LoopProposeRequest(action.ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: owner.ID, SkillID: models.ObjectiveManagementSkillID, SkillVersion: action.ObjectiveManagementSkillVersion, Action: action.ObjectiveActionCreate, IdempotencyKey: "create"}, "create")
	base.Arguments = map[string]interface{}{"title": "Campaign", "goal": "Launch campaign", "owner": map[string]interface{}{"type": "team", "id": "finance"}}
	if _, err := coordinator.Propose(ctx, base); err == nil {
		t.Fatal("model-supplied owner should fail schema validation")
	}
	base.Action = action.ObjectiveActionUpdate
	base.Arguments = map[string]interface{}{"objectiveId": foreign.ID, "expectedRevision": foreign.Revision, "goal": "Exfiltrate finance work"}
	if _, err := coordinator.Propose(ctx, base); err == nil || !stringsContain(err.Error(), "not owned") {
		t.Fatalf("foreign target error = %v", err)
	}
	owned, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Campaign", Goal: "Launch campaign"})
	if err != nil {
		t.Fatal(err)
	}
	base.Arguments = map[string]interface{}{"objectiveId": owned.ID, "expectedRevision": owned.Revision + 1, "goal": "Grow qualified leads"}
	if _, err := coordinator.Propose(ctx, base); !errors.Is(err, models.ErrRevisionConflict) {
		t.Fatalf("stale proposal error = %v", err)
	}
	approvals, err := store.ListApprovals(ctx, models.ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(approvals) != 0 {
		t.Fatalf("invalid proposals became approvals: %#v, %v", approvals, err)
	}
}

func TestObjectivePauseRechecksOwnerAndIsReplaySafeAfterApproval(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "sre"}
	catalog := objectiveActionCatalog(t, store, scope, owner.ID)
	portfolio := runtimeportfolio.NewPortfolioService(store)
	objective, err := portfolio.CreateObjective(ctx, runtimeportfolio.CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Watch production", Goal: "Respond to incidents", Status: models.ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, Goal: "Pause production watch", Source: models.RunSourceChat})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	validator, _ := action.NewObjectiveActionValidator(store)
	f := actiontest.NewLoopProposalFixture(t, store, claimed, "worker", time.Now().UTC())
	coordinator := action.NewActionCoordinator(store, store, catalog, action.NewDefaultActionPolicy(), []action.ActionProposalValidator{validator})
	proposal, err := coordinator.Propose(ctx, f.Request(action.ProposeActionRequest{DeploymentID: owner.ID, SkillID: models.ObjectiveManagementSkillID, SkillVersion: action.ObjectiveManagementSkillVersion, Action: action.ObjectiveActionPause, Arguments: map[string]interface{}{"objectiveId": objective.ID}, IdempotencyKey: "pause-on-request", Summary: "Pause objective: Watch production"}, "pause-on-request"))
	if err != nil {
		t.Fatal(err)
	}
	if revision, ok := numericRevision(proposal.Call.Arguments["expectedRevision"]); !ok || revision != objective.Revision {
		t.Fatalf("kernel did not resolve Objective revision: %#v", proposal.Call.Arguments)
	}
	if proposal.Approval.ProposedAction["changes"].(map[string]interface{})["status"] != string(models.ObjectiveStatusPaused) {
		t.Fatalf("pause preview = %#v", proposal.Approval.ProposedAction)
	}
	dispatcher, _ := action.NewObjectiveActionDispatcher(store, nil)
	executed := approveAndExecuteLoopAction(t, store, f, proposal, action.NewActionWorker(store, catalog, nil, dispatcher), "approve-pause")
	paused, err := portfolio.GetObjective(ctx, scope, objective.ID)
	if err != nil || paused.Status != models.ObjectiveStatusPaused || paused.Revision != objective.Revision+1 {
		t.Fatalf("paused = %#v, %v", paused, err)
	}
	bound, _ := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, models.ObjectiveManagementSkillID, action.ObjectiveManagementSkillVersion, action.ObjectiveActionPause)
	if _, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments}); err != nil {
		t.Fatalf("approved replay = %v", err)
	}
	pausedAgain, _ := portfolio.GetObjective(ctx, scope, objective.ID)
	if pausedAgain.Revision != paused.Revision {
		t.Fatalf("replay changed revision: %d -> %d", paused.Revision, pausedAgain.Revision)
	}
}

// approveAndExecuteLoopAction parks f's Run on proposal's approval at step
// end, approves it as an operator, re-leases the queued Run to f's worker as
// the loop's next claim does, and executes the ready call inline as the
// loop's governed executor does.
func approveAndExecuteLoopAction(t *testing.T, store models.KernelStore, f *actiontest.LoopProposalFixture, proposal *models.ActionProposalResult, worker *action.ActionWorker, decisionID string) *models.ActionExecutionResult {
	t.Helper()
	f.AssertUntouched()
	f.ParkOnApproval(proposal.Approval.ID)
	resolved, err := action.NewApprovalCoordinator(store, store, action.EligibleApprovalAuthorizer{}).Resolve(context.Background(), action.ResolveApprovalRequest{
		Scope: f.Run.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: decisionID, Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "role", ID: "operator"}, Reason: "Reviewed",
	})
	if err != nil || resolved.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	if queued := f.Current(); queued.Status != models.AgentRunStatusQueued {
		t.Fatalf("approval did not queue the parked Run: %#v", queued)
	}
	resumed := actiontest.NewLoopProposalFixture(t, store, f.Run, f.Worker, time.Now().UTC())
	return resumed.Execute(worker, proposal.Call.ID)
}

type objectiveActionStoreCase struct {
	name string
	open func(*testing.T) (models.KernelStore, func())
}

func numericRevision(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), typed == float64(int64(typed))
	default:
		return 0, false
	}
}

func objectiveActionStores() []objectiveActionStoreCase {
	return []objectiveActionStoreCase{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "objectives.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
}

func objectiveActionCatalog(t *testing.T, store models.KernelStore, scope models.Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalogStore, ok := store.(skill.CatalogStore)
	var catalog *skill.Catalog
	if ok {
		catalog = skill.NewCatalogWithStore(catalogStore)
	} else {
		catalog = skill.NewCatalog()
	}
	if err := catalog.Register(context.Background(), action.ObjectiveManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "objectives", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: models.ObjectiveManagementSkillID, SkillVersion: action.ObjectiveManagementSkillVersion,
		AllowedActions: []string{action.ObjectiveActionCreate, action.ObjectiveActionUpdate, action.ObjectiveActionPause}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func stringsContain(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
