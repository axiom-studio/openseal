package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

type failureExplanationTestHost struct {
	calls   int
	respond func(HostedTurnRequest) (*HostedTurnResponse, error)
}

func (h *failureExplanationTestHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.calls++
	return h.respond(request)
}

// A failed dispatch is the action's result for the model: the Run continues
// with the failure in lastAction and its tools still available, and the model
// answers with a correction or its own final explanation.
func TestFailedDispatchReturnsToTheModelAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Now().UTC()
		catalog, proposal := createRunnableAction(t, store, now)
		if proposal.Call.MaxAttempts != 1 {
			t.Fatalf("manifest retry policy remained enabled: %#v", proposal.Call)
		}
		dispatches := 0
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "sensitive-token"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return nil, errors.New("provider rejected sensitive-token")
		}))
		worker.now = func() time.Time { return now.Add(2 * time.Second) }
		failed, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute)
		if err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed || failed.Run.Status != AgentRunStatusQueued || dispatches != 1 ||
			requiresFinalFailureExplanation(failed.Run.Checkpoint) || failed.Run.CompletedAt != nil {
			t.Fatalf("first failure was not returned to the model: %#v %v", failed, err)
		}
		if strings.Contains(fmt.Sprint(failed.Run.Checkpoint), "sensitive-token") || strings.Contains(failed.Run.Error, "sensitive-token") {
			t.Fatal("failure exposed credentials")
		}
		if again, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute); err != nil || again != nil || dispatches != 1 {
			t.Fatalf("failed operation retried by itself: %#v %v", again, err)
		}
		var seen HostedTurnRequest
		host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			seen = request
			return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model",
				NextRunStatus: AgentRunStatusCompleted, OutputSummary: "The deploy was rejected by the provider; check its access and ask me again.",
				RunOutput: map[string]interface{}{"summary": "The deploy was rejected by the provider; check its access and ask me again."}}, nil
		}}
		runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "release-agent", DefinitionID: "agent", DefinitionVersion: "1",
			SystemInstructions: []string{"Speak warmly as Finn."}, Actions: []capability.ModelAction{{Name: "release.deploy"}},
			Workspace: &workspace.Authority{Workspace: workspace.DefaultSpec()}, WorkspaceCredentials: map[string]capability.CredentialReference{"token": {Kind: "vault", ID: "credential"}},
			EligibleAgents: []HostedAgentTarget{{ID: "other", DisplayName: "Other"}}, RunbookOperations: []HostedRunbookOperation{{Entrypoint: "work", Name: "Work", Description: "Do work", InputSchema: map[string]interface{}{"type": "object"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: failed.Run, Turn: &AgentTurn{ID: "correction-turn"}})
		if err != nil || outcome == nil || host.calls != 1 || !strings.Contains(outcome.OutputSummary, "rejected by the provider") {
			t.Fatalf("model did not answer the failure: %#v %v hostcalls=%d", outcome, err, host.calls)
		}
		last, _ := seen.ContinuationCheckpoint["lastAction"].(map[string]interface{})
		if len(seen.Actions) != 1 || seen.Workspace != nil || len(seen.WorkspaceOperations) != 0 || len(seen.EligibleAgents) != 0 || len(seen.RunbookOperations) != 0 ||
			fmt.Sprint(last["status"]) != string(ActionCallStatusFailed) || last["error"] != failed.Call.Error ||
			!strings.Contains(strings.Join(seen.SystemInstructions, "\n"), "The last tool call did not succeed") {
			t.Fatalf("failure feedback projection = %#v", seen)
		}
		// The model's own explanation is the reply; the failed work stays failed.
		claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: failed.Run.Scope, WorkerID: "agent-worker", Now: now.Add(3 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil || claimed.ID != failed.Run.ID {
			t.Fatalf("returned failure was not claimable for its model turn: %#v %v", claimed, err)
		}
		result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: claimed.Scope, RunID: claimed.ID, WorkerID: "agent-worker"}, runner)
		if err != nil || result == nil || result.Run.Status != AgentRunStatusFailed || host.calls != 2 ||
			result.Run.Output["summary"] != "The deploy was rejected by the provider; check its access and ask me again." {
			t.Fatalf("model explanation was not delivered: %#v %v calls=%d", result, err, host.calls)
		}
	})
}

