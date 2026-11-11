package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// umbrellaCatalog serves the Team and Agent fixtures of resolverCatalog and
// activates Skills through a real catalog.
type umbrellaCatalog struct {
	*resolverCatalog
	skills *skill.Catalog
}

func (c *umbrellaCatalog) ActivateSkills(ctx context.Context, scope skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return c.skills.Activate(ctx, scope, deploymentID, host)
}

func umbrellaTestAction(name string, risk capability.RiskLevel, credentials ...capability.CredentialRequirement) skill.Action {
	return skill.Action{Name: name, Description: name, Risk: risk, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencySupported,
		InputSchema: map[string]interface{}{"type": "object"}, Credentials: credentials}
}

func newUmbrellaFixture(t *testing.T) (*umbrellaCatalog, Scope) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "one"}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	catalog := skill.NewCatalog()
	access := capability.CredentialRequirement{Name: "access", Kind: "oauth2"}
	for _, definition := range []*skill.Definition{
		{ID: "gmail", Version: "1", Name: "Gmail", Transport: skill.TransportReference{Kind: "local"}, Actions: map[string]skill.Action{"send": umbrellaTestAction("send", capability.RiskLevelExternal, access)}},
		{ID: "notes", Version: "1", Name: "Notes", Transport: skill.TransportReference{Kind: "local"}, Actions: map[string]skill.Action{"write": umbrellaTestAction("write", capability.RiskLevelWrite)}},
		{ID: "forum", Version: "1", Name: "Forum", Transport: skill.TransportReference{Kind: "local"}, Actions: map[string]skill.Action{"post": umbrellaTestAction("post", capability.RiskLevelExternal, access)}},
		{ID: "drive", Version: "1", Name: "Drive", Transport: skill.TransportReference{Kind: "local"}, Actions: map[string]skill.Action{"read": umbrellaTestAction("read", capability.RiskLevelRead, access)}},
	} {
		if err := catalog.Register(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
	}
	for _, binding := range []*skill.Binding{
		{ID: "gmail", DeploymentID: "member", SkillID: "gmail", SkillVersion: "1", AllowedActions: []string{"send"}, MaximumRisk: capability.RiskLevelExternal,
			Credentials: map[string]skill.CredentialReference{"access": {Kind: "oauth2", ID: "member-grant"}}},
		{ID: "notes", DeploymentID: "member", SkillID: "notes", SkillVersion: "1", AllowedActions: []string{"write"}, MaximumRisk: capability.RiskLevelWrite},
		{ID: "forum", DeploymentID: "crew", SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"post"}, MaximumRisk: capability.RiskLevelExternal,
			Credentials: map[string]skill.CredentialReference{"access": {Kind: "oauth2", ID: "team-grant"}}},
	} {
		binding.Scope, binding.Revision = skillScope, 1
		if err := catalog.Bind(t.Context(), binding); err != nil {
			t.Fatal(err)
		}
	}
	// The host supplies one connected app per umbrella: Drive.
	catalog.SetHostBindingProvider(func(_ context.Context, scope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if actor.Umbrella.ID != deploymentID {
			t.Fatalf("host asked for %s under umbrella %#v", deploymentID, actor.Umbrella)
		}
		return &skill.HostBindingSet{Fingerprint: deploymentID, Bindings: []*skill.Binding{{
			ID: "drive-" + deploymentID, Scope: scope, DeploymentID: deploymentID, SkillID: "drive", SkillVersion: "1",
			AllowedActions: []string{"read"}, MaximumRisk: capability.RiskLevelRead, Revision: 1,
			Credentials: map[string]skill.CredentialReference{"access": {Kind: "oauth2", ID: "drive-" + deploymentID}},
		}}}, nil
	})
	return &umbrellaCatalog{skills: catalog, resolverCatalog: &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "member", Scope: skillScope, DefinitionID: "member", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "member", Version: "1", Purpose: "Help", SystemPrompt: "Help.", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal}},
		teamDeployment: &kernelteam.Deployment{
			ID: "crew", Scope: skillScope, DefinitionID: "crew", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
			Roster: []kernelteam.RosterAssignment{{ID: "member", RoleID: "member", AgentDeploymentID: "member"}},
		},
		teamDefinition: &kernelteam.Definition{ID: "crew", Version: "1", Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelExternal}, Roles: []kernelteam.RoleSlot{{ID: "member"}}},
	}}, scope
}

