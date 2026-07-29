package openseal

import (
	"context"
	"testing"
)

func TestAgentRunEntrypointIsValidatedBeforePersistence(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	deploymentScope := SkillScope{Kind: scope.Kind, ID: scope.ID}
	definition := &AgentDefinition{
		ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: "Operate safely.",
		Authority: AgentAuthorityPolicy{MaximumRisk: SkillRiskRead, MaxConcurrentRuns: 1},
		Runbook: &RunbookDefinition{
			APIVersion: RunbookAPIVersion, ID: "operator-runbook", Version: "1", Name: "Operator",
			Entrypoints: map[string]string{"manual:start": "done"},
			Steps:       map[string]RunbookStep{"done": {Kind: RunbookStepEnd, End: &RunbookEndStep{}}},
		},
	}
	if _, err := engine.RegisterAgentDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "operator", Scope: deploymentScope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "admin", "test"); err != nil {
		t.Fatal(err)
	}

	request := CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "operator"}, AssignedAgentID: "operator",
		Entrypoint: "manual:stale", Goal: "Operate", Source: RunSourceManual,
	}
	if _, err := engine.CreateAgentRunCommand(ctx, request); err == nil {
		t.Fatal("undefined entrypoint created a Run")
	}
	runs, err := engine.ListAgentRuns(ctx, AgentRunFilter{Scope: scope})
	if err != nil || len(runs) != 0 {
		t.Fatalf("rejected entrypoint persisted runs=%#v error=%v", runs, err)
	}

	request.Entrypoint = "manual:start"
	created, err := engine.CreateAgentRunCommand(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Run.Entrypoint != "manual:start" {
		t.Fatalf("entrypoint = %q", created.Run.Entrypoint)
	}
	if created.Run.Context["runbookDefinitionId"] != "operator-runbook" || created.Run.Context["runbookDefinitionVersion"] != "1" {
		t.Fatalf("Runbook identity context = %#v", created.Run.Context)
	}
}
