package runtime

import (
	"context"
	"testing"
	"time"

	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestTeamCreateIsOnlyInTheCurrentContract(t *testing.T) {
	ctx := context.Background()
	definition := TeamManagementSkill()
	if definition.Version != "2.2.0" {
		t.Fatalf("version = %s", definition.Version)
	}
	action, ok := definition.Actions[runtimeaction.TeamActionCreateTeam]
	if !ok || action.Risk != skill.RiskLevelWrite || action.Review != skill.ActionReviewAlways || action.Idempotency != skill.IdempotencyRequired {
		t.Fatalf("create_team = %#v", action)
	}
	if !teamCreateGetsBuilderCard(t, definition) {
		t.Fatal("create_team does not get the agent-builder card policy")
	}

	// The builder dispatcher takes the current create_team and passes a
	// 2.0.0 binding's on to the next dispatcher.
	f := newAgentBuilderDispatchFixture(t)
	fallback := runtimeaction.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	dispatcher, err := newTestAgentBuilderDispatcher(f.store, f.agents, f.teams, f.catalog, f.provision, nil, fallback)
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
	if err := fixture.catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := fixture.catalog.Bind(ctx, &skill.Binding{
		ID: "bundled:teams", Revision: 1, Scope: skill.ScopeReference{Kind: fixture.scope.Kind, ID: fixture.scope.ID}, DeploymentID: "finn",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{runtimeaction.TeamActionCreateTeam}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Propose(ctx, loopProposeRequest(runtimeaction.ProposeActionRequest{
		Scope: fixture.scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "finn",
		SkillID: definition.ID, SkillVersion: definition.Version, Action: runtimeaction.TeamActionCreateTeam,
		Arguments: teamCreateArgumentsFor(""), Summary: "Review the team",
	}, "create-team"))
	if err != nil {
		t.Fatal(err)
	}
	approval := result.Approval
	if approval == nil {
		t.Fatalf("create_team was not held for review: %#v", result)
	}
	return approval.ExpiresAt.Equal(now.Add(30*24*time.Hour)) && approval.TimeoutDecision == ApprovalTimeoutExpire && len(approval.Destinations) == 0
}
