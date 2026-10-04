package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSourceAccessChallengeHostProjectionRequiresKernelOrigin(t *testing.T) {
	store := NewMemoryStore()
	f := newForegroundClarificationFixture(t, store, true)
	sourceAccessChallengeInstall(t, f)
	runner, err := NewHostedTurnRunner(&recordingTurnHost{}, HostedTurnRunnerConfig{AgentID: f.run.AssignedAgentID, DefinitionID: "browser", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	input := TurnExecutionContext{Run: f.run, Turn: &AgentTurn{ID: "host-turn"}}
	request, err := runner.buildRequest(input)
	if err != nil || request.SourceAccessChallenge != nil {
		t.Fatalf("raw context created trusted interaction: %#v %v", request.SourceAccessChallenge, err)
	}
	input, err = (conversationWorkTurnRunner{runs: store, conversations: store}).input(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	request, err = runner.buildRequest(input)
	if err != nil || request.SourceAccessChallenge == nil || request.SourceAccessChallenge.Phase != "question" || request.SourceAccessChallenge.FailureID != "challenged-read" {
		t.Fatalf("canonical host projection: %#v %v", request.SourceAccessChallenge, err)
	}
	raw, err := MarshalHostedTurnModelInput(request)
	var model map[string]interface{}
	if err != nil || json.Unmarshal(raw, &model) != nil || model["sourceAccessChallenge"] != nil {
		t.Fatalf("host interaction leaked into model input: %s %v", raw, err)
	}
	request.SourceAccessChallenge.Phase = "forged"
	if input.sourceAccessChallenge.Phase != "question" {
		t.Fatal("host request aliased trusted ephemeral state")
	}
	runner.config.AgentID = "other-agent"
	request, err = runner.buildRequest(input)
	if err != nil || request.SourceAccessChallenge != nil {
		t.Fatalf("another Agent received interaction: %#v %v", request.SourceAccessChallenge, err)
	}
}

func sourceAccessChallengeAdmission(t *testing.T, f *foregroundClarificationFixture, turnID string, disposition ActionDisposition) *ActionProposalResult {
	t.Helper()
	if f.run.Status == AgentRunStatusQueued {
		var err error
		f.run, _, err = NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.run.Scope, f.run.ID, RunTransitionRequest{ExpectedRevision: f.run.Revision, Status: AgentRunStatusRunning})
		if err != nil {
			t.Fatal(err)
		}
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), &skill.Definition{ID: "reader", Version: "1.0.0", Name: "Reader", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://reader.invalid"},
		Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read source", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead,
			InputSchema: map[string]interface{}{"type": "object"}, Idempotency: skill.IdempotencySupported}}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "source-binding", Scope: skill.ScopeReference{Kind: f.run.Scope.Kind, ID: f.run.Scope.ID}, DeploymentID: f.run.AssignedAgentID,
		SkillID: "reader", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	coordinator := NewActionCoordinator(f.store, f.store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: disposition, EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "requester"}}, Reason: "ordinary action policy"}, nil
	}))
	result, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: f.run.Scope, RunID: f.run.ID, WorkerID: "foreground-worker", TurnID: turnID,
		DeploymentID: f.run.AssignedAgentID, SkillID: "reader", SkillVersion: "1.0.0", Action: "read", Arguments: map[string]interface{}{},
		IdempotencyKey: "challenge-followup", Summary: "Read selected source"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSourceAccessChallengeActionAdmissionCannotSkipAnswerOrApproval(t *testing.T) {
	for _, stage := range []string{"no answer", "answer only", "classifier only", "wrong proposal", "ordinary approval"} {
		t.Run(stage, func(t *testing.T) {
			forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
				f := newForegroundClarificationFixture(t, store, true)
				sourceAccessChallengeInstall(t, f)
				f.ask(t, "Would you like the alternative browser?", "user_message")
				if stage == "no answer" {
					// Even an operator resuming the same wait cannot manufacture a saved answer.
					var err error
					f.run, _, err = NewRunActivityService(store, store).TransitionRun(t.Context(), f.run.Scope, f.run.ID, RunTransitionRequest{
						ExpectedRevision: f.run.Revision, Status: AgentRunStatusQueued,
					})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					sourceAccessChallengeAcceptAnswer(t, f, f.question(t), "action-answer", "Yes, try that browser.")
				}
				turnID := ""
				if stage != "no answer" && stage != "answer only" {
					result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: f.run.Scope, RunID: f.run.ID, WorkerID: "foreground-worker"},
						TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
							return &TurnOutcome{NextRunStatus: AgentRunStatusRunning}, nil
						}))
					if err != nil {
						t.Fatal(err)
					}
					f.run = result.Run
				}
				if stage == "wrong proposal" || stage == "ordinary approval" {
					result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: f.run.Scope, RunID: f.run.ID, WorkerID: "foreground-worker"},
						TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
							return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{{Type: "skill", Capability: "reader.read", Summary: "Read selected source", IdempotencyKey: "challenge-followup"}}}, nil
						}))
					if err != nil {
						t.Fatal(err)
					}
					f.run, turnID = result.Run, result.Turn.ID
					if stage == "wrong proposal" {
						turnID = "unrelated-turn"
					}
				}
				result := sourceAccessChallengeAdmission(t, f, turnID, ActionDispositionRequireApproval)
				if stage == "ordinary approval" {
					if result.Call.Status != ActionCallStatusWaitingApproval || result.Call.ApprovalID == "" {
						t.Fatalf("answer bypassed ordinary approval: %#v", result.Call)
					}
				} else if result.Call.Status != ActionCallStatusDenied {
					t.Fatalf("%s admitted an action: %#v", stage, result.Call)
				}
			})
		})
	}
}

