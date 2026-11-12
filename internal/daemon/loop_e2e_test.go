package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

// desktopLoop is the daemon's task wiring over a real kernel: SQLite under
// t.TempDir(), the desktop resolver, the provider step host against an
// httptest Chat Completions endpoint and the generated-files publisher.
type desktopLoop struct {
	kernel   *opensealkernel.Engine
	store    *sqlite.Store
	provider *fakeProvider
}

const loopAgent = "analyst"

func newDesktopLoop(t *testing.T, provider *fakeProvider, wrap func(models.ArtifactContentStore) models.ArtifactContentStore) *desktopLoop {
	t.Helper()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var content models.ArtifactContentStore
	local, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content = local
	if wrap != nil {
		content = wrap(local)
	}
	resolver := &DesktopTurnResolver{
		Scope: localScope, Host: provider.host(t, 0),
		Publisher: &TaskArtifactPublisher{Scope: localScope, Content: content, Catalog: artifactcatalog.NewArtifactCatalog(store)},
	}
	scopes := workqueue.WorkerScopeSourceFunc(func(context.Context) ([]models.Scope, error) { return []models.Scope{localScope}, nil })
	kernel, err := opensealkernel.New(
		opensealkernel.WithPersistentStore(store),
		opensealkernel.WithDynamicAgentRunWorkers(runworker.DynamicAgentRunWorkerConfig{
			Kind: models.RunKindAgentWork, Concurrency: 1, WorkerIDPrefix: "desktop-test",
			PollInterval: 10 * time.Millisecond, ReconcileInterval: 10 * time.Millisecond,
		}, scopes, resolver),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kernel.Close() })
	if err := resolver.Bind(kernel, content, nil); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	scope := capability.ScopeReference{Kind: localScope.Kind, ID: localScope.ID}
	definition, err := kernel.RegisterAgentDefinition(ctx, &opensealkernel.AgentDefinition{
		ID: loopAgent, Version: "1", DisplayName: "Analyst", Purpose: "Write reports", SystemPrompt: "Write the reports the user asks for.",
		Authority: opensealkernel.AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := kernel.CreateAgentDeployment(ctx, &opensealkernel.AgentDeployment{
		ID: loopAgent, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: opensealkernel.AgentRolloutActive, Environment: "local", Capacity: opensealkernel.AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "local-operator", "desktop loop test"); err != nil {
		t.Fatal(err)
	}
	if err := kernel.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kernel.Stop)
	return &desktopLoop{kernel: kernel, store: store, provider: provider}
}

// start creates the task the way the daemon's run creation dispatcher does.
func (l *desktopLoop) start(t *testing.T, goal string) *models.AgentRun {
	t.Helper()
	request, err := ResolveDesktopWork(t.Context(), l.kernel, localScope, models.CreateAgentRunRequest{
		Scope: localScope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: loopAgent},
		AssignedAgentID: loopAgent, Goal: goal, IdempotencyKey: "task-" + goal,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := l.kernel.CreateAgentRunCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	l.kernel.WakeAgentWorkers()
	return result.Run
}

func (l *desktopLoop) wait(t *testing.T, runID string, want models.AgentRunStatus) *models.AgentRun {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		run, err := l.kernel.GetAgentRun(t.Context(), localScope, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == want {
			return run
		}
		switch run.Status {
		case models.AgentRunStatusCompleted, models.AgentRunStatusFailed, models.AgentRunStatusCanceled, models.AgentRunStatusPaused:
			t.Fatalf("run %s is %s (%s), want %s", runID, run.Status, run.Error, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stayed %s, want %s", runID, run.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (l *desktopLoop) steps(t *testing.T, runID string) []*models.AgentTurn {
	t.Helper()
	steps, err := l.kernel.ListAgentTurns(t.Context(), models.AgentTurnFilter{Scope: localScope, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func (l *desktopLoop) artifacts(t *testing.T, runID string) []*models.Artifact {
	t.Helper()
	artifacts, err := l.kernel.ListArtifacts(t.Context(), models.ArtifactFilter{Scope: localScope, ProducerRunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

// finalCall answers with turn_final_answer carrying one generated file.
func finalCall(id string) map[string]interface{} {
	args, _ := json.Marshal(map[string]interface{}{
		"answer":         "The report is attached.",
		"generatedFiles": []interface{}{map[string]string{"name": "report.md", "mediaType": "text/markdown", "text": "# Report\n\nAll good."}},
	})
	return chatCall(id, string(models.ControlToolFinalAnswer), string(args))
}

// offersGeneratedFiles reports whether the request's turn_final_answer tool
// carries the publisher's generatedFiles property.
func offersGeneratedFiles(body map[string]interface{}) bool {
	tools, _ := body["tools"].([]interface{})
	for _, raw := range tools {
		tool, _ := raw.(map[string]interface{})
		function, _ := tool["function"].(map[string]interface{})
		if function["name"] != string(models.ControlToolFinalAnswer) {
			continue
		}
		parameters, _ := function["parameters"].(map[string]interface{})
		properties, _ := parameters["properties"].(map[string]interface{})
		_, ok := properties[generatedFilesProperty]
		return ok
	}
	return false
}

func toolMessages(body map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	messages, _ := body["messages"].([]interface{})
	for _, raw := range messages {
		if message, ok := raw.(map[string]interface{}); ok && message["role"] == "tool" {
			out = append(out, message)
		}
	}
	return out
}

func TestDesktopTaskLoopRunsStepsAndPublishesGeneratedFiles(t *testing.T) {
	const narration = "Checking the forecast first."
	var unknownToolResult atomic.Value
	provider := newFakeProvider(t, func(call int, w http.ResponseWriter, body map[string]interface{}) {
		if !offersGeneratedFiles(body) {
			t.Errorf("step %d: turn_final_answer does not offer generatedFiles", call)
		}
		switch call {
		case 1:
			chatReply(w, "tool_calls", map[string]interface{}{"role": "assistant", "content": narration,
				"tool_calls": []interface{}{chatCall("call-1", "lookup_weather", `{"city":"Lisbon"}`)}}, map[string]int{"prompt_tokens": 900, "completion_tokens": 20})
		case 2:
			if results := toolMessages(body); len(results) == 1 {
				content, _ := results[0]["content"].(string)
				unknownToolResult.Store(content)
			}
			chatReply(w, "tool_calls", map[string]interface{}{"role": "assistant", "content": "",
				"tool_calls": []interface{}{finalCall("call-2")}}, map[string]int{"prompt_tokens": 1000, "completion_tokens": 40})
		default:
			t.Errorf("unexpected provider call %d", call)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	loop := newDesktopLoop(t, provider, nil)
	run := loop.wait(t, loop.start(t, "Write the weather report").ID, models.AgentRunStatusCompleted)

	if got := provider.n.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if text, _ := unknownToolResult.Load().(string); !strings.Contains(strings.ToLower(text), "lookup_weather") {
		t.Fatalf("the unknown tool's error result = %q", text)
	}
	steps := loop.steps(t, run.ID)
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	if steps[0].OutputSummary != narration {
		t.Fatalf("step 1 narration = %q", steps[0].OutputSummary)
	}
	refs, _ := run.Output["artifactRefs"].([]interface{})
	if len(refs) != 1 {
		t.Fatalf("run output artifactRefs = %#v", run.Output)
	}
	artifacts := loop.artifacts(t, run.ID)
	if len(artifacts) != 1 || artifacts[0].Name != "report.md" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	if ref, _ := refs[0].(map[string]interface{}); ref["id"] != artifacts[0].ID {
		t.Fatalf("artifact ref %#v does not name %s", refs[0], artifacts[0].ID)
	}
	if artifacts[0].Provenance.TurnID != steps[1].ID || artifacts[0].Provenance.RunID != run.ID {
		t.Fatalf("artifact provenance = %#v, final step %s", artifacts[0].Provenance, steps[1].ID)
	}
}

// failFirstPut refuses the first content write.
type failFirstPut struct {
	models.ArtifactContentStore
	mu    sync.Mutex
	puts  int
	fails int
}

func (s *failFirstPut) Put(ctx context.Context, write models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	s.mu.Lock()
	s.puts++
	fail := s.puts == 1
	if fail {
		s.fails++
	}
	s.mu.Unlock()
	if fail {
		return models.ArtifactStoredContent{}, errors.New("simulated disk interruption")
	}
	return s.ArtifactContentStore.Put(ctx, write)
}

func TestDesktopTaskLoopResumesPublicationWithoutModelReinvocation(t *testing.T) {
	provider := newFakeProvider(t, func(call int, w http.ResponseWriter, body map[string]interface{}) {
		if call != 1 {
			t.Errorf("the model was called again (call %d)", call)
		}
		chatReply(w, "tool_calls", map[string]interface{}{"role": "assistant", "content": "",
			"tool_calls": []interface{}{finalCall("call-1")}}, map[string]int{"prompt_tokens": 800, "completion_tokens": 30})
	})
	content := &failFirstPut{}
	loop := newDesktopLoop(t, provider, func(inner models.ArtifactContentStore) models.ArtifactContentStore {
		content.ArtifactContentStore = inner
		return content
	})
	created := loop.start(t, "Write the paused report")
	paused := loop.wait(t, created.ID, models.AgentRunStatusPaused)
	if len(loop.artifacts(t, created.ID)) != 0 {
		t.Fatal("a failed publication registered an artifact")
	}
	if _, err := loop.kernel.CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{
		Scope: localScope, RunID: paused.ID, ExpectedRevision: paused.Revision, Kind: agentrun.AgentRunCommandResume,
		Actor: models.ActivityActor{Type: "user", ID: "local-operator"},
	}); err != nil {
		t.Fatal(err)
	}
	loop.kernel.WakeAgentWorkers()
	run := loop.wait(t, created.ID, models.AgentRunStatusCompleted)

	if got := provider.n.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	if content.fails != 1 {
		t.Fatalf("content failures = %d, want 1", content.fails)
	}
	artifacts := loop.artifacts(t, run.ID)
	refs, _ := run.Output["artifactRefs"].([]interface{})
	if len(artifacts) != 1 || len(refs) != 1 {
		t.Fatalf("artifacts = %d, refs = %#v", len(artifacts), run.Output["artifactRefs"])
	}
	modelSteps := 0
	for _, step := range loop.steps(t, run.ID) {
		if step.Model != "" {
			modelSteps++
		}
		if step.ID == artifacts[0].Provenance.TurnID && step.Model == "" {
			t.Fatalf("artifact provenance names step %s, which made no model call", step.ID)
		}
	}
	if modelSteps != 1 {
		t.Fatalf("model steps = %d, want 1", modelSteps)
	}
}

func TestDesktopTaskLoopCanceledStepPublishesNothing(t *testing.T) {
	called := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	provider := newFakeProvider(t, func(call int, w http.ResponseWriter, body map[string]interface{}) {
		once.Do(func() { close(called) })
		<-release
		chatReply(w, "tool_calls", map[string]interface{}{"role": "assistant", "content": "",
			"tool_calls": []interface{}{finalCall("call-1")}}, map[string]int{"prompt_tokens": 800, "completion_tokens": 30})
	})
	loop := newDesktopLoop(t, provider, nil)
	created := loop.start(t, "Write the canceled report")
	select {
	case <-called:
	case <-time.After(20 * time.Second):
		t.Fatal("the step never reached the provider")
	}
	current, err := loop.kernel.GetAgentRun(t.Context(), localScope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.kernel.CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{
		Scope: localScope, RunID: current.ID, ExpectedRevision: current.Revision, Kind: agentrun.AgentRunCommandCancel,
		Actor: models.ActivityActor{Type: "user", ID: "local-operator"}, Summary: "Stop",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	run := loop.wait(t, created.ID, models.AgentRunStatusCanceled)
	// Stopping waits for the worker, so its step has settled however it ended.
	loop.kernel.Stop()

	if refs, ok := run.Output["artifactRefs"]; ok {
		t.Fatalf("canceled run output carries artifactRefs %#v", refs)
	}
	if artifacts := loop.artifacts(t, run.ID); len(artifacts) != 0 {
		t.Fatalf("canceled step published %d artifacts", len(artifacts))
	}
	final, err := loop.kernel.GetAgentRun(t.Context(), localScope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != models.AgentRunStatusCanceled {
		t.Fatalf("run status after the step settled = %s", final.Status)
	}
	if got := provider.n.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}
