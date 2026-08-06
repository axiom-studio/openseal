package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestObjectiveManagementSkillVersionsRunbookOnlyContract(t *testing.T) {
	definition := ObjectiveManagementSkill()
	if definition.Version != "1.1.0" {
		t.Fatalf("Objective management Skill version = %q", definition.Version)
	}
	for _, actionName := range []string{ObjectiveActionCreate, ObjectiveActionUpdate} {
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
			scope := Scope{Kind: "tenant", ID: "tenant-a"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}
			catalog := objectiveActionCatalog(t, store, scope, owner.ID)
			portfolio := NewPortfolioService(store)
			run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, Goal: "Create release objective", Source: RunSourceChat})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil || claimed.ID != run.ID {
				t.Fatalf("claim = %#v, %v", claimed, err)
			}
			validator, err := NewObjectiveActionValidator(store)
			if err != nil {
				t.Fatal(err)
			}
			coordinator := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator)
			proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
				Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: owner.ID,
				SkillID: ObjectiveManagementSkillID, SkillVersion: ObjectiveManagementSkillVersion, Action: ObjectiveActionCreate,
				Arguments:      map[string]interface{}{"title": "Publish release notes", "goal": "Create and review release notes", "priority": 2},
				IdempotencyKey: "message-42:create-release-notes", Summary: "Create objective: Publish release notes", TurnID: "turn-42",
			})
			if err != nil {
				t.Fatal(err)
			}
			if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval || proposal.Run.Status != AgentRunStatusWaitingForApproval || proposal.Call.TurnID != "turn-42" {
				t.Fatalf("proposal lifecycle = %#v", proposal)
			}
			preview := proposal.Approval.ProposedAction
			if preview["resourceType"] != "objective" || preview["operation"] != ObjectiveActionCreate || preview["resultingStatus"] != string(ObjectiveStatusDraft) || preview["owner"] == nil || preview["arguments"] != nil {
				t.Fatalf("typed approval preview = %#v", preview)
			}
			resolved, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{
				Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "decision-42", Approve: true, Principal: ApprovalPrincipal{Type: "role", ID: "operator"}, Reason: "Looks correct",
			})
			if err != nil || resolved.Call.Status != ActionCallStatusReady || resolved.Run.Status != AgentRunStatusWaitingForDependency {
				t.Fatalf("resolve = %#v, %v", resolved, err)
			}
			dispatcher, err := NewObjectiveActionDispatcher(store, nil)
			if err != nil {
				t.Fatal(err)
			}
			worker := NewActionWorker(store, catalog, nil, dispatcher)
			executed, err := worker.RunOnce(ctx, scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.Status != ActionCallStatusSucceeded || executed.Run.Status != AgentRunStatusQueued || executed.Call.Output["resourceType"] != "objective" || executed.Call.Output["operation"] != ObjectiveActionCreate || executed.Call.Output["created"] != true {
				t.Fatalf("execution = %#v", executed)
			}
			objectives, err := portfolio.ListObjectives(ctx, ObjectiveFilter{Scope: scope, Owner: &owner})
			if err != nil || len(objectives) != 1 || objectives[0].Status != ObjectiveStatusDraft || objectives[0].Title != "Publish release notes" {
				t.Fatalf("objectives = %#v, %v", objectives, err)
			}
			bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, ObjectiveManagementSkillID, ObjectiveManagementSkillVersion, ObjectiveActionCreate)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments})
			if err != nil || replayed["created"] != false {
				t.Fatalf("idempotent replay = %#v, %v", replayed, err)
			}
		})
	}
}

