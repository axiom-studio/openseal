//go:build ignore

// Seeds a synthetic checkpoint into an isolated UI-test SQLite workspace.
// The skill is deliberately uninstalled; this fixture cannot publish anything.
// -card agent-update seeds an update_agent review card instead: the builder's
// agent_profile preview, decided by the local operator who asked for it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func main() {
	path := flag.String("db", "", "isolated test database")
	card := flag.String("card", "action", "action or agent-update")
	flag.Parse()
	if *path == "" {
		panic("-db is required")
	}
	store, err := sqlite.New(*path)
	check(err)
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	scope := models.Scope{Kind: "local", ID: "default"}
	if *card == "agent-update" {
		seedAgentUpdateCard(ctx, store, scope, now)
		return
	}
	if *card != "action" {
		panic("-card must be action or agent-update")
	}
	run := &models.AgentRun{ID: "ui-approval-run", RootRunID: "ui-approval-run", Kind: models.RunKindAgentWork, Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "synthetic-reviewer"}, AssignedAgentID: "synthetic-reviewer", Goal: "Review synthetic action checkpoint", Source: models.RunSourceManual, Status: models.AgentRunStatusQueued, Revision: 1, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	run = claimed(ctx, store, run, now)
	waiting(run, "ui-approval")
	call := &models.ActionCall{ID: "ui-action", Scope: scope, RunID: run.ID, TurnID: "ui-approval-turn", Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop, DeploymentID: "synthetic-reviewer", SkillID: "uninstalled-ui-test-fixture", SkillVersion: "1", Action: "review-fixture", Status: models.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"destination": "Synthetic destination — no external delivery", "text": "Review fixture only"}, ApprovalID: "ui-approval", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	approval := &models.ApprovalCheckpoint{ID: "ui-approval", Scope: scope, RunID: run.ID, ActionCallID: call.ID, Status: models.ApprovalStatusPending, Risk: skill.RiskLevelProduction, Summary: "Review a synthetic action", PolicyReason: "UI test checkpoint; the skill is uninstalled", EligibleApprovers: []models.ApprovalPrincipal{{Type: "role", ID: "operator"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now}
	result, err := store.CreateActionProposal(ctx, models.ActionProposalRecord{Call: call, Approval: approval, Run: run, ExpectedRunRevision: run.Revision - 1, Lease: &models.AgentRunLeaseGuard{WorkerID: seedWorker, Now: now}, Event: &models.ActivityEvent{ID: "ui-approval-event", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Synthetic review requested", CreatedAt: now}})
	check(err)
	check(json.NewEncoder(os.Stdout).Encode(result))
}

// The run loop proposes a call while it holds the Run's lease, so the seed
// claims the Run as a synthetic worker first. The Run becomes available an
// hour ahead, so the running daemon's workers never claim it first.
const seedWorker = "ui-seed-worker"

func claimed(ctx context.Context, store *sqlite.Store, run *models.AgentRun, now time.Time) *models.AgentRun {
	run.AvailableAt = now.Add(time.Hour)
	check(store.CreateAgentRun(ctx, run))
	current, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: run.Scope, WorkerID: seedWorker, Now: run.AvailableAt, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	check(err)
	if current == nil || current.ID != run.ID {
		panic("seed isolated approval: the seeded Run was not claimed")
	}
	return current
}

// waiting is the Run as the proposal leaves it: in review, lease released.
func waiting(run *models.AgentRun, approvalID string) {
	run.Status = models.AgentRunStatusWaitingForApproval
	run.WakeCondition = &models.WakeCondition{Type: "approval", Reference: approvalID}
	run.LeaseOwner = ""
	run.LeaseExpiresAt = nil
	run.Revision++
}

func check(err error) {
	if err != nil {
		panic(fmt.Errorf("seed isolated approval: %w", err))
	}
}

// seedAgentUpdateCard puts a builder Run in review on an update_agent card
// whose preview has the validator's shape (pkg/runtime agentUpdatePreview).
func seedAgentUpdateCard(ctx context.Context, store *sqlite.Store, scope models.Scope, now time.Time) {
	run := &models.AgentRun{ID: "ui-profile-run", RootRunID: "ui-profile-run", Kind: models.RunKindAgentWork, Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "synthetic-builder"}, AssignedAgentID: "synthetic-builder", ActingUser: "local-operator", Goal: "Change the evidence analyst", Source: models.RunSourceManual, Status: models.AgentRunStatusQueued, Revision: 1, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	run = claimed(ctx, store, run, now)
	waiting(run, "ui-profile-approval")
	arguments := map[string]interface{}{"agentId": "synthetic-analyst", "expectedDeploymentRevision": 3, "purpose": "Review sources and flag weak evidence.", "removeSkillBindingIds": []interface{}{"binding-search"}, "rationale": "The person asked for weak evidence to be flagged."}
	call := &models.ActionCall{ID: "ui-profile-action", Scope: scope, RunID: run.ID, TurnID: "ui-profile-turn", Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop, DeploymentID: "synthetic-builder", SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionUpdateAgent, Status: models.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Arguments: arguments, ApprovalID: "ui-profile-approval", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	profile := func(purpose string) map[string]interface{} {
		return map[string]interface{}{"deploymentId": "synthetic-analyst", "displayName": "Evidence analyst", "purpose": purpose, "systemPrompt": "Compare the supplied sources and cite each claim.", "personality": "Calm and exact.", "operatingPrinciples": []interface{}{"Cite every claim."}}
	}
	preview := map[string]interface{}{
		"skillId": models.AgentManagementSkillID, "skillVersion": models.AgentManagementSkillVersion, "action": models.AgentActionUpdateAgent,
		"risk": "write", "sideEffect": "write", "resourceType": "agent_profile", "operation": models.AgentActionUpdateAgent,
		"agent": profile("Review sources and flag weak evidence."), "current": profile("Review sources"),
		"changes": map[string]interface{}{"purpose": "Review sources and flag weak evidence."},
		"skills":  []interface{}{}, "removedSkills": []interface{}{map[string]interface{}{"bindingId": "binding-search", "skillId": "synthetic.search", "name": "Synthetic search"}},
		"rationale": "The person asked for weak evidence to be flagged.",
	}
	approval := &models.ApprovalCheckpoint{ID: "ui-profile-approval", Scope: scope, RunID: run.ID, ActionCallID: call.ID, Status: models.ApprovalStatusPending, Risk: skill.RiskLevelWrite, Summary: "Change the evidence analyst's purpose", ProposedAction: preview, EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "local-operator"}}, ExpiresAt: now.Add(time.Hour), TimeoutDecision: models.ApprovalTimeoutExpire, Revision: 1, CreatedAt: now, UpdatedAt: now}
	result, err := store.CreateActionProposal(ctx, models.ActionProposalRecord{Call: call, Approval: approval, Run: run, ExpectedRunRevision: run.Revision - 1, Lease: &models.AgentRunLeaseGuard{WorkerID: seedWorker, Now: now}, Event: &models.ActivityEvent{ID: "ui-profile-event", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Agent change review requested", CreatedAt: now}})
	check(err)
	check(json.NewEncoder(os.Stdout).Encode(result))
}
