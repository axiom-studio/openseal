//go:build integration

package openseal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	sdkresolver "github.com/axiom-studio/skills.sdk/resolver"
)

// TestLiveClawHubSkillModelE2E exercises the production registry, verifier,
// installer, compiler, binding catalog, restart restore, and configured model.
// It is opt-in because it performs real network and model calls.
func TestLiveClawHubSkillModelE2E(t *testing.T) {
	if os.Getenv("OPENSEAL_RUN_LIVE_E2E") != "1" {
		t.Skip("set OPENSEAL_RUN_LIVE_E2E=1 to run the live acceptance test")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	baseURL := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_MODEL"))
	if apiKey == "" || baseURL == "" || model == "" {
		t.Fatal("OPENAI_API_KEY, OPENSEAL_LLM_BASE_URL, and OPENSEAL_LLM_MODEL are required")
	}

	skillRef := strings.TrimSpace(os.Getenv("OPENSEAL_E2E_CLAWHUB_SKILL"))
	if skillRef == "" {
		skillRef = "yuyonghao-summarize"
	}
	reference, err := clawhub.ParseSkillReference(skillRef)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workspace := t.TempDir()
	skillsDirectory := filepath.Join(workspace, "skills")
	databasePath := filepath.Join(workspace, "kernel.db")
	store, err := runtime.NewSQLiteStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	storeOpen := true
	t.Cleanup(func() {
		if storeOpen {
			_ = store.Close()
		}
	})
	registry := clawhub.NewClawHubClient("")
	engine, err := New(WithStore(store), WithClawHubRegistry("https://clawhub.ai", registry, skillsDirectory))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := engine.InstallClawHubSkill(ctx, ClawHubInstallRequest{Reference: reference})
	if err != nil {
		t.Fatal(err)
	}
	if installed.Verification == nil || !installed.Verification.OK || installed.Verification.Decision != "pass" {
		t.Fatalf("registry verification did not pass: %#v", installed.Verification)
	}
	definition := installed.Compilation.Definition
	if definition == nil || definition.Prompt == nil || strings.TrimSpace(definition.Prompt.Instructions) == "" {
		t.Fatalf("real skill did not compile a prompt capability: %#v", definition)
	}

	scope := SkillScope{Kind: "agent", ID: "live-clawhub-e2e"}
	if err := engine.BindSkill(ctx, &SkillBinding{
		ID: "summarizer", Scope: scope, DeploymentID: "live-e2e",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnablePrompt: true, MaximumRisk: SkillRiskRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	prompts, err := engine.ListModelSkillPrompts(ctx, scope, "live-e2e")
	if err != nil || len(prompts) != 1 {
		t.Fatalf("bound model prompt = %#v, %v", prompts, err)
	}
	resolved, err := engine.ResolveSkillPrompt(ctx, scope, "live-e2e", definition.ID, definition.Version)
	if err != nil || resolved == nil {
		t.Fatalf("resolve compiled prompt = %#v, %v", resolved, err)
	}

	runScope := Scope{Kind: scope.Kind, ID: scope.ID}
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: runScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "live-clawhub-agent"},
		AssignedAgentID: "live-clawhub-agent", Goal: "Execute the bound real ClawHub summarizer", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.ClaimNextAgentRun(ctx, AgentRunClaimRequest{
		Scope: runScope, WorkerID: "live-clawhub-worker", AssignedAgentID: "live-clawhub-agent", LeaseDuration: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim durable agent run = %#v, %v", claimed, err)
	}
	var response string
	advanced, err := engine.AdvanceAgentRun(ctx, AdvanceAgentRunRequest{
		Scope: runScope, RunID: run.ID, WorkerID: "live-clawhub-worker", Model: model,
	}, TurnRunnerFunc(func(turnContext context.Context, _ TurnExecutionContext) (*TurnOutcome, error) {
		step := &executor.StepDefinition{Id: "live-clawhub-model", Type: executor.NodeTypeAI, Config: map[string]interface{}{
			"provider": "openai-compatible", "model": model, "apiKey": apiKey, "baseUrl": baseURL,
			"systemPrompt": resolved.Instructions,
			"prompt":       "Summarize this sentence in one sentence and preserve the marker OPENSEAL_E2E_OK: OpenSeal installed, verified, compiled, bound, and executed a real ClawHub skill.",
			"temperature":  float64(0), "maxTokens": float64(160),
		}}
		result, executeErr := executor.NewAIExecutor().Execute(turnContext, step, executor.NewResolver(sdkresolver.Config{}))
		if executeErr != nil {
			return nil, executeErr
		}
		response, _ = result.Output["response"].(string)
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Real ClawHub skill completed",
			Decisions: []TurnDecision{{Summary: "Use verified bound ClawHub prompt", EvidenceRefs: []string{"skill:" + definition.ID + "@" + definition.Version}}},
			RunOutput: map[string]interface{}{"response": response, "skillDigest": definition.Source.Digest},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Run.Status != AgentRunStatusCompleted || advanced.Turn.Status != AgentTurnStatusCompleted {
		t.Fatalf("durable agent turn did not complete: %#v", advanced)
	}
	if !strings.Contains(response, "OPENSEAL_E2E_OK") {
		t.Fatalf("model did not preserve acceptance marker (response length %d)", len(response))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	storeOpen = false

	reopened, err := runtime.NewSQLiteStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := New(WithStore(reopened), WithClawHubRegistry("https://clawhub.ai", registry, skillsDirectory))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.GetSkillDefinition(ctx, definition.ID, definition.Version)
	if err != nil || restored == nil || restored.Source == nil || restored.Source.Digest != definition.Source.Digest {
		t.Fatalf("restart restore lost installed definition provenance: %#v, %v", restored, err)
	}
	restoredRun, err := restarted.GetAgentRun(ctx, runScope, run.ID)
	if err != nil || restoredRun.Status != AgentRunStatusCompleted || restoredRun.Output["skillDigest"] != definition.Source.Digest {
		t.Fatalf("restart restore lost durable run completion: %#v, %v", restoredRun, err)
	}
	activity, err := restarted.ListActivity(ctx, ActivityFilter{Scope: runScope, RunID: run.ID})
	if err != nil || len(activity) == 0 || activity[len(activity)-1].TurnID != advanced.Turn.ID {
		t.Fatalf("restart restore lost durable activity: %#v, %v", activity, err)
	}
	t.Logf("live ClawHub skill accepted: skill=%s version=%s digest=%s response_bytes=%d", definition.ID, definition.Version, definition.Source.Digest, len(response))
}
