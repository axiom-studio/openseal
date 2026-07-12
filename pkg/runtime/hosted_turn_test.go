package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type recordingTurnHost struct {
	request  HostedTurnRequest
	response *HostedTurnResponse
	err      error
}

func (h *recordingTurnHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.request = request
	return h.response, h.err
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
		ModelProvider:      "openai-compatible", Model: "deepseek-v4-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(context.Background(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run-2", Scope: Scope{Kind: "tenant", ID: "1"}, AssignedAgentID: "agent-3", Goal: "Analyze launch feedback", Context: map[string]interface{}{"source": "https://example.test/evidence"}, Checkpoint: map[string]interface{}{"cursor": "next"}},
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
	if len(host.request.SkillPrompts) != 1 || host.request.SkillPrompts[0].Instructions != "Summarize sources." || host.request.SkillPrompts[0].Reference != "skill:summarize@1.0.0" || outcome.RunOutput["answer"] != "done" || len(outcome.Decisions) != 1 || outcome.Decisions[0].EvidenceRefs[0] != "skill:summarize@1.0.0" || outcome.ModelProvider != "openai-compatible" || outcome.Model != "deepseek-v4-flash" {
		t.Fatalf("request=%#v outcome=%#v", host.request, outcome)
	}
	encoded, _ := json.Marshal(host.request)
	for _, forbidden := range []string{"apiKey", "credential", "secret", "vault"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
			t.Fatalf("host envelope exposes forbidden credential surface %q: %s", forbidden, encoded)
		}
	}
}

func TestHostedTurnRunnerRejectsUnauthorizedActionProposal(t *testing.T) {
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model",
		ProposedActions: []TurnAction{{Capability: "shell.exec", Summary: "run arbitrary command"}},
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

func TestHostedTurnRunnerRequiresOneRunningActionProposal(t *testing.T) {
	for name, actions := range map[string][]TurnAction{
		"terminal": {{Type: "skill_action", Capability: "release.deploy", Summary: "Deploy", InputRef: "/inputs/deploy"}},
		"multiple": {
			{Type: "skill_action", Capability: "release.deploy", Summary: "Deploy", InputRef: "/inputs/deploy"},
			{Type: "skill_action", Capability: "release.deploy", Summary: "Deploy again", InputRef: "/inputs/deploy"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			host := &recordingTurnHost{response: &HostedTurnResponse{
				APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
				ModelProvider: "test", Model: "test-model",
				ProposedActions: actions,
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
				t.Fatal("invalid action proposal lifecycle was accepted")
			}
		})
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
