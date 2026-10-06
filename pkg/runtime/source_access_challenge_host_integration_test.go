package runtime

import (
	"context"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSourceAccessChallengeLatestHostGateRequiresCanonicalOrigin(t *testing.T) {
	for _, mode := range []string{"canonical", "raw checkpoint", "wrong agent", "final marker"} {
		t.Run(mode, func(t *testing.T) {
			store := NewMemoryStore()
			f := newForegroundClarificationFixture(t, store, true)
			sourceAccessChallengeInstall(t, f)
			input := TurnExecutionContext{Run: f.run, Turn: &AgentTurn{ID: "challenge-host-turn"}}
			if mode != "raw checkpoint" {
				var err error
				input, err = (conversationWorkTurnRunner{runs: store, conversations: store}).input(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
			}
			agentID := f.run.AssignedAgentID
			if mode == "wrong agent" {
				agentID = "other-agent"
			}
			if mode == "final marker" {
				input.Run = cloneAgentRun(input.Run)
				input.Run.Checkpoint = checkpointTerminalFailure(input.Run.Checkpoint, "action_failed")
			}
			host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
				if request.SourceAccessChallenge == nil || !request.CanAskConversationQuestion {
					t.Fatal("host lost verified challenge")
				}
				return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "test",
					NextRunStatus: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "user_message", Reference: f.conversation.ID},
					RunOutput: map[string]interface{}{"summary": "Would you like to try the alternative browser?"}}, nil
			}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: agentID, DefinitionID: "browser", DefinitionVersion: "1"})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "canonical" {
				if host.calls != 1 || outcome.NextRunStatus != AgentRunStatusWaitingForEvent {
					t.Fatalf("verified question stopped: %#v calls=%d", outcome, host.calls)
				}
			} else if host.calls != 0 || outcome.Model != "terminal-failure-report" {
				t.Fatalf("unverified failure reopened model: %#v calls=%d", outcome, host.calls)
			}
		})
	}
}

func TestSourceAccessChallengeLatestCatalogGateUsesPersistedRun(t *testing.T) {
	for _, mode := range []string{"foreground", "independent", "no store", "stale revision", "forged ID", "generic failure"} {
		t.Run(mode, func(t *testing.T) {
			store := NewMemoryStore()
			f := newForegroundClarificationFixture(t, store, true)
			sourceAccessChallengeInstall(t, f)
			run := cloneAgentRun(f.run)
			if mode == "independent" {
				background := newConversationTaskClarificationFixture(t, store, true)
				background.transition(t, RunTransitionRequest{Status: AgentRunStatusQueued, Checkpoint: sourceAccessChallengeTestCheckpoint("source_access_challenge", nil)})
				run = cloneAgentRun(background.work)
			}
			run.Kind = RunKindAgentWork // The production conversation adapter lowers only its presentation.
			if mode == "stale revision" {
				run.Revision++
			}
			if mode == "forged ID" {
				run.ID = "copied-context"
			}
			if mode == "generic failure" {
				run.Checkpoint = checkpointTerminalFailure(nil, "action_failed")
			}
			catalog := &resolverCatalog{
				deployment: &kernelagent.AgentDeployment{ID: run.AssignedAgentID, Scope: skill.ScopeReference(run.Scope), DefinitionID: "browser", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
				definition: &kernelagent.AgentDefinition{ID: "browser", Version: "1", SystemPrompt: "Be truthful."},
				activation: &skill.ActivationSnapshot{SnapshotID: "challenge-snapshot", Scope: skill.ScopeReference(run.Scope), DeploymentID: run.AssignedAgentID},
			}
			host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
				if request.SourceAccessChallenge == nil || request.ConversationTasks == nil || request.ConversationTasks.CanStart {
					t.Fatalf("interaction lost canonical task context or granted new work: %#v", request.ConversationTasks)
				}
				if mode == "independent" {
					own := false
					for _, task := range request.ConversationTasks.Tasks {
						own = own || task.TaskID == run.Context[ConversationTaskContextKey] && task.WorkRunID == run.ID && task.Goal == run.Goal
					}
					if !own {
						t.Fatal("Atlas cannot identify its exact independent task")
					}
				}
				return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "test",
					NextRunStatus: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "user_message"},
					RunOutput: map[string]interface{}{"summary": "Would you like to try the alternative browser?"}}, nil
			}}
			config := CatalogTurnResolverConfig{Host: host, ConversationTasks: store}
			if mode == "no store" {
				config.ConversationTasks = nil
			}
			binding, err := ResolveCatalogTurnRunner(context.Background(), catalog, run, config)
			if err != nil {
				t.Fatal(err)
			}
			allowed := mode == "foreground" || mode == "independent"
			if allowed && (binding.Model == "terminal-failure-report" || len(catalog.activationDeployments) != 1) {
				t.Fatalf("canonical challenge stopped before hosted continuation: %#v", binding)
			}
			if !allowed && (binding.Model != "terminal-failure-report" || len(catalog.activationDeployments) != 0 || host.calls != 0) {
				t.Fatalf("unproven challenge reopened capabilities: %#v", binding)
			}
			if allowed {
				canonical, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				input, err := (conversationWorkTurnRunner{runs: store, conversations: store}).input(t.Context(), TurnExecutionContext{Run: canonical, Turn: &AgentTurn{ID: "catalog-challenge-turn"}})
				if err != nil {
					t.Fatal(err)
				}
				input.Run = run
				outcome, err := binding.Runner.RunTurn(t.Context(), input)
				if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusWaitingForEvent || host.calls != 1 {
					t.Fatalf("catalog-to-host challenge failed: %#v %v calls=%d", outcome, err, host.calls)
				}
			}
		})
	}
}
