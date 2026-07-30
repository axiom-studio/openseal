package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestHostedTurnRunnerProjectsLatestGovernedScreenshotAsEphemeralMedia(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("jpeg-bytes"))
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "vision-turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "vision-model", OutputSummary: "inspected",
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "browser", DefinitionID: "browser", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := checkpointTerminalAction(nil, &ActionCall{
		ID: "snapshot-call", SkillID: "browser", SkillVersion: "1", Action: "snapshot", Status: ActionCallStatusSucceeded,
		Output: map[string]interface{}{"modelMedia": map[string]interface{}{"mediaType": "image/jpeg", "contentBase64": encoded, "detail": "low"}},
	}, nil)
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "vision-run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Inspect the page", Checkpoint: checkpoint},
		Turn: &AgentTurn{ID: "vision-turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.request.ModelMedia) != 1 || host.request.ModelMedia[0].DataBase64 != encoded || host.request.ModelMedia[0].SourceActionCallID != "snapshot-call" {
		t.Fatalf("model media = %#v", host.request.ModelMedia)
	}
	modelInput, err := MarshalHostedTurnModelInput(host.request)
	if err != nil || strings.Contains(string(modelInput), encoded) || !strings.Contains(string(modelInput), `"attached":true`) {
		t.Fatalf("text model input leaked media: %s err=%v", modelInput, err)
	}
}

type recordingTurnHost struct {
	request  HostedTurnRequest
	response *HostedTurnResponse
	err      error
}

func TestHostedTurnRetriesWhenPostActionReasoningFails(t *testing.T) {
	host := &recordingTurnHost{err: errors.New("unexpected EOF")}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "browser", DefinitionID: "browser", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := checkpointTerminalAction(nil, &ActionCall{
		ID: "snapshot-call", SkillID: "skill-browser", SkillVersion: "2.0.5", Action: "browser-snapshot",
		Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"url": "https://fixture.test/thread"},
	}, nil)
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Capture the page", Checkpoint: checkpoint},
		Turn: &AgentTurn{ID: "summary-turn"},
	})
	if outcome != nil || !errors.Is(err, ErrTurnHostUnavailable) {
		t.Fatalf("post-action retry outcome=%#v err=%v", outcome, err)
	}
}

func TestHostedTurnPreservesTerminalHostFailure(t *testing.T) {
	host := &recordingTurnHost{err: NewTurnHostFailure(
		"provider_quota_exhausted",
		"Model provider quota is exhausted; choose a credential with available quota.",
		false,
	)}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Work"}, Turn: &AgentTurn{ID: "turn"},
	})
	var failure *TurnHostFailure
	if outcome != nil || !errors.As(err, &failure) || failure.Code != "provider_quota_exhausted" || failure.Retryable || errors.Is(err, ErrTurnHostUnavailable) {
		t.Fatalf("terminal outcome=%#v failure=%#v err=%v", outcome, failure, err)
	}
}

func TestHostedTurnPreservesTypedRetryableHostFailure(t *testing.T) {
	host := &recordingTurnHost{err: NewTurnHostFailure("provider_rate_limited", "Model provider is temporarily rate limited.", true)}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Work"}, Turn: &AgentTurn{ID: "turn"},
	})
	var failure *TurnHostFailure
	if !errors.As(err, &failure) || failure.Code != "provider_rate_limited" || !errors.Is(err, ErrTurnHostUnavailable) {
		t.Fatalf("retryable failure=%#v err=%v", failure, err)
	}
}

func TestHostedTurnDoesNotTrustModelVisibleLastActionWithoutKernelHistory(t *testing.T) {
	host := &recordingTurnHost{err: errors.New("unexpected EOF")}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "browser", DefinitionID: "browser", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Capture the page", Checkpoint: map[string]interface{}{
			"lastAction": map[string]interface{}{"actionCallId": "invented", "status": "succeeded"},
		}},
		Turn: &AgentTurn{ID: "summary-turn"},
	})
	if !errors.Is(err, ErrTurnHostUnavailable) {
		t.Fatalf("untrusted lastAction error = %v", err)
	}
}

func (h *recordingTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.request = request
	return h.response, h.err
}

func hostedTestAgentTargets(ids ...string) []HostedAgentTarget {
	targets := make([]HostedAgentTarget, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, HostedAgentTarget{ID: id, DisplayName: id})
	}
	return targets
}

func TestHostedTurnRunnerAuthorizesCallableRunbookProposal(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn-runbook",
		ModelProvider: "openai-compatible", Model: "test-model", NextRunStatus: AgentRunStatusRunning,
		ProposedRunbook: &TurnRunbookProposal{
			Entrypoint: "collect-release-evidence", Summary: "Collect release evidence deterministically",
			Arguments: map[string]interface{}{"release": "2.0.0"},
		},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "release-agent", DefinitionID: "release-agent", DefinitionVersion: "2",
		RunbookOperations: []HostedRunbookOperation{{
			Entrypoint: "collect-release-evidence", Name: "Collect release evidence",
			Description: "Collect the exact evidence required for a release.",
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"release": map[string]interface{}{"type": "string"}},
				"required":   []interface{}{"release"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Prepare the release", Checkpoint: map[string]interface{}{}},
		Turn: &AgentTurn{ID: "turn-runbook"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ProposedRunbook == nil || outcome.ProposedRunbook.Entrypoint != "collect-release-evidence" ||
		len(host.request.RunbookOperations) != 1 {
		t.Fatalf("outcome=%#v request=%#v", outcome, host.request)
	}

	host.response.ProposedRunbook.Arguments = map[string]interface{}{}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Prepare", Checkpoint: map[string]interface{}{}},
		Turn: &AgentTurn{ID: "turn-runbook"},
	}); err == nil {
		t.Fatal("expected invalid runbook arguments to fail")
	}
	host.response.ProposedRunbook.Arguments = map[string]interface{}{"release": "2.0.0"}
	host.response.ProposedRunbook.Entrypoint = "undeclared"
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Prepare", Checkpoint: map[string]interface{}{}},
		Turn: &AgentTurn{ID: "turn-runbook"},
	}); err == nil {
		t.Fatal("expected undeclared runbook entrypoint to fail")
	}
}

