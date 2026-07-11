package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

type countedWorkforceGenerator struct {
	payload []byte
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

func (g *countedWorkforceGenerator) Generate(ctx context.Context, _ authoring.GenerateRequest) ([]byte, error) {
	g.calls.Add(1)
	if g.started != nil {
		g.once.Do(func() { close(g.started) })
	}
	if g.release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-g.release:
		}
	}
	return g.payload, g.err
}

func TestWorkforceAuthoringPrepareQueuesDurableRunAndConcurrentWorkersGenerateOnce(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generator := testAuthoringGenerator(t)
	generator.started, generator.release = make(chan struct{}), make(chan struct{})
	compiler, _ := authoring.NewCompiler(generator)
	service, err := NewWorkforceAuthoringRunService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	request := testPrepareWorkforceRequest()
	changeSet, run, replay, err := service.Prepare(context.Background(), request)
	if err != nil || replay || changeSet.Status != authoring.ChangeSetEvaluating || run.Status != AgentRunStatusQueued || generator.calls.Load() != 0 {
		t.Fatalf("prepared changeSet=%#v run=%#v replay=%t calls=%d err=%v", changeSet, run, replay, generator.calls.Load(), err)
	}
	if changeSet.Generation.RunID != run.ID || changeSet.Generation.Request.InvocationKey != "workforce-change-set:"+changeSet.ID+":0" {
		t.Fatalf("generation linkage = %#v, run = %s", changeSet.Generation, run.ID)
	}
	replayed, replayRun, wasReplay, err := service.Prepare(context.Background(), request)
	if err != nil || !wasReplay || replayed.ID != changeSet.ID || replayRun.ID != run.ID || generator.calls.Load() != 0 {
		t.Fatalf("replay changeSet=%#v run=%#v replay=%t calls=%d err=%v", replayed, replayRun, wasReplay, generator.calls.Load(), err)
	}

	scope := Scope{Kind: request.Scope.Kind, ID: request.Scope.ID}
	first, _ := NewWorkforceAuthoringWorker(service, nil, WorkforceAuthoringWorkerConfig{Scope: scope, WorkerID: "worker-one", LeaseDuration: time.Minute, GenerationTimeout: 10 * time.Second})
	second, _ := NewWorkforceAuthoringWorker(service, nil, WorkforceAuthoringWorkerConfig{Scope: scope, WorkerID: "worker-two", LeaseDuration: time.Minute, GenerationTimeout: 10 * time.Second})
	result := make(chan error, 1)
	go func() {
		worked, runErr := first.RunOnce(context.Background())
		if !worked && runErr == nil {
			runErr = context.Canceled
		}
		result <- runErr
	}()
	<-generator.started
	if worked, err := second.RunOnce(context.Background()); err != nil || worked {
		t.Fatalf("second worker worked=%t err=%v", worked, err)
	}
	close(generator.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if generator.calls.Load() != 1 {
		t.Fatalf("model calls = %d", generator.calls.Load())
	}
	generated, err := service.changeSets.Get(context.Background(), request.Scope, changeSet.ID)
	if err != nil || generated.Status != authoring.ChangeSetReview || generated.CandidateDigest == "" || generated.Generation.Attempt != 1 {
		t.Fatalf("generated = %#v, err = %v", generated, err)
	}
	finished, err := store.GetAgentRun(context.Background(), scope, run.ID)
	if err != nil || finished.Status != AgentRunStatusCompleted || finished.Output["changeSetId"] != changeSet.ID {
		t.Fatalf("finished run = %#v, err = %v", finished, err)
	}
	events, err := store.ListActivity(context.Background(), ActivityFilter{Scope: scope, RunID: run.ID, Limit: 20})
	if err != nil || len(events) < 3 || events[len(events)-1].EventType != "workforce.generation.completed" {
		t.Fatalf("events = %#v, err = %v", events, err)
	}
}

func TestWorkforceAuthoringStartupRecoversIntentPersistedBeforeEnqueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	generator := testAuthoringGenerator(t)
	compiler, _ := authoring.NewCompiler(generator)
	changeSets, _ := authoring.NewChangeSetService(compiler, store)
	request := testPrepareWorkforceRequest()
	prepared, _, err := changeSets.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if runs, err := store.ListAgentRuns(context.Background(), AgentRunFilter{Scope: Scope{Kind: request.Scope.Kind, ID: request.Scope.ID}}); err != nil || len(runs) != 0 {
		t.Fatalf("runs before crash = %#v, err = %v", runs, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recoveredService, _ := NewWorkforceAuthoringRunService(compiler, reopened)
	scope := Scope{Kind: request.Scope.Kind, ID: request.Scope.ID}
	worker, _ := NewWorkforceAuthoringWorker(recoveredService, nil, WorkforceAuthoringWorkerConfig{Scope: scope, WorkerID: "restart-worker"})
	runs, err := recoveredService.RecoverPending(context.Background(), scope, 100)
	if err != nil || len(runs) != 1 || runs[0].Context["changeSetId"] != prepared.ID {
		t.Fatalf("recovered runs = %#v, err = %v", runs, err)
	}
	if again, err := recoveredService.RecoverPending(context.Background(), scope, 100); err != nil || len(again) != 1 || again[0].ID != runs[0].ID {
		t.Fatalf("idempotent recovery = %#v, err = %v", again, err)
	}
	if worked, err := worker.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("recovered worker worked=%t err=%v", worked, err)
	}
	generated, err := recoveredService.changeSets.Get(context.Background(), request.Scope, prepared.ID)
	if err != nil || generated.Status != authoring.ChangeSetReview || generator.calls.Load() != 1 {
		t.Fatalf("generated = %#v calls=%d err=%v", generated, generator.calls.Load(), err)
	}
}

func TestWorkforceAuthoringTimeoutIsAuditableAndRetryCreatesNewRun(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generator := testAuthoringGenerator(t)
	generator.started, generator.release = make(chan struct{}), make(chan struct{})
	compiler, _ := authoring.NewCompiler(generator)
	service, _ := NewWorkforceAuthoringRunService(compiler, store)
	request := testPrepareWorkforceRequest()
	changeSet, firstRun, _, err := service.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: request.Scope.Kind, ID: request.Scope.ID}
	worker, _ := NewWorkforceAuthoringWorker(service, nil, WorkforceAuthoringWorkerConfig{
		Scope: scope, WorkerID: "timeout-worker", LeaseDuration: time.Minute, GenerationTimeout: 10 * time.Millisecond,
	})
	if worked, err := worker.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("timed out run worked=%t err=%v", worked, err)
	}
	failed, err := service.changeSets.Get(context.Background(), request.Scope, changeSet.ID)
	if err != nil || failed.Status != authoring.ChangeSetFailed || failed.Generation.Attempt != 1 || failed.Generation.LastError == "" {
		t.Fatalf("failed change set = %#v, err = %v", failed, err)
	}
	failedRun, _ := store.GetAgentRun(context.Background(), scope, firstRun.ID)
	if failedRun.Status != AgentRunStatusFailed || failedRun.Error == "" {
		t.Fatalf("failed run = %#v", failedRun)
	}
	close(generator.release)
	retried, retryRun, err := service.Retry(context.Background(), authoring.RetryChangeSetGenerationRequest{
		Scope: request.Scope, ChangeSetID: failed.ID, ExpectedRevision: failed.Revision,
		Reason: "retry after provider timeout", Actor: request.Actor,
	})
	if err != nil || retried.Status != authoring.ChangeSetEvaluating || retryRun.ID == firstRun.ID || retried.Generation.RunID != retryRun.ID {
		t.Fatalf("retried=%#v run=%#v err=%v", retried, retryRun, err)
	}
	if retried.Generation.Request.InvocationKey != "workforce-change-set:"+changeSet.ID+":1" {
		t.Fatalf("retry invocation key = %q", retried.Generation.Request.InvocationKey)
	}
	if worked, err := worker.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("retry worked=%t err=%v", worked, err)
	}
	completed, err := service.changeSets.Get(context.Background(), request.Scope, changeSet.ID)
	if err != nil || completed.Status != authoring.ChangeSetReview || completed.Generation.Attempt != 2 || generator.calls.Load() != 2 {
		t.Fatalf("completed=%#v calls=%d err=%v", completed, generator.calls.Load(), err)
	}
}

