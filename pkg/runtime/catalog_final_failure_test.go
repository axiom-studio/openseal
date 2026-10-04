package runtime

import (
	"context"
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
			host := &failureExplanationTestHost{respond: func(HostedTurnRequest) (*HostedTurnResponse, error) {
				t.Fatal("catalog failure requested an explanatory model")
				return nil, nil
			}}
			config := CatalogTurnResolverConfig{Host: host, SkillHosts: SkillHostCapabilityResolverFunc(func(context.Context, skill.ScopeReference, string) (*skill.HostCapabilityState, error) {
				t.Fatal("failure explanation attempted runtime preparation")
				return nil, nil
			})}
			binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, config)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.activationDeployments) != 0 || len(binding.ModelActions) != 0 || len(binding.PreparedRuntimes) != 0 || len(binding.RunbookOperations) != 0 ||
				binding.BudgetReservation != (BudgetUsage{}) || binding.ModelProvider != "host" || binding.Model != "terminal-failure-report" {
				t.Fatalf("failure report prepared executable work or model usage: %#v", binding)
			}
			if binding.DeploymentID != "seal" || binding.ActionDeploymentID != "seal" || binding.DefinitionID != "seal" || binding.DefinitionVersion != "1" {
				t.Fatalf("kernel failure report lost verified Agent identity: %#v", binding)
			}
			outcome, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
			if err != nil || outcome == nil || host.calls != 0 || outcome.OutputSummary != TerminalFailureReply("action_failed") ||
				len(outcome.ProposedActions) != 0 || outcome.ProposedTask != nil || outcome.ProposedFork != nil || outcome.ProposedDelegation != nil || outcome.ProposedRunbook != nil {
				t.Fatalf("kernel report used model or granted new operations: %#v %v calls=%d", outcome, err, host.calls)
			}
			catalog.deployment.RolloutStatus = kernelagent.RolloutPaused
			if _, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, config); err == nil {
				t.Fatal("inactive Agent identity bypassed for final answer")
			}
			catalog.deployment.RolloutStatus = kernelagent.RolloutActive
			config.Host = nil
			withoutHost, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, config)
			if err != nil || withoutHost == nil || withoutHost.ModelProvider != "host" || withoutHost.Model != "terminal-failure-report" || host.calls != 0 {
				t.Fatalf("kernel failure reply depended on model host: %#v %v", withoutHost, err)
			}
		})
	}
}
