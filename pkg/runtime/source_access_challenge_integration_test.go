package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skillerror"
)

type sourceChallengeDispatchFixture struct {
	chat        *foregroundClarificationFixture
	coordinator *ActionCoordinator
	worker      *ActionWorker
	dispatches  []string
	credentials int
	policies    int
}

func newSourceChallengeDispatchFixture(t *testing.T, store conversationTaskClarificationStore, independent bool, failure error) *sourceChallengeDispatchFixture {
	t.Helper()
	var f *foregroundClarificationFixture
	if independent {
		// Use the canonical independent-task service fixture, whose source is
		// already completed. Its first unrelated wait is not the challenge.
		task := newConversationTaskClarificationFixture(t, store, true)
		task.transition(t, RunTransitionRequest{Status: AgentRunStatusQueued})
		f = &foregroundClarificationFixture{store: store, service: task.service, scheduler: task.scheduler,
			conversation: task.conversation, trigger: task.trigger, run: task.work}
		if task.source.Status != AgentRunStatusCompleted {
			t.Fatal("independent task source must already be completed")
		}
	} else {
		f = newForegroundClarificationFixture(t, store, true)
	}
	x := &sourceChallengeDispatchFixture{chat: f}
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), &skill.Definition{ID: "challenge-reader", Version: "1.0.0", Name: "Source reader",
		Transport: skill.TransportReference{Kind: "http", Endpoint: "https://reader.invalid"},
		Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read one source", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"browser": map[string]interface{}{"type": "string"}}, "required": []interface{}{"browser"}},
			Credentials: []skill.CredentialRequirement{{Name: "token", Kind: "api-token"}}, Idempotency: skill.IdempotencySupported}}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "challenge-reader-binding", Scope: skill.ScopeReference{Kind: f.run.Scope.Kind, ID: f.run.Scope.ID},
		DeploymentID: f.run.AssignedAgentID, SkillID: "challenge-reader", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "reader-token"}}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	x.coordinator = NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		x.policies++
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "ordinary source-read policy"}, nil
	}))
	x.worker = NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		x.credentials++
		return map[string]string{"token": "SECRET_READ_TOKEN"}, nil
	}), ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
		browser, _ := input.Arguments["browser"].(string)
		x.dispatches = append(x.dispatches, browser)
		if browser == "light" {
			return nil, failure
		}
		if browser != "full" || input.Run.ID != f.run.ID || input.Run.Scope != f.run.Scope {
			t.Fatalf("dispatch escaped original work: %#v", input)
		}
		return map[string]interface{}{"content": "The requested source was read."}, nil
	}))
	return x
}

func (f *sourceChallengeDispatchFixture) advance(t *testing.T, outcome *TurnOutcome) *AgentTurn {
	t.Helper()
	result, err := NewTurnCoordinator(f.chat.store, f.chat.store, f.chat.store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: f.chat.run.Scope, RunID: f.chat.run.ID, WorkerID: "foreground-worker",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) { return outcome, nil }))
	if err != nil || result == nil || result.Run == nil || result.Turn == nil {
		t.Fatalf("advance source interaction: %#v %v", result, err)
	}
	f.chat.run, f.chat.turn = result.Run, result.Turn
	return result.Turn
}

func (f *sourceChallengeDispatchFixture) propose(t *testing.T, browser, key, turnID string) *ActionProposalResult {
	t.Helper()
	result, err := f.coordinator.Propose(t.Context(), ProposeActionRequest{Scope: f.chat.run.Scope, RunID: f.chat.run.ID, WorkerID: "foreground-worker",
		TurnID: turnID, DeploymentID: f.chat.run.AssignedAgentID, SkillID: "challenge-reader", SkillVersion: "1.0.0", Action: "read",
		Arguments: map[string]interface{}{"browser": browser}, IdempotencyKey: key, Summary: "Read the requested source"})
	if err != nil || result == nil {
		t.Fatalf("admit source read: %#v %v", result, err)
	}
	f.chat.run = result.Run
	return result
}

