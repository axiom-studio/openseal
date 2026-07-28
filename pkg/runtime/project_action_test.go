package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestGovernedProjectCreateUsesApprovalActionAndIdempotencyLifecycle(t *testing.T) {
	for _, testCase := range projectActionStores() {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "tenant-a"}
			owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"}
			objective := createProjectActionObjective(t, store, scope, owner, "monitor-feedback")
			catalog := projectActionCatalog(t, store, scope, owner.ID)
			run := createAndClaimProjectActionRun(t, store, scope, owner, "researcher", "conversation-worker")
			validator, err := NewProjectActionValidator(store)
			if err != nil {
				t.Fatal(err)
			}
			coordinator := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator)
			proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
				Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: owner.ID,
				SkillID: ProjectManagementSkillID, SkillVersion: ProjectManagementSkillVersion, Action: ProjectActionCreate,
				Arguments: map[string]interface{}{
					"title": "Customer research", "purpose": "Turn recurring customer feedback into cited decisions",
					"objectiveRefs": []interface{}{objective.ID},
					"milestones":    []interface{}{map[string]interface{}{"id": "evidence-review", "title": "Review evidence", "status": "pending", "objectiveRefs": []interface{}{objective.ID}}},
					"deliverables":  []interface{}{map[string]interface{}{"id": "weekly-brief", "title": "Weekly cited brief", "status": "planned", "objectiveRefs": []interface{}{objective.ID}}},
				},
				IdempotencyKey: "message-42:create-customer-research", Summary: "Create customer research Project", TurnID: "turn-42",
			})
			if err != nil {
				t.Fatal(err)
			}
			if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval || proposal.Run.Status != AgentRunStatusWaitingForApproval {
				t.Fatalf("proposal lifecycle = %#v", proposal)
			}
			preview := proposal.Approval.ProposedAction
			if preview["resourceType"] != "project" || preview["operation"] != ProjectActionCreate || preview["resultingStatus"] != string(ProjectStatusDraft) || preview["owner"] == nil || preview["arguments"] != nil {
				t.Fatalf("typed approval preview = %#v", preview)
			}
			resolved, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{
				Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "decision-42", Approve: true, Principal: ApprovalPrincipal{Type: "role", ID: "operator"}, Reason: "Plan is bounded",
			})
			if err != nil || resolved.Call.Status != ActionCallStatusReady {
				t.Fatalf("resolve = %#v, %v", resolved, err)
			}
			dispatcher, err := NewProjectActionDispatcher(store, nil)
			if err != nil {
				t.Fatal(err)
			}
			executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["resourceType"] != "project" || executed.Call.Output["created"] != true {
				t.Fatalf("execution = %#v", executed)
			}
			projects, err := store.ListProjects(ctx, ProjectFilter{Scope: scope, Owner: &owner})
			if err != nil || len(projects) != 1 || projects[0].Status != ProjectStatusDraft || projects[0].Title != "Customer research" || len(projects[0].Milestones) != 1 {
				t.Fatalf("projects = %#v, %v", projects, err)
			}
			bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, ProjectManagementSkillID, ProjectManagementSkillVersion, ProjectActionCreate)
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

func TestProjectActionCatalogPublishesExecutableCompositeSchemas(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	catalog := projectActionCatalog(t, NewMemoryStore(), scope, "research-team")
	bound, err := catalog.Resolve(context.Background(), skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "research-team", ProjectManagementSkillID, ProjectManagementSkillVersion, ProjectActionCreate)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]interface{}{"title": "Research", "purpose": "Coordinate evidence", "objectiveRefs": []interface{}{"monitor-feedback"}}
	for name, invalid := range map[string]interface{}{
		"team reference": []interface{}{"research-team"},
		"milestone":      []interface{}{"weekly-brief"},
		"deliverable":    []interface{}{"research-report"},
	} {
		input := cloneMap(base)
		switch name {
		case "team reference":
			input["teamRefs"] = invalid
		case "milestone":
			input["milestones"] = invalid
		case "deliverable":
			input["deliverables"] = invalid
		}
		if err := catalog.ValidateInput(context.Background(), bound, input); err == nil {
			t.Fatalf("catalog accepted untyped %s", name)
		}
	}
	typed := cloneMap(base)
	typed["teamRefs"] = []interface{}{map[string]interface{}{"kind": "team_deployment", "id": "research-team"}}
	typed["milestones"] = []interface{}{map[string]interface{}{"id": "weekly-brief", "title": "Weekly brief", "status": "pending"}}
	typed["deliverables"] = []interface{}{map[string]interface{}{"id": "research-report", "title": "Research report", "status": "planned"}}
	if err := catalog.ValidateInput(context.Background(), bound, typed); err != nil {
		t.Fatalf("catalog rejected typed Project composites: %v", err)
	}
}