func TestHostedTurnRunnerUsesDurableIdentityAndAuthorizedPromptProjection(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn-7", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "openai-compatible", Model: "deepseek-v4-flash",
		SkillSelections: []HostedSkillSelection{{SkillRef: "skill:summarize@1.0.0", Disposition: HostedSkillApplied, Summary: "Applied faithful summarization"}},
		OutputSummary:   "Research completed", RunOutput: map[string]interface{}{"answer": "done"},
		Usage: TurnUsage{InputTokens: 10, OutputTokens: 4},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent-3", DefinitionID: "researcher", DefinitionVersion: "2",
		SystemInstructions: []string{"Preserve evidence."},
		SkillPrompts:       []HostedSkillPrompt{{SkillID: "summarize", Version: "1.0.0", Name: "summarize", Instructions: "Summarize sources."}},
		Actions:            []capability.ModelAction{{Name: "web.search", SkillID: "web", Version: "1", Action: "search"}},
		ModelCredential:    &capability.CredentialReference{Kind: "model-provider", ID: "credential-17"},
		ModelProvider:      "openai-compatible", Model: "deepseek-v4-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(context.Background(), TurnExecutionContext{
		Run: &AgentRun{ID: "run-2", Scope: Scope{Kind: "tenant", ID: "1"}, AssignedAgentID: "agent-3", Goal: "Analyze launch feedback", Context: map[string]interface{}{"source": "https://example.test/evidence"}, Checkpoint: map[string]interface{}{"cursor": "next"},
			Output: map[string]interface{}{
				"summary":              "delegated",
				"dependencyGroups":     map[string]interface{}{"group-1": map[string]interface{}{"status": "satisfied", "dependencies": map[string]interface{}{"analysis": map[string]interface{}{"result": map[string]interface{}{"output": map[string]interface{}{"finding": "visual debugging matters"}}}}}},
				"collaborationResults": map[string]interface{}{"request-1": map[string]interface{}{"requestId": "request-1", "output": map[string]interface{}{"brief": "Launch accessibility"}}},
			},
			Budget: &BudgetPolicy{MaxTurns: 10, MaxDurationMS: 120000, MaxActions: 4}, BudgetUsage: BudgetUsage{Turns: 2, DurationMS: 15000},
			BudgetReservations: map[string]BudgetReservation{"pending": {ID: "pending", Usage: BudgetUsage{Turns: 1, DurationMS: 5000}, CreatedAt: time.Now()}},
			BudgetAllocations:  map[string]BudgetPolicy{"child": {MaxTurns: 2, MaxDurationMS: 30000, MaxActions: 1}}},
		Turn: &AgentTurn{ID: "turn-7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if host.request.InvocationID != "turn-7" || host.request.TurnID != "turn-7" || host.request.RunID != "run-2" || host.request.Goal != "Analyze launch feedback" {
		t.Fatalf("host request = %#v", host.request)
	}
	if host.request.InputContext["source"] != "https://example.test/evidence" {
		t.Fatalf("input context = %#v", host.request.InputContext)
	}
	if host.request.Budget == nil || host.request.Budget.EffectiveUsage.Turns != 3 || host.request.Budget.Remaining.MaxTurns != 5 || host.request.Budget.Remaining.MaxDurationMS != 70000 || host.request.Budget.Remaining.MaxActions != 3 {
		t.Fatalf("hosted budget = %#v", host.request.Budget)
	}
	if host.request.DependencyResults["group-1"].(map[string]interface{})["status"] != "satisfied" {
		t.Fatalf("dependency results = %#v", host.request.DependencyResults)
	}
	if host.request.CollaborationResults["request-1"].(map[string]interface{})["output"].(map[string]interface{})["brief"] != "Launch accessibility" {
		t.Fatalf("collaboration results = %#v", host.request.CollaborationResults)
	}
	if host.request.ModelCredential == nil || host.request.ModelCredential.Kind != "model-provider" || host.request.ModelCredential.ID != "credential-17" {
		t.Fatalf("model credential reference = %#v", host.request.ModelCredential)
	}
	if len(host.request.SkillPrompts) != 1 || host.request.SkillPrompts[0].Instructions != "Summarize sources." || host.request.SkillPrompts[0].Reference != "skill:summarize@1.0.0" || outcome.RunOutput["answer"] != "done" || len(outcome.Decisions) != 1 || outcome.Decisions[0].EvidenceRefs[0] != "skill:summarize@1.0.0" || outcome.ModelProvider != "openai-compatible" || outcome.Model != "deepseek-v4-flash" {
		t.Fatalf("request=%#v outcome=%#v", host.request, outcome)
	}
	encoded, _ := MarshalHostedTurnModelInput(host.request)
	for _, forbidden := range []string{"apiKey", "credential", "secret"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("host envelope exposes forbidden credential surface %q: %s", forbidden, encoded)
		}
	}
}

