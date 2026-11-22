package runloop

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

type finalGuardFunc func([]*models.SessionEntry) *agentrun.LoopFailure

func (f finalGuardFunc) RefuseFinal(entries []*models.SessionEntry) *agentrun.LoopFailure {
	return f(entries)
}

// driveGuardedFinal drives a fresh Run whose producer answers at once, with
// guard as the binding's FinalGuard.
func driveGuardedFinal(t *testing.T, store models.KernelStore, guard agentrun.FinalGuard) (Exit, *models.AgentRun, *explanationFinal) {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "guard"}
	now := time.Now().UTC().Truncate(time.Millisecond)
	created, err := portfolio.NewService(store, models.WithClock(func() time.Time { return now })).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a", Goal: "work", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: explanationWorker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
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
	final := &explanationFinal{result: agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryComplete}}
	exit := loop.Drive(ctx, Claim{Run: claimed, WorkerID: explanationWorker, Binding: &agentrun.TurnRunnerBinding{
		Producer: explanationProducer{answer: "The weekly report is ready."}, Final: final, FinalGuard: guard,
	}})
	stored, err := store.GetAgentRun(ctx, scope, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	return exit, stored, final
}

func TestRunLoopFinalGuardRefusalFailsRunWithoutDelivering(t *testing.T) {
	explanationStores(t, func(t *testing.T, store models.KernelStore) {
		asked := 0
		exit, run, final := driveGuardedFinal(t, store, finalGuardFunc(func([]*models.SessionEntry) *agentrun.LoopFailure {
			asked++
			return &agentrun.LoopFailure{Kind: "routine_script", Code: "routine_script_failed", Message: "The routine's script did not run: python3 weekly.py", Explainable: true}
		}))
		if exit.Err != nil || asked != 1 || final.calls != 0 {
			t.Fatalf("exit = %#v, asked = %d, deliveries = %d; want one refusal and no delivery", exit, asked, final.calls)
		}
		if run.Status != models.AgentRunStatusFailed || run.FailureExplanation == nil || run.FailureExplanation.Code != "routine_script_failed" ||
			run.Error != "The routine's script did not run: python3 weekly.py" {
			t.Fatalf("run = status %q error %q explanation %#v; want the guard's failure", run.Status, run.Error, run.FailureExplanation)
		}
	})
}

func TestRunLoopFinalGuardAcceptanceCompletesRun(t *testing.T) {
	explanationStores(t, func(t *testing.T, store models.KernelStore) {
		exit, run, final := driveGuardedFinal(t, store, finalGuardFunc(func([]*models.SessionEntry) *agentrun.LoopFailure { return nil }))
		if exit.Err != nil || final.calls != 1 || run.Status != models.AgentRunStatusCompleted {
			t.Fatalf("exit = %#v, deliveries = %d, status = %q; want completed", exit, final.calls, run.Status)
		}
	})
}
