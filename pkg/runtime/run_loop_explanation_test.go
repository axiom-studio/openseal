package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

const explanationWorker = "explain-worker"

type explanationProducer struct{ answer string }

func (explanationProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (p explanationProducer) Produce(context.Context, StepInput) (StepOutput, error) {
	return StepOutput{Final: &LoopFinal{Answer: p.answer}}, nil
}

type explanationFinal struct {
	result FinalDeliveryResult
	calls  int
}

func (f *explanationFinal) Deliver(context.Context, FinalDelivery) (FinalDeliveryResult, error) {
	f.calls++
	return f.result, nil
}

func explanationStores(t *testing.T, test func(*testing.T, KernelStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "run-loop-explanation.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		test(t, store)
	})
}

// driveExplanation claims a Run that is in failure-explanation mode and
// drives its explanation step with final as the delivery result.
func driveExplanation(t *testing.T, store KernelStore, result FinalDeliveryResult) (RunLoopExit, *AgentRun, *explanationFinal) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "explain"}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a", Goal: "work", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: explanationWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	failure := &RunFailureExplanation{Kind: RunFailureKindExecution, Code: "execution_failed", Message: "The report could not be built."}
	explaining, _, err := NewRunActivityService(store, store).TransitionRun(ctx, scope, claimed.ID, RunTransitionRequest{
		Status: AgentRunStatusRunning, ExpectedRevision: claimed.Revision, LeaseOwner: explanationWorker,
		Actor: ActivityActor{Type: "worker", ID: explanationWorker}, FailureExplanation: failure,
		Summary: "The agent will explain what went wrong",
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewRunLoop(store, nil, nil, RunLoopConfig{
		PlanToolEntries: func(*TurnRunnerBinding, *AgentSession, *AgentTurn, *SessionEntry, []LoopToolCall) ([]*SessionEntry, error) {
			return nil, nil
		},
		PlainTextFinal: func(context.Context, ControlContext, string) (ControlResult, error) { return ControlResult{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	final := &explanationFinal{result: result}
	exit := loop.Drive(ctx, RunLoopClaim{Run: explaining, WorkerID: explanationWorker, Binding: &TurnRunnerBinding{
		Producer: explanationProducer{answer: "I could not build the report."}, Final: final,
	}})
	stored, err := store.GetAgentRun(ctx, scope, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	return exit, stored, final
}

func TestRunLoopPostedExplanationEndsFailedExplained(t *testing.T) {
	explanationStores(t, func(t *testing.T, store KernelStore) {
		exit, run, final := driveExplanation(t, store, FinalDeliveryResult{Kind: FinalDeliveryFail, Output: map[string]interface{}{
			"summary": "I could not build the report.", "conversationId": "c1", "triggerMessageId": "m1", "messageId": "m2", "replayed": false,
		}})
		if exit.Err != nil || exit.Reason != RunLoopExitFinal || final.calls != 1 {
			t.Fatalf("exit = %#v, deliveries = %d; want final", exit, final.calls)
		}
		if run.Status != AgentRunStatusFailed || run.FailureExplanation == nil || !run.FailureExplanation.Explained || run.Error != "The report could not be built." {
			t.Fatalf("run = status %q error %q explanation %#v; want failed and explained", run.Status, run.Error, run.FailureExplanation)
		}
		if run.Output["messageId"] != "m2" || run.Output["summary"] != "I could not build the report." {
			t.Fatalf("output = %#v; want the posted explanation", run.Output)
		}
	})
}

func TestRunLoopUnpostedExplanationEndsFailedUnexplained(t *testing.T) {
	explanationStores(t, func(t *testing.T, store KernelStore) {
		exit, run, final := driveExplanation(t, store, FinalDeliveryResult{Kind: FinalDeliveryFail, Failure: &LoopFailure{
			Kind: RunFailureKindExecution, Code: runFailureCodeFinalDelivery, Message: "The agent's reply could not be posted.",
		}})
		if exit.Err != nil || exit.Reason != RunLoopExitFailed || final.calls != 1 {
			t.Fatalf("exit = %#v, deliveries = %d; want failed", exit, final.calls)
		}
		if run.Status != AgentRunStatusFailed || run.FailureExplanation == nil || run.FailureExplanation.Explained {
			t.Fatalf("run = status %q explanation %#v; want failed and unexplained", run.Status, run.FailureExplanation)
		}
		if run.Output["messageId"] != nil {
			t.Fatalf("output = %#v; want no posted message", run.Output)
		}
	})
}
