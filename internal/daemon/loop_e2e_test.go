package daemon

import (
	"context"
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
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

var localScope = models.Scope{Kind: "local", ID: "default"}

// desktopLoop is the daemon's task wiring over a real kernel: SQLite under
// t.TempDir(), the desktop resolver and the provider step host against an
// httptest Chat Completions endpoint.
type desktopLoop struct {
	kernel   *opensealkernel.Engine
	store    *sqlite.Store
	provider *fakeProvider
}

const loopAgent = "analyst"

func newDesktopLoop(t *testing.T, provider *fakeProvider, wrap func(models.ArtifactContentStore) models.ArtifactContentStore) *desktopLoop {
	t.Helper()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "kernel.db"))
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
	resolver := &DesktopTurnResolver{Scope: localScope, Host: provider.host(t, 0)}
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

// finalReply is the model's plain final reply.
func finalReply(w http.ResponseWriter, usage map[string]int) {
	chatReply(w, "stop", map[string]interface{}{"role": "assistant", "content": "The report is ready."}, usage)
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

func TestDesktopTaskLoopRunsStepsToItsFinalReply(t *testing.T) {
	const narration = "Checking the forecast first."
	var unknownToolResult atomic.Value
	provider := newFakeProvider(t, func(call int, w http.ResponseWriter, body map[string]interface{}) {
		switch call {
		case 1:
			chatReply(w, "tool_calls", map[string]interface{}{"role": "assistant", "content": narration,
				"tool_calls": []interface{}{chatCall("call-1", "lookup_weather", `{"city":"Lisbon"}`)}}, map[string]int{"prompt_tokens": 900, "completion_tokens": 20})
		case 2:
			if results := toolMessages(body); len(results) == 1 {
				content, _ := results[0]["content"].(string)
				unknownToolResult.Store(content)
			}
			finalReply(w, map[string]int{"prompt_tokens": 1000, "completion_tokens": 40})
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
	if run.Output["summary"] != "The report is ready." {
		t.Fatalf("run output = %#v", run.Output)
	}
}

func TestDesktopTaskLoopCanceledStepEndsCanceled(t *testing.T) {
	called := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	provider := newFakeProvider(t, func(call int, w http.ResponseWriter, body map[string]interface{}) {
		once.Do(func() { close(called) })
		<-release
		finalReply(w, map[string]int{"prompt_tokens": 800, "completion_tokens": 30})
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
	if _, err := loop.kernel.CommandAgentRun(t.Context(), agentrun.CommandRequest{
		Scope: localScope, RunID: current.ID, ExpectedRevision: current.Revision, Kind: agentrun.CommandCancel,
		Actor: models.ActivityActor{Type: "user", ID: "local-operator"}, Summary: "Stop",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	run := loop.wait(t, created.ID, models.AgentRunStatusCanceled)
	// Stopping waits for the worker, so its step has settled however it ended.
	loop.kernel.Stop()

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
