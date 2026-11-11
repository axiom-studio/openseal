package openseal

import (
	"errors"
	"slices"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

// ownerSpaceFixture is an engine with one Agent definition and one Team
// definition in a scope that has no space.
type ownerSpaceFixture struct {
	t          *testing.T
	engine     *Engine
	scope      capability.ScopeReference
	spaceScope runtime.Scope
}

func newOwnerSpaceFixture(t *testing.T) *ownerSpaceFixture {
	t.Helper()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	f := &ownerSpaceFixture{t: t, engine: engine, scope: capability.ScopeReference{Kind: "tenant", ID: "owners"}}
	f.spaceScope = runtime.Scope{Kind: f.scope.Kind, ID: f.scope.ID}
	if _, err := engine.RegisterAgentDefinition(t.Context(), &AgentDefinition{
		ID: "helper", Version: "1", DisplayName: "Helper", Purpose: "Help", SystemPrompt: "Help.",
		Authority: AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RegisterTeamDefinition(t.Context(), &TeamDefinition{
		ID: "crew", Version: "1", DisplayName: "Crew", Purpose: "Work together",
		Approvals: TeamApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *ownerSpaceFixture) space(name, ownerID string) *runtime.Space {
	f.t.Helper()
	space, err := f.engine.CreateSpace(f.t.Context(), runtime.CreateSpaceRequest{Scope: f.spaceScope, Name: name, Look: runtime.DefaultSpaceLook, OwnerID: ownerID})
	if err != nil {
		f.t.Fatal(err)
	}
	return space
}

func (f *ownerSpaceFixture) agent(id string) *AgentDeployment {
	return &AgentDeployment{
		ID: id, Scope: f.scope, DefinitionID: "helper", ActiveVersion: "1",
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}
}

func (f *ownerSpaceFixture) team(id string, agents ...string) *TeamDeployment {
	roster := make([]TeamRosterAssignment, 0, len(agents))
	for _, agent := range agents {
		roster = append(roster, TeamRosterAssignment{ID: agent, AgentDeploymentID: agent})
	}
	return &TeamDeployment{ID: id, Scope: f.scope, DefinitionID: "crew", ActiveVersion: "1", Status: TeamDeploymentActive, Roster: roster}
}

// spacesOf returns the ids of every space the deployment is a member of.
func (f *ownerSpaceFixture) spacesOf(memberType runtime.SpaceMemberType, id string) []string {
	f.t.Helper()
	members, err := f.engine.ListScopeSpaceMembers(f.t.Context(), f.spaceScope)
	if err != nil {
		f.t.Fatal(err)
	}
	spaces := []string{}
	for _, member := range members {
		if member.Type == memberType && member.ID == id {
			spaces = append(spaces, member.SpaceID)
		}
	}
	slices.Sort(spaces)
	return spaces
}

func (f *ownerSpaceFixture) members(spaceID string) []string {
	f.t.Helper()
	listed, err := f.engine.ListSpaceMembers(f.t.Context(), f.spaceScope, spaceID)
	if err != nil {
		f.t.Fatal(err)
	}
	keys := make([]string, 0, len(listed))
	for _, member := range listed {
		keys = append(keys, string(member.Type)+":"+member.ID)
	}
	slices.Sort(keys)
	return keys
}

// controlSpaces returns the space of each control conversation of the Agent.
func (f *ownerSpaceFixture) controlSpaces(agentID string) []string {
	f.t.Helper()
	conversations, err := f.engine.ListConversations(f.t.Context(), runtime.ConversationFilter{
		Scope: f.spaceScope, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	spaces := []string{}
	for _, conversation := range conversations {
		if conversation.Origin == nil || conversation.Origin.Kind != runtime.ConversationReferenceAgentControl {
			f.t.Fatalf("conversation of %s = %+v", agentID, conversation)
		}
		spaces = append(spaces, conversation.SpaceID)
	}
	return spaces
}

func TestEngineCreatesNoSpaceOnItsOwn(t *testing.T) {
	f := newOwnerSpaceFixture(t)
	ctx := t.Context()
	for _, owner := range []string{"", "ana"} {
		if space, err := f.engine.DefaultSpace(ctx, f.spaceScope, owner); !errors.Is(err, ErrNoDefaultSpace) || space != nil {
			t.Fatalf("default space of %q = %+v, err = %v", owner, space, err)
		}
	}
	// Neither listing nor asking for a default space created one.
	spaces, err := f.engine.ListSpaces(ctx, f.spaceScope)
	if err != nil || len(spaces) != 0 {
		t.Fatalf("spaces = %+v, err = %v", spaces, err)
	}
	if _, _, err := f.engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: f.spaceScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Title: "Plan", CreatedBy: "ana", IdempotencyKey: "plan",
	}); !errors.Is(err, ErrNoDefaultSpace) {
		t.Fatalf("conversation with nowhere to go: %v", err)
	}
	if spaces, err = f.engine.ListSpaces(ctx, f.spaceScope); err != nil || len(spaces) != 0 {
		t.Fatalf("spaces after a refused conversation = %+v, err = %v", spaces, err)
	}

	// The first space an owner creates is their default, and only theirs.
	home, side, other := f.space("Home", "ana"), f.space("Side", "ana"), f.space("Den", "bo")
	if !home.IsDefault || side.IsDefault || !other.IsDefault || home.Access != runtime.SpaceAccessOwner {
		t.Fatalf("home = %+v, side = %+v, other = %+v", home, side, other)
	}
	if found, err := f.engine.DefaultSpace(ctx, f.spaceScope, "ana"); err != nil || found.ID != home.ID {
		t.Fatalf("ana's default = %+v, err = %v", found, err)
	}
	if err := f.engine.DeleteSpace(ctx, f.spaceScope, home.ID); !errors.Is(err, ErrDefaultSpaceProtected) {
		t.Fatalf("deleting a default space: %v", err)
	}
}

func TestEngineCreateAgentDeploymentJoinsItsCreatorsDefaultSpace(t *testing.T) {
	f := newOwnerSpaceFixture(t)
	other, home := f.space("Den", "bo"), f.space("Home", "ana")
	f.space("Side", "ana")
	if _, _, err := f.engine.CreateAgentDeployment(t.Context(), f.agent("scout"), "user", "ana", "create"); err != nil {
		t.Fatal(err)
	}
	if got := f.spacesOf(runtime.SpaceMemberAgent, "scout"); !slices.Equal(got, []string{home.ID}) {
		t.Fatalf("scout is in %v, want only %s", got, home.ID)
	}
	if got := f.controlSpaces("scout"); !slices.Equal(got, []string{home.ID}) {
		t.Fatalf("control conversations are in %v, want only %s", got, home.ID)
	}
	if got := f.members(other.ID); len(got) != 0 {
		t.Fatalf("another person's default space has %v", got)
	}
}

func TestEngineCreateAgentDeploymentWithoutADefaultSpaceStaysInNoSpace(t *testing.T) {
	for name, actor := range map[string][2]string{
		"no person":                  {"service", "provisioner"},
		"person with no space":       {"user", "bo"},
		"service named like a owner": {"service", "ana"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOwnerSpaceFixture(t)
			ctx := t.Context()
			home := f.space("Home", "ana")
			created, _, err := f.engine.CreateAgentDeployment(ctx, f.agent("scout"), actor[0], actor[1], "create")
			if err != nil || created == nil || created.ID != "scout" {
				t.Fatalf("created = %+v, err = %v", created, err)
			}
			if _, err := f.engine.GetAgentDeployment(ctx, f.scope, "scout"); err != nil {
				t.Fatal(err)
			}
			if got := f.spacesOf(runtime.SpaceMemberAgent, "scout"); len(got) != 0 {
				t.Fatalf("scout is in %v, want no space", got)
			}
			if got := f.controlSpaces("scout"); len(got) != 0 {
				t.Fatalf("control conversations before any space = %v", got)
			}
			// Its first space gives it the control conversation, once.
			for range 2 {
				if _, _, err := f.engine.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: f.spaceScope, SpaceID: home.ID, Type: runtime.SpaceMemberAgent, MemberID: "scout"}); err != nil {
					t.Fatal(err)
				}
			}
			if got := f.controlSpaces("scout"); !slices.Equal(got, []string{home.ID}) {
				t.Fatalf("control conversations after joining = %v, want only %s", got, home.ID)
			}
		})
	}
}

func TestEngineCreateAgentDeploymentInSpace(t *testing.T) {
	f := newOwnerSpaceFixture(t)
	ctx := t.Context()
	home, work := f.space("Home", "ana"), f.space("Work", "ana")
	if _, _, err := f.engine.CreateAgentDeploymentInSpace(ctx, work.ID, f.agent("scout"), "user", "ana", "create"); err != nil {
		t.Fatal(err)
	}
	if got := f.spacesOf(runtime.SpaceMemberAgent, "scout"); !slices.Equal(got, []string{work.ID}) {
		t.Fatalf("scout is in %v, want only %s", got, work.ID)
	}
	if got := f.members(home.ID); len(got) != 0 {
		t.Fatalf("the creator's default space has %v", got)
	}
	if got := f.controlSpaces("scout"); !slices.Equal(got, []string{work.ID}) {
		t.Fatalf("control conversations are in %v, want only %s", got, work.ID)
	}

	// A space that does not exist creates nothing.
	if _, _, err := f.engine.CreateAgentDeploymentInSpace(ctx, "missing", f.agent("ghost"), "user", "ana", "create"); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("create in an unknown space: %v", err)
	}
	if deployment, err := f.engine.GetAgentDeployment(ctx, f.scope, "ghost"); err == nil {
		t.Fatalf("a deployment was created in no space: %+v", deployment)
	}
	if got := f.spacesOf(runtime.SpaceMemberAgent, "ghost"); len(got) != 0 {
		t.Fatalf("ghost is in %v", got)
	}
	if got := f.controlSpaces("ghost"); len(got) != 0 {
		t.Fatalf("ghost has control conversations in %v", got)
	}
}

func TestEngineCreateTeamDeploymentJoinsItsCreatorsDefaultSpace(t *testing.T) {
	f := newOwnerSpaceFixture(t)
	ctx := t.Context()
	other, home := f.space("Den", "bo"), f.space("Home", "ana")
	side := f.space("Side", "ana")
	// The Agents were provisioned by no person, so they start in no space.
	for _, id := range []string{"scout", "scribe"} {
		if _, _, err := f.engine.CreateAgentDeployment(ctx, f.agent(id), "service", "provisioner", "create"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.engine.CreateTeamDeployment(ctx, f.team("desk", "scout", "scribe"), "user", "ana", "create"); err != nil {
		t.Fatal(err)
	}
	if got := f.members(home.ID); !slices.Equal(got, []string{"agent:scout", "agent:scribe", "team:desk"}) {
		t.Fatalf("the creator's default space has %v", got)
	}
	if got := append(f.members(other.ID), f.members(side.ID)...); len(got) != 0 {
		t.Fatalf("other spaces have %v", got)
	}

	// No person, or a person with no default space: nothing is created.
	for name, actor := range map[string][2]string{
		"no person":            {"service", "provisioner"},
		"person with no space": {"user", "cy"},
	} {
		_, _, err := f.engine.CreateTeamDeployment(ctx, f.team("stray", "scout"), actor[0], actor[1], "create")
		if !errors.Is(err, ErrNoDefaultSpace) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if team, err := f.engine.GetTeamDeployment(ctx, f.scope, "stray"); err == nil {
			t.Fatalf("%s: a Team was created: %+v", name, team)
		}
		if got := f.spacesOf(runtime.SpaceMemberTeam, "stray"); len(got) != 0 {
			t.Fatalf("%s: stray is in %v", name, got)
		}
	}
	if teams, err := f.engine.ListTeamDeployments(ctx, f.scope); err != nil || len(teams) != 1 {
		t.Fatalf("teams = %+v, err = %v", teams, err)
	}
}

func TestEngineDeleteSpaceMovesItsTeamToItsOwnersDefaultSpace(t *testing.T) {
	f := newOwnerSpaceFixture(t)
	ctx := t.Context()
	// bo's default space comes first by position: the fallback goes by
	// owner, not by position.
	other, home := f.space("Den", "bo"), f.space("Home", "ana")
	side := f.space("Side", "ana")
	for _, id := range []string{"scout", "scribe"} {
		if _, _, err := f.engine.CreateAgentDeploymentInSpace(ctx, side.ID, f.agent(id), "user", "bo", "create"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.engine.CreateTeamDeploymentInSpace(ctx, side.ID, f.team("desk", "scout", "scribe"), "user", "bo", "create"); err != nil {
		t.Fatal(err)
	}
	chat, _, err := f.engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: f.spaceScope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "desk"}, Title: "Plan", SpaceID: side.ID, CreatedBy: "bo", IdempotencyKey: "plan",
	})
	if err != nil || chat.SpaceID != side.ID {
		t.Fatalf("chat = %+v, err = %v", chat, err)
	}
	if got := append(f.members(home.ID), f.members(other.ID)...); len(got) != 0 {
		t.Fatalf("default spaces before the delete have %v", got)
	}

	if err := f.engine.DeleteSpace(ctx, f.spaceScope, side.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.members(home.ID); !slices.Equal(got, []string{"agent:scout", "agent:scribe", "team:desk"}) {
		t.Fatalf("the owner's default space has %v", got)
	}
	if got := f.members(other.ID); len(got) != 0 {
		t.Fatalf("another owner's default space has %v", got)
	}
	if moved, err := f.engine.GetConversation(ctx, f.spaceScope, chat.ID); err != nil || moved.SpaceID != home.ID {
		t.Fatalf("chat after the delete = %+v, err = %v", moved, err)
	}
	if got := f.controlSpaces("scout"); !slices.Equal(got, []string{home.ID}) {
		t.Fatalf("control conversations after the delete are in %v", got)
	}
	if spaces, err := f.engine.ListSpaces(ctx, f.spaceScope); err != nil || len(spaces) != 2 {
		t.Fatalf("spaces = %+v, err = %v", spaces, err)
	}
}