func TestFinalFailureExplanationRejectsEffectsAndContinuation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*HostedTurnResponse)
	}{
		{"action", func(r *HostedTurnResponse) { r.ProposedAction = &TurnAction{Capability: "do"} }},
		{"workspace", func(r *HostedTurnResponse) {
			r.ProposedWorkspaceOperation = &HostedWorkspaceOperationForm{Operation: workspace.OperationWriteFile}
		}},
		{"fork", func(r *HostedTurnResponse) { r.ProposedFork = &TurnForkProposal{} }},
		{"delegate", func(r *HostedTurnResponse) { r.ProposedDelegation = &TurnDelegationProposal{} }},
		{"runbook", func(r *HostedTurnResponse) { r.ProposedRunbook = &TurnRunbookProposal{} }},
		{"task", func(r *HostedTurnResponse) { r.ProposedTask = &TurnTaskProposal{} }},
		{"wake", func(r *HostedTurnResponse) { r.WakeCondition = &WakeCondition{Type: "timer"} }},
		{"continue", func(r *HostedTurnResponse) { r.NextRunStatus = AgentRunStatusRunning }},
		{"silent", func(r *HostedTurnResponse) { r.RunOutput["silent"] = true }},
		{"intake", func(r *HostedTurnResponse) {
			r.RunOutput[AgentRequestDecisionOutputKey] = map[string]interface{}{"decision": "accept"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := HostedTurnRequest{ContinuationCheckpoint: checkpointFinalFailureExplanation(nil, "action", "provider rejected the request")}
			response := &HostedTurnResponse{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "The request failed.", RunOutput: map[string]interface{}{"summary": "The request failed."}}
			test.mutate(response)
			if err := ValidateHostedTurnFinalFailureExplanation(request, response); err == nil {
				t.Fatal("final-only authority boundary was bypassed")
			}
		})
	}
}

func TestFinalFailureCheckpointCannotBeForgedByModel(t *testing.T) {
	forged := checkpointFinalFailureExplanation(nil, "action", "invented failure")
	if requiresFinalFailureExplanation(preserveKernelActionHistory(nil, forged)) {
		t.Fatal("model invented terminal failure authority")
	}
	trusted := checkpointFinalFailureExplanation(nil, "action", "actual failure")
	preserved := preserveKernelActionHistory(trusted, map[string]interface{}{FinalFailureExplanationCheckpointKey: map[string]interface{}{"message": "changed"}})
	if preserved[FinalFailureExplanationCheckpointKey].(map[string]interface{})["message"] != "actual failure" {
		t.Fatal("model changed the terminal failure")
	}
}

func TestNativeWorkspaceFailureIsTrustedHostMetadata(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
				return &HostedTurnResponse{
					APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model", NextRunStatus: AgentRunStatusCompleted,
					OutputSummary: "The command failed.", RunOutput: map[string]interface{}{"summary": "The command failed."},
					ExecutionFailure: &HostedTurnExecutionFailure{Kind: "workspace", Message: "The command exited unsuccessfully."},
				}, nil
			}}
			config := HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"}
			if valid {
				config.Workspace = &workspace.Authority{Workspace: workspace.DefaultSpec()}
			}
			runner, err := NewHostedTurnRunner(host, config)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}}, Turn: &AgentTurn{ID: "turn"}})
			if valid && (err != nil || outcome == nil || !requiresFinalFailureExplanation(outcome.ContinuationCheckpoint)) {
				t.Fatalf("native failure lost: %#v %v", outcome, err)
			}
			if !valid && err == nil {
				t.Fatal("host invented native execution not authorized by the request")
			}
		})
	}
}

