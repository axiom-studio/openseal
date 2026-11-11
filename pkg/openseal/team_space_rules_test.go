package openseal

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestMemoryTeamRostersFollowSpaces(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	exerciseTeamSpaceRules(t, engine)
}

func TestSQLiteTeamRostersFollowSpaces(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "team-spaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	exerciseTeamSpaceRules(t, engine)
}

// exerciseTeamSpaceRules is the contract every store keeps: a Team's Agents
// are in each of its spaces.
func exerciseTeamSpaceRules(t *testing.T, engine *Engine) {
	t.Helper()
	ctx := t.Context()
	scope := capability.ScopeReference{Kind: "tenant", ID: "rules"}
	spaceScope := runtime.Scope{Kind: scope.Kind, ID: scope.ID}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "rules-helper", Version: "1", DisplayName: "Helper", Purpose: "Help", SystemPrompt: "Help.",
		Authority: AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every Agent starts in Default; finn stands for the assistant the host
	// keeps in every space.
	for _, id := range []string{"ana", "ben", "cy", "finn"} {
		if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
			ID: id, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "7", "create"); err != nil {
			t.Fatal(err)
		}
	}
	newSpace := func(name string, agents ...string) string {
		t.Helper()
		space, err := engine.CreateSpace(ctx, CreateSpaceRequest{Scope: spaceScope, Name: name, Look: runtime.DefaultSpaceLook})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range agents {
			if _, _, err := engine.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: spaceScope, SpaceID: space.ID, Type: runtime.SpaceMemberAgent, MemberID: id}); err != nil {
				t.Fatal(err)
			}
		}
		return space.ID
	}
	members := func(spaceID string) []string {
		t.Helper()
		listed, err := engine.ListSpaceMembers(ctx, spaceScope, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(listed))
		for _, member := range listed {
			keys = append(keys, string(member.Type)+":"+member.ID)
		}
		slices.Sort(keys)
		return keys
	}
	roster := func(teamID string) []string {
		t.Helper()
		team, err := engine.GetTeamDeployment(ctx, scope, teamID)
		if err != nil {
			t.Fatal(err)
		}
		return teamRosterAgents(team.Roster)
	}
	// team builds one role per Agent, the shape HelloSeal's composer makes.
	team := func(id, version string, agents ...string) *TeamDeployment {
		t.Helper()
		roles := make([]TeamRoleSlot, 0, len(agents))
		assignments := make([]TeamRosterAssignment, 0, len(agents))
		for _, agent := range agents {
			roles = append(roles, TeamRoleSlot{ID: agent, DisplayName: agent, Purpose: "Help", MinimumMembers: 1, MaximumMembers: 1})
			assignments = append(assignments, TeamRosterAssignment{ID: agent, RoleID: agent, AgentDeploymentID: agent})
		}
		if _, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
			ID: id, Version: version, DisplayName: strings.ToUpper(id), Purpose: "Work together", Roles: roles,
			Approvals: TeamApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
		}); err != nil {
			t.Fatal(err)
		}
		return &TeamDeployment{ID: id, Scope: scope, DefinitionID: id, ActiveVersion: version, Status: TeamDeploymentActive, Roster: assignments}
	}
	// join moves the Team to a definition version whose roster adds agents.
	join := func(id, version string, agents ...string) error {
		t.Helper()
		current, err := engine.GetTeamDeployment(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		next := team(id, version, append(teamRosterAgents(current.Roster), agents...)...)
		_, _, err = engine.UpdateTeamDeployment(ctx, next, current.Revision, "user", "7", "add members")
		return err
	}
	defaultID := runtime.DefaultSpaceID(spaceScope)
	work := newSpace("Work", "ana", "ben", "finn")

	// Rule 1: a Team is created from, and joined by, Agents in its space.
	_, _, err = engine.CreateTeamDeploymentInSpace(ctx, work, team("crew", "0", "ana", "cy"), "user", "7", "create")
	if !errors.Is(err, ErrTeamAgentOutsideSpace) || err.Error() != `a team's agents must be in its space: agent "cy" is not in space "Work"` {
		t.Fatalf("create with an outside Agent: %v", err)
	}
	if _, _, err := engine.CreateTeamDeploymentInSpace(ctx, work, team("crew", "1", "ana", "ben"), "user", "7", "create"); err != nil {
		t.Fatal(err)
	}
	if got := members(work); !slices.Equal(got, []string{"agent:ana", "agent:ben", "agent:finn", "team:crew"}) || slices.Contains(members(defaultID), "team:crew") {
		t.Fatalf("work = %v, default = %v", got, members(defaultID))
	}
	if err := join("crew", "2", "cy"); !errors.Is(err, ErrTeamAgentOutsideSpace) {
		t.Fatalf("join from outside the space: %v", err)
	}
	if err := join("crew", "3", "finn"); err != nil {
		t.Fatalf("join from the space: %v", err)
	}

	// Rule 3: a Team added to a space brings its Agents, once.
	lab := newSpace("Lab", "cy")
	for range 2 {
		if _, _, err := engine.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: spaceScope, SpaceID: lab, Type: runtime.SpaceMemberTeam, MemberID: "crew"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := members(lab); !slices.Equal(got, []string{"agent:ana", "agent:ben", "agent:cy", "agent:finn", "team:crew"}) {
		t.Fatalf("lab = %v", got)
	}
	// In two spaces, an Agent must be in both to join: dee is only in Work.
	if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "dee", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "7", "create"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: spaceScope, SpaceID: work, Type: runtime.SpaceMemberAgent, MemberID: "dee"}); err != nil {
		t.Fatal(err)
	}
	if err := join("crew", "4", "dee"); !errors.Is(err, ErrTeamAgentOutsideSpace) || !strings.Contains(err.Error(), `not in space "Lab"`) {
		t.Fatalf("join from one of two spaces: %v", err)
	}

	// Rule 2: leaving a space leaves that space's Teams, and no others.
	if _, _, err := engine.CreateTeamDeployment(ctx, team("elsewhere", "1", "ben", "cy"), "user", "7", "create"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeploymentInSpace(ctx, work, team("solo", "1", "ana"), "user", "7", "create"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeploymentInSpace(ctx, lab, team("lone", "1", "ana"), "user", "7", "create"); err != nil {
		t.Fatal(err)
	}
	remove := func(spaceID, agentID string, confirm bool) error {
		return engine.DeleteSpaceMember(ctx, DeleteSpaceMemberRequest{
			Scope: spaceScope, SpaceID: spaceID, Type: runtime.SpaceMemberAgent, MemberID: agentID, ActorType: "user", ActorID: "7", ConfirmTeams: confirm,
		})
	}
	// needsConfirmation is the Teams an unconfirmed removal reported.
	needsConfirmation := func(err error) []SpaceAgentRemovalTeam {
		t.Helper()
		var pending *SpaceAgentRemovalNeedsConfirmation
		if !errors.As(err, &pending) {
			t.Fatalf("want a removal that needs confirmation, got %v", err)
		}
		return pending.Teams
	}
	// An Agent on no Team of the space leaves at once.
	if err := remove(work, "dee", false); err != nil || slices.Contains(members(work), "agent:dee") {
		t.Fatalf("removing an Agent on no Team: %v, work = %v", err, members(work))
	}
	// A Team that keeps members: unconfirmed, nothing changes.
	if got := needsConfirmation(remove(work, "ben", false)); !slices.Equal(got, []SpaceAgentRemovalTeam{{ID: "crew", DisplayName: "CREW", Outcome: SpaceTeamLeaves, Remaining: 2}}) {
		t.Fatalf("unconfirmed removal of ben = %+v", got)
	}
	if !slices.Contains(members(work), "agent:ben") || !slices.Equal(roster("crew"), []string{"ana", "ben", "finn"}) {
		t.Fatalf("unconfirmed removal changed work = %v, crew = %v", members(work), roster("crew"))
	}
	if err := remove(work, "ben", true); err != nil {
		t.Fatal(err)
	}
	if got := roster("crew"); !slices.Equal(got, []string{"ana", "finn"}) {
		t.Fatalf("crew after ben left Work = %v", got)
	}
	if got := roster("elsewhere"); !slices.Equal(got, []string{"ben", "cy"}) {
		t.Fatalf("a Team outside Work changed: %v", got)
	}
	if slices.Contains(members(work), "agent:ben") || !slices.Contains(members(lab), "agent:ben") {
		t.Fatalf("work = %v, lab = %v", members(work), members(lab))
	}
	// The removal went through the registry: ben's role is gone in a new
	// definition version, and the activation names who did it and why.
	crew, err := engine.GetTeamDeployment(ctx, scope, "crew")
	if err != nil {
		t.Fatal(err)
	}
	crewDefinition, err := engine.GetTeamDefinition(ctx, crew.DefinitionID, crew.ActiveVersion)
	if err != nil || crew.ActiveVersion != "5" || len(crewDefinition.Roles) != 2 {
		t.Fatalf("crew version = %q, definition = %+v, err = %v", crew.ActiveVersion, crewDefinition, err)
	}
	activations, err := engine.ListTeamDefinitionActivations(ctx, scope, "crew")
	if err != nil || len(activations) != int(crew.Revision) {
		t.Fatalf("activations = %+v, err = %v", activations, err)
	}
	if last := activations[len(activations)-1]; last.ActorType != "user" || last.ActorID != "7" || last.FromVersion != "3" || last.ToVersion != "5" ||
		last.Reason != "Agent ben left the space Work" {
		t.Fatalf("activation = %+v", last)
	}

	// A Team that would be left without a member is archived. Unconfirmed,
	// nothing changes: not the space, and not the other Team listing ana.
	if got := needsConfirmation(remove(work, "ana", false)); !slices.Equal(got, []SpaceAgentRemovalTeam{
		{ID: "crew", DisplayName: "CREW", Outcome: SpaceTeamLeaves, Remaining: 1},
		{ID: "solo", DisplayName: "SOLO", Outcome: SpaceTeamArchived, Remaining: 0},
	}) {
		t.Fatalf("unconfirmed removal of ana = %+v", got)
	}
	status := func(teamID string) TeamDeploymentStatus {
		t.Helper()
		team, err := engine.GetTeamDeployment(ctx, scope, teamID)
		if err != nil {
			t.Fatal(err)
		}
		return team.Status
	}
	if !slices.Contains(members(work), "agent:ana") || !slices.Equal(roster("crew"), []string{"ana", "finn"}) || status("solo") != TeamDeploymentActive {
		t.Fatalf("unconfirmed removal changed work = %v, crew = %v, solo = %s", members(work), roster("crew"), status("solo"))
	}
	if err := remove(work, "ana", true); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(members(work), "agent:ana") || !slices.Equal(roster("crew"), []string{"finn"}) {
		t.Fatalf("after ana left: work = %v, crew = %v", members(work), roster("crew"))
	}
	// Archived the way a retire does: same roster, one more signed revision.
	if status("solo") != TeamDeploymentArchived || !slices.Equal(roster("solo"), []string{"ana"}) {
		t.Fatalf("solo = %s %v", status("solo"), roster("solo"))
	}
	if soloActivations, err := engine.ListTeamDefinitionActivations(ctx, scope, "solo"); err != nil || len(soloActivations) != 2 ||
		soloActivations[1].ActorID != "7" || soloActivations[1].Reason != "Agent ana left the space Work" {
		t.Fatalf("solo activations = %+v, err = %v", soloActivations, err)
	}
	// ana's Team in another space is untouched.
	if status("lone") != TeamDeploymentActive || !slices.Equal(roster("lone"), []string{"ana"}) || !slices.Contains(members(lab), "agent:ana") {
		t.Fatalf("lone = %s %v, lab = %v", status("lone"), roster("lone"), members(lab))
	}
	// Leaving again changes nothing: an archived Team is not asked about.
	if err := remove(work, "ana", false); err != nil {
		t.Fatal(err)
	}

	// A Team that already breaks the rule keeps working: here "elsewhere" is
	// in Work without ben and cy, as data from before the rule can be.
	if _, _, err := engine.Store().(runtime.SpaceStore).PutSpaceMember(ctx, spaceScope, &runtime.SpaceMember{
		SpaceID: work, Type: runtime.SpaceMemberTeam, ID: "elsewhere", AddedAt: crew.UpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := engine.GetTeamDeployment(ctx, scope, "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	elsewhere.Status = TeamDeploymentPaused
	if elsewhere, _, err = engine.UpdateTeamDeployment(ctx, elsewhere, elsewhere.Revision, "user", "7", "pause"); err != nil {
		t.Fatalf("update a Team with Agents outside its space: %v", err)
	}
	if err := join("elsewhere", "2", "finn"); err != nil {
		t.Fatalf("join a Team with Agents outside its space: %v", err)
	}
	if listed, err := engine.ListTeamDeployments(ctx, scope); err != nil || len(listed) != 4 {
		t.Fatalf("teams = %+v, err = %v", listed, err)
	}
	chat, _, err := engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: spaceScope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "elsewhere"}, Title: "Plan", SpaceID: work, IdempotencyKey: "plan",
	})
	if err != nil || chat.SpaceID != work {
		t.Fatalf("chat = %+v, err = %v", chat, err)
	}

	// A Team created without naming a space joins Default with its Agents.
	if err := remove(defaultID, "finn", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeployment(ctx, team("strays", "1", "finn"), "user", "7", "create"); err != nil {
		t.Fatal(err)
	}
	if got := members(defaultID); !slices.Contains(got, "team:strays") || !slices.Contains(got, "agent:finn") {
		t.Fatalf("default after an unplaced create = %v", got)
	}

	// A deleted space's Teams fall back to Default with their Agents.
	only := newSpace("Only", "dee")
	if err := remove(defaultID, "dee", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeploymentInSpace(ctx, only, team("duo", "1", "dee"), "user", "7", "create"); err != nil {
		t.Fatal(err)
	}
	if err := engine.DeleteSpace(ctx, spaceScope, only); err != nil {
		t.Fatal(err)
	}
	if got := members(defaultID); !slices.Contains(got, "team:duo") || !slices.Contains(got, "agent:dee") {
		t.Fatalf("default after the space was deleted = %v", got)
	}
}