func TestProjectActionsRejectInjectedOwnerForeignObjectivesTargetsAndStaleRevision(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "marketing"}
	foreignOwner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "finance"}
	ownedObjective := createProjectActionObjective(t, store, scope, owner, "campaign")
	foreignObjective := createProjectActionObjective(t, store, scope, foreignOwner, "close-books")
	foreignProject, _, err := NewProjectService(store, store).Create(ctx, CreateProjectRequest{Project: &Project{Scope: scope, Owner: foreignOwner, Title: "Finance", Purpose: "Close books", Status: ProjectStatusDraft, ObjectiveRefs: []string{foreignObjective.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := projectActionCatalog(t, store, scope, owner.ID)
	run := createAndClaimProjectActionRun(t, store, scope, owner, "marketing-lead", "worker")
	validator, _ := NewProjectActionValidator(store)
	coordinator := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator)
	base := ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: owner.ID, SkillID: ProjectManagementSkillID, SkillVersion: ProjectManagementSkillVersion, IdempotencyKey: "proposal"}
	base.Action = ProjectActionCreate
	base.Arguments = map[string]interface{}{"title": "Campaign", "purpose": "Launch", "objectiveRefs": []interface{}{ownedObjective.ID}, "owner": map[string]interface{}{"type": "team", "id": foreignOwner.ID}}
	if _, err := coordinator.Propose(ctx, base); err == nil {
		t.Fatal("model-supplied owner should fail schema validation")
	}
	base.Arguments = map[string]interface{}{"title": "Campaign", "purpose": "Launch", "objectiveRefs": []interface{}{foreignObjective.ID}}
	if _, err := coordinator.Propose(ctx, base); err == nil || !stringsContain(err.Error(), "owner must match") {
		t.Fatalf("foreign objective error = %v", err)
	}
	base.Action = ProjectActionUpdate
	base.Arguments = map[string]interface{}{"projectId": foreignProject.ID, "expectedRevision": foreignProject.Revision, "purpose": "Exfiltrate"}
	if _, err := coordinator.Propose(ctx, base); err == nil || !stringsContain(err.Error(), "not owned") {
		t.Fatalf("foreign target error = %v", err)
	}
	owned, _, err := NewProjectService(store, store).Create(ctx, CreateProjectRequest{Project: &Project{Scope: scope, Owner: owner, Title: "Campaign", Purpose: "Launch", Status: ProjectStatusDraft, ObjectiveRefs: []string{ownedObjective.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	base.Arguments = map[string]interface{}{"projectId": owned.ID, "expectedRevision": owned.Revision + 1, "purpose": "Grow leads"}
	if _, err := coordinator.Propose(ctx, base); !errors.Is(err, ErrProjectConflict) {
		t.Fatalf("stale proposal error = %v", err)
	}
	approvals, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(approvals) != 0 {
		t.Fatalf("invalid proposals became approvals: %#v, %v", approvals, err)
	}
}

func TestProjectPauseIsCASAndReplaySafe(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"}
	objective := createProjectActionObjective(t, store, scope, owner, "watch-production")
	project, _, err := NewProjectService(store, store).Create(ctx, CreateProjectRequest{Project: &Project{Scope: scope, Owner: owner, Title: "Production watch", Purpose: "Coordinate incident response", Status: ProjectStatusActive, ObjectiveRefs: []string{objective.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := projectActionCatalog(t, store, scope, owner.ID)
	run := createAndClaimProjectActionRun(t, store, scope, owner, owner.ID, "worker")
	validator, _ := NewProjectActionValidator(store)
	proposal, err := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator).Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: owner.ID, SkillID: ProjectManagementSkillID, SkillVersion: ProjectManagementSkillVersion,
		Action: ProjectActionPause, Arguments: map[string]interface{}{"projectId": project.ID}, IdempotencyKey: "pause-on-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if revision, ok := numericRevision(proposal.Call.Arguments["expectedRevision"]); !ok || revision != project.Revision {
		t.Fatalf("kernel did not resolve Project revision: %#v", proposal.Call.Arguments)
	}
	if proposal.Approval.ProposedAction["changes"].(map[string]interface{})["status"] != string(ProjectStatusPaused) {
		t.Fatalf("pause preview = %#v", proposal.Approval.ProposedAction)
	}
	if _, err = NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "approve-pause", Approve: true, Principal: ApprovalPrincipal{Type: "role", ID: "operator"}}); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewProjectActionDispatcher(store, nil)
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := store.GetProject(ctx, scope, project.ID)
	if err != nil || paused.Status != ProjectStatusPaused || paused.Revision != project.Revision+1 {
		t.Fatalf("paused = %#v, %v", paused, err)
	}
	bound, _ := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, ProjectManagementSkillID, ProjectManagementSkillVersion, ProjectActionPause)
	if _, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments}); err != nil {
		t.Fatalf("approved replay = %v", err)
	}
	pausedAgain, _ := store.GetProject(ctx, scope, project.ID)
	if pausedAgain.Revision != paused.Revision {
		t.Fatalf("replay changed revision: %d -> %d", paused.Revision, pausedAgain.Revision)
	}
}

func TestGovernedProjectUpdateComposesMultipleObjectivesAndIsReplaySafe(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "go-to-market"}
	research := createProjectActionObjective(t, store, scope, owner, "research-pain-points")
	outreach := createProjectActionObjective(t, store, scope, owner, "follow-up-leads")
	service := NewProjectService(store, store)
	project, _, err := service.Create(ctx, CreateProjectRequest{Project: &Project{
		Scope: scope, Owner: owner, Title: "Market research", Purpose: "Understand customer pain", Status: ProjectStatusDraft, ObjectiveRefs: []string{research.ID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := projectActionCatalog(t, store, scope, owner.ID)
	run := createAndClaimProjectActionRun(t, store, scope, owner, "research-lead", "worker")
	validator, _ := NewProjectActionValidator(store)
	arguments := map[string]interface{}{
		"projectId": project.ID, "expectedRevision": project.Revision,
		"purpose":       "Research pain points and follow up with qualified leads",
		"objectiveRefs": []interface{}{research.ID, outreach.ID},
		"milestones": []interface{}{
			map[string]interface{}{"id": "evidence", "title": "Synthesize cited evidence", "status": "pending", "objectiveRefs": []interface{}{research.ID}},
			map[string]interface{}{"id": "outreach", "title": "Complete approved follow-up", "status": "pending", "objectiveRefs": []interface{}{outreach.ID}},
		},
	}
	proposal, err := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator).Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: owner.ID, SkillID: ProjectManagementSkillID, SkillVersion: ProjectManagementSkillVersion,
		Action: ProjectActionUpdate, Arguments: arguments, IdempotencyKey: "expand-market-research",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval.ProposedAction["projectId"] != project.ID || proposal.Approval.ProposedAction["expectedRevision"] == nil || proposal.Approval.ProposedAction["current"] == nil {
		t.Fatalf("update preview = %#v", proposal.Approval.ProposedAction)
	}
	if _, err = NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "approve-update", Approve: true, Principal: ApprovalPrincipal{Type: "role", ID: "operator"}}); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewProjectActionDispatcher(store, nil)
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Get(ctx, scope, project.ID)
	if err != nil || updated.Revision != project.Revision+1 || len(updated.ObjectiveRefs) != 2 || len(updated.Milestones) != 2 || updated.Purpose != "Research pain points and follow up with qualified leads" {
		t.Fatalf("updated = %#v, %v", updated, err)
	}
	bound, _ := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, ProjectManagementSkillID, ProjectManagementSkillVersion, ProjectActionUpdate)
	if _, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments}); err != nil {
		t.Fatalf("approved replay = %v", err)
	}
	replayed, _ := service.Get(ctx, scope, project.ID)
	if replayed.Revision != updated.Revision {
		t.Fatalf("replay changed revision: %d -> %d", updated.Revision, replayed.Revision)
	}
}