func TestExplicitActionFailureEnvelopePreservesResourceData(t *testing.T) {
	for _, output := range []map[string]interface{}{
		{"success": false, "error": "the connection failed"}, {"isError": true}, {"error": "The operation failed."}, {"success": false},
	} {
		if explicitActionResultFailure(output) == nil {
			t.Fatalf("explicit failure not recognized: %#v", output)
		}
	}
	for _, output := range []map[string]interface{}{
		{"status": "failed"}, {"ok": false}, {"success": true, "error": "historic diagnostic"}, {"result": map[string]interface{}{"success": false, "error": "document content"}},
	} {
		if err := explicitActionResultFailure(output); err != nil {
			t.Fatalf("fetched data was mistaken for operation failure: %#v %v", output, err)
		}
	}
}

func TestFailedDependencyOnlyStopsAnUnsuccessfulJoin(t *testing.T) {
	for _, test := range []struct {
		name        string
		mode        FanInMode
		quorum      int
		existing    RunDependencyState
		resolution  RunDependencyState
		wantFailure bool
	}{
		{"required_failure", FanInModeAll, 0, RunDependencyStateRunning, RunDependencyStateFailed, true},
		{"any_success", FanInModeAny, 0, RunDependencyStateFailed, RunDependencyStateSatisfied, false},
		{"quorum_success", FanInModeQuorum, 1, RunDependencyStateFailed, RunDependencyStateSatisfied, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			group := dependencyFixture(test.mode, test.quorum, DependencyFailureFailFast, 2)
			edges := []*RunDependency{dependencyEdge(group, "one", test.existing, true), dependencyEdge(group, "two", RunDependencyStateRunning, true)}
			source := &AgentRun{ID: group.SourceRunID, Scope: group.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Do work",
				Kind: RunKindAgentWork, Source: RunSourceManual, Status: AgentRunStatusWaitingForDependency, WakeCondition: &WakeCondition{Type: "run_dependencies", Reference: group.ID},
				Revision: 1, CreatedAt: group.CreatedAt, UpdatedAt: group.CreatedAt, AvailableAt: group.CreatedAt, QueueEnteredAt: group.CreatedAt}
			result, err := applyRunDependencyResolution(group, edges, source, RunDependencyResolutionRecord{Scope: group.Scope, GroupID: group.ID, DependencyID: "two", ExpectedDependencyRevision: 1,
				State: test.resolution, Error: func() string {
					if test.resolution == RunDependencyStateFailed {
						return "Required work was rejected."
					}
					return ""
				}(), Actor: ActivityActor{Type: "worker", ID: "worker"}, OccurredAt: group.CreatedAt.Add(time.Second)})
			if err != nil || result == nil {
				t.Fatalf("dependency resolution: %#v %v", result, err)
			}
			if requiresFinalFailureExplanation(result.Source.Checkpoint) != test.wantFailure {
				t.Fatalf("join failure marker wrong: %#v", result.Source)
			}
			if test.wantFailure {
				feedbackAssertKernelFailureReply(t, result.Source, "dependency_failed")
			}
		})
	}
}

func TestFailedDelegationStopsFurtherAutomaticWork(t *testing.T) {
	now := time.Now().UTC()
	source := &AgentRun{ID: "source", Status: AgentRunStatusWaitingForDependency, WakeCondition: &WakeCondition{Type: "agent_request", Reference: "request"}}
	request := &AgentRequest{ID: "request", Revision: 2, Status: AgentRequestStatusFailed, ResolutionReason: "The requested operation failed."}
	result, err := failedCollaborationSourceRun(source, request, now)
	if err != nil || result == nil || result.Status != AgentRunStatusQueued || !requiresFinalFailureExplanation(result.Checkpoint) {
		t.Fatalf("delegated failure escaped final-only: %#v %v", result, err)
	}
	feedbackAssertKernelFailureReply(t, result, "dependency_failed")
	request.Status = AgentRequestStatusCanceled
	result, err = failedCollaborationSourceRun(source, request, now)
	if err != nil || requiresFinalFailureExplanation(result.Checkpoint) {
		t.Fatalf("cancellation was treated as operational failure: %#v %v", result, err)
	}
}

