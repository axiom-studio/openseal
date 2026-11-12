package openseal

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestEngineSpacesUseDefaultStore(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	scope := models.Scope{Kind: "tenant", ID: "7"}
	space, err := engine.CreateSpace(t.Context(), CreateSpaceRequest{Scope: scope, Name: "Work", Look: models.SpaceLook("harbor")})
	if err != nil {
		t.Fatal(err)
	}
	conversation, _, err := engine.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Title: "Plan", SpaceID: space.ID, IdempotencyKey: "plan",
	})
	if err != nil || conversation.SpaceID != space.ID {
		t.Fatalf("conversation = %+v, err = %v", conversation, err)
	}
	spaces, err := engine.ListSpaces(t.Context(), scope)
	// The first space an owner creates is their default; nothing else exists.
	if err != nil || len(spaces) != 1 || spaces[0].ID != space.ID || !spaces[0].IsDefault || spaces[0].Access != models.SpaceAccessOwner {
		t.Fatalf("spaces = %+v, err = %v", spaces, err)
	}
	if found, err := engine.DefaultSpace(t.Context(), scope, ""); err != nil || found.ID != space.ID {
		t.Fatalf("default space = %+v, err = %v", found, err)
	}
}

func TestEngineCreatedDeploymentsJoinCreatorDefaultWhenInNoSpace(t *testing.T) {
	ctx := t.Context()
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	scope := capability.ScopeReference{Kind: "tenant", ID: "9"}
	spaceScope := models.Scope{Kind: scope.Kind, ID: scope.ID}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "helper", Version: "1", DisplayName: "Helper", Purpose: "Help", SystemPrompt: "Help.",
		Authority: AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	createAgent := func(id string) {
		t.Helper()
		if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
			ID: id, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "create"); err != nil {
			t.Fatal(err)
		}
	}
	defaultSpace := seedSpace(t, engine, spaceScope, "operator").ID
	createAgent("first")
	members, err := engine.ListSpaceMembers(ctx, spaceScope, defaultSpace)
	if err != nil || len(members) != 1 || members[0].ID != "first" || members[0].Type != models.SpaceMemberAgent {
		t.Fatalf("defaultSpace members = %+v, err = %v", members, err)
	}
	// The Agent's control chat is in the operator's default space as well.
	control, err := engine.ListConversations(ctx, models.ConversationFilter{Scope: spaceScope, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: "first"}})
	if err != nil || len(control) != 1 || control[0].SpaceID != defaultSpace {
		t.Fatalf("control chats = %+v, err = %v", control, err)
	}
	teamDefinition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: "crew", Version: "1", DisplayName: "Crew", Purpose: "Work together",
		Approvals: TeamApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: "crew-one", Scope: scope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version, Status: TeamDeploymentActive,
		Roster: []TeamRosterAssignment{{ID: "helper", AgentDeploymentID: "first"}},
	}, "user", "operator", "create"); err != nil {
		t.Fatal(err)
	}
	members, err = engine.ListSpaceMembers(ctx, spaceScope, defaultSpace)
	if err != nil || len(members) != 2 || members[1].ID != "crew-one" || members[1].Type != models.SpaceMemberTeam || members[1].Position != 1 {
		t.Fatalf("defaultSpace members with team = %+v, err = %v", members, err)
	}
	// A host PUT into another space stays idempotent and keeps the default.
	work, err := engine.CreateSpace(ctx, CreateSpaceRequest{Scope: spaceScope, Name: "Work", Look: models.SpaceLook("harbor"), OwnerID: "operator"})
	if err != nil || work.IsDefault {
		t.Fatalf("work = %+v, err = %v", work, err)
	}
	createAgent("second")
	for range 2 {
		if _, _, err := engine.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: spaceScope, SpaceID: work.ID, Type: models.SpaceMemberAgent, MemberID: "second"}); err != nil {
			t.Fatal(err)
		}
	}
	if workMembers, err := engine.ListSpaceMembers(ctx, spaceScope, work.ID); err != nil || len(workMembers) != 1 {
		t.Fatalf("work members = %+v, err = %v", workMembers, err)
	}
	// A deployment already in a space is not added to the default space.
	if err := engine.DeleteSpaceMember(ctx, portfolio.DeleteSpaceMemberRequest{Scope: spaceScope, SpaceID: defaultSpace, Type: models.SpaceMemberAgent, MemberID: "second"}); err != nil {
		t.Fatal(err)
	}
	if placed, err := engine.spaces.EnsureMemberHasSpace(ctx, spaceScope, "operator", models.SpaceMemberAgent, "second"); err != nil || placed != nil {
		t.Fatalf("already placed = %+v, err = %v", placed, err)
	}
}
