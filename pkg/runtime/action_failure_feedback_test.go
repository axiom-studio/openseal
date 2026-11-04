package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// approvedFailedRun approves a governed external action and fails its
// execution with cause, as in Run c7a9efec: an approved Slack post failed with
// channel_not_found.
func approvedFailedRun(t *testing.T, store KernelStore, now time.Time, cause error) (*skill.Catalog, *ActionProposalResult, *ActionExecutionResult) {
	t.Helper()
	proposal := createApprovalForStore(t, store, now)
	approvals := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
	approvals.now = func() time.Time { return now.Add(2 * time.Second) }
	if _, err := approvals.Resolve(t.Context(), ResolveApprovalRequest{
		Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "approve", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: "alice"},
	}); err != nil {
		t.Fatal(err)
	}
	catalog, _ := governedActionCatalog(t)
	dispatches := 0
	worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "secret"}, nil
	}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return nil, cause
	}))
	worker.now = func() time.Time { return now.Add(3 * time.Second) }
	failed, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute)
	if err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed || dispatches != 1 {
		t.Fatalf("approved action did not fail once: %#v %v dispatches=%d", failed, err, dispatches)
	}
	return catalog, proposal, failed
}

func releaseModelActions() []capability.ModelAction {
	return []capability.ModelAction{{
		Name: "release.deploy", SkillID: "release", Version: "1.0.0", Action: "deploy", DeploymentID: "release-agent",
		BindingID: "release-binding", BindingRevision: 1, SideEffect: capability.SideEffectExternal,
		ExternalOperationPolicy: capability.ExternalOperationForbidden,
	}}
}

// Run c7a9efec: an approved external action failed with a provider error. Its
// failure, approval recovery and correction state are all recorded, and the
// next turn must reach the model with that failure (with its tools still on
// offer) instead of failing in milliseconds. The model may explain or retry.
func TestApprovedExternalActionFailureReachesTheModel(t *testing.T) {
	for _, answer := range []string{"explain", "retry"} {
		t.Run(answer, func(t *testing.T) {
			store := NewMemoryStore()
			now := time.Now().UTC()
			_, proposal, failed := approvedFailedRun(t, store, now, errors.New("runId=r1 Atlas error: execution: slack API error: channel_not_found"))
			if failed.Run.Status != AgentRunStatusQueued || requiresFinalFailureExplanation(failed.Run.Checkpoint) ||
				failed.Run.Checkpoint[approvalRecoveryCheckpointKey] == nil {
				t.Fatalf("approved failure was not returned to the model: %#v", failed.Run)
			}
			if _, active := ReadToolFeedbackCorrection(failed.Run.Checkpoint); !active {
				t.Fatalf("approved failure has no correction state: %#v", failed.Run.Checkpoint)
			}
			host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
				last, _ := request.ContinuationCheckpoint["lastAction"].(map[string]interface{})
				if len(request.Actions) != 1 || request.ContinuationCheckpoint[approvalRecoveryCheckpointKey] == nil ||
					!strings.Contains(fmt.Sprint(last["error"]), "channel_not_found") ||
					!strings.Contains(strings.Join(request.SystemInstructions, "\n"), "The last tool call did not succeed") {
					t.Fatalf("model did not get the failure with its tools: %#v", request)
				}
				response := &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model",
					Usage: TurnUsage{InputTokens: 20, OutputTokens: 5}}
				if answer == "explain" {
					response.NextRunStatus = AgentRunStatusCompleted
					response.RunOutput = map[string]interface{}{"summary": "Slack says I'm not in that channel. Invite me and I'll post it."}
				} else {
					response.NextRunStatus = AgentRunStatusRunning
					response.ProposedAction = &TurnAction{Type: "skill_action", Capability: "release.deploy", Summary: "Deploy to staging instead",
						IdempotencyKey: "deploy-staging", InputRef: "/actionInputs/deploy"}
					response.ContinuationCheckpoint = map[string]interface{}{"actionInputs": map[string]interface{}{"deploy": map[string]interface{}{"environment": "staging"}}}
				}
				return response, nil
			}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "release-agent", DefinitionID: "agent", DefinitionVersion: "1", Actions: releaseModelActions()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: proposal.Call.Scope, WorkerID: "agent-worker", Now: now.Add(4 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
				t.Fatal(err)
			}
			result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: proposal.Call.Scope, RunID: proposal.Call.RunID, WorkerID: "agent-worker"}, runner)
			if err != nil || result == nil || host.calls != 1 || result.Turn.Status != AgentTurnStatusCompleted || result.Turn.Usage.InputTokens != 20 {
				t.Fatalf("turn after the approved failure did not reach the model: %#v %v calls=%d", result, err, host.calls)
			}
			if answer == "explain" {
				if result.Run.Status != AgentRunStatusFailed || result.Run.Output["summary"] != "Slack says I'm not in that channel. Invite me and I'll post it." {
					t.Fatalf("model explanation was not the reply: %#v", result.Run)
				}
				return
			}
			if len(result.Turn.RequestedActions) != 1 || result.Turn.RequestedActions[0].Capability != "release.deploy" {
				t.Fatalf("model retry was not proposed: %#v", result.Turn)
			}
		})
	}
}