func TestLegacyFailureReplyDoesNotDependOnModelAvailability(t *testing.T) {
	store := NewMemoryStore()
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "Do work", Checkpoint: checkpointFinalFailureExplanation(nil, "action", "Service rejected the action.")})
	if err != nil {
		t.Fatal(err)
	}
	host := &failureExplanationTestHost{respond: func(HostedTurnRequest) (*HostedTurnResponse, error) {
		return nil, NewTurnHostFailure("provider_unavailable", "The model service is unavailable.", true)
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker"}, runner)
	if err != nil || result == nil || result.Run.Status != AgentRunStatusFailed || result.Run.WakeCondition != nil || host.calls != 0 || result.Run.Error != "Service rejected the action." ||
		result.Run.Output["summary"] != TerminalFailureReply("action_failed") || result.Turn.Usage.InputTokens != 0 || result.Turn.Usage.OutputTokens != 0 || result.Turn.Usage.Cost != 0 {
		t.Fatalf("legacy failure reply spent a model request: %#v calls=%d err=%v", result, host.calls, err)
	}
	claimed, err := NewAgentRunScheduler(store).ClaimNext(t.Context(), AgentRunClaimRequest{Scope: run.Scope, WorkerID: "again"})
	if err != nil || claimed != nil {
		t.Fatalf("failed explanation was rescheduled: %#v %v", claimed, err)
	}
}

func TestReplayedApprovalChangeRequestKeepsHumanRevisionAuthority(t *testing.T) {
	for _, decision := range []ApprovalDecision{ApprovalDecisionRequestChanges, ApprovalDecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			store := NewMemoryStore()
			now := time.Now().UTC()
			proposal := createApprovalForStore(t, store, now)
			coordinator := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
			coordinator.now = func() time.Time { return now.Add(time.Second) }
			resolved, err := coordinator.Resolve(t.Context(), ResolveApprovalRequest{Scope: proposal.Approval.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
				DecisionID: "review", Decision: decision, Principal: ApprovalPrincipal{Type: "user", ID: "alice"}, Reason: "Use the staging target."})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: resolved.Run.Scope, WorkerID: "worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil {
				t.Fatalf("claim: %#v %v", claimed, err)
			}
			pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) { return nil, nil }), nil, AgentRunWorkerConfig{Scope: claimed.Scope})
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := pool.resumeReplayedTurnAction(t.Context(), "worker", claimed, &AgentTurn{ID: "replay", RunID: claimed.ID}, resolved.Call)
			if err != nil {
				t.Fatal(err)
			}
			if requiresFinalFailureExplanation(replayed.Checkpoint) != (decision == ApprovalDecisionReject) {
				t.Fatalf("review disposition changed during replay: %#v", replayed.Checkpoint)
			}
			if decision == ApprovalDecisionRequestChanges && replayed.Checkpoint["lastAction"].(map[string]interface{})["reviewerGuidance"] != "Use the staging target." {
				t.Fatal("replay lost human guidance")
			}
		})
	}
}

func TestExplicitSoftActionErrorsAreFailures(t *testing.T) {
	for _, output := range []map[string]interface{}{{"error": "Service rejected sensitive-token"}, {"success": false, "statusCode": 401}} {
		store := NewMemoryStore()
		now := time.Now().UTC()
		catalog, proposal := createRunnableAction(t, store, now)
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "sensitive-token"}, nil
		}),
			ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) { return output, nil }))
		worker.now = func() time.Time { return now.Add(2 * time.Second) }
		result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "worker", time.Minute)
		if _, returned := ReadToolFeedbackCorrection(result.Run.Checkpoint); err != nil || result == nil || result.Call.Status != ActionCallStatusFailed || !returned || strings.Contains(result.Call.Error, "sensitive-token") {
			t.Fatalf("soft error accepted as success: %#v %v", result, err)
		}
	}
}