func umbrellaRunActions(t *testing.T, catalog *umbrellaCatalog, run *AgentRun) []string {
	t.Helper()
	binding, err := ResolveLoopBinding(t.Context(), catalog, run, loopBindingTestConfig(t, newTestKernelStore(t), &progressAckStepHost{}))
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, action := range binding.ModelActions {
		actions = append(actions, action.DeploymentID+":"+action.SkillID)
	}
	slices.Sort(actions)
	return actions
}

func TestTeamRunUsesTeamConnectionsAndNeverTheMembers(t *testing.T) {
	catalog, scope := newUmbrellaFixture(t)
	teamRun := &AgentRun{ID: "team-run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "member", Context: map[string]interface{}{}}
	got := umbrellaRunActions(t, catalog, teamRun)
	want := []string{"crew:drive", "crew:forum", "member:notes"}
	if !slices.Equal(got, want) {
		t.Fatalf("Team run actions = %v, want %v (member Gmail and member Drive must not appear)", got, want)
	}

	agentRun := &AgentRun{ID: "agent-run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "member"}, AssignedAgentID: "member", Context: map[string]interface{}{}}
	got = umbrellaRunActions(t, catalog, agentRun)
	want = []string{"member:drive", "member:gmail", "member:notes"}
	if !slices.Equal(got, want) {
		t.Fatalf("1:1 run actions = %v, want %v (no Team binding or Team app)", got, want)
	}
}

func TestTeamAndMemberActivationsNeverShareASnapshot(t *testing.T) {
	catalog, scope := newUmbrellaFixture(t)
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	agentRun := &AgentRun{ID: "run", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "member"}, AssignedAgentID: "member"}
	teamRun := &AgentRun{ID: "run", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "member"}
	ctx := t.Context()
	oneToOne, err := catalog.skills.Activate(skill.WithRunActor(ctx, RunSkillActor(agentRun)), skillScope, "member", skill.HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	inTeam, err := catalog.skills.Activate(skill.WithoutConnections(skill.WithRunActor(ctx, RunSkillActor(teamRun))), skillScope, "member", skill.HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := catalog.skills.Activate(skill.WithRunActor(ctx, RunSkillActor(agentRun)), skillScope, "member", skill.HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	if oneToOne.SnapshotID == inTeam.SnapshotID || again.SnapshotID != oneToOne.SnapshotID || len(again.Skills) != 3 || len(inTeam.Skills) != 1 ||
		!inTeam.WithoutConnections || inTeam.Umbrella == nil || inTeam.Umbrella.ID != "crew" {
		t.Fatalf("1:1 %s (%d skills), Team %s (%d skills), again %s (%d skills)", oneToOne.SnapshotID, len(oneToOne.Skills), inTeam.SnapshotID, len(inTeam.Skills), again.SnapshotID, len(again.Skills))
	}
}

func TestRunUmbrellaFailsClosed(t *testing.T) {
	catalog, scope := newUmbrellaFixture(t)
	ctx := context.Background()
	if owner, err := RunUmbrella(ctx, catalog, &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "member"}, AssignedAgentID: "member"}); err != nil || owner.ID != "member" {
		t.Fatalf("1:1 umbrella = %#v, %v", owner, err)
	}
	if owner, err := RunUmbrella(ctx, catalog, &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "member"}); err != nil || owner.ID != "crew" {
		t.Fatalf("Team umbrella = %#v, %v", owner, err)
	}
	for name, run := range map[string]*AgentRun{
		"Agent run for another Agent": {Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}, AssignedAgentID: "member"},
		"non-member":                  {Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "outsider"},
		"no acting Agent":             {Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}},
	} {
		if _, err := RunUmbrella(ctx, catalog, run); !errors.Is(err, ErrRunUmbrellaUnavailable) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	catalog.deployment.RolloutStatus = kernelagent.RolloutPaused
	if _, err := RunUmbrella(ctx, catalog, &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "member"}); !errors.Is(err, ErrRunUmbrellaUnavailable) {
		t.Fatalf("inactive member: err = %v", err)
	}
	catalog.deployment.RolloutStatus = kernelagent.RolloutActive
	catalog.teamDeployment.Status = kernelteam.DeploymentPaused
	if _, err := RunUmbrella(ctx, catalog, &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "member"}); !errors.Is(err, ErrRunUmbrellaUnavailable) {
		t.Fatalf("paused Team: err = %v", err)
	}
}