func TestObjectiveActionsRejectInjectedOwnerForeignTargetAndStaleRevisionBeforeApproval(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "marketing"}
	foreignOwner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "finance"}
	catalog := objectiveActionCatalog(t, store, scope, owner.ID)
	portfolio := NewPortfolioService(store)
	foreign, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: foreignOwner, Title: "Close books", Goal: "Close monthly books"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: "marketing-lead", Goal: "Manage campaign objectives", Source: RunSourceChat})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	validator, _ := NewObjectiveActionValidator(store)
	coordinator := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator)
	base := ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: owner.ID, SkillID: ObjectiveManagementSkillID, SkillVersion: ObjectiveManagementSkillVersion, Action: ObjectiveActionCreate, IdempotencyKey: "create"}
	base.Arguments = map[string]interface{}{"title": "Campaign", "goal": "Launch campaign", "owner": map[string]interface{}{"type": "team", "id": "finance"}}
	if _, err := coordinator.Propose(ctx, base); err == nil {
		t.Fatal("model-supplied owner should fail schema validation")
	}
	base.Action = ObjectiveActionUpdate
	base.Arguments = map[string]interface{}{"objectiveId": foreign.ID, "expectedRevision": foreign.Revision, "goal": "Exfiltrate finance work"}
	if _, err := coordinator.Propose(ctx, base); err == nil || !stringsContain(err.Error(), "not owned") {
		t.Fatalf("foreign target error = %v", err)
	}
	owned, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Campaign", Goal: "Launch campaign"})
	if err != nil {
		t.Fatal(err)
	}
	base.Arguments = map[string]interface{}{"objectiveId": owned.ID, "expectedRevision": owned.Revision + 1, "goal": "Grow qualified leads"}
	if _, err := coordinator.Propose(ctx, base); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale proposal error = %v", err)
	}
	approvals, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(approvals) != 0 {
		t.Fatalf("invalid proposals became approvals: %#v, %v", approvals, err)
	}
}

func TestObjectivePauseRechecksOwnerAndIsReplaySafeAfterApproval(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"}
	catalog := objectiveActionCatalog(t, store, scope, owner.ID)
	portfolio := NewPortfolioService(store)
	objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Watch production", Goal: "Respond to incidents", Status: ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID, Goal: "Pause production watch", Source: RunSourceChat})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	validator, _ := NewObjectiveActionValidator(store)
	coordinator := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator)
	proposal, err := coordinator.Propose(ctx, ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: owner.ID, SkillID: ObjectiveManagementSkillID, SkillVersion: ObjectiveManagementSkillVersion, Action: ObjectiveActionPause, Arguments: map[string]interface{}{"objectiveId": objective.ID}, IdempotencyKey: "pause-on-request"})
	if err != nil {
		t.Fatal(err)
	}
	if revision, ok := numericRevision(proposal.Call.Arguments["expectedRevision"]); !ok || revision != objective.Revision {
		t.Fatalf("kernel did not resolve Objective revision: %#v", proposal.Call.Arguments)
	}
	if proposal.Approval.ProposedAction["changes"].(map[string]interface{})["status"] != string(ObjectiveStatusPaused) {
		t.Fatalf("pause preview = %#v", proposal.Approval.ProposedAction)
	}
	_, err = NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: 1, DecisionID: "approve-pause", Approve: true, Principal: ApprovalPrincipal{Type: "role", ID: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewObjectiveActionDispatcher(store, nil)
	worker := NewActionWorker(store, catalog, nil, dispatcher)
	executed, err := worker.RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := portfolio.GetObjective(ctx, scope, objective.ID)
	if err != nil || paused.Status != ObjectiveStatusPaused || paused.Revision != objective.Revision+1 {
		t.Fatalf("paused = %#v, %v", paused, err)
	}
	bound, _ := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, ObjectiveManagementSkillID, ObjectiveManagementSkillVersion, ObjectiveActionPause)
	if _, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments}); err != nil {
		t.Fatalf("approved replay = %v", err)
	}
	pausedAgain, _ := portfolio.GetObjective(ctx, scope, objective.ID)
	if pausedAgain.Revision != paused.Revision {
		t.Fatalf("replay changed revision: %d -> %d", paused.Revision, pausedAgain.Revision)
	}
}

type objectiveActionStoreCase struct {
	name string
	open func(*testing.T) (KernelStore, func())
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
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "objectives.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
}

func objectiveActionCatalog(t *testing.T, store KernelStore, scope Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalogStore, ok := store.(skill.CatalogStore)
	var catalog *skill.Catalog
	if ok {
		catalog = skill.NewCatalogWithStore(catalogStore)
	} else {
		catalog = skill.NewCatalog()
	}
	if err := catalog.Register(context.Background(), ObjectiveManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "objectives", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: ObjectiveManagementSkillID, SkillVersion: ObjectiveManagementSkillVersion,
		AllowedActions: []string{ObjectiveActionCreate, ObjectiveActionUpdate, ObjectiveActionPause}, MaximumRisk: skill.RiskLevelWrite,
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