func TestHostedTurnRunnerRejectsUnsafeCollaborationResultsBeforeHostDispatch(t *testing.T) {
	host := &recordingTurnHost{}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{
			ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Synthesize delegated work",
			Output: map[string]interface{}{"collaborationResults": map[string]interface{}{
				"request": map[string]interface{}{"output": map[string]interface{}{"apiKey": "must-not-reach-host"}},
			}},
		},
		Turn: &AgentTurn{ID: "turn"},
	})
	if !errors.Is(err, ErrUnsafeSharedContext) {
		t.Fatalf("unsafe collaboration projection error = %v", err)
	}
	if host.request.InvocationID != "" {
		t.Fatalf("unsafe collaboration result reached host: %#v", host.request)
	}
}

func TestHostedTurnRunnerRejectsIncompleteModelCredentialReference(t *testing.T) {
	_, err := NewHostedTurnRunner(&recordingTurnHost{}, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
		ModelCredential: &capability.CredentialReference{Kind: "model-provider"},
	})
	if err == nil || !strings.Contains(err.Error(), "model credential reference") {
		t.Fatalf("incomplete model credential error = %v", err)
	}
}

func TestHostedTurnRunnerDoesNotRetryConfigurationFailures(t *testing.T) {
	host := &recordingTurnHost{err: fmt.Errorf("%w: Agent requires a configured model provider", ErrTurnHostConfiguration)}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Respond"},
		Turn: &AgentTurn{ID: "turn"},
	})
	if !errors.Is(err, ErrTurnHostConfiguration) || errors.Is(err, ErrTurnHostUnavailable) || !strings.Contains(err.Error(), "configured model provider") {
		t.Fatalf("configuration failure = %v", err)
	}
}

func TestHostedTurnRunnerKeepsPublisherCollidingPromptsBindingExact(t *testing.T) {
	references := []string{
		"skill:research@1.0.0#binding:alice@1",
		"skill:research@1.0.0#binding:bob@1",
	}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model", OutputSummary: "done",
		SkillSelections: []HostedSkillSelection{
			{SkillRef: references[0], Disposition: HostedSkillApplied, Summary: "Applied the first exact prompt"},
			{SkillRef: references[1], Disposition: HostedSkillNotApplied, Summary: "Did not need the second exact prompt"},
		},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
		SkillPrompts: []HostedSkillPrompt{
			{SkillID: "research", Version: "1.0.0", BindingID: "alice", BindingRevision: 1, Instructions: "Research with the first selected installation."},
			{SkillID: "research", Version: "1.0.0", BindingID: "bob", BindingRevision: 1, Instructions: "Research with the second selected installation."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Research"}, Turn: &AgentTurn{ID: "turn"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(host.request.SkillPrompts) != 2 || host.request.SkillPrompts[0].Reference != references[0] || host.request.SkillPrompts[1].Reference != references[1] {
		t.Fatalf("exact prompt references = %#v", host.request.SkillPrompts)
	}
	encoded, _ := json.Marshal(host.request.SkillPrompts)
	if strings.Contains(string(encoded), "clawhub::") || strings.Contains(string(encoded), "sourceIdentity") {
		t.Fatalf("hosted prompt leaked source provenance: %s", encoded)
	}
	if _, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
		SkillPrompts: []HostedSkillPrompt{
			{SkillID: "research", Version: "1.0.0", Instructions: "First."},
			{SkillID: "research", Version: "1.0.0", Instructions: "Second."},
		},
	}); err == nil {
		t.Fatal("duplicate unqualified prompt references were accepted")
	}
}

func TestHostedTurnRunnerRejectsUnsafeDependencyProjection(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model", OutputSummary: "done",
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Resume", Output: map[string]interface{}{
			"dependencyGroups": map[string]interface{}{"group": map[string]interface{}{"result": map[string]interface{}{"apiToken": "resolved-secret"}}},
		}},
		Turn: &AgentTurn{ID: "turn"},
	})
	if !errors.Is(err, ErrUnsafeSharedContext) || host.request.InvocationID != "" {
		t.Fatalf("error=%v host request=%#v", err, host.request)
	}
}

