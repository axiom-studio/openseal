package runtime

import (
	"context"
	"errors"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestFinalFailureCatalogDoesNotPrepareFailedCapabilities(t *testing.T) {
	for _, owner := range []ObjectiveOwner{{Type: OwnerTypeAgent, ID: "seal"}, {Type: OwnerTypeTeam, ID: "team"}} {
		t.Run(string(owner.Type), func(t *testing.T) {
			scope := Scope{Kind: "tenant", ID: "11"}
			catalog := &bindingCatalog{
				deployment: &kernelagent.AgentDeployment{
					ID: "seal", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "seal",
					ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
					Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "vault", ID: "model-account"}},
				},
				definition: &kernelagent.AgentDefinition{ID: "seal", Version: "1", SystemPrompt: "Be clear.", Personality: "Friendly and bubbly"},
				// Activation is deliberately unavailable. Even a broken Skill
				// must not block the final answer.
			}
			run := &AgentRun{
				ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: owner, AssignedAgentID: "seal", Status: AgentRunStatusRunning,
				Goal: "Explain the stopped attempt", Context: map[string]interface{}{},
				FailureExplanation: &RunFailureExplanation{Kind: conversation.RunFailureKindExecution, Code: "action_failed", Message: "The image service rejected the request."},
			}
			config := bindingTestConfig(t, newTestKernelStore(t), &modelsteptest.RecordingStepHost{})
			config.SkillHosts = runworker.SkillHostCapabilityResolverFunc(func(context.Context, skill.ScopeReference, string) (*skill.HostCapabilityState, error) {
				t.Fatal("failure explanation attempted runtime preparation")
				return nil, nil
			})
			binding, err := runworker.ResolveLoopBinding(t.Context(), catalog, run, config)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.activationDeployments) != 0 || len(binding.ModelActions) != 0 || len(binding.PreparedRuntimes) != 0 || len(binding.RunbookOperations) != 0 {
				t.Fatalf("failure explanation prepared executable work: %#v", binding)
			}
			if binding.DeploymentID != "seal" || binding.ActionDeploymentID != "seal" || binding.DefinitionID != "seal" || binding.DefinitionVersion != "1" ||
				binding.ModelCredential == nil || binding.ModelCredential.ID != "model-account" {
				t.Fatalf("failure explanation lost verified Agent identity: %#v", binding)
			}
			if _, ok := binding.Producer.(*modelstep.ModelStepProducer); !ok {
				t.Fatalf("failure explanation producer = %T", binding.Producer)
			}
			catalog.deployment.RolloutStatus = kernelagent.RolloutPaused
			if _, err := runworker.ResolveLoopBinding(t.Context(), catalog, run, config); err == nil {
				t.Fatal("inactive Agent identity bypassed for final answer")
			}
			catalog.deployment.RolloutStatus = kernelagent.RolloutActive
			config.Host = nil
			if _, err := runworker.ResolveLoopBinding(t.Context(), catalog, run, config); !errors.Is(err, modelstep.ErrTurnHostUnavailable) {
				t.Fatalf("explanation without a model host = %v", err)
			}
		})
	}
}