func TestSourceAccessChallengeIndependentTaskResumesWithCompletedSource(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newConversationTaskClarificationFixture(t, store, true)
		f.transition(t, RunTransitionRequest{Status: AgentRunStatusQueued, Checkpoint: sourceAccessChallengeTestCheckpoint("source_access_challenge", nil)})
		interaction, err := resolveSourceAccessChallengeInteraction(t.Context(), store, f.work)
		if err != nil || interaction == nil || interaction.Phase != "question" || f.source.Status != AgentRunStatusCompleted {
			t.Fatalf("completed source prevented independent question: %#v %v", interaction, err)
		}
		f.ask(t, "The source needs another browser. Would you like to try it?")
		question := f.question(t)
		answer := f.post(t, PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer, Content: "Yes, for this source.",
			ReplyToMessageID: question.ID, ResolvesMessageID: question.ID, RequiresResponse: true, IdempotencyKey: "challenge-answer"})
		accepted, _, err := f.scheduler.ScheduleMessage(t.Context(), f.work.Scope, f.conversation.ID, answer.ID)
		if err != nil || accepted == nil || accepted.Run == nil || accepted.Run.ID != f.work.ID {
			t.Fatalf("independent answer started another Run: %#v %v", accepted, err)
		}
		f.work = accepted.Run
		interaction, err = resolveSourceAccessChallengeInteraction(t.Context(), store, f.work)
		if err != nil || interaction == nil || interaction.Phase != "answer" {
			t.Fatalf("independent answer proof lost: %#v %v", interaction, err)
		}
		result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: f.work.Scope, RunID: f.work.ID, WorkerID: "task-worker"},
			TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning}, nil
			}))
		if err != nil || result == nil || result.Run.ID != f.work.ID || result.Run.Status != AgentRunStatusRunning {
			t.Fatalf("independent classifier did not preserve work: %#v %v", result, err)
		}
		f.work = result.Run
		interaction, err = resolveSourceAccessChallengeInteraction(t.Context(), store, f.work)
		if err != nil || interaction == nil || interaction.Phase != "continue" {
			t.Fatalf("independent continuation missing: %#v %v", interaction, err)
		}
		f.assertSingleForeground(t)
	})
}

func TestSourceAccessChallengeRejectsDisguisedMixedRateLimit(t *testing.T) {
	run := &AgentRun{Status: AgentRunStatusRunning, Checkpoint: sourceAccessChallengeTestCheckpoint("source_access_challenge", map[string]string{
		"failures": `[{"index":0,"failureKind":"source_rate_limited","httpStatus":429}]`,
	})}
	if hasCanonicalSourceAccessChallenge(run) || sourceAccessChallengeInteraction(run) != nil {
		t.Fatal("nested source throttling became an access-challenge continuation")
	}
}