type projectActionStoreCase struct {
	name string
	open func(*testing.T) (ProjectKernelStore, func())
}

func projectActionStores() []projectActionStoreCase {
	return []projectActionStoreCase{
		{name: "memory", open: func(*testing.T) (ProjectKernelStore, func()) { return NewMemoryStore(), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (ProjectKernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "projects.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
}

func projectActionCatalog(t *testing.T, store ProjectKernelStore, scope Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalogStore, ok := store.(skill.CatalogStore)
	var catalog *skill.Catalog
	if ok {
		catalog = skill.NewCatalogWithStore(catalogStore)
	} else {
		catalog = skill.NewCatalog()
	}
	if err := catalog.Register(context.Background(), ProjectManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(context.Background(), &skill.Binding{ID: "projects", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID, SkillID: ProjectManagementSkillID, SkillVersion: ProjectManagementSkillVersion, AllowedActions: []string{ProjectActionCreate, ProjectActionUpdate, ProjectActionPause}, MaximumRisk: skill.RiskLevelWrite}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func createProjectActionObjective(t *testing.T, store PortfolioStore, scope Scope, owner ObjectiveOwner, id string) *Objective {
	t.Helper()
	objective, err := NewPortfolioService(store).CreateObjective(context.Background(), CreateObjectiveRequest{Scope: scope, Owner: owner, Title: id, Goal: "Complete " + id, Status: ObjectiveStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	return objective
}

func createAndClaimProjectActionRun(t *testing.T, store KernelStore, scope Scope, owner ObjectiveOwner, assignedAgentID, workerID string) *AgentRun {
	t.Helper()
	run, err := NewPortfolioService(store).CreateAgentRun(context.Background(), CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: assignedAgentID, Goal: "Manage project", Source: RunSourceChat})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: scope, WorkerID: workerID, Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return run
}
