//go:build ignore

// Seeds a synthetic checkpoint into an isolated UI-test SQLite workspace.
// The skill is deliberately uninstalled; this fixture cannot publish anything.
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
	run := &runtime.AgentRun{ID: "ui-approval-run", RootRunID: "ui-approval-run", Kind: runtime.RunKindAgentWork, Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "synthetic-reviewer"}, AssignedAgentID: "synthetic-reviewer", Goal: "Review synthetic action checkpoint", Source: runtime.RunSourceManual, Status: runtime.AgentRunStatusWaitingForApproval, WakeCondition: &runtime.WakeCondition{Type: "approval", Reference: "ui-approval"}, Revision: 1, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	check(store.CreateAgentRun(ctx, run))
	call := &runtime.ActionCall{ID: "ui-action", Scope: scope, RunID: run.ID, DeploymentID: "synthetic-reviewer", SkillID: "uninstalled-ui-test-fixture", SkillVersion: "1", Action: "review-fixture", Status: runtime.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"destination": "Synthetic destination — no external delivery", "text": "Review fixture only"}, ApprovalID: "ui-approval", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	call.InvocationDigest = runtime.ComputeActionInvocationDigest(call)
	approval := &runtime.ApprovalCheckpoint{ID: "ui-approval", Scope: scope, RunID: run.ID, ActionCallID: call.ID, Status: runtime.ApprovalStatusPending, Risk: skill.RiskLevelProduction, Summary: "Review a synthetic action", PolicyReason: "UI test checkpoint; the skill is uninstalled", EligibleApprovers: []runtime.ApprovalPrincipal{{Type: "role", ID: "operator"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now}
	run.Revision++
	result, err := store.CreateActionProposal(ctx, runtime.ActionProposalRecord{Call: call, Approval: approval, Run: run, ExpectedRunRevision: 1, Event: &runtime.ActivityEvent{ID: "ui-approval-event", Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Synthetic review requested", CreatedAt: now}})
	check(err)
	check(json.NewEncoder(os.Stdout).Encode(result))
}
func check(err error) {
	if err != nil {
		panic(fmt.Errorf("seed isolated approval: %w", err))
	}
}
