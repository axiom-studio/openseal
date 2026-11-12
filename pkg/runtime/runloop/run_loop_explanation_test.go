package runloop

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

const explanationWorker = "explain-worker"

type explanationProducer struct{ answer string }

func (explanationProducer) Kind() models.StepProducerKind { return models.StepProducerDeterministic }

func (p explanationProducer) Produce(context.Context, agentrun.StepInput) (agentrun.StepOutput, error) {
	return agentrun.StepOutput{Final: &agentrun.LoopFinal{Answer: p.answer}}, nil
}

type explanationFinal struct {
	result agentrun.FinalDeliveryResult
	calls  int
}

func (f *explanationFinal) Deliver(context.Context, agentrun.FinalDelivery) (agentrun.FinalDeliveryResult, error) {
	f.calls++
	return f.result, nil
}

func explanationStores(t *testing.T, test func(*testing.T, models.KernelStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.New(filepath.Join(t.TempDir(), "run-loop-explanation.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		test(t, store)
	})
}

// driveExplanation claims a Run that is in failure-explanation mode and
// drives its explanation step with final as the delivery result.
func driveExplanation(t *testing.T, store models.KernelStore, result agentrun.FinalDeliveryResult) (Exit, *models.AgentRun, *explanationFinal) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "explain"}
	now := time.Now().UTC().Truncate(time.Millisecond)
	portfolio := portfolio.NewService(store, models.WithClock(func() time.Time { return now }))
	created, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a", Goal: "work", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: explanationWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	failure := &models.RunFailureExplanation{Kind: conversation.RunFailureKindExecution, Code: "execution_failed", Message: "The report could not be built."}
	explaining, _, err := activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, claimed.ID, activity.RunTransitionRequest{
		Status: models.AgentRunStatusRunning, ExpectedRevision: claimed.Revision, LeaseOwner: explanationWorker,
		Actor: models.ActivityActor{Type: "worker", ID: explanationWorker}, FailureExplanation: failure,
		Summary: "The agent will explain what went wrong",
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := New(store, nil, nil, Config{
		PlanToolEntries: func(*agentrun.TurnRunnerBinding, *models.AgentSession, *models.AgentTurn, *models.SessionEntry, []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
			return nil, nil
		},
		PlainTextFinal: func(context.Context, agentrun.ControlContext, string) (control.Result, error) {
			return control.Result{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	final := &explanationFinal{result: result}
	exit := loop.Drive(ctx, Claim{Run: explaining, WorkerID: explanationWorker, Binding: &agentrun.TurnRunnerBinding{
		Producer: explanationProducer{answer: "I could not build the report."}, Final: final,
	}})
	stored, err := store.GetAgentRun(ctx, scope, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	return exit, stored, final
}

func TestRunLoopPostedExplanationEndsFailedExplained(t *testing.T) {
	explanationStores(t, func(t *testing.T, store models.KernelStore) {
		exit, run, final := driveExplanation(t, store, agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryFail, Output: map[string]interface{}{
			"summary": "I could not build the report.", "conversationId": "c1", "triggerMessageId": "m1", "messageId": "m2", "replayed": false,
		}})
		if exit.Err != nil || exit.Reason != ExitFinal || final.calls != 1 {
			t.Fatalf("exit = %#v, deliveries = %d; want final", exit, final.calls)
		}
		if run.Status != models.AgentRunStatusFailed || run.FailureExplanation == nil || !run.FailureExplanation.Explained || run.Error != "The report could not be built." {
			t.Fatalf("run = status %q error %q explanation %#v; want failed and explained", run.Status, run.Error, run.FailureExplanation)
		}
		if run.Output["messageId"] != "m2" || run.Output["summary"] != "I could not build the report." {
			t.Fatalf("output = %#v; want the posted explanation", run.Output)
		}
	})
}

func TestRunLoopUnpostedExplanationEndsFailedUnexplained(t *testing.T) {
	explanationStores(t, func(t *testing.T, store models.KernelStore) {
		exit, run, final := driveExplanation(t, store, agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryFail, Failure: &agentrun.LoopFailure{
			Kind: conversation.RunFailureKindExecution, Code: runFailureCodeFinalDelivery, Message: "The agent's reply could not be posted.",
		}})
		if exit.Err != nil || exit.Reason != ExitFailed || final.calls != 1 {
			t.Fatalf("exit = %#v, deliveries = %d; want failed", exit, final.calls)
		}
		if run.Status != models.AgentRunStatusFailed || run.FailureExplanation == nil || run.FailureExplanation.Explained {
			t.Fatalf("run = status %q explanation %#v; want failed and unexplained", run.Status, run.FailureExplanation)
		}
		if run.Output["messageId"] != nil {
			t.Fatalf("output = %#v; want no posted message", run.Output)
		}
	})
}