func TestWorkforceAuthoringWorkerRequiresLeaseLongerThanGenerationTimeout(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	compiler, _ := authoring.NewCompiler(testAuthoringGenerator(t))
	service, _ := NewWorkforceAuthoringRunService(compiler, store)
	_, err = NewWorkforceAuthoringWorker(service, nil, WorkforceAuthoringWorkerConfig{
		Scope: Scope{Kind: "tenant", ID: "one"}, LeaseDuration: time.Minute, GenerationTimeout: time.Minute,
	})
	if err == nil {
		t.Fatal("worker should reject a lease that can expire during generation")
	}
}

func TestWorkforceAuthoringNeverPersistsProviderSecrets(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const secret = "sk-provider-secret-in-error"
	generator := testAuthoringGenerator(t)
	generator.err = errors.New("provider https://user:" + secret + "@example.invalid failed with Authorization: Bearer " + secret)
	compiler, _ := authoring.NewCompiler(generator)
	service, _ := NewWorkforceAuthoringRunService(compiler, store)
	request := testPrepareWorkforceRequest()
	changeSet, run, _, err := service.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: request.Scope.Kind, ID: request.Scope.ID}
	worker, _ := NewWorkforceAuthoringWorker(service, nil, WorkforceAuthoringWorkerConfig{Scope: scope, WorkerID: "safe-worker"})
	if worked, err := worker.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("provider failure worked=%t err=%v", worked, err)
	}
	failed, _ := service.changeSets.Get(context.Background(), request.Scope, changeSet.ID)
	failedRun, _ := store.GetAgentRun(context.Background(), scope, run.ID)
	events, _ := store.ListActivity(context.Background(), ActivityFilter{Scope: scope, RunID: run.ID, Limit: 20})
	persisted, _ := json.Marshal(struct {
		ChangeSet *authoring.ChangeSet
		Run       *AgentRun
		Events    []*ActivityEvent
	}{failed, failedRun, events})
	if strings.Contains(string(persisted), secret) {
		t.Fatalf("provider secret persisted: %s", persisted)
	}
	if failed.Generation.FailureCode != "provider_failed" || failed.Generation.LastError != "The workforce generation provider failed" || failedRun.Error != failed.Generation.LastError {
		t.Fatalf("public failure changeSet=%#v run=%#v", failed.Generation, failedRun)
	}
}

func testAuthoringGenerator(t *testing.T) *countedWorkforceGenerator {
	t.Helper()
	candidate := testApplicableWorkforceChangeSet().Result.Candidate
	payload, err := json.Marshal(authoring.GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	return &countedWorkforceGenerator{payload: payload}
}

func testPrepareWorkforceRequest() authoring.CreateChangeSetRequest {
	return authoring.CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "authoring"}, Prompt: "Create a durable operations Team",
		Placement: authoring.ChangeSetPlacement{Environment: "test"}, Actor: authoring.ChangeSetActor{Type: "user", ID: "7"},
		IdempotencyKey: "authoring-request-one",
	}
}
