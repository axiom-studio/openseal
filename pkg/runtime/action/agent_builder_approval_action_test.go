package action

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestAgentBuilderApprovalActionsMatchCreateAndUpdateAndTeam(t *testing.T) {
	bound := func(skillID, action string) *skill.BoundAction {
		return &skill.BoundAction{Definition: &skill.Definition{ID: skillID}, Action: skill.Action{Name: action}}
	}
	for _, tc := range []struct {
		bound *skill.BoundAction
		want  bool
	}{
		{bound(models.AgentManagementSkillID, models.AgentActionCreateAgent), true},
		{bound(models.AgentManagementSkillID, models.AgentActionUpdateAgent), true},
		{bound(models.TeamManagementSkillID, "create_team"), true},
		{bound(models.AgentManagementSkillID, models.AgentActionTryAgent), false},
		{bound(models.AgentManagementSkillID, models.AgentActionListAgents), false},
		{bound(models.AgentManagementSkillID, models.AgentActionAmendBehavior), false},
		{bound(models.TeamManagementSkillID, models.TeamActionUpdateMember), false},
		{bound("acme.notes", models.AgentActionCreateAgent), false},
		{nil, false},
	} {
		if got := isAgentBuilderApprovalAction(tc.bound); got != tc.want {
			t.Fatalf("isAgentBuilderApprovalAction(%#v) = %v", tc.bound, got)
		}
	}

	// A create_team card gets the same policy as an agent card.
	coordinator := NewActionCoordinator(kerneltest.NewStore(t), kerneltest.NewStore(t), skill.NewCatalog(), NewDefaultActionPolicy(), nil)
	coordinator.newID = func() string { return "approval-team" }
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	team := bound(models.TeamManagementSkillID, "create_team")
	team.Action.Risk, team.Action.Review = skill.RiskLevelWrite, skill.ActionReviewAlways
	call := &models.ActionCall{ID: "call-team"}
	approval := coordinator.newApprovalCheckpoint(context.Background(),
		ProposeActionRequest{Scope: models.Scope{Kind: "tenant", ID: "tenant-a"}}, &models.AgentRun{ID: "run-team"}, team, map[string]interface{}{},
		map[string]interface{}{"resourceType": "team", "operation": "create_team"}, call,
		ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "operator"}},
			ApprovalTTL: time.Hour, ApprovalTimeout: models.ApprovalTimeoutApprove, ApprovalDestinations: []models.ApprovalDestination{{EndpointID: "slack"}},
		}, "Create the team", now)
	if !approval.ExpiresAt.Equal(now.Add(agentBuilderApprovalTTL)) || approval.TimeoutDecision != models.ApprovalTimeoutExpire || len(approval.Destinations) != 0 {
		t.Fatalf("create_team card = %#v", approval)
	}
}