func (f *sourceChallengeDispatchFixture) initialFailure(t *testing.T) *ActionExecutionResult {
	t.Helper()
	turn := f.advance(t, &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{{Type: "skill", Capability: "challenge-reader.read", Summary: "Read source with the light browser", IdempotencyKey: "light-read"}}})
	proposal := f.propose(t, "light", "light-read", turn.ID)
	if proposal.Call.Status != ActionCallStatusReady {
		t.Fatalf("initial read not admitted: %#v", proposal.Call)
	}
	result, err := f.worker.RunOnce(t.Context(), f.chat.run.Scope, "action-worker", time.Minute)
	if err != nil || result == nil || result.Call.ID != proposal.Call.ID || result.Call.Status != ActionCallStatusFailed || result.Call.Attempt != 1 || result.Call.MaxAttempts != 1 {
		t.Fatalf("initial dispatch did not fail exactly once: %#v %v", result, err)
	}
	f.chat.run = result.Run
	return result
}

func (f *sourceChallengeDispatchFixture) assertNoDispatch(t *testing.T, count int) {
	t.Helper()
	result, err := f.worker.RunOnce(t.Context(), f.chat.run.Scope, "action-worker", time.Minute)
	if err != nil || result != nil || len(f.dispatches) != count || f.credentials != count {
		t.Fatalf("unexpected dispatch or credential resolution: %#v %v dispatches=%v credentials=%d", result, err, f.dispatches, f.credentials)
	}
}

func TestSourceAccessChallengeIntegrationDispatchesOnlyAfterCanonicalAnswer(t *testing.T) {
	if MaximumToolFeedbackCorrections != 0 {
		t.Fatal("integration must retain zero generic failed-action corrections")
	}
	for _, independent := range []bool{false, true} {
		t.Run(fmt.Sprintf("independent=%v", independent), func(t *testing.T) {
			forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
				f := newSourceChallengeDispatchFixture(t, store, independent, skillerror.NewActionError("source_access_challenge", "untrusted details", nil))
				failed := f.initialFailure(t)
				if failed.Run.Status != AgentRunStatusQueued || requiresFinalFailureExplanation(failed.Run.Checkpoint) {
					t.Fatalf("typed challenge prematurely terminalized: %#v", failed.Run)
				}
				interaction := sourceAccessChallengeAssertPhase(t, f.chat, "question")
				if interaction.FailureID != failed.Call.ID {
					t.Fatal("interaction did not bind the real failed action receipt")
				}
				f.assertNoDispatch(t, 1)
				f.advance(t, sourceAccessChallengeTestWait("The source blocked this browser. Would you like me to try the fuller browser?"))
				if _, err := f.chat.scheduler.ReconcileScope(t.Context(), f.chat.run.Scope); err != nil {
					t.Fatal(err)
				}
				question, err := store.FindChannelMessageByIdempotencyKey(t.Context(), f.chat.run.Scope, f.chat.conversation.ID, clarificationQuestionKey(f.chat.run))
				if err != nil || question == nil || !question.RequiresResponse {
					t.Fatalf("question was not projected: %#v %v", question, err)
				}
				f.assertNoDispatch(t, 1)
				sourceAccessChallengeAcceptAnswer(t, f.chat, question, "browser-answer", "Yes, try the fuller browser for this source.")
				sourceAccessChallengeAssertPhase(t, f.chat, "answer")
				f.assertNoDispatch(t, 1)
				f.advance(t, &TurnOutcome{NextRunStatus: AgentRunStatusRunning})
				sourceAccessChallengeAssertPhase(t, f.chat, "continue")
				f.assertNoDispatch(t, 1)
				turn := f.advance(t, &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{{Type: "skill", Capability: "challenge-reader.read", Summary: "Read the source with the selected browser", IdempotencyKey: "full-read"}}})
				proposal := f.propose(t, "full", "full-read", turn.ID)
				if proposal.Call.Status != ActionCallStatusReady || proposal.Call.ID == failed.Call.ID || f.policies != 2 {
					t.Fatalf("verified action skipped ordinary admission: %#v policies=%d", proposal.Call, f.policies)
				}
				result, err := f.worker.RunOnce(t.Context(), f.chat.run.Scope, "action-worker", time.Minute)
				if err != nil || result == nil || result.Call.ID != proposal.Call.ID || result.Call.Status != ActionCallStatusSucceeded || result.Call.Attempt != 1 || result.Run.ID != failed.Run.ID {
					t.Fatalf("verified interaction could not dispatch once: %#v %v", result, err)
				}
				f.chat.run = result.Run
				if _, active := ReadToolFeedbackCorrection(result.Run.Checkpoint); active || terminalFailureCheckpointActive(result.Run.Checkpoint) {
					t.Fatal("successful selected action retained the old failed-attempt stop")
				}
				if len(f.dispatches) != 2 || f.dispatches[0] != "light" || f.dispatches[1] != "full" {
					t.Fatalf("dispatch order = %v", f.dispatches)
				}
				f.assertNoDispatch(t, 2)
			})
		})
	}
}