func TestHostedTurnRunnerRejectsUnauthorizedActionProposal(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model",
		ProposedAction: &TurnAction{Capability: "shell.exec", Summary: "run arbitrary command"},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(context.Background(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Work"}, Turn: &AgentTurn{ID: "turn"},
	})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostedTurnRunnerRequiresRunningActionProposal(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model",
		ProposedAction: &TurnAction{Type: "skill_action", Capability: "release.deploy", Summary: "Deploy", InputRef: "/inputs/deploy"},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
		Actions: []capability.ModelAction{{Name: "release.deploy", SkillID: "release", Version: "1", Action: "deploy"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Deploy"}, Turn: &AgentTurn{ID: "turn"},
	})
	if err == nil {
		t.Fatal("terminal action proposal lifecycle was accepted")
	}
}

func TestHostedTurnRunnerReusesSucceededSemanticActionAcrossRestart(t *testing.T) {
	action := capability.ModelAction{
		Name: "kubernetes.get_resource", BindingID: "cluster-binding", BindingRevision: 3,
		SkillID: "kubernetes", Version: "1", Action: "get_resource",
		SideEffect: capability.SideEffectRead,
	}
	arguments := map[string]interface{}{"kind": "Deployment", "name": "api", "namespace": "system"}
	call := &ActionCall{
		ID: "completed-call", DeploymentID: "sre-agent", BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		SkillID: action.SkillID, SkillVersion: action.Version, Action: action.Action, Arguments: arguments,
		Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"generation": 7},
	}
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	checkpoint := appendActionHistory(map[string]interface{}{"phase": "inspect"}, call)
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn-after-restart", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Read it again",
		ProposedAction: &TurnAction{
			Type: "skill_action", Capability: action.Name, BindingID: action.BindingID, BindingRevision: action.BindingRevision,
			Summary: "Repeat the read", IdempotencyKey: "a-new-model-key", InputRef: "/actionInputs/read",
		},
		ContinuationCheckpoint: map[string]interface{}{
			"actionInputs":             map[string]interface{}{"read": arguments},
			actionHistoryCheckpointKey: []interface{}{map[string]interface{}{"actionCallId": "invented"}},
		},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "sre-agent", DefinitionID: "sre", DefinitionVersion: "1", Actions: []capability.ModelAction{action},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "scheduled-run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Investigate", Checkpoint: checkpoint},
		Turn: &AgentTurn{ID: "turn-after-restart"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ProposedActions) != 0 || outcome.NextRunStatus != AgentRunStatusRunning || outcome.OutputSummary != "A matching succeeded action was reused; continue from its durable evidence." {
		t.Fatalf("duplicate action was not suppressed: %#v", outcome)
	}
	last := outcome.ContinuationCheckpoint["lastAction"].(map[string]interface{})
	if last["actionCallId"] != "completed-call" || actionHistoryEntries(outcome.ContinuationCheckpoint)[0]["actionCallId"] != "completed-call" {
		t.Fatalf("authoritative evidence was not restored: %#v", outcome.ContinuationCheckpoint)
	}
}

func TestHostedTurnRunnerPreservesAndExplainsProposalRecovery(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "repair-turn", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Take the missing observation",
		ContinuationCheckpoint: map[string]interface{}{},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery := map[string]interface{}{
		"attempt": 1, "capability": "skill-browser.camoufox-commit", "error": "target requires a current observation",
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "post", Checkpoint: map[string]interface{}{
			proposalRecoveryCheckpointKey: recovery,
		}},
		Turn: &AgentTurn{ID: "repair-turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.request.SystemInstructions) == 0 ||
		!strings.Contains(host.request.SystemInstructions[len(host.request.SystemInstructions)-1], proposalRecoveryCheckpointKey) {
		t.Fatalf("proposal recovery instruction = %#v", host.request.SystemInstructions)
	}
	if !reflect.DeepEqual(outcome.ContinuationCheckpoint[proposalRecoveryCheckpointKey], recovery) {
		t.Fatalf("proposal recovery checkpoint = %#v", outcome.ContinuationCheckpoint)
	}
}

func TestHostedTurnRunnerRefreshesObservationAfterInterveningAction(t *testing.T) {
	action := capability.ModelAction{
		Name: "browser.snapshot", BindingID: "browser-binding", BindingRevision: 4,
		SkillID: "skill-browser", Version: "1.1.3", Action: "browser-snapshot", SideEffect: capability.SideEffectRead,
	}
	arguments := map[string]interface{}{"sessionId": "reddit-e2e-daily"}
	observation := &ActionCall{
		ID: "snapshot-before-fill", DeploymentID: "browser-agent", BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		SkillID: action.SkillID, SkillVersion: action.Version, Action: action.Action, Arguments: arguments,
		Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"generation": 10},
	}
	observation.SemanticDigest = ComputeActionSemanticDigest(observation)
	checkpoint := appendActionHistory(nil, observation)
	checkpoint = appendActionHistory(checkpoint, &ActionCall{
		ID: "username-fill", DeploymentID: "browser-agent", BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		SkillID: action.SkillID, SkillVersion: action.Version, Action: "browser-fill-secret",
		Arguments: map[string]interface{}{"sessionId": "reddit-e2e-daily", "credentialField": "username"}, Status: ActionCallStatusSucceeded,
	})
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "refresh-turn", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Refresh the stale snapshot",
		ProposedAction: &TurnAction{
			Type: "skill_action", Capability: action.Name, BindingID: action.BindingID, BindingRevision: action.BindingRevision,
			Summary: "Take a fresh snapshot", InputRef: "/actionInputs/snapshot",
		},
		ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{"snapshot": arguments}},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "browser-agent", DefinitionID: "browser", DefinitionVersion: "1", Actions: []capability.ModelAction{action},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "scheduled-run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Comment once", Checkpoint: checkpoint},
		Turn: &AgentTurn{ID: "refresh-turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ProposedActions) != 1 || outcome.ProposedActions[0].Capability != action.Name {
		t.Fatalf("stale observation was incorrectly reused: %#v", outcome)
	}
}

