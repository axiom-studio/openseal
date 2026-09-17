package daemon

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
	"testing"
)

type workCatalog struct {
	team   *team.Deployment
	agents map[string]*agent.AgentDeployment
}

func (c workCatalog) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*team.Deployment, error) {
	if scope.Kind != "local" || scope.ID != "default" || id != "team" {
		return nil, team.ErrDeploymentNotFound
	}
	return c.team, nil
}
func (c workCatalog) GetAgentDeployment(_ context.Context, scope capability.ScopeReference, id string) (*agent.AgentDeployment, error) {
	if scope.Kind != "local" || scope.ID != "default" {
		return nil, agent.ErrDeploymentNotFound
	}
	return c.agents[id], nil
}
func TestDesktopTeamWorkUsesCurrentRosterAndHostAuthority(t *testing.T) {
	scope := runtime.Scope{Kind: "local", ID: "default"}
	request := runtime.CreateAgentRunRequest{Scope: scope, Kind: runtime.RunKindAgentWork, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "team"}, AssignedAgentID: "lead", Goal: "Review evidence", IdempotencyKey: "task"}
	for _, tc := range []struct {
		name         string
		status       team.DeploymentStatus
		lead         string
		memberStatus agent.RolloutStatus
		wantError    bool
	}{
		{"active team", team.DeploymentActive, "lead", agent.RolloutActive, false},
		{"paused team", team.DeploymentPaused, "lead", agent.RolloutActive, true},
		{"outsider lead", team.DeploymentActive, "outsider", agent.RolloutActive, true},
		{"unavailable peer", team.DeploymentActive, "lead", agent.RolloutPaused, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := workCatalog{team: &team.Deployment{ID: "team", Status: tc.status, Roster: []team.RosterAssignment{{AgentDeploymentID: "lead"}, {AgentDeploymentID: "peer"}}}, agents: map[string]*agent.AgentDeployment{"lead": {RolloutStatus: agent.RolloutActive}, "peer": {RolloutStatus: tc.memberStatus}, "outsider": {RolloutStatus: agent.RolloutActive}}}
			req := request
			req.AssignedAgentID = tc.lead
			got, err := ResolveDesktopWork(t.Context(), c, scope, req)
			if (err != nil) != tc.wantError {
				t.Fatalf("request = %#v error=%v", got, err)
			}
			if err == nil && (got.Owner != req.Owner || got.Actor.ID != "local-operator" || got.Budget == nil || got.Budget.MaxTurns != 16) {
				t.Fatalf("lost host authority: %#v", got)
			}
		})
	}
	request.Context = map[string]interface{}{"collaboration": "forged"}
	if _, err := DesktopWorkRequest(scope, request); err == nil {
		t.Fatal("accepted renderer-owned team context")
	}
}