func TestSourceAccessChallengeIntegrationManualResumeCannotDispatchBeforeAnswer(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newSourceChallengeDispatchFixture(t, store, false, skillerror.NewActionError("source_access_challenge", "", nil))
		f.initialFailure(t)
		f.advance(t, sourceAccessChallengeTestWait("Would you like the fuller browser?"))
		activity := NewRunActivityService(store, store)
		var err error
		f.chat.run, _, err = activity.TransitionRun(t.Context(), f.chat.run.Scope, f.chat.run.ID, RunTransitionRequest{ExpectedRevision: f.chat.run.Revision, Status: AgentRunStatusQueued})
		if err != nil {
			t.Fatal(err)
		}
		f.chat.run, _, err = activity.TransitionRun(t.Context(), f.chat.run.Scope, f.chat.run.ID, RunTransitionRequest{ExpectedRevision: f.chat.run.Revision, Status: AgentRunStatusRunning})
		if err != nil {
			t.Fatal(err)
		}
		proposal := f.propose(t, "full", "unanswered-read", f.chat.turn.ID)
		if proposal.Call.Status != ActionCallStatusDenied {
			t.Fatalf("manual resume bypassed answer: %#v", proposal.Call)
		}
		f.assertNoDispatch(t, 1)
	})
}

func TestSourceAccessChallengeIntegrationDoesNotContinueOtherFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
	}{
		{"source 429", skillerror.NewActionError("source_rate_limited", "", nil)},
		{"challenge normalized to 429", skillerror.NewActionError("source_access_challenge", "", map[string]string{"httpStatus": "429"})},
		{"mixed source throttling", skillerror.NewActionError("source_reads_failed", "", map[string]string{"failures": `[{"index":0,"failureKind":"source_rate_limited","httpStatus":429},{"index":1,"failureKind":"source_access_challenge"}]`})},
		{"proxy authentication", skillerror.NewActionError("browser_proxy_authentication_failed", "", nil)},
		{"untyped access challenge", errors.New("source_access_challenge: try another browser")},
	} {
		t.Run(test.name, func(t *testing.T) {
			forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
				f := newSourceChallengeDispatchFixture(t, store, false, test.failure)
				failed := f.initialFailure(t)
				if failed.Run.Status != AgentRunStatusFailed || failed.Run.CompletedAt == nil || failed.Run.WakeCondition != nil {
					t.Fatalf("ordinary failed action gained continuation: %#v", failed.Run)
				}
				if interaction, err := resolveSourceAccessChallengeInteraction(t.Context(), store, failed.Run); err != nil || interaction != nil {
					t.Fatalf("ordinary failure gained interaction: %#v %v", interaction, err)
				}
				f.assertNoDispatch(t, 1)
				if next, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: failed.Run.Scope, WorkerID: "unexpected-worker", Now: time.Now().Add(time.Hour), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil || next != nil {
					t.Fatalf("ordinary failure queued another turn: %#v %v", next, err)
				}
			})
		})
	}
}
