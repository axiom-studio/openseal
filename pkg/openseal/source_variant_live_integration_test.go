//go:build integration

package openseal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
	sdkresolver "github.com/axiom-studio/skills.sdk/resolver"
	_ "github.com/lib/pq"
)

// TestLiveSourceQualifiedSkillCollisionE2E proves the publisher-collision case
// against a real PostgreSQL kernel store and configured model provider. The
// public ClawHub registry currently keeps slugs globally unique, so this test
// uses retained OpenClaw source bundles to exercise the protocol case without
// adding a test-only product API or inserting kernel rows directly.
func TestLiveSourceQualifiedSkillCollisionE2E(t *testing.T) {
	if os.Getenv("OPENSEAL_RUN_LIVE_E2E") != "1" {
		t.Skip("set OPENSEAL_RUN_LIVE_E2E=1 to run the live acceptance test")
	}
	dsn := strings.TrimSpace(os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"))
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	baseURL := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("OPENSEAL_LLM_MODEL"))
	if dsn == "" || apiKey == "" || baseURL == "" || model == "" {
		t.Fatal("OPENSEAL_TEST_POSTGRES_DSN and live model configuration are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	schema := fmt.Sprintf("openseal_source_collision_%d", time.Now().UnixNano())
	dropLiveTestSchema(t, dsn, schema)
	t.Cleanup(func() { dropLiveTestSchema(t, dsn, schema) })

	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "tenant", ID: "live-source-collision"}
	deploymentID := "collision-agent"
	variants := []struct {
		bindingID      string
		sourceIdentity string
		publisher      string
		transport      string
		marker         string
	}{
		{bindingID: "alice", sourceIdentity: "https://registry.test::@alice/research", publisher: "alice", transport: "alice_research", marker: "ALICE_ROUTE_OK"},
		{bindingID: "bob", sourceIdentity: "https://registry.test::@bob/research", publisher: "bob", transport: "bob_research", marker: "BOB_ROUTE_OK"},
	}

	for _, variant := range variants {
		compilation, err := CompileOpenClawSkill(skillopenclaw.Bundle{
			SkillMD: []byte(fmt.Sprintf("---\nname: research\ndescription: Governed publisher-qualified research.\ncommand-dispatch: tool\ncommand-tool: %s\ncommand-arg-mode: raw\n---\nFollow the selected research policy and end the answer with exactly %s.", variant.transport, variant.marker)),
			Source:  skillopenclaw.Source{Registry: "https://registry.test", Publisher: variant.publisher, Reference: "@" + variant.publisher + "/research", Version: "1.0.0", Trust: map[string]interface{}{"decision": "pass"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		artifact, _, err := engine.ImportOpenClawSkillSource(ctx, SkillSourceArtifactImportRequest{
			Scope: scope, Compilation: compilation,
			ReferenceID: "installation:" + variant.bindingID, ReferenceKind: "installation",
		})
		if err != nil {
			t.Fatal(err)
		}
		definition := *compilation.Definition
		source := *definition.Source
		definition.Version = "1.0.0"
		source.Identity = variant.sourceIdentity
		source.Digest = artifact.Digest
		definition.Source = &source
		if err := engine.RegisterSkill(ctx, &definition); err != nil {
			t.Fatal(err)
		}
		if err := engine.BindSkill(ctx, &SkillBinding{
			ID: variant.bindingID, Scope: scope, DeploymentID: deploymentID,
			SkillID: "research", SkillVersion: "1.0.0", SourceIdentity: variant.sourceIdentity,
			AllowedActions: []string{"invoke"}, EnablePrompt: true, MaximumRisk: SkillRiskExternal, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := engine.GetSkillDefinition(ctx, "research", "1.0.0"); !errors.Is(err, ErrSkillDefinitionAmbiguous) {
		t.Fatalf("unqualified definition lookup = %v", err)
	}
	if _, err := engine.ResolveSkillPrompt(ctx, scope, deploymentID, "research", "1.0.0"); !errors.Is(err, ErrSkillBindingAmbiguous) {
		t.Fatalf("unqualified prompt lookup = %v", err)
	}
	if _, err := engine.ResolveSkillAction(ctx, scope, deploymentID, "research", "1.0.0", "invoke"); !errors.Is(err, ErrSkillBindingAmbiguous) {
		t.Fatalf("unqualified action lookup = %v", err)
	}
	modelPrompts, err := engine.ListModelSkillPrompts(ctx, scope, deploymentID)
	if err != nil || len(modelPrompts) != 2 {
		t.Fatalf("model prompt catalog = %#v, %v", modelPrompts, err)
	}
	modelActions, err := engine.ListModelSkillActions(ctx, scope, deploymentID)
	if err != nil || len(modelActions) != 2 {
		t.Fatalf("model action catalog = %#v, %v", modelActions, err)
	}
	assertSourceIdentitiesAbsent(t, variants, modelPrompts, modelActions)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := New(WithPersistentStore(reopened))
	if err != nil {
		t.Fatal(err)
	}

	dispatched := make(map[string]int)
	var dispatchedMu sync.Mutex
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(_ context.Context, invocation ToolInvocation) (map[string]interface{}, error) {
		dispatchedMu.Lock()
		dispatched[invocation.Name]++
		dispatchedMu.Unlock()
		return map[string]interface{}{"transport": invocation.Name, "accepted": true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	for _, variant := range variants {
		definition, err := restarted.GetSkillDefinitionVariant(ctx, "research", "1.0.0", variant.sourceIdentity)
		if err != nil || definition == nil || definition.Source == nil || definition.Source.Identity != variant.sourceIdentity {
			t.Fatalf("restored exact definition for %s = %#v, %v", variant.bindingID, definition, err)
		}
		if _, err := restarted.ExportOpenClawSkillSourceForReference(ctx, scope, definition.Source.Digest, "installation:"+variant.bindingID); err != nil {
			t.Fatalf("restored source artifact for %s: %v", variant.bindingID, err)
		}
		selection := SkillBindingReference{ID: variant.bindingID, Revision: 1}
		prompt, err := restarted.ResolveExactSkillPrompt(ctx, scope, deploymentID, "research", "1.0.0", selection)
		if err != nil || prompt == nil || !strings.Contains(prompt.Instructions, variant.marker) {
			t.Fatalf("restored exact prompt for %s = %#v, %v", variant.bindingID, prompt, err)
		}
		bound, err := restarted.ResolveExactSkillAction(ctx, scope, deploymentID, "research", "1.0.0", "invoke", selection)
		if err != nil {
			t.Fatal(err)
		}
		output, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Bound: bound, Arguments: map[string]interface{}{"command": "research collision-safe routing"}})
		if err != nil || output["transport"] != variant.transport {
			t.Fatalf("exact transport for %s = %#v, %v", variant.bindingID, output, err)
		}

		response := executeLiveSourceVariantPrompt(t, ctx, apiKey, baseURL, model, prompt.Instructions, variant.marker)
		runScope := Scope{Kind: scope.Kind, ID: scope.ID}
		run, err := restarted.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: runScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}, AssignedAgentID: deploymentID,
			Goal: "Execute one exact publisher-qualified research Skill", Source: RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := restarted.ClaimNextAgentRun(ctx, AgentRunClaimRequest{Scope: runScope, WorkerID: "live-collision-worker", AssignedAgentID: deploymentID, LeaseDuration: time.Minute})
		if err != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("claim exact source run = %#v, %v", claimed, err)
		}
		advanced, err := restarted.AdvanceAgentRun(ctx, AdvanceAgentRunRequest{Scope: runScope, RunID: run.ID, WorkerID: "live-collision-worker", Model: model}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Exact source-qualified Skill completed", RunOutput: map[string]interface{}{"response": response, "route": variant.bindingID}}, nil
		}))
		if err != nil || advanced.Run.Status != AgentRunStatusCompleted {
			t.Fatalf("complete exact source run = %#v, %v", advanced, err)
		}
		activity, err := restarted.ListActivity(ctx, ActivityFilter{Scope: runScope, RunID: run.ID})
		if err != nil || len(activity) == 0 {
			t.Fatalf("exact source activity = %#v, %v", activity, err)
		}
		assertLiveE2ESecretAbsent(t, apiKey, advanced.Run, advanced.Turn, activity)
		assertSourceIdentitiesAbsent(t, variants, advanced.Run, advanced.Turn, activity)
	}
	for _, variant := range variants {
		if dispatched[variant.transport] != 1 {
			t.Fatalf("transport %s dispatch count = %d", variant.transport, dispatched[variant.transport])
		}
	}
	t.Log("live source-qualified Skill collision accepted across PostgreSQL restart, exact transport dispatch, two model calls, durable Runs, and activity")
}

func executeLiveSourceVariantPrompt(t *testing.T, ctx context.Context, apiKey, baseURL, model, instructions, marker string) string {
	t.Helper()
	step := &executor.StepDefinition{Id: "live-source-variant", Type: executor.NodeTypeAI, Config: map[string]interface{}{
		"provider": "openai-compatible", "model": model, "apiKey": apiKey, "baseUrl": baseURL,
		"systemPrompt": instructions,
		"prompt":       "In one short sentence confirm that the exact governed research policy was selected. Preserve its required route marker.",
		"temperature":  float64(0), "maxTokens": float64(120),
	}}
	result, err := executor.NewAIExecutor().Execute(ctx, step, executor.NewResolver(sdkresolver.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	response, _ := result.Output["response"].(string)
	if !strings.Contains(response, marker) {
		t.Fatalf("model response did not preserve exact route marker %s (response length %d)", marker, len(response))
	}
	return response
}

func assertSourceIdentitiesAbsent(t *testing.T, variants []struct {
	bindingID      string
	sourceIdentity string
	publisher      string
	transport      string
	marker         string
}, values ...interface{}) {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		if bytes.Contains(encoded, []byte(variant.sourceIdentity)) {
			t.Fatalf("source identity leaked into model-visible or run state for binding %s", variant.bindingID)
		}
	}
}

func dropLiveTestSchema(t *testing.T, dsn, schema string) {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL cleanup connection: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
		t.Fatalf("drop live test schema: %v", err)
	}
}
