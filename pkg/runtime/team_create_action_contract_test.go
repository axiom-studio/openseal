package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder/agentbuildertest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestTeamCreateIsOnlyInTheCurrentContract(t *testing.T) {
	ctx := context.Background()
	definition := collaboration.TeamManagementSkill()
	if definition.Version != "2.2.0" {
		t.Fatalf("version = %s", definition.Version)
	}
	action, ok := definition.Actions[models.TeamActionCreateTeam]
	if !ok || action.Risk != skill.RiskLevelWrite || action.Review != skill.ActionReviewAlways || action.Idempotency != skill.IdempotencyRequired {
		t.Fatalf("create_team = %#v", action)
	}
	if !teamCreateGetsBuilderCard(t, definition) {
		t.Fatal("create_team does not get the agent-builder card policy")
	}

	// The builder dispatcher takes the current create_team and passes a
	// 2.0.0 binding's on to the next dispatcher.
	f := agentbuildertest.NewDispatchFixture(t)
	fallback := runtimeaction.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	dispatcher, err := agentbuildertest.NewDispatcher(f.Store, f.Agents, f.Teams, f.Catalog, f.Provision, nil, fallback)
	if err != nil {
		t.Fatal(err)
	}
	old := *definition
	old.Version = "2.0.0"
	if result, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Bound: &skill.BoundAction{Definition: &old, Action: action}}); err != nil || result["fallback"] != true {
		t.Fatalf("create_team matched a 2.0.0 binding: %#v, %v", result, err)
	}
	if result, _ := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Bound: &skill.BoundAction{Definition: definition, Action: action}}); result["fallback"] == true {
		t.Fatal("the builder dispatcher does not take create_team")
	}
}

// teamCreateGetsBuilderCard proposes create_team under a policy that would
// loosen its card every way it can and reports whether the card still got
// the agent-builder policy: the builder's 30-day expiry, expire on timeout
// and no outside destinations.
func teamCreateGetsBuilderCard(t *testing.T, definition *skill.Definition) bool {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fixture, _, run, coordinator := agentBuilderApprovalCoordinator(t, generousBuilderPolicy(), now)
	if err := fixture.Catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Catalog.Bind(ctx, &skill.Binding{
		ID: "bundled:teams", Revision: 1, Scope: skill.ScopeReference{Kind: fixture.Scope.Kind, ID: fixture.Scope.ID}, DeploymentID: "finn",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{models.TeamActionCreateTeam}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Propose(ctx, actiontest.LoopProposeRequest(runtimeaction.ProposeActionRequest{
		Scope: fixture.Scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "finn",
		SkillID: definition.ID, SkillVersion: definition.Version, Action: models.TeamActionCreateTeam,
		Arguments: teamCreateArgumentsFor(""), Summary: "Review the team",
	}, "create-team"))
	if err != nil {
		t.Fatal(err)
	}
	approval := result.Approval
	if approval == nil {
		t.Fatalf("create_team was not held for review: %#v", result)
	}
	return approval.ExpiresAt.Equal(now.Add(30*24*time.Hour)) && approval.TimeoutDecision == models.ApprovalTimeoutExpire && len(approval.Destinations) == 0
}
