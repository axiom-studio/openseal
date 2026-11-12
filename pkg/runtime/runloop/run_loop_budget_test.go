package runloop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type budgetPlanningProducer struct {
	plan  models.BudgetUsage
	err   error
	calls int
	seen  agentrun.StepInput
}

func (p *budgetPlanningProducer) Kind() models.StepProducerKind { return models.StepProducerModel }

func (p *budgetPlanningProducer) Produce(context.Context, agentrun.StepInput) (agentrun.StepOutput, error) {
	return agentrun.StepOutput{}, errors.New("not produced in budget tests")
}

func (p *budgetPlanningProducer) PlanStepBudget(_ context.Context, in agentrun.StepInput) (models.BudgetUsage, error) {
	p.calls++
	p.seen = in
	return p.plan, p.err
}

type deterministicBudgetProducer struct{}

func (deterministicBudgetProducer) Kind() models.StepProducerKind {
	return models.StepProducerDeterministic
}

func (deterministicBudgetProducer) Produce(context.Context, agentrun.StepInput) (agentrun.StepOutput, error) {
	return agentrun.StepOutput{}, nil
}

type stepBudgetFixture struct {
	t        *testing.T
	store    *sqlite.Store
	activity *runtimeactivity.RunActivityService
	turns    *runtimeactivity.AgentTurnService
	run      *models.AgentRun
	step     *models.AgentTurn
	now      time.Time
}

func newStepBudgetFixture(t *testing.T, budget *models.BudgetPolicy) *stepBudgetFixture {
	t.Helper()
	ctx := t.Context()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "local", ID: "step-budget-" + strings.ReplaceAll(t.Name(), "/", "-")}
	run, err := portfolio.NewService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "bounded work", Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning, Summary: "started",
		Actor: models.ActivityActor{Type: "worker", ID: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &stepBudgetFixture{
		t: t, store: store, activity: activity, turns: runtimeactivity.NewAgentTurnService(store, store), run: run,
		now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
	}
	f.step = f.begin()
	return f
}