func TestHostedTurnRunnerBoundsRegeneratedNoProgressInteraction(t *testing.T) {
	action := capability.ModelAction{
		Name: "browser.click", BindingID: "browser-binding", BindingRevision: 4,
		SkillID: "skill-browser", Version: "2", Action: "camoufox-click", SideEffect: capability.SideEffectWrite,
		SemanticArguments: map[string]string{"intent": "intent"},
	}
	priorArguments := map[string]interface{}{"sessionId": "run-1", "target": "s1:e26", "intent": "Open community rules", "idempotencyKey": "first-key"}
	prior := &ActionCall{ID: "no-progress", DeploymentID: "browser-agent", BindingID: action.BindingID, BindingRevision: action.BindingRevision, SkillID: action.SkillID, SkillVersion: action.Version, Action: action.Action, Arguments: priorArguments, Status: ActionCallStatusSucceeded}
	intent := computeActionProgressIntentDigest(prior, action.SemanticArguments)
	prior.Output = map[string]interface{}{"progress": map[string]interface{}{"changed": false, "intentDigest": intent, "beforeDigest": "sha256:same", "afterDigest": "sha256:same"}}
	checkpoint := appendActionHistory(nil, prior)
	checkpoint = appendActionHistory(checkpoint, &ActionCall{ID: "fresh-snapshot", Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"observationDigest": "sha256:same"}})
	regenerated := map[string]interface{}{"sessionId": "run-1", "target": "s2:e47", "intent": "Open community rules", "idempotencyKey": "second-key"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn-2", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Try again",
		ProposedAction:         &TurnAction{Type: "skill_action", Capability: action.Name, BindingID: action.BindingID, BindingRevision: action.BindingRevision, Summary: "Open rules", IdempotencyKey: "second-key", InputRef: "/actionInputs/click"},
		ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{"click": regenerated}},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "browser-agent", DefinitionID: "browser", DefinitionVersion: "1", Actions: []capability.ModelAction{action}})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run-1", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Inspect rules", Checkpoint: checkpoint}, Turn: &AgentTurn{ID: "turn-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ProposedActions) != 0 || !strings.Contains(outcome.OutputSummary, "authoritative observation unchanged") {
		t.Fatalf("no-progress action was not bounded: %#v", outcome)
	}
}

func TestHostedTurnRunnerEnforcesExternalOperationPolicy(t *testing.T) {
	tests := []struct {
		name       string
		sideEffect capability.SideEffect
		policy     capability.ExternalOperationPolicy
		external   *ExternalOperationIdentity
		wantErr    string
	}{
		{name: "omitted non-external", sideEffect: capability.SideEffectRead, external: &ExternalOperationIdentity{Resource: "browser-session:one", Operation: "login"}, wantErr: "forbids an external operation identity"},
		{name: "omitted external", sideEffect: capability.SideEffectExternal, external: &ExternalOperationIdentity{Resource: "https://forum.example/topics/42", Operation: "comment:create"}},
		{name: "forbidden", sideEffect: capability.SideEffectExternal, policy: capability.ExternalOperationForbidden, external: &ExternalOperationIdentity{Resource: "browser-session:one", Operation: "login"}, wantErr: "forbids an external operation identity"},
		{name: "required", sideEffect: capability.SideEffectExternal, policy: capability.ExternalOperationRequired, wantErr: "requires an external operation identity"},
		{name: "required present", sideEffect: capability.SideEffectExternal, policy: capability.ExternalOperationRequired, external: &ExternalOperationIdentity{Resource: "https://forum.example/topics/42", Operation: "comment:create"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action := capability.ModelAction{
				Name: "browser.action", BindingID: "browser-binding", BindingRevision: 1, SkillID: "skill-browser", Version: "1.0.0",
				Action: "browser-action", SideEffect: test.sideEffect, ExternalOperationPolicy: test.policy,
			}
			host := &recordingTurnHost{response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "policy-turn", ModelProvider: "test", Model: "test-model",
				NextRunStatus: AgentRunStatusRunning, OutputSummary: "Act", ProposedAction: &TurnAction{
					Type: "skill_action", Capability: action.Name, Summary: "Act", InputRef: "/actionInputs/call", ExternalOperation: test.external,
				}, ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{"call": map[string]interface{}{}}},
			}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1", Actions: []capability.ModelAction{action}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "act"}, Turn: &AgentTurn{ID: "policy-turn"}})
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error=%v want %q", err, test.wantErr)
			}
		})
	}
}

func TestHostedTurnRunnerSuppressesSucceededSideEffectAfterLaterObservation(t *testing.T) {
	action := capability.ModelAction{
		Name: "browser.submit", BindingID: "browser-binding", BindingRevision: 4,
		SkillID: "skill-browser", Version: "1.1.3", Action: "browser-click", SideEffect: capability.SideEffectExternal,
	}
	arguments := map[string]interface{}{"sessionId": "reddit-e2e-daily", "target": "s12:e7", "intent": "Submit one comment"}
	submission := &ActionCall{
		ID: "submitted-comment", DeploymentID: "browser-agent", BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		SkillID: action.SkillID, SkillVersion: action.Version, Action: action.Action, Arguments: arguments,
		Status: ActionCallStatusSucceeded, Output: map[string]interface{}{"success": true},
	}
	submission.SemanticDigest = ComputeActionSemanticDigest(submission)
	checkpoint := appendActionHistory(nil, submission)
	checkpoint = appendActionHistory(checkpoint, &ActionCall{
		ID: "verification-snapshot", DeploymentID: "browser-agent", BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		SkillID: action.SkillID, SkillVersion: action.Version, Action: "browser-snapshot",
		Arguments: map[string]interface{}{"sessionId": "reddit-e2e-daily"}, Status: ActionCallStatusSucceeded,
	})
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "duplicate-turn", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Submit the comment again",
		ProposedAction: &TurnAction{
			Type: "skill_action", Capability: action.Name, BindingID: action.BindingID, BindingRevision: action.BindingRevision,
			Summary: "Submit one comment", InputRef: "/actionInputs/submit",
		},
		ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{"submit": arguments}},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "browser-agent", DefinitionID: "browser", DefinitionVersion: "1", Actions: []capability.ModelAction{action},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "scheduled-run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Comment once", Checkpoint: checkpoint},
		Turn: &AgentTurn{ID: "duplicate-turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.ProposedActions) != 0 || outcome.ContinuationCheckpoint["lastAction"].(map[string]interface{})["actionCallId"] != "submitted-comment" {
		t.Fatalf("succeeded side effect was not suppressed: %#v", outcome)
	}
}