// Every failed action result goes back to the model, whatever the tool or
// error: MCP tool errors, HTTP failures, timeouts, permission and validation
// errors, and soft failure envelopes alike.
func TestEveryActionFailureIsReturnedToTheModel(t *testing.T) {
	for name, dispatch := range map[string]func() (map[string]interface{}, error){
		"mcp tool error": func() (map[string]interface{}, error) { return map[string]interface{}{"isError": true}, nil },
		"mcp error text": func() (map[string]interface{}, error) {
			return nil, errors.New("mcp tool linear.create_issue: team not found")
		},
		"http 500": func() (map[string]interface{}, error) {
			return nil, errors.New("POST https://api.example.invalid/v1/charges: HTTP 500 Internal Server Error")
		},
		"timeout": func() (map[string]interface{}, error) { return nil, context.DeadlineExceeded },
		"permission denied": func() (map[string]interface{}, error) {
			return nil, errors.New("403 Forbidden: missing scope chat:write")
		},
		"provider rejected": func() (map[string]interface{}, error) {
			return map[string]interface{}{"success": false, "error": "invalid email address"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := NewMemoryStore()
			now := time.Now().UTC()
			catalog, proposal := createRunnableAction(t, store, now)
			worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
				return map[string]string{"token": "secret"}, nil
			}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) { return dispatch() }))
			worker.now = func() time.Time { return now.Add(2 * time.Second) }
			failed, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute)
			if err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed {
				t.Fatalf("failure was not recorded: %#v %v", failed, err)
			}
			feedback, active := ReadToolFeedbackCorrection(failed.Run.Checkpoint)
			if failed.Run.Status != AgentRunStatusQueued || requiresFinalFailureExplanation(failed.Run.Checkpoint) || !active || feedback.Message == "" {
				t.Fatalf("failure was not returned to the model: %#v", failed.Run)
			}
			var seen HostedTurnRequest
			host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
				seen = request
				return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model",
					NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "That didn't work: " + feedback.Message}}, nil
			}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "release-agent", DefinitionID: "agent", DefinitionVersion: "1", Actions: releaseModelActions()})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: failed.Run, Turn: &AgentTurn{ID: "feedback-turn"}})
			last, _ := seen.ContinuationCheckpoint["lastAction"].(map[string]interface{})
			if err != nil || outcome == nil || host.calls != 1 || len(seen.Actions) != 1 || last["error"] != failed.Call.Error {
				t.Fatalf("model did not get the failure as the action's result: %#v %v", seen, err)
			}
		})
	}
}