func (f *stepBudgetFixture) begin() *models.AgentTurn {
	f.t.Helper()
	step, err := f.turns.BeginTurn(f.t.Context(), runtimeactivity.BeginAgentTurnRequest{
		Scope: f.run.Scope, RunID: f.run.ID, Producer: models.StepProducerModel, WorkerID: "worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return step
}

func (f *stepBudgetFixture) reserve(binding *agentrun.TurnRunnerBinding) (*models.AgentRun, *models.BudgetAdmission) {
	f.t.Helper()
	run, admission, err := reserveStepBudget(f.t.Context(), f.activity, f.now, f.run, f.step, binding,
		agentrun.StepInput{Binding: binding, Mode: agentrun.StepModeNormal, WorkerID: "worker"}, "worker")
	if err != nil {
		f.t.Fatal(err)
	}
	f.run = run
	return run, admission
}

// apply finishes the step and applies it with transition, filling the budget
// settlement the way applyStep does.
func (f *stepBudgetFixture) apply(finish runtimeactivity.FinishAgentTurnRequest, transition runtimeactivity.RunTransitionRequest, produced bool) *models.AgentRun {
	f.t.Helper()
	ctx := f.t.Context()
	finished, err := f.turns.FinishTurn(ctx, f.run.Scope, f.step.ID, finish)
	if err != nil {
		f.t.Fatal(err)
	}
	f.step = finished
	transition.ExpectedRevision = f.run.Revision
	transition.TurnID, transition.AppliedTurn, transition.CausationID = finished.ID, finished.Sequence, finished.ID
	fillStepBudgetSettlement(&transition, f.run, finished, produced)
	run, _, err := f.activity.TransitionRun(ctx, f.run.Scope, f.run.ID, transition)
	if err != nil {
		f.t.Fatal(err)
	}
	f.run = run
	return run
}

func (f *stepBudgetFixture) eventTypes() []string {
	f.t.Helper()
	events, err := f.store.ListActivity(f.t.Context(), models.ActivityFilter{Scope: f.run.Scope, RunID: f.run.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.EventType)
	}
	return types
}

func TestRunLoopBudgetReservationPerStep(t *testing.T) {
	f := newStepBudgetFixture(t, &models.BudgetPolicy{MaxTurns: 2, MaxInputTokens: 1000})
	producer := &budgetPlanningProducer{plan: models.BudgetUsage{InputTokens: 100, Turns: 5}}
	binding := &agentrun.TurnRunnerBinding{Producer: producer, BudgetReservation: models.BudgetUsage{InputTokens: 20}}

	run, admission := f.reserve(binding)
	if admission != nil {
		t.Fatalf("admission = %#v", admission)
	}
	reservation, ok := run.BudgetReservations[f.step.ID]
	if !ok || reservation.Usage != (models.BudgetUsage{Turns: 1, InputTokens: 120}) || !reservation.CreatedAt.Equal(f.now) {
		t.Fatalf("reservation = %#v (ok=%v)", reservation, ok)
	}
	if producer.calls != 1 || producer.seen.Run == nil || producer.seen.Run == f.run || producer.seen.Step.ID != f.step.ID {
		t.Fatalf("planner input = %#v", producer.seen)
	}

	// Same plan again: no write.
	before := f.run.Revision
	if _, admission := f.reserve(binding); admission != nil || f.run.Revision != before {
		t.Fatalf("unchanged reservation wrote: admission=%#v revision %d -> %d", admission, before, f.run.Revision)
	}

	// A retrying step with a different plan is reconciled in place.
	producer.plan = models.BudgetUsage{InputTokens: 200}
	f.now = f.now.Add(time.Minute)
	run, _ = f.reserve(binding)
	reconciled := run.BudgetReservations[f.step.ID]
	if reconciled.Usage != (models.BudgetUsage{Turns: 1, InputTokens: 220}) || !reconciled.CreatedAt.Equal(reservation.CreatedAt) {
		t.Fatalf("reconciled = %#v", reconciled)
	}

	run = f.apply(runtimeactivity.FinishAgentTurnRequest{
		ExpectedRevision: f.step.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: "worker",
		Usage: models.TurnUsage{InputTokens: 150, OutputTokens: 9, NativeOperations: 1},
	}, runtimeactivity.RunTransitionRequest{Status: models.AgentRunStatusRunning, Summary: "Applied step 1", Actor: models.ActivityActor{Type: "worker", ID: "worker"}}, true)
	if len(run.BudgetReservations) != 0 || run.BudgetUsage.Turns != 1 || run.BudgetUsage.InputTokens != 150 || run.BudgetUsage.Actions != 1 {
		t.Fatalf("settled run budget = %#v reservations=%#v", run.BudgetUsage, run.BudgetReservations)
	}

	// The second step reserves under its own id; MaxTurns 2 still has room.
	f.step = f.begin()
	run, admission = f.reserve(binding)
	if admission != nil || run.BudgetReservations[f.step.ID].Usage.Turns != 1 {
		t.Fatalf("second step: admission=%#v reservations=%#v", admission, run.BudgetReservations)
	}
	types := strings.Join(f.eventTypes(), ",")
	for _, want := range []string{"budget.reserved", "budget.reservation_reconciled"} {
		if !strings.Contains(types, want) {
			t.Fatalf("events %s miss %s", types, want)
		}
	}

	// Applying the second step exhausts MaxTurns: the loop pauses.
	finished, err := f.turns.FinishTurn(t.Context(), f.run.Scope, f.step.ID, runtimeactivity.FinishAgentTurnRequest{
		ExpectedRevision: f.step.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: "worker",
		Usage: models.TurnUsage{InputTokens: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !stepExhaustsBudget(f.run, finished, true) {
		t.Fatal("second step should exhaust MaxTurns 2")
	}
	if stepExhaustsBudget(f.run, finished, false) {
		t.Fatal("a step that ran no producer charges no Turn")
	}
	pause := stepBudgetPause(f.run, "worker", stepBudgetExhaustedAfterStepSummary, nil, "turns")
	pause.TurnID, pause.AppliedTurn, pause.CausationID = finished.ID, finished.Sequence, finished.ID
	fillStepBudgetSettlement(&pause, f.run, finished, true)
	paused, event, err := f.activity.TransitionRun(t.Context(), f.run.Scope, f.run.ID, pause)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != models.AgentRunStatusPaused || paused.BudgetState != models.BudgetStateExhausted || paused.BudgetUsage.Turns != 2 ||
		event.EventType != "budget.exhausted" || len(paused.BudgetReservations) != 0 {
		t.Fatalf("paused = %#v event=%#v", paused, event)
	}
}

func TestRunLoopBudgetUnboundedRunReservesNothing(t *testing.T) {
	f := newStepBudgetFixture(t, nil)
	producer := &budgetPlanningProducer{plan: models.BudgetUsage{InputTokens: 1 << 40}}
	before := f.run.Revision
	run, admission := f.reserve(&agentrun.TurnRunnerBinding{Producer: producer})
	if admission != nil || run.Revision != before || len(run.BudgetReservations) != 0 || producer.calls != 0 {
		t.Fatalf("unbounded run: admission=%#v run=%#v calls=%d", admission, run, producer.calls)
	}
	if id := stepReservationID(run, f.step); id != "" {
		t.Fatalf("reservation id = %q", id)
	}
}

func TestRunLoopBudgetDeterministicProducerReservesFloor(t *testing.T) {
	f := newStepBudgetFixture(t, &models.BudgetPolicy{MaxTurns: 3, MaxActions: 4})
	run, admission := f.reserve(&agentrun.TurnRunnerBinding{Producer: deterministicBudgetProducer{}, BudgetReservation: models.BudgetUsage{Actions: 1}})
	if admission != nil || run.BudgetReservations[f.step.ID].Usage != (models.BudgetUsage{Turns: 1, Actions: 1}) {
		t.Fatalf("admission=%#v reservations=%#v", admission, run.BudgetReservations)
	}
	if stepReservationID(run, f.step) != f.step.ID {
		t.Fatal("reserved step should settle its own reservation")
	}
}

func TestRunLoopBudgetAdmissionRefusesStep(t *testing.T) {
	f := newStepBudgetFixture(t, &models.BudgetPolicy{MaxInputTokens: 10})
	before := *f.run
	run, admission := f.reserve(&agentrun.TurnRunnerBinding{Producer: deterministicBudgetProducer{}, BudgetReservation: models.BudgetUsage{InputTokens: 11}})
	if admission == nil || admission.TurnID != f.step.ID || !admission.EvaluatedAt.Equal(f.now) ||
		admission.Dimension != "input_tokens" || admission.Reason != models.BudgetAdmissionReservation || admission.Remaining != 10 {
		t.Fatalf("admission = %#v", admission)
	}
	if err := admission.Validate(); err != nil {
		t.Fatal(err)
	}
	if run.Revision != before.Revision || len(run.BudgetReservations) != 0 {
		t.Fatalf("refused reservation wrote the Run: %#v", run)
	}

	finish := stepBudgetAdmissionFinish(f.step, "worker", *admission)
	run = f.apply(finish, stepBudgetPause(f.run, "worker", "", admission, ""), false)
	if f.step.Status != models.AgentTurnStatusCanceled || f.step.FailureCode != StepFailureBudgetExhausted || f.step.BudgetAdmission == nil {
		t.Fatalf("step = %#v", f.step)
	}
	if run.Status != models.AgentRunStatusPaused || run.BudgetUsage != (models.BudgetUsage{}) || len(run.BudgetReservations) != 0 ||
		run.BudgetAdmission == nil || run.BudgetAdmission.TurnID != f.step.ID {
		t.Fatalf("paused run = %#v", run)
	}
	types := f.eventTypes()
	if types[len(types)-1] != "budget.exhausted" {
		t.Fatalf("events = %v", types)
	}
}

func TestRunLoopBudgetPlannerAdmission(t *testing.T) {
	f := newStepBudgetFixture(t, &models.BudgetPolicy{MaxInputTokens: 100})
	want := models.BudgetAdmission{Reason: models.BudgetAdmissionHostedInput, Dimension: "input_tokens", Required: 500, Remaining: 100}
	producer := &budgetPlanningProducer{err: &models.BudgetAdmissionError{Admission: want}}
	_, admission := f.reserve(&agentrun.TurnRunnerBinding{Producer: producer})
	if admission == nil || admission.Dimension != "input_tokens" || admission.Required != 500 || admission.TurnID != f.step.ID {
		t.Fatalf("admission = %#v", admission)
	}

	producer.err = models.ErrBudgetExhausted
	_, admission = f.reserve(&agentrun.TurnRunnerBinding{Producer: producer})
	if admission == nil || admission.Dimension != "unknown" || admission.Reason != models.BudgetAdmissionHostedInput {
		t.Fatalf("bare exhaustion admission = %#v", admission)
	}

	producer.err = errors.New("planner broke")
	if _, _, err := reserveStepBudget(t.Context(), f.activity, f.now, f.run, f.step, &agentrun.TurnRunnerBinding{Producer: producer}, agentrun.StepInput{}, "worker"); err == nil || !strings.Contains(err.Error(), "planner broke") {
		t.Fatalf("planner error = %v", err)
	}
}

func TestRunLoopDurationBudgetDeadline(t *testing.T) {
	parent := t.Context()

	unbounded, cancel, charge, ok := stepDurationDeadline(parent, &models.AgentRun{}, "step")
	defer cancel()
	if !ok || charge != 0 {
		t.Fatalf("unbounded: ok=%v charge=%d", ok, charge)
	}
	if _, has := unbounded.Deadline(); has {
		t.Fatal("unbounded step has a deadline")
	}

	spent := &models.AgentRun{
		Budget:      &models.BudgetPolicy{MaxDurationMS: 1000},
		BudgetUsage: models.BudgetUsage{DurationMS: 600},
		BudgetReservations: map[string]models.BudgetReservation{
			"other": {ID: "other", Usage: models.BudgetUsage{DurationMS: 400}},
			"step":  {ID: "step", Usage: models.BudgetUsage{DurationMS: 900}},
		},
	}
	if _, cancel, _, ok := stepDurationDeadline(parent, spent, "step"); ok {
		cancel()
		t.Fatal("no duration left must refuse the step before it starts")
	}
	finish := stepDurationExpiredFinish(&models.AgentTurn{Revision: 3}, "worker", models.TurnUsage{}, 0, 0)
	if finish.Status != models.AgentTurnStatusCanceled || finish.FailureCode != StepFailureDurationBudget ||
		finish.Usage != (models.TurnUsage{}) || finish.OutputSummary != stepDurationExpiredSummary || finish.ExpectedRevision != 3 {
		t.Fatalf("before-start finish = %#v", finish)
	}
	pause := stepBudgetPause(&models.AgentRun{Budget: spent.Budget, BudgetUsage: spent.BudgetUsage}, "worker", stepDurationExpiredSummary, nil, "duration_ms")
	if pause.Status != models.AgentRunStatusPaused || pause.EventType != "budget.exhausted" ||
		pause.Payload["dimension"] != "duration_ms" || pause.Payload["used"] != int64(600) || pause.Payload["limit"] != int64(1000) {
		t.Fatalf("duration pause = %#v", pause)
	}

	left := &models.AgentRun{
		Budget: &models.BudgetPolicy{MaxDurationMS: 30}, BudgetUsage: models.BudgetUsage{DurationMS: 10},
		BudgetReservations: map[string]models.BudgetReservation{"other": {ID: "other", Usage: models.BudgetUsage{DurationMS: 15}}},
	}
	stepCtx, cancel, charge, ok := stepDurationDeadline(parent, left, "step")
	defer cancel()
	if !ok || charge != 5 {
		t.Fatalf("bounded: ok=%v charge=%d", ok, charge)
	}
	<-stepCtx.Done()
	if !stepDurationExpired(parent, stepCtx) {
		t.Fatal("deadline expiry not detected")
	}
	finish = stepDurationExpiredFinish(&models.AgentTurn{}, "worker", models.TurnUsage{InputTokens: 40, DurationMS: 2}, 3, charge)
	if finish.FailureCode != StepFailureDurationBudget || finish.Usage.DurationMS != 5 || finish.Usage.InputTokens != 40 ||
		finish.OutputSummary != stepDurationExceededSummary {
		t.Fatalf("mid-step finish = %#v", finish)
	}
	if finish := stepDurationExpiredFinish(&models.AgentTurn{}, "worker", models.TurnUsage{InputTokens: -1}, 9, 5); finish.Usage != (models.TurnUsage{DurationMS: 9}) {
		t.Fatalf("invalid producer usage must not be charged: %#v", finish.Usage)
	}

	// A canceled parent (lease loss, shutdown) is not a duration expiry.
	canceledParent, cancelParent := context.WithCancel(parent)
	child, cancelChild, _, _ := stepDurationDeadline(canceledParent, left, "step")
	defer cancelChild()
	cancelParent()
	<-child.Done()
	if stepDurationExpired(canceledParent, child) {
		t.Fatal("parent cancellation reported as duration expiry")
	}
}

func TestBudgetUsageForStepChargesTurnOnlyWhenProduced(t *testing.T) {
	usage := models.TurnUsage{InputTokens: 3, OutputTokens: 4, Cost: 0.0000015, DurationMS: 7, NativeOperations: 2}
	want := models.BudgetUsage{Turns: 1, InputTokens: 3, OutputTokens: 4, CostMicros: 2, DurationMS: 7, Actions: 2}
	if got := budgetUsageForStep(usage, true); got != want {
		t.Fatalf("produced = %#v", got)
	}
	want.Turns = 0
	if got := budgetUsageForStep(usage, false); got != want {
		t.Fatalf("not produced = %#v", got)
	}

	req := runtimeactivity.RunTransitionRequest{Status: models.AgentRunStatusRunning}
	fillStepBudgetSettlement(&req, &models.AgentRun{}, &models.AgentTurn{ID: "s"}, false)
	if req.ActivityUsageDelta != nil || req.BudgetUsageDelta != nil || req.SettleBudgetReservationID != "" || req.EventType != "" {
		t.Fatalf("empty settlement = %#v", req)
	}
	fillStepBudgetSettlement(&req, &models.AgentRun{}, &models.AgentTurn{ID: "s", Usage: usage}, true)
	if req.ActivityUsageDelta == nil || req.ActivityUsageDelta.Turns != 1 || req.BudgetUsageDelta != nil {
		t.Fatalf("unbounded settlement = %#v", req)
	}
}