func TestHostedTurnRunnerCarriesOneDurableWorkProposal(t *testing.T) {
	tests := []struct {
		name       string
		response   *HostedTurnResponse
		assertions func(*testing.T, *TurnOutcome)
	}{
		{
			name: "delegation",
			response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model", OutputSummary: "Delegate analysis",
				ProposedDelegation: &TurnDelegationProposal{
					StepID: "analyze-findings", AssignedAgentID: "analyst", Goal: "Analyze the collected evidence",
					Context: map[string]interface{}{"projectId": "research-1"}, Checkpoint: map[string]interface{}{},
					Budget: &BudgetPolicy{MaxTurns: 4},
				},
			},
			assertions: func(t *testing.T, outcome *TurnOutcome) {
				if outcome.ProposedDelegation == nil || outcome.ProposedDelegation.AssignedAgentID != "analyst" || outcome.ProposedFork != nil {
					t.Fatalf("outcome = %#v", outcome)
				}
			},
		},
		{
			name: "fork",
			response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model", OutputSummary: "Split research",
				ProposedFork: &TurnForkProposal{
					ForkID: "compare-products", Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
					Branches: []RunForkBranch{
						{ID: "product-a", AssignedAgentID: "researcher-a", Goal: "Analyze product A", Context: map[string]interface{}{"projectId": "research-1"}, Checkpoint: map[string]interface{}{}},
						{ID: "product-b", AssignedAgentID: "researcher-b", Goal: "Analyze product B", Context: map[string]interface{}{"projectId": "research-1"}, Checkpoint: map[string]interface{}{}},
					},
				},
			},
			assertions: func(t *testing.T, outcome *TurnOutcome) {
				if outcome.ProposedFork == nil || len(outcome.ProposedFork.Branches) != 2 || outcome.ProposedDelegation != nil {
					t.Fatalf("outcome = %#v", outcome)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := &recordingTurnHost{response: test.response}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
				AgentID: "lead", DefinitionID: "lead-definition", DefinitionVersion: "1",
				EligibleAgents: hostedTestAgentTargets("analyst", "researcher-a", "researcher-b"),
			})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
				Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Coordinate research"}, Turn: &AgentTurn{ID: "turn"},
			})
			if err != nil {
				t.Fatal(err)
			}
			test.assertions(t, outcome)
		})
	}
}

