package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type participationCatalog struct {
	teamReads   int
	onTeamRead  func(int)
	deployment  *team.Deployment
	definition  *team.Definition
	agents      map[string]*agent.AgentDeployment
	definitions map[string]*agent.AgentDefinition
	agentReads  []string
}

func (c *participationCatalog) GetTeamDeployment(_ context.Context, scope capability.ScopeReference, id string) (*team.Deployment, error) {
	if scope != (capability.ScopeReference{Kind: "local", ID: "default"}) || id != "team" {
		return nil, team.ErrDeploymentNotFound
	}
	c.teamReads++
	if c.onTeamRead != nil {
		c.onTeamRead(c.teamReads)
	}
	return c.deployment, nil
}
func (c *participationCatalog) GetTeamDefinition(_ context.Context, id, version string) (*team.Definition, error) {
	if id != "team-definition" || version != "1" {
		return nil, errors.New("unexpected team definition")
	}
	return c.definition, nil
}
func (c *participationCatalog) GetAgentDeployment(_ context.Context, scope capability.ScopeReference, id string) (*agent.AgentDeployment, error) {
	if scope != (capability.ScopeReference{Kind: "local", ID: "default"}) {
		return nil, agent.ErrDeploymentNotFound
	}
	c.agentReads = append(c.agentReads, id)
	return c.agents[id], nil
}
func (c *participationCatalog) GetAgentDefinition(_ context.Context, id, version string) (*agent.AgentDefinition, error) {
	if version != "1" {
		return nil, errors.New("unexpected agent version")
	}
	return c.definitions[id], nil
}
func participationFixture() (*participationCatalog, runtime.ConversationParticipantQuery) {
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	c := &participationCatalog{
		deployment: &team.Deployment{ID: "team", Scope: scope, DefinitionID: "team-definition", ActiveVersion: "1", Status: team.DeploymentActive, Revision: 1},
		definition: &team.Definition{ID: "team-definition", Version: "1", DisplayName: "Research", Purpose: "Review evidence", Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}},
		agents:     map[string]*agent.AgentDeployment{}, definitions: map[string]*agent.AgentDefinition{},
	}
	for _, id := range []string{"z", "a", "observer", "disabled"} {
		participation := team.RoleChannelActive
		if id == "observer" {
			participation = team.RoleChannelObserveOnly
		}
		if id == "disabled" {
			participation = team.RoleChannelDisabled
		}
		c.definition.Roles = append(c.definition.Roles, team.RoleSlot{ID: id, DisplayName: id, Purpose: "Review evidence", ChannelParticipation: participation})
		c.deployment.Roster = append(c.deployment.Roster, team.RosterAssignment{ID: "assignment-" + id, RoleID: id, AgentDeploymentID: id})
		c.agents[id] = &agent.AgentDeployment{ID: id, Scope: scope, DefinitionID: "definition-" + id, ActiveVersion: "1", RolloutStatus: agent.RolloutActive, Environment: "local", Revision: 1, Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: 1}}
		c.definitions["definition-"+id] = &agent.AgentDefinition{ID: "definition-" + id, Version: "1", DisplayName: id, Purpose: "Review evidence", SystemPrompt: "Report uncertainty", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}}
	}
	query := runtime.ConversationParticipantQuery{Conversation: &runtime.Conversation{ID: "channel", Scope: runtime.Scope{Kind: "local", ID: "default"}, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: "team"}, Status: runtime.ConversationStatusActive}}
	return c, query
}
func TestDesktopParticipationUsesCanonicalActiveRoster(t *testing.T) {
	catalog, query := participationFixture()
	source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 2)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := source.ResolveConversationParticipants(t.Context(), query)
	if err != nil || len(bindings) != 2 || bindings[0].Participant.ID != "a" || bindings[1].Participant.ID != "z" || bindings[0].SemanticRoles[0] != "a" {
		t.Fatalf("roster: %#v %v", bindings, err)
	}
	for _, id := range catalog.agentReads {
		if id == "observer" || id == "disabled" {
			t.Fatal("non-speaking member was invited")
		}
	}
	// Returning a binding never transfers definition, credentials, or ownership.
	bindings[0].SemanticRoles[0] = "forged"
	catalog.agents["a"].RolloutStatus = agent.RolloutPaused
	catalog.definition.Roles[0].ChannelParticipation = "" // legacy default is active
	refreshed, err := source.ResolveConversationParticipants(t.Context(), query)
	if err != nil || len(refreshed) != 1 || refreshed[0].Participant.ID != "z" || refreshed[0].SemanticRoles[0] != "z" {
		t.Fatalf("stale roster reused: %#v %v", refreshed, err)
	}
	catalog.deployment.Status = team.DeploymentPaused
	if _, err := source.ResolveConversationParticipants(t.Context(), query); err == nil {
		t.Fatal("paused team remained eligible")
	}
}
func TestDesktopParticipationRejectsBrokenAuthority(t *testing.T) {
	tests := map[string]func(*participationCatalog, *runtime.ConversationParticipantQuery){
		"foreign-channel": func(_ *participationCatalog, q *runtime.ConversationParticipantQuery) {
			q.Conversation.Scope.ID = "other"
		},
		"archived-channel": func(_ *participationCatalog, q *runtime.ConversationParticipantQuery) {
			q.Conversation.Status = runtime.ConversationStatusArchived
		},
		"foreign-trigger": func(_ *participationCatalog, q *runtime.ConversationParticipantQuery) {
			q.Trigger = &runtime.ChannelMessage{Scope: q.Conversation.Scope, ConversationID: "other"}
		},
		"wrong-team-id": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) { c.deployment.ID = "other" },
		"foreign-team": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.deployment.Scope.ID = "other"
		},
		"team-activation": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.deployment.Activation = &workforce.ActivationContinuation{ChangeSetID: "pending"}
		},
		"team-version": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) { c.definition.Version = "2" },
		"unknown-role": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.deployment.Roster[0].RoleID = "unknown"
		},
		"duplicate-member": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.deployment.Roster[1].AgentDeploymentID = "z"
		},
		"foreign-agent": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.agents["a"].Scope.ID = "other"
		},
		"wrong-agent-id": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) { c.agents["a"].ID = "other" },
		"missing-agent":  func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) { delete(c.agents, "a") },
		"agent-version": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.definitions["definition-a"].Version = "2"
		},
		"agent-definition": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.definitions["definition-a"].ID = "other"
		},
		"role-definition-requirement": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.definition.Roles[0].RequiredDefinitionIDs = []string{"other"}
		},
		"role-skill-requirement": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.definition.Roles[0].RequiredSkillIDs = []string{"missing"}
		},
		"model-credential": func(c *participationCatalog, _ *runtime.ConversationParticipantQuery) {
			c.agents["a"].Credentials = map[string]capability.CredentialReference{agent.ModelProviderCredentialBinding: {Kind: "vault", ID: "private"}}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			catalog, query := participationFixture()
			source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 2)
			if err != nil {
				t.Fatal(err)
			}
			change(catalog, &query)
			if bindings, err := source.ResolveConversationParticipants(t.Context(), query); err == nil || len(bindings) > 0 {
				t.Fatalf("untrusted roster accepted: %#v %v", bindings, err)
			}
		})
	}
}
func TestDesktopParticipationEnforcesLimitAndEmptyRoster(t *testing.T) {
	catalog, query := participationFixture()
	source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ResolveConversationParticipants(t.Context(), query); err == nil {
		t.Fatal("participant limit ignored")
	}
	catalog.agents["a"].RolloutStatus = agent.RolloutRetired
	catalog.agents["z"].RolloutStatus = agent.RolloutPending
	if _, err := source.ResolveConversationParticipants(t.Context(), query); !errors.Is(err, runtime.ErrNoConversationParticipants) {
		t.Fatalf("no eligible members: %v", err)
	}
}

func TestDesktopParticipationHonorsAuthoredTeamPolicy(t *testing.T) {
	catalog, query := participationFixture()
	catalog.definition.Coordination = team.CoordinationPolicy{MaximumSpeakersPerRound: 1, QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true}
	source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 8)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := source.ResolvePolicy(t.Context(), query.Conversation)
	if err != nil || policy.MaximumSpeakers != 1 || !policy.Participation.QuietByDefault || !policy.Participation.RequireRoleRelevance || !policy.Participation.SuppressDuplicateContent {
		t.Fatalf("authored policy ignored: %#v %v", policy, err)
	}
	catalog.definition.Coordination = team.CoordinationPolicy{MaximumSpeakersPerRound: 8}
	policy, err = source.ResolvePolicy(t.Context(), query.Conversation)
	if err != nil || policy.MaximumSpeakers != 3 || policy.Participation.QuietByDefault || policy.Participation.RequireRoleRelevance || policy.Participation.SuppressDuplicateContent {
		t.Fatalf("host cap or explicit false policy ignored: %#v %v", policy, err)
	}
}
