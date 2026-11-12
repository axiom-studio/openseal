package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestTeamCreateIsOnlyInTheCurrentContract(t *testing.T) {
	definition := TeamManagementSkill()
	if definition.Version != "2.2.0" {
		t.Fatalf("version = %s", definition.Version)
	}
	action, ok := definition.Actions[TeamActionCreateTeam]
	if !ok || action.Risk != skill.RiskLevelWrite || action.Review != skill.ActionReviewAlways || action.Idempotency != skill.IdempotencyRequired {
		t.Fatalf("create_team = %#v", action)
	}
	if !isAgentBuilderApprovalAction(&skill.BoundAction{Definition: definition, Action: action}) {
		t.Fatal("create_team does not get the agent-builder card policy")
	}
	old := *definition
	old.Version = "2.0.0"
	if isTeamCreateAction(&skill.BoundAction{Definition: &old, Action: action}) {
		t.Fatal("create_team matched a 2.0.0 binding")
	}
	if !isAgentBuilderDispatchedAction(&skill.BoundAction{Definition: definition, Action: action}) {
		t.Fatal("the builder dispatcher does not take create_team")
	}
}