func TestHostedTurnRunnerResolvesUniqueAgentDisplayNameToCanonicalID(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Delegate verification",
		ProposedDelegation: &TurnDelegationProposal{
			StepID: "verify", AssignedAgentID: "Agent 74", Goal: "Verify the result",
			Context: map[string]interface{}{}, Checkpoint: map[string]interface{}{},
		},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "lead", DefinitionID: "lead", DefinitionVersion: "1",
		EligibleAgents: []HostedAgentTarget{
			{ID: "74", DisplayName: "Agent 74", Purpose: "Verify bounded work"},
			{ID: "researcher", DisplayName: "Researcher", Purpose: "Research evidence"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Coordinate"},
		Turn: &AgentTurn{ID: "turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ProposedDelegation == nil || outcome.ProposedDelegation.AssignedAgentID != "74" ||
		len(host.request.EligibleAgents) != 2 || host.request.EligibleAgents[0].DisplayName != "Agent 74" {
		t.Fatalf("request=%#v outcome=%#v", host.request, outcome)
	}
	if _, duplicated := host.request.InputContext["eligibleAgents"]; duplicated {
		t.Fatalf("eligible Agents were duplicated into inputContext: %#v", host.request.InputContext)
	}
	modelInput, err := MarshalHostedTurnModelInput(host.request)
	if err != nil || !strings.Contains(string(modelInput), `"eligibleAgents":[{"id":"74","displayName":"Agent 74"`) {
		t.Fatalf("model input = %s, %v", modelInput, err)
	}
}

func TestHostedTurnRunnerRejectsAmbiguousOrUnknownAgentName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reference string
		want      []string
	}{
		{name: "ambiguous", reference: "Reviewer", want: []string{"ambiguous", "reviewer-primary", "reviewer-backup"}},
		{name: "unknown", reference: "Publisher", want: []string{"not eligible", "reviewer-primary", "reviewer-backup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &recordingTurnHost{response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model",
				ProposedDelegation: &TurnDelegationProposal{
					StepID: "review", AssignedAgentID: tc.reference, Goal: "Review",
					Context: map[string]interface{}{}, Checkpoint: map[string]interface{}{},
				},
			}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
				AgentID: "lead", DefinitionID: "lead", DefinitionVersion: "1",
				EligibleAgents: []HostedAgentTarget{
					{ID: "reviewer-primary", DisplayName: "Reviewer"},
					{ID: "reviewer-backup", DisplayName: "Reviewer"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
				Run:  &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Coordinate"},
				Turn: &AgentTurn{ID: "turn"},
			})
			if err == nil {
				t.Fatal("invalid Agent name was accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestHostedTurnRunnerRejectsInvalidWorkProposals(t *testing.T) {
	validFork := &TurnForkProposal{
		ForkID: "compare", Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches: []RunForkBranch{
			{ID: "a", AssignedAgentID: "agent-a", Goal: "Analyze A", Context: map[string]interface{}{}, Checkpoint: map[string]interface{}{}},
			{ID: "b", AssignedAgentID: "agent-b", Goal: "Analyze B", Context: map[string]interface{}{}, Checkpoint: map[string]interface{}{}},
		},
	}
	validDelegation := &TurnDelegationProposal{StepID: "review", AssignedAgentID: "reviewer", Goal: "Review", Context: map[string]interface{}{}, Checkpoint: map[string]interface{}{}}
	for name, mutate := range map[string]func(*HostedTurnResponse){
		"terminal fork": func(response *HostedTurnResponse) {
			response.NextRunStatus, response.ProposedFork = AgentRunStatusCompleted, validFork
		},
		"fork and delegation": func(response *HostedTurnResponse) {
			response.ProposedFork, response.ProposedDelegation = validFork, validDelegation
		},
		"action and fork": func(response *HostedTurnResponse) {
			response.ProposedFork = validFork
			response.ProposedAction = &TurnAction{Type: "skill_action", Capability: "research.read", Summary: "Read", InputRef: "/actionInputs/read"}
		},
		"unsafe branch context": func(response *HostedTurnResponse) {
			fork := *validFork
			fork.Branches = append([]RunForkBranch(nil), validFork.Branches...)
			fork.Branches[0].Context = map[string]interface{}{"apiToken": "secret"}
			response.ProposedFork = &fork
		},
		"malformed delegate id": func(response *HostedTurnResponse) {
			delegation := *validDelegation
			delegation.AssignedAgentID = "../../agent"
			response.ProposedDelegation = &delegation
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model",
			}
			mutate(response)
			host := &recordingTurnHost{response: response}
			actions := []capability.ModelAction(nil)
			if response.ProposedAction != nil {
				actions = []capability.ModelAction{{Name: "research.read", SkillID: "research", Version: "1", Action: "read"}}
			}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
				AgentID: "lead", DefinitionID: "lead-definition", DefinitionVersion: "1", Actions: actions,
				EligibleAgents: hostedTestAgentTargets("agent-a", "agent-b", "reviewer"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Coordinate"}, Turn: &AgentTurn{ID: "turn"}}); err == nil {
				t.Fatal("invalid hosted work proposal was accepted")
			}
		})
	}
}

func TestHostedTurnRunnerRejectsChildBudgetsBelowPortableFloor(t *testing.T) {
	parentBudget := &BudgetPolicy{
		MaxAttempts: 10, MaxTurns: 10, MaxInputTokens: 100000, MaxOutputTokens: 30000,
		MaxTotalTokens: 130000, MaxCostMicros: 100000, MaxDurationMS: 900000, MaxActions: 10,
	}
	minimum := hostedMinimumChildBudget(*parentBudget)
	valid := minimum
	tests := []struct {
		name     string
		response *HostedTurnResponse
		want     string
	}{
		{
			name: "delegation input",
			response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model",
				ProposedDelegation: &TurnDelegationProposal{
					StepID: "analyze", AssignedAgentID: "analyst", Goal: "Analyze evidence", Checkpoint: map[string]interface{}{},
					Budget: func() *BudgetPolicy {
						value := valid
						value.MaxInputTokens = HostedTurnMinimumChildInputTokens - 1
						return &value
					}(),
				},
			},
			want: "maxInputTokens",
		},
		{
			name: "fork output",
			response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model",
				ProposedFork: &TurnForkProposal{
					ForkID: "research", Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
					Branches: []RunForkBranch{
						{ID: "a", AssignedAgentID: "a", Goal: "Analyze A", Checkpoint: map[string]interface{}{}, Budget: &valid},
						{ID: "b", AssignedAgentID: "b", Goal: "Analyze B", Checkpoint: map[string]interface{}{}, Budget: func() *BudgetPolicy {
							value := valid
							value.MaxOutputTokens = HostedTurnMinimumChildOutputTokens - 1
							return &value
						}()},
					},
				},
			},
			want: "maxOutputTokens",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := &recordingTurnHost{response: test.response}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
				AgentID: "lead", DefinitionID: "lead", DefinitionVersion: "1",
				EligibleAgents: hostedTestAgentTargets("analyst", "a", "b"),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
				Run: &AgentRun{
					ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Coordinate",
					Budget: parentBudget,
				},
				Turn: &AgentTurn{ID: "turn"},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "hosted minimum") {
				t.Fatalf("under-floor proposal error = %v", err)
			}
		})
	}
}

