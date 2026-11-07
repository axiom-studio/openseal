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

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func main() {
	path := flag.String("db", "", "isolated test database")
	card := flag.String("card", "action", "action or agent-update")
	flag.Parse()
	if *path == "" {
		panic("-db is required")
	}
	store, err := runtime.NewSQLiteStore(*path)
	check(err)
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	scope := runtime.Scope{Kind: "local", ID: "default"}
	if *card == "agent-update" {
		seedAgentUpdateCard(ctx, store, scope, now)
		return
	}
	if *card != "action" {
		panic("-card must be action or agent-update")
	}
	run := &runtime.AgentRun{ID: "ui-approval-run", RootRunID: "ui-approval-run", Kind: runtime.RunKindAgentWork, Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "synthetic-reviewer"}, AssignedAgentID: "synthetic-reviewer", Goal: "Review synthetic action checkpoint", Source: runtime.RunSourceManual, Status: runtime.AgentRunStatusQueued, Revision: 1, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	run = claimed(ctx, store, run, now)
	waiting(run, "ui-approval")
	call := &runtime.ActionCall{ID: "ui-action", Scope: scope, RunID: run.ID, TurnID: "ui-approval-turn", Origin: runtime.ActionOriginLoop, Execution: runtime.ActionExecutionLoop, DeploymentID: "synthetic-reviewer", SkillID: "uninstalled-ui-test-fixture", SkillVersion: "1", Action: "review-fixture", Status: runtime.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"destination": "Synthetic destination — no external delivery", "text": "Review fixture only"}, ApprovalID: "ui-approval", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	call.InvocationDigest = runtime.ComputeActionInvocationDigest(call)
	approval := &runtime.ApprovalCheckpoint{ID: "ui-approval", Scope: scope, RunID: run.ID, ActionCallID: call.ID, Status: runtime.ApprovalStatusPending, Risk: skill.RiskLevelProduction, Summary: "Review a synthetic action", PolicyReason: "UI test checkpoint; the skill is uninstalled", EligibleApprovers: []runtime.ApprovalPrincipal{{Type: "role", ID: "operator"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now}
	result, err := store.CreateActionProposal(ctx, runtime.ActionProposalRecord{Call: call, Approval: approval, Run: run, ExpectedRunRevision: run.Revision - 1, Lease: &runtime.AgentRunLeaseGuard{WorkerID: seedWorker, Now: now}, Event: &runtime.ActivityEvent{ID: "ui-approval-event", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Synthetic review requested", CreatedAt: now}})
	check(err)
	check(json.NewEncoder(os.Stdout).Encode(result))
}

// The run loop proposes a call while it holds the Run's lease, so the seed
// claims the Run as a synthetic worker first. The Run becomes available an
// hour ahead, so the running daemon's workers never claim it first.
const seedWorker = "ui-seed-worker"

func claimed(ctx context.Context, store *runtime.SQLiteStore, run *runtime.AgentRun, now time.Time) *runtime.AgentRun {
	run.AvailableAt = now.Add(time.Hour)
	check(store.CreateAgentRun(ctx, run))
	current, err := store.ClaimNextAgentRun(ctx, runtime.AgentRunClaim{Scope: run.Scope, WorkerID: seedWorker, Now: run.AvailableAt, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	check(err)
	if current == nil || current.ID != run.ID {
		panic("seed isolated approval: the seeded Run was not claimed")
	}
	return current
}

// waiting is the Run as the proposal leaves it: in review, lease released.
func waiting(run *runtime.AgentRun, approvalID string) {
	run.Status = runtime.AgentRunStatusWaitingForApproval
	run.WakeCondition = &runtime.WakeCondition{Type: "approval", Reference: approvalID}
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
func seedAgentUpdateCard(ctx context.Context, store *runtime.SQLiteStore, scope runtime.Scope, now time.Time) {
	run := &runtime.AgentRun{ID: "ui-profile-run", RootRunID: "ui-profile-run", Kind: runtime.RunKindAgentWork, Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "synthetic-builder"}, AssignedAgentID: "synthetic-builder", ActingUser: "local-operator", Goal: "Change the evidence analyst", Source: runtime.RunSourceManual, Status: runtime.AgentRunStatusQueued, Revision: 1, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	run = claimed(ctx, store, run, now)
	waiting(run, "ui-profile-approval")
	arguments := map[string]interface{}{"agentId": "synthetic-analyst", "expectedDeploymentRevision": 3, "purpose": "Review sources and flag weak evidence.", "removeSkillBindingIds": []interface{}{"binding-search"}, "rationale": "The person asked for weak evidence to be flagged."}
	call := &runtime.ActionCall{ID: "ui-profile-action", Scope: scope, RunID: run.ID, TurnID: "ui-profile-turn", Origin: runtime.ActionOriginLoop, Execution: runtime.ActionExecutionLoop, DeploymentID: "synthetic-builder", SkillID: runtime.AgentManagementSkillID, SkillVersion: runtime.AgentManagementSkillVersion, Action: runtime.AgentActionUpdateAgent, Status: runtime.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Arguments: arguments, ApprovalID: "ui-profile-approval", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	call.InvocationDigest = runtime.ComputeActionInvocationDigest(call)
	profile := func(purpose string) map[string]interface{} {
		return map[string]interface{}{"deploymentId": "synthetic-analyst", "displayName": "Evidence analyst", "purpose": purpose, "systemPrompt": "Compare the supplied sources and cite each claim.", "personality": "Calm and exact.", "operatingPrinciples": []interface{}{"Cite every claim."}}
	}
	preview := map[string]interface{}{
		"skillId": runtime.AgentManagementSkillID, "skillVersion": runtime.AgentManagementSkillVersion, "action": runtime.AgentActionUpdateAgent,
		"risk": "write", "sideEffect": "write", "resourceType": "agent_profile", "operation": runtime.AgentActionUpdateAgent,
		"agent": profile("Review sources and flag weak evidence."), "current": profile("Review sources"),
		"changes": map[string]interface{}{"purpose": "Review sources and flag weak evidence."},
		"skills":  []interface{}{}, "removedSkills": []interface{}{map[string]interface{}{"bindingId": "binding-search", "skillId": "synthetic.search", "name": "Synthetic search"}},
		"rationale": "The person asked for weak evidence to be flagged.",
	}
	approval := &runtime.ApprovalCheckpoint{ID: "ui-profile-approval", Scope: scope, RunID: run.ID, ActionCallID: call.ID, Status: runtime.ApprovalStatusPending, Risk: skill.RiskLevelWrite, Summary: "Change the evidence analyst's purpose", ProposedAction: preview, EligibleApprovers: []runtime.ApprovalPrincipal{{Type: "user", ID: "local-operator"}}, ExpiresAt: now.Add(time.Hour), TimeoutDecision: runtime.ApprovalTimeoutExpire, Revision: 1, CreatedAt: now, UpdatedAt: now}
	result, err := store.CreateActionProposal(ctx, runtime.ActionProposalRecord{Call: call, Approval: approval, Run: run, ExpectedRunRevision: run.Revision - 1, Lease: &runtime.AgentRunLeaseGuard{WorkerID: seedWorker, Now: now}, Event: &runtime.ActivityEvent{ID: "ui-profile-event", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Agent change review requested", CreatedAt: now}})
	check(err)
	check(json.NewEncoder(os.Stdout).Encode(result))
}
