package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type externalConversationWorkforceCatalogStub struct {
	agent      *agent.AgentDeployment
	definition *agent.AgentDefinition
	team       *kernelteam.Deployment
}

func (s *externalConversationWorkforceCatalogStub) GetDeployment(
	context.Context,
	capability.ScopeReference,
	string,
) (*agent.AgentDeployment, error) {
	return s.agent, nil
}

func (s *externalConversationWorkforceCatalogStub) GetDefinition(
	context.Context,
	string,
	string,
) (*agent.AgentDefinition, error) {
	return s.definition, nil
}

func (s *externalConversationWorkforceCatalogStub) GetTeamDeployment(
	context.Context,
	capability.ScopeReference,
	string,
) (*kernelteam.Deployment, error) {
	return s.team, nil
}

func TestCatalogExternalConversationRunbookResolverProvesTeamRosterAndExactDefinition(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "one"}
	handler := ExternalConversationHandler{
		Kind: ExternalConversationHandlerRunbook, ID: "respond", Version: "1.0.0",
		Trigger: "on-message", AssignedAgentID: "agent-live",
	}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: handler.ID, Version: handler.Version, Name: "Respond",
		Entrypoints: map[string]string{"respond": "done"},
		Triggers: map[string]runbook.Trigger{"on-message": {
			Kind: runbook.TriggerEvent, EventType: capability.ConversationEventMessageReceived, Entrypoint: "respond",
		}},
		Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
	}
	catalog := &externalConversationWorkforceCatalogStub{
		agent: &agent.AgentDeployment{
			ID: "agent-live", DefinitionID: "agent", ActiveVersion: "1.0.0", RolloutStatus: agent.RolloutActive,
		},
		definition: &agent.AgentDefinition{ID: "agent", Version: "1.0.0", Runbook: definition},
		team: &kernelteam.Deployment{
			ID: "team-live", Status: kernelteam.DeploymentActive,
			Roster: []kernelteam.RosterAssignment{{ID: "responder", AgentDeploymentID: "agent-live"}},
		},
	}
	resolver, err := NewCatalogExternalConversationRunbookResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &ExternalConversationEndpoint{
		ID: "endpoint", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-live"},
		DeploymentID: "team-live", Handler: handler,
	}
	resolved, err := resolver.ResolveExternalConversationRunbook(t.Context(), scope, endpoint, handler)
	if err != nil || resolved.AssignedAgentID != "agent-live" || resolved.Definition != definition {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}

	catalog.team.Roster = nil
	if _, err := resolver.ResolveExternalConversationRunbook(t.Context(), scope, endpoint, handler); err == nil {
		t.Fatal("non-roster Runbook Agent was accepted")
	}
}