func TestHostedTurnRunnerRejectsChildBudgetsBeyondRemainingCapacity(t *testing.T) {
	parentBudget := &BudgetPolicy{
		MaxAttempts: 10, MaxTurns: 10, MaxInputTokens: 100000, MaxOutputTokens: 30000,
		MaxTotalTokens: 130000, MaxCostMicros: 100000, MaxDurationMS: 900000, MaxActions: 10,
	}
	minimum := hostedMinimumChildBudget(*parentBudget)
	tests := []struct {
		name     string
		response *HostedTurnResponse
		want     string
	}{
		{
			name: "delegation exceeds remaining actions",
			response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model",
				ProposedDelegation: &TurnDelegationProposal{
					StepID: "analyze", AssignedAgentID: "analyst", Goal: "Analyze evidence", Checkpoint: map[string]interface{}{},
					Budget: &minimum,
				},
			},
			want: "maxActions",
		},
		{
			name: "fork aggregate exceeds remaining input",
			response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
				ModelProvider: "test", Model: "test-model",
				ProposedFork: &TurnForkProposal{
					ForkID: "research", Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
					Branches: []RunForkBranch{
						{ID: "a", AssignedAgentID: "a", Goal: "Analyze A", Checkpoint: map[string]interface{}{}, Budget: &minimum},
						{ID: "b", AssignedAgentID: "b", Goal: "Analyze B", Checkpoint: map[string]interface{}{}, Budget: &minimum},
					},
				},
			},
			want: "maxInputTokens",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := &recordingTurnHost{response: test.response}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
				AgentID: "lead", DefinitionID: "lead", DefinitionVersion: "1",
				EligibleAgents: hostedTestAgentTargets("analyst", "a", "b"),
			})
			if err != nil {
				t.Fatal(err)
			}
			run := &AgentRun{
				ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Coordinate",
				Budget: parentBudget,
			}
			switch test.name {
			case "delegation exceeds remaining actions":
				run.BudgetUsage.Actions = parentBudget.MaxActions
			case "fork aggregate exceeds remaining input":
				run.BudgetUsage.InputTokens = parentBudget.MaxInputTokens - HostedTurnMinimumChildInputTokens*2 + 1
			}
			_, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "remaining capacity") {
				t.Fatalf("over-capacity proposal error = %v", err)
			}
		})
	}
}

func TestHostedTurnRunnerLeavesUnboundedChildDimensionsUnconstrained(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model",
		ProposedDelegation: &TurnDelegationProposal{
			StepID: "analyze", AssignedAgentID: "analyst", Goal: "Analyze evidence", Checkpoint: map[string]interface{}{},
			Budget: &BudgetPolicy{MaxTurns: HostedTurnMinimumChildTurns},
		},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "lead", DefinitionID: "lead", DefinitionVersion: "1",
		EligibleAgents: hostedTestAgentTargets("analyst"),
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{
		Run: &AgentRun{
			ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}, Goal: "Coordinate",
			Budget: &BudgetPolicy{MaxTurns: 10},
		},
		Turn: &AgentTurn{ID: "turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ProposedDelegation == nil || host.request.Budget.MinimumChild.MaxTurns != HostedTurnMinimumChildTurns ||
		host.request.Budget.MinimumChild.MaxInputTokens != 0 || host.request.Budget.MinimumChild.MaxActions != 0 {
		t.Fatalf("unbounded child projection = request %#v outcome %#v", host.request.Budget, outcome)
	}
}

func TestHostedTurnRunnerRejectsUnauthorizedSkillEvidence(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn-1", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model",
		SkillSelections: []HostedSkillSelection{{SkillRef: "skill:summarize@1.0.0", Disposition: HostedSkillNotApplied, Summary: "Summarization was not needed"}},
		Decisions:       []TurnDecision{{Summary: "Applied an unbound Skill", EvidenceRefs: []string{"skill:unbound@1.0.0"}}},
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "agent-1", DefinitionID: "definition-1", DefinitionVersion: "1",
		SkillPrompts: []HostedSkillPrompt{{SkillID: "summarize", Version: "1.0.0", Instructions: "Summarize faithfully."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run-1", Scope: Scope{Kind: "tenant", ID: "7"}, Goal: "Summarize evidence"},
		Turn: &AgentTurn{ID: "turn-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "unauthorized Skill") {
		t.Fatalf("expected unauthorized Skill evidence rejection, got %v", err)
	}
}

func TestHostedTurnRunnerRequiresExhaustiveSkillSelections(t *testing.T) {
	for name, selections := range map[string][]HostedSkillSelection{
		"missing": nil,
		"duplicate": {
			{SkillRef: "skill:summarize@1.0.0", Disposition: HostedSkillApplied, Summary: "Used summarization"},
			{SkillRef: "skill:summarize@1.0.0", Disposition: HostedSkillNotApplied, Summary: "Duplicate"},
		},
		"unauthorized": {{SkillRef: "skill:imagegen@1.0.0", Disposition: HostedSkillNotApplied, Summary: "Not relevant"}},
	} {
		t.Run(name, func(t *testing.T) {
			host := &recordingTurnHost{response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn-1", NextRunStatus: AgentRunStatusCompleted, SkillSelections: selections,
				ModelProvider: "test", Model: "test-model",
			}}
			runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
				AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1",
				SkillPrompts: []HostedSkillPrompt{{SkillID: "summarize", Version: "1.0.0", Instructions: "Summarize faithfully."}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.RunTurn(t.Context(), TurnExecutionContext{
				Run: &AgentRun{ID: "run-1", Scope: Scope{Kind: "tenant", ID: "7"}, Goal: "Summarize"}, Turn: &AgentTurn{ID: "turn-1"},
			})
			if err == nil {
				t.Fatal("expected invalid Skill selection contract to be rejected")
			}
		})
	}
}

func TestHostedTurnRunnerRequiresActualModelIdentity(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(t.Context(), TurnExecutionContext{Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Work"}, Turn: &AgentTurn{ID: "turn"}})
	if err == nil || !strings.Contains(err.Error(), "actual model provider and model") {
		t.Fatalf("error=%v", err)
	}
}

func TestHostedTurnRunnerRejectsMismatchedReplayEnvelope(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: "another-turn"}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.RunTurn(context.Background(), TurnExecutionContext{
		Run: &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "1"}, Goal: "Work"}, Turn: &AgentTurn{ID: "turn"},
	})
	if err == nil || !strings.Contains(err.Error(), "mismatched") {
		t.Fatalf("error = %v", err)
	}
}
