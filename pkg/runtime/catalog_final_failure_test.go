package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestFinalFailureCatalogDoesNotPrepareFailedCapabilities(t *testing.T) {
	for _, owner := range []ObjectiveOwner{{Type: OwnerTypeAgent, ID: "seal"}, {Type: OwnerTypeTeam, ID: "team"}} {
		t.Run(string(owner.Type), func(t *testing.T) {
			scope := Scope{Kind: "tenant", ID: "11"}
			spec := workspace.DefaultSpec()
			spec.Policy.CredentialBindings = []string{"MISSING_CREDENTIAL"}
			catalog := &resolverCatalog{
				deployment: &kernelagent.AgentDeployment{
					ID: "seal", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "seal",
					ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
					DefaultWorkspaceID: spec.ID, Workspaces: []workspace.Spec{spec},
					Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "vault", ID: "model-account"}},
				},
				definition: &kernelagent.AgentDefinition{ID: "seal", Version: "1", SystemPrompt: "Be clear.", Personality: "Friendly and bubbly"},
				// Activation is deliberately unavailable. Even a broken Skill or
				// missing Workspace credential must not block the final answer.
			}
			run := &AgentRun{
				ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: owner, AssignedAgentID: "seal",
				Goal: "Explain the stopped attempt", Context: map[string]interface{}{},
				Checkpoint: checkpointFinalFailureExplanation(nil, "action", "The image service rejected the request."),
			}
			host := &recordingTurnHost{response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", ModelProvider: "test", Model: "model",
				NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "The image request failed, so I stopped."},
			}}
			config := CatalogTurnResolverConfig{Host: host, SkillHosts: SkillHostCapabilityResolverFunc(func(context.Context, skill.ScopeReference, string) (*skill.HostCapabilityState, error) {
				t.Fatal("failure explanation attempted runtime preparation")
				return nil, nil
			})}
			binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, config)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.activationDeployments) != 0 || len(binding.ModelActions) != 0 || len(binding.PreparedRuntimes) != 0 || len(binding.RunbookOperations) != 0 || binding.BudgetReservation.Turns != 1 {
				t.Fatalf("failure explanation prepared executable work: %#v", binding)
			}
			if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err != nil {
				t.Fatal(err)
			}
			request := host.request
			if request.ModelCredential == nil || request.ModelCredential.ID != "model-account" || request.AgentID != "seal" || request.DefinitionVersion != "1" || !strings.Contains(strings.Join(request.SystemInstructions, "\n"), "Friendly and bubbly") {
				t.Fatalf("final answer lost verified identity, model or voice: %#v", request)
			}
			if len(request.Actions) != 0 || len(request.EligibleAgents) != 0 || request.Workspace != nil || len(request.WorkspaceCredentials) != 0 || len(request.WorkspaceOperations) != 0 || len(request.RunbookOperations) != 0 {
				t.Fatal("final answer still has executable capabilities")
			}
			catalog.deployment.RolloutStatus = kernelagent.RolloutPaused
			if _, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, config); err == nil {
				t.Fatal("inactive Agent identity bypassed for final answer")
			}
			catalog.deployment.RolloutStatus = kernelagent.RolloutActive
			config.Host = nil
			if _, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, config); !errors.Is(err, ErrTurnHostUnavailable) {
				t.Fatalf("missing model host = %v", err)
			}
		})
	}
}