// A turn that fails before or without a model answer (a host refusal, an
// invalid model outcome) is recorded with its cause and returned to the
// model for one answer-only explanation, which becomes the chat reply.
func TestFailedTurnIsExplainedInTheConversation(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "explained-turn"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Chat", IdempotencyKey: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, conversations, conversation, ConversationParticipantUser, MessageIntentQuestion, "Post the update", "question")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	var requests []HostedTurnRequest
	host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		requests = append(requests, request)
		if len(requests) == 1 {
			return nil, NewTurnHostFailure("previous_attempt_failed", "The host refused this turn.", false)
		}
		return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model",
			NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "I couldn't post that update because my last step was refused."}}, nil
	}}
	agentTurns := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", Actions: releaseModelActions()})
		return &TurnRunnerBinding{DeploymentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", Runner: runner}, err
	})
	conversationRunner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, conversations), ConversationRunTurnRunnerConfig{AgentTurns: agentTurns})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	advance := func(run *AgentRun, worker string) *AdvanceAgentRunResult {
		binding, err := conversationRunner.ResolveTurnRunner(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: worker}, binding.Runner)
		if result == nil {
			t.Fatal("turn was not applied")
		}
		return result
	}
	first := advance(scheduled.Run, "first")
	failure, _ := first.Run.Checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	if first.Turn.Status != AgentTurnStatusFailed || first.Run.Status != AgentRunStatusQueued || failure["turnError"] != "The host refused this turn." ||
		first.Event.Payload["turnError"] != "The host refused this turn." {
		t.Fatalf("failed turn lost its cause or ended the Run: %#v %#v", first.Run, first.Event)
	}
	second := advance(first.Run, "second")
	failure, _ = second.Run.Checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	if len(requests) != 2 || len(requests[1].Actions) != 0 || second.Run.Status != AgentRunStatusFailed || failure["explained"] != true {
		t.Fatalf("failure was not explained by the model: %#v requests=%d", second.Run, len(requests))
	}
	messageID, _ := second.Run.Output["messageId"].(string)
	message, err := store.GetChannelMessage(ctx, scope, conversation.ID, messageID)
	if err != nil || message == nil || message.Content != "I couldn't post that update because my last step was refused." || message.ReplyToMessageID != trigger.ID {
		t.Fatalf("explanation was not the chat reply: %#v %v", message, err)
	}
}

// When no model can answer (here the Workspace is out of credits), the Run
// fails at once with its typed cause, even after an earlier tool failure, so
// the app shows its credit stop.
func TestCreditStopKeepsItsCauseAfterAToolFailure(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	_, proposal, failed := approvedFailedRun(t, store, now, errors.New("slack API error: channel_not_found"))
	if _, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: proposal.Call.Scope, WorkerID: "agent-worker", Now: now.Add(4 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: failed.Run.Scope, RunID: failed.Run.ID, WorkerID: "agent-worker"},
		TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			return nil, NewTurnHostFailure("gateway_credits_exhausted", "No model credits.", false)
		}))
	if err == nil || result == nil || result.Run.Status != AgentRunStatusFailed || result.Run.CreditError == nil {
		t.Fatalf("credit stop lost its cause: %#v %v", result, err)
	}
}

func TestFallbackReplyNamesTheToolAndItsAnswer(t *testing.T) {
	checkpoint := checkpointTerminalAction(nil, &ActionCall{ID: "post", SkillID: "skill-slack", Action: "slack-send-message", Status: ActionCallStatusFailed,
		SideEffect: skill.SideEffectExternal, Error: "runId=abc Atlas error: execution: slack API error: channel_not_found"}, nil)
	want := "I couldn't finish this request because Slack reported: slack API error: channel_not_found. I stopped this attempt. Ask me to try again when you're ready."
	if got := terminalFailureReplyFromCheckpoint(checkpoint); got != want {
		t.Fatalf("fallback reply = %q", got)
	}
	stopped := checkpointTerminalFailure(checkpointFinalFailureExplanation(checkpoint, "action", "runId=abc Atlas error: execution: slack API error: channel_not_found"), "action_failed")
	if got := terminalFailureReplyFromCheckpoint(stopped); got != want {
		t.Fatalf("stopped fallback reply = %q", got)
	}
	// A failure the model already handled is not the cause of a later stop.
	other := checkpointFinalFailureExplanation(checkpoint, "execution", "The model returned an invalid answer.")
	if got := terminalFailureReplyFromCheckpoint(checkpointTerminalFailure(other, "execution_failed")); got != TerminalFailureReply("execution_failed") {
		t.Fatalf("unrelated failure attributed: %q", got)
	}
}
