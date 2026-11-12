package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/validation"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// teamCreateTestProvisioner creates Teams the way the Engine does: its
// Agents must be in the space, then the registry create and the placement.
type teamCreateTestProvisioner struct {
	*agentBuilderTestProvisioner
	teams       *kernelteam.Registry
	teamCreates int
}

func (p *teamCreateTestProvisioner) CreateTeamDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelteam.Deployment, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	scope := Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}
	agents := make([]string, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		agents = append(agents, assignment.AgentDeploymentID)
	}
	if err := p.spaces.CheckSpaceAgents(ctx, scope, spaceID, agents); err != nil {
		return nil, nil, err
	}
	created, activation, err := p.teams.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err != nil {
		return nil, nil, err
	}
	p.teamCreates++
	if _, _, err := p.spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: SpaceMemberTeam, MemberID: created.ID}); err != nil {
		return nil, nil, err
	}
	return created, activation, nil
}

type teamCreateFixture struct {
	*agentBuilderDispatchFixture
	teamProvision *teamCreateTestProvisioner
	teamValidator *TeamCreateActionValidator
}

func newTeamCreateFixture(t *testing.T) *teamCreateFixture {
	t.Helper()
	ctx := context.Background()
	base := newAgentBuilderDispatchFixture(t)
	if err := base.catalog.Register(ctx, TeamManagementSkill()); err != nil {
		t.Fatal(err)
	}
	skillScope := skill.ScopeReference{Kind: base.scope.Kind, ID: base.scope.ID}
	for _, binding := range []*skill.Binding{
		{
			ID: "bundled:teams", Revision: 1, Scope: skillScope, DeploymentID: "finn",
			SkillID: TeamManagementSkillID, SkillVersion: TeamManagementSkillVersion,
			AllowedActions: []string{TeamActionCreateTeam}, MaximumRisk: skill.RiskLevelWrite,
		},
		{
			ID: "bundled:teams", Revision: 1, Scope: skillScope, DeploymentID: "researcher",
			SkillID: TeamManagementSkillID, SkillVersion: TeamManagementSkillVersion,
			AllowedActions: []string{TeamActionCreateTeam}, MaximumRisk: skill.RiskLevelWrite,
		},
	} {
		if err := base.catalog.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	for _, spaceID := range []string{base.homeID, base.salesID} {
		for _, agentID := range []string{"researcher", "platform"} {
			if _, _, err := base.spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: base.scope, SpaceID: spaceID, Type: SpaceMemberAgent, MemberID: agentID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	provision := &teamCreateTestProvisioner{agentBuilderTestProvisioner: base.provision, teams: base.teams}
	dispatcher, err := newTestAgentBuilderDispatcher(base.store, base.agents, base.teams, base.catalog, provision, base.recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	base.dispatcher = dispatcher
	validator, err := NewTeamCreateActionValidator(base.agents, base.teams, base.spaces, base.store, base.store)
	if err != nil {
		t.Fatal(err)
	}
	return &teamCreateFixture{agentBuilderDispatchFixture: base, teamProvision: provision, teamValidator: validator}
}

func (f *teamCreateFixture) teamBound(t *testing.T, deploymentID string) *skill.BoundAction {
	t.Helper()
	bound, err := f.catalog.Resolve(context.Background(), skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, deploymentID,
		TeamManagementSkillID, TeamManagementSkillVersion, TeamActionCreateTeam)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// proposeTeam runs the coordinator's order: resolve, schema check, validate.
func (f *teamCreateFixture) proposeTeam(t *testing.T, arguments map[string]interface{}) (map[string]interface{}, map[string]interface{}, error) {
	t.Helper()
	ctx := context.Background()
	bound := f.teamBound(t, "finn")
	input := ActionProposalValidationInput{Run: f.run, Bound: bound, Arguments: arguments}
	resolved, handled, err := f.teamValidator.ResolveActionProposalArguments(ctx, input)
	if err != nil {
		return nil, nil, err
	}
	if handled {
		input.Arguments = resolved
	}
	if err := f.catalog.ValidateInput(ctx, bound, input.Arguments); err != nil {
		return nil, input.Arguments, err
	}
	preview, err := f.teamValidator.ValidateActionProposal(ctx, input)
	return preview, input.Arguments, err
}

func (f *teamCreateFixture) executeTeam(t *testing.T, callID string, arguments map[string]interface{}, approver ApprovalPrincipal) (map[string]interface{}, error) {
	t.Helper()
	call := &ActionCall{
		ID: callID, Scope: f.scope, RunID: f.run.ID, DeploymentID: "finn", SkillID: TeamManagementSkillID,
		SkillVersion: TeamManagementSkillVersion, Action: TeamActionCreateTeam, Arguments: arguments,
		ApprovalID: "approval-" + callID,
	}
	decider := approver
	f.store.approvals[call.ApprovalID] = &ApprovalCheckpoint{
		ID: call.ApprovalID, Scope: f.scope, RunID: f.run.ID, ActionCallID: callID,
		Status: ApprovalStatusApproved, DecisionBy: &decider,
	}
	return f.dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Call: call, Run: f.run, Bound: f.teamBound(t, "finn"), Arguments: arguments,
	})
}

func (f *teamCreateFixture) teamInSpace(t *testing.T, spaceID, teamID string) bool {
	t.Helper()
	members, err := f.spaces.ListSpaceMembers(context.Background(), f.scope, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.Type == SpaceMemberTeam && member.ID == teamID {
			return true
		}
	}
	return false
}

func teamCreateArgumentsFor(spaceID string) map[string]interface{} {
	arguments := map[string]interface{}{
		"displayName": " Launch Crew ",
		"purpose":     "Ship product launches.",
		"members":     []interface{}{"researcher", "platform"},
		"rationale":   "The person wants a launch team.",
	}
	if spaceID != "" {
		arguments["spaceId"] = spaceID
	}
	return arguments
}

func TestTeamCreatePreview(t *testing.T) {
	f := newTeamCreateFixture(t)
	preview, _, err := f.proposeTeam(t, teamCreateArgumentsFor(f.salesID))
	if err != nil {
		t.Fatal(err)
	}
	assertAgentBuilderJSON(t, preview, fmt.Sprintf(`{
		"skillId": "openseal.teams", "skillVersion": "2.2.0", "action": "create_team", "risk": "write", "sideEffect": "write",
		"resourceType": "team", "operation": "create_team",
		"team": {"displayName": "Launch Crew", "purpose": "Ship product launches."},
		"approvals": {"maximumRisk": "write"},
		"members": [
			{"agentId": "researcher", "displayName": "Researcher", "purpose": "Researcher work"},
			{"agentId": "platform", "displayName": "Platform Helper", "purpose": "Platform Helper work"}
		],
		"space": {"id": %q, "name": "Sales"},
		"rationale": "The person wants a launch team."
	}`, f.salesID))
	// The preview is what the D11 approval rule and the drafts rail key on.
	if !isAgentBuilderApproval(&ApprovalCheckpoint{ProposedAction: preview}) {
		t.Fatal("create_team preview is not recognised as an agent-builder card")
	}
}

func TestTeamCreateResolvesSpaceToDefault(t *testing.T) {
	f := newTeamCreateFixture(t)
	preview, arguments, err := f.proposeTeam(t, teamCreateArgumentsFor(""))
	if err != nil {
		t.Fatal(err)
	}
	defaultSpace := f.homeID
	if arguments["spaceId"] != defaultSpace {
		t.Fatalf("spaceId = %#v, want %q", arguments["spaceId"], defaultSpace)
	}
	if space := preview["space"].(map[string]interface{}); space["id"] != defaultSpace || space["name"] != "Home" {
		t.Fatalf("space = %#v", space)
	}
}

func TestTeamCreateRefusals(t *testing.T) {
	f := newTeamCreateFixture(t)
	ctx := context.Background()
	with := func(key string, value interface{}) map[string]interface{} {
		arguments := teamCreateArgumentsFor(f.salesID)
		arguments[key] = value
		return arguments
	}
	for name, test := range map[string]struct {
		arguments map[string]interface{}
		want      string
	}{
		"one member":         {with("members", []interface{}{"researcher"}), ""},
		"builder as member":  {with("members", []interface{}{"researcher", "finn"}), "cannot be a member"},
		"unknown member":     {with("members", []interface{}{"researcher", "nobody"}), "does not exist"},
		"retired member":     {with("members", []interface{}{"researcher", "old-agent"}), "retired"},
		"agent name":         {with("displayName", " researcher "), "already used"},
		"team name":          {with("displayName", "ops crew"), "already used"},
		"unknown space":      {with("spaceId", "space-gone"), "does not exist"},
		"no rationale":       {with("rationale", " "), "rationale"},
		"control characters": {with("displayName", "Launch\nCrew"), "control characters"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := f.proposeTeam(t, test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}

	// The schema refuses duplicates; the validator refuses them on its own
	// too, as it also runs at execution.
	input := ActionProposalValidationInput{
		Run: f.run, Bound: f.teamBound(t, "finn"),
		Arguments: with("members", []interface{}{"researcher", " researcher"}),
	}
	if _, err := f.teamValidator.ValidateActionProposal(ctx, input); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate members err = %v", err)
	}
	input.Arguments = with("members", []interface{}{"researcher"})
	if _, err := f.teamValidator.ValidateActionProposal(ctx, input); err == nil || !strings.Contains(err.Error(), "2 to 12") {
		t.Fatalf("one member err = %v", err)
	}

	input.Arguments = with("purpose", "Use api key sk-live-0123456789abcdefghijklmnop for deploys")
	_, err := f.teamValidator.ValidateActionProposal(ctx, input)
	var sensitive *validation.SensitiveInputError
	if !errors.As(err, &sensitive) && !errors.Is(err, validation.ErrSensitiveInput) {
		t.Fatalf("sensitive purpose err = %v", err)
	}

	// Only the Run's own Agent may create through its binding.
	input.Arguments = teamCreateArgumentsFor(f.salesID)
	input.Bound = f.teamBound(t, "researcher")
	if _, err := f.teamValidator.ValidateActionProposal(ctx, input); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("foreign binding err = %v", err)
	}
}

func TestTeamCreateExecutesLikeTheComposer(t *testing.T) {
	ctx := context.Background()
	f := newTeamCreateFixture(t)
	_, arguments, err := f.proposeTeam(t, teamCreateArgumentsFor(f.salesID))
	if err != nil {
		t.Fatal(err)
	}
	output, err := f.executeTeam(t, "call-team", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	teamID := teamBuilderDefinitionID(f.scope, "Launch Crew", "call-team")
	if !strings.HasPrefix(teamID, "team-launch-crew-") || len(teamID) != len("team-launch-crew-")+8 {
		t.Fatalf("team id = %q", teamID)
	}
	if output["resourceType"] != "team" || output["operation"] != TeamActionCreateTeam || output["replayed"] != false ||
		output["deploymentId"] != teamID || output["displayName"] != "Launch Crew" {
		t.Fatalf("output = %#v", output)
	}
	if space := output["space"].(map[string]interface{}); space["id"] != f.salesID || space["name"] != "Sales" {
		t.Fatalf("space = %#v", space)
	}

	definition, err := f.teams.GetDefinition(ctx, teamID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if definition.DisplayName != "Launch Crew" || definition.Purpose != "Ship product launches." ||
		definition.Approvals.MaximumRisk != capability.RiskLevelWrite ||
		definition.Provenance != (workforce.DefinitionProvenance{Source: "agent-builder", Reference: "call-team", CreatedBy: "person-1"}) {
		t.Fatalf("definition = %#v", definition)
	}

	deployment, err := f.teams.GetDeployment(ctx, f.reference(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	wantRoster := []kernelteam.RosterAssignment{
		{ID: "researcher-1", AgentDeploymentID: "researcher", DisplayName: "Researcher", Purpose: "Researcher work", ChannelParticipation: kernelteam.ChannelActive},
		{ID: "platform-helper-2", AgentDeploymentID: "platform", DisplayName: "Platform Helper", Purpose: "Platform Helper work", ChannelParticipation: kernelteam.ChannelActive},
	}
	if deployment.Status != kernelteam.DeploymentActive || deployment.DefinitionID != teamID || deployment.ActiveVersion != "1" ||
		!reflect.DeepEqual(deployment.Roster, wantRoster) {
		t.Fatalf("deployment = %#v", deployment)
	}
	if !f.teamInSpace(t, f.salesID, teamID) || f.teamInSpace(t, f.homeID, teamID) {
		t.Fatal("team is not only in the reviewed space")
	}

	// A replay of the same call converges on the same Team.
	replay, err := f.executeTeam(t, "call-team", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	if replay["replayed"] != true || replay["deploymentId"] != teamID {
		t.Fatalf("replay = %#v", replay)
	}
	versions, err := f.teams.ListDefinitionVersions(ctx, teamID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("definition versions = %d, %v", len(versions), err)
	}
	if f.teamProvision.teamCreates != 1 {
		t.Fatalf("team creates = %d, want 1", f.teamProvision.teamCreates)
	}
	if !f.teamInSpace(t, f.salesID, teamID) {
		t.Fatal("replay dropped the space membership")
	}
}

func TestTeamCreateWithoutPurposeUsesComposerDefault(t *testing.T) {
	ctx := context.Background()
	f := newTeamCreateFixture(t)
	arguments := teamCreateArgumentsFor("")
	delete(arguments, "purpose")
	_, arguments, err := f.proposeTeam(t, arguments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.executeTeam(t, "call-plain", arguments, agentBuilderApprover); err != nil {
		t.Fatal(err)
	}
	teamID := teamBuilderDefinitionID(f.scope, "Launch Crew", "call-plain")
	definition, err := f.teams.GetDefinition(ctx, teamID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if definition.Purpose != "Launch Crew team" {
		t.Fatalf("purpose = %q", definition.Purpose)
	}
	if !f.teamInSpace(t, f.homeID, teamID) || f.teamInSpace(t, f.salesID, teamID) {
		t.Fatal("team is not only in the person's default space")
	}
}

func TestTeamCreateRefusesNonUserApprover(t *testing.T) {
	f := newTeamCreateFixture(t)
	_, arguments, err := f.proposeTeam(t, teamCreateArgumentsFor(f.salesID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.executeTeam(t, "call-role", arguments, ApprovalPrincipal{Type: "role", ID: "admins"}); err == nil ||
		!strings.Contains(err.Error(), "only by a person") {
		t.Fatalf("role approver err = %v", err)
	}
	if f.teamProvision.teamCreates != 0 {
		t.Fatal("a Team was created without a person's approval")
	}
}

func TestTeamCreateFailsWhenNameTakenWhileWaiting(t *testing.T) {
	ctx := context.Background()
	f := newTeamCreateFixture(t)
	_, arguments, err := f.proposeTeam(t, teamCreateArgumentsFor(f.salesID))
	if err != nil {
		t.Fatal(err)
	}
	// Another Team takes the name while the card waits.
	first := teamCreateArgumentsFor(f.salesID)
	first["members"] = []interface{}{"platform", "researcher"}
	if _, err := f.executeTeam(t, "call-first", first, agentBuilderApprover); err != nil {
		t.Fatal(err)
	}
	if _, err := f.executeTeam(t, "call-second", arguments, agentBuilderApprover); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("second create err = %v", err)
	}
	if _, err := f.teams.GetDefinition(ctx, teamBuilderDefinitionID(f.scope, "Launch Crew", "call-second"), "1"); !errors.Is(err, kernelteam.ErrDefinitionNotFound) {
		t.Fatalf("second definition err = %v", err)
	}
}

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

// Earlier bindings keep matching update_profile in hosts until the reconcile
// upgrades them; update_member exists only in the current version.
func TestTeamMemberActionIsOnlyInTheCurrentContract(t *testing.T) {
	for version, want := range map[string]bool{"2.2.0": true, "2.1.0": false, "2.0.0": false, "1.0.0": false} {
		if got := IsSupportedTeamManagementSkillVersion(version); got != (version != "1.0.0") {
			t.Errorf("IsSupportedTeamManagementSkillVersion(%s) = %v", version, got)
		}
		definition := TeamManagementSkill()
		definition.Version = version
		bound := &skill.BoundAction{Definition: definition, Action: definition.Actions[TeamActionUpdateMember]}
		if got := isTeamMemberAction(bound); got != want {
			t.Errorf("isTeamMemberAction(%s) = %v", version, got)
		}
	}
	encoded, err := json.Marshal(TeamManagementSkill())
	if err != nil || strings.Contains(strings.ToLower(string(encoded)), "role") {
		t.Fatalf("openseal.teams still mentions roles: %s, %v", encoded, err)
	}
}

func TestTeamCreateRefusesAgentsOutsideTheSpace(t *testing.T) {
	ctx := context.Background()
	f := newTeamCreateFixture(t)
	ops, err := f.spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: f.scope, Name: "Ops", Look: DefaultSpaceLook, OwnerID: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: f.scope, SpaceID: ops.ID, Type: SpaceMemberAgent, MemberID: "researcher"}); err != nil {
		t.Fatal(err)
	}
	// The card is refused before review, and so is a Team approved while
	// one of its Agents left the space.
	if _, _, err := f.proposeTeam(t, teamCreateArgumentsFor(ops.ID)); !errors.Is(err, ErrTeamAgentOutsideSpace) ||
		!strings.Contains(err.Error(), `agent "platform" is not in space "Ops"`) {
		t.Fatalf("propose err = %v", err)
	}
	_, arguments, err := f.proposeTeam(t, teamCreateArgumentsFor(f.salesID))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.spaces.DeleteSpaceMember(ctx, DeleteSpaceMemberRequest{Scope: f.scope, SpaceID: f.salesID, Type: SpaceMemberAgent, MemberID: "platform"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.executeTeam(t, "call-outside", arguments, agentBuilderApprover); !errors.Is(err, ErrTeamAgentOutsideSpace) {
		t.Fatalf("execute err = %v", err)
	}
	if f.teamProvision.teamCreates != 0 {
		t.Fatalf("team creates = %d", f.teamProvision.teamCreates)
	}
}