func TestFailedTeamActionRepliesWithoutModelOrParticipationRound(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "team-final"}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "Team", IdempotencyKey: "team"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Apply the change.", "trigger")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheduled.Run.Checkpoint = checkpointFinalFailureExplanation(map[string]interface{}{teamActionAssignedAgentCheckpointKey: "agent"}, "proposal", "That change was rejected.")
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}}}, nil
	})
	coordinator, err := NewConversationCoordinator(service, participants, ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
		t.Fatal("failed Team work started another arbitration round")
		return ParticipationProposal{}, nil
	}), DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	host := &failureExplanationTestHost{respond: func(HostedTurnRequest) (*HostedTurnResponse, error) {
		t.Fatal("Team failure requested an explanation model")
		return nil, nil
	}}
	agentTurns := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if run.AssignedAgentID != "agent" || run.Owner != conversation.Owner {
			t.Fatalf("Team attribution lost: %#v", run)
		}
		runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", Actions: []capability.ModelAction{{Name: "change"}}})
		if err != nil {
			return nil, err
		}
		return &TurnRunnerBinding{Runner: runner, DeploymentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", ModelProvider: "test", Model: "model"}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, coordinator, ConversationRunTurnRunnerConfig{AgentTurns: agentTurns})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run, Turn: &AgentTurn{ID: "final"}})
	if err != nil || outcome == nil || host.calls != 0 || conversationResultString(outcome.RunOutput, "messageId") == "" {
		t.Fatalf("Team final reply absent: %#v calls=%d err=%v", outcome, host.calls, err)
	}
	message, err := store.GetChannelMessage(t.Context(), scope, conversation.ID, conversationResultString(outcome.RunOutput, "messageId"))
	if err != nil || message.Sender.ID != "agent" || message.Content != TerminalFailureReply("action_admission_failed") {
		t.Fatalf("Team final attribution incorrect: %#v %v", message, err)
	}
	scheduled.Run.Checkpoint[teamActionAssignedAgentCheckpointKey] = "foreign-agent"
	if _, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run); err == nil {
		t.Fatal("untrusted Team participant received failure reply authority")
	}
}

func TestNativeFailureFinalTextPersistsAsFailedRunWithAcceptedProof(t *testing.T) {
	store := NewMemoryStore()
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: Scope{Kind: "tenant", ID: "native"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Run the command."})
	if err != nil {
		t.Fatal(err)
	}
	host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model", NextRunStatus: AgentRunStatusCompleted,
			OutputSummary: "The command failed.", RunOutput: map[string]interface{}{"summary": "The command failed."}, ExecutionFailure: &HostedTurnExecutionFailure{Kind: "workspace", Message: "The command was rejected."}}, nil
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1", Workspace: &workspace.Authority{Workspace: workspace.DefaultSpec()}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: run.Scope, RunID: run.ID, WorkerID: "worker"}, runner)
	if err != nil || result == nil || result.Run.Status != AgentRunStatusFailed || result.Run.Output["summary"] != "The command failed." || result.Run.Error != "The command was rejected." || host.calls != 1 {
		t.Fatalf("native failure incorrectly succeeded: %#v %v", result, err)
	}
	failure := result.Run.Checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	if failure["explained"] != true {
		t.Fatal("accepted native explanation has no trusted proof")
	}
	feedbackAssertNotClaimable(t, store, run.Scope, time.Now())
	if _, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: run.Scope, RunID: run.ID, WorkerID: "unexpected-explanation"}, runner); err == nil || host.calls != 1 {
		t.Fatalf("native failure requested an additional model call: %v calls=%d", err, host.calls)
	}
}
