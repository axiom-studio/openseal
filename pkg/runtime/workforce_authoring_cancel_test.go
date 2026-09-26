package runtime

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkforceAuthoringUserStopCancelsProviderAndDoesNotRequeue(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generator := &cancellationBoundaryWorkforceGenerator{started: make(chan struct{})}
	compiler, _ := authoring.NewCompiler(generator)
	service, _ := NewWorkforceAuthoringRunService(compiler, store)
	request := testPrepareWorkforceRequest()
	request.Actor = authoring.ChangeSetActor{Type: "user", ID: "7"}
	change, run, _, err := service.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorkforceAuthoringWorker(service, nil, WorkforceAuthoringWorkerConfig{Scope: run.Scope, WorkerID: "stop-worker", LeaseDuration: time.Minute, GenerationTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _, err := worker.RunOnce(ctx); result <- err }()
	select {
	case <-generator.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider never started")
	}
	if _, err := service.changeSets.CancelPreparedGeneration(t.Context(), change.Scope, change.ID, change.Revision, request.Actor); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not cancel provider")
	}
	current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || current.Status != AgentRunStatusCanceled || current.LeaseOwner != "" {
		t.Fatalf("run=%+v err=%v", current, err)
	}
	recovered, err := service.RecoverPending(t.Context(), run.Scope, 10)
	if err != nil || len(recovered) != 0 {
		t.Fatalf("stopped work requeued: %v %v", recovered, err)
	}
	saved, err := service.changeSets.Get(t.Context(), change.Scope, change.ID)
	if err != nil || saved.Prompt != change.Prompt || saved.Generation.FailureCode != "canceled" {
		t.Fatalf("draft=%+v err=%v", saved, err)
	}
}
