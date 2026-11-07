package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type budgetPlanningProducer struct {
	plan  BudgetUsage
	err   error
	calls int
	seen  StepInput
}

func (p *budgetPlanningProducer) Kind() StepProducerKind { return StepProducerModel }

func (p *budgetPlanningProducer) Produce(context.Context, StepInput) (StepOutput, error) {
	return StepOutput{}, errors.New("not produced in budget tests")
}

func (p *budgetPlanningProducer) PlanStepBudget(_ context.Context, in StepInput) (BudgetUsage, error) {
	p.calls++
	p.seen = in
	return p.plan, p.err
}

type deterministicBudgetProducer struct{}

func (deterministicBudgetProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (deterministicBudgetProducer) Produce(context.Context, StepInput) (StepOutput, error) {
	return StepOutput{}, nil
}

type stepBudgetFixture struct {
	t        *testing.T
	store    *MemoryStore
	activity *RunActivityService
	turns    *AgentTurnService
	run      *AgentRun
	step     *AgentTurn
	now      time.Time
}

func newStepBudgetFixture(t *testing.T, budget *BudgetPolicy) *stepBudgetFixture {
	t.Helper()
	ctx := t.Context()
	store := NewMemoryStore()
	scope := Scope{Kind: "local", ID: "step-budget-" + strings.ReplaceAll(t.Name(), "/", "-")}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "bounded work", Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	run, _, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "started",
		Actor: ActivityActor{Type: "worker", ID: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &stepBudgetFixture{
		t: t, store: store, activity: activity, turns: NewAgentTurnService(store, store), run: run,
		now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
	}
	f.step = f.begin()
	return f
}

func (f *stepBudgetFixture) begin() *AgentTurn {
	f.t.Helper()
	step, err := f.turns.BeginTurn(f.t.Context(), BeginAgentTurnRequest{
		Scope: f.run.Scope, RunID: f.run.ID, Producer: StepProducerModel, WorkerID: "worker", LeaseDuration: time.Minute,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return step
}

func (f *stepBudgetFixture) reserve(binding *TurnRunnerBinding) (*AgentRun, *BudgetAdmission) {
	f.t.Helper()
	run, admission, err := reserveStepBudget(f.t.Context(), f.activity, f.now, f.run, f.step, binding,
		StepInput{Binding: binding, Mode: StepModeNormal, WorkerID: "worker"}, "worker")
	if err != nil {
		f.t.Fatal(err)
	}
	f.run = run
	return run, admission
}

// apply finishes the step and applies it with transition, filling the budget
// settlement the way applyStep does.
func (f *stepBudgetFixture) apply(finish FinishAgentTurnRequest, transition RunTransitionRequest, produced bool) *AgentRun {
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
	events, err := f.store.ListActivity(f.t.Context(), ActivityFilter{Scope: f.run.Scope, RunID: f.run.ID})
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
	f := newStepBudgetFixture(t, &BudgetPolicy{MaxTurns: 2, MaxInputTokens: 1000})
	producer := &budgetPlanningProducer{plan: BudgetUsage{InputTokens: 100, Turns: 5}}
	binding := &TurnRunnerBinding{Producer: producer, BudgetReservation: BudgetUsage{InputTokens: 20}}

	run, admission := f.reserve(binding)
	if admission != nil {
		t.Fatalf("admission = %#v", admission)
	}
	reservation, ok := run.BudgetReservations[f.step.ID]
	if !ok || reservation.Usage != (BudgetUsage{Turns: 1, InputTokens: 120}) || !reservation.CreatedAt.Equal(f.now) {
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
	producer.plan = BudgetUsage{InputTokens: 200}
	f.now = f.now.Add(time.Minute)
	run, _ = f.reserve(binding)
	reconciled := run.BudgetReservations[f.step.ID]
	if reconciled.Usage != (BudgetUsage{Turns: 1, InputTokens: 220}) || !reconciled.CreatedAt.Equal(reservation.CreatedAt) {
		t.Fatalf("reconciled = %#v", reconciled)
	}

	run = f.apply(FinishAgentTurnRequest{
		ExpectedRevision: f.step.Revision, Status: AgentTurnStatusCompleted, WorkerID: "worker",
		Usage: TurnUsage{InputTokens: 150, OutputTokens: 9, NativeOperations: 1},
	}, RunTransitionRequest{Status: AgentRunStatusRunning, Summary: "Applied step 1", Actor: ActivityActor{Type: "worker", ID: "worker"}}, true)
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
	finished, err := f.turns.FinishTurn(t.Context(), f.run.Scope, f.step.ID, FinishAgentTurnRequest{
		ExpectedRevision: f.step.Revision, Status: AgentTurnStatusCompleted, WorkerID: "worker",
		Usage: TurnUsage{InputTokens: 10},
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
	if paused.Status != AgentRunStatusPaused || paused.BudgetState != BudgetStateExhausted || paused.BudgetUsage.Turns != 2 ||
		event.EventType != "budget.exhausted" || len(paused.BudgetReservations) != 0 {
		t.Fatalf("paused = %#v event=%#v", paused, event)
	}
}

func TestRunLoopBudgetUnboundedRunReservesNothing(t *testing.T) {
	f := newStepBudgetFixture(t, nil)
	producer := &budgetPlanningProducer{plan: BudgetUsage{InputTokens: 1 << 40}}
	before := f.run.Revision
	run, admission := f.reserve(&TurnRunnerBinding{Producer: producer})
	if admission != nil || run.Revision != before || len(run.BudgetReservations) != 0 || producer.calls != 0 {
		t.Fatalf("unbounded run: admission=%#v run=%#v calls=%d", admission, run, producer.calls)
	}
	if id := stepReservationID(run, f.step); id != "" {
		t.Fatalf("reservation id = %q", id)
	}
}

func TestRunLoopBudgetDeterministicProducerReservesFloor(t *testing.T) {
	f := newStepBudgetFixture(t, &BudgetPolicy{MaxTurns: 3, MaxActions: 4})
	run, admission := f.reserve(&TurnRunnerBinding{Producer: deterministicBudgetProducer{}, BudgetReservation: BudgetUsage{Actions: 1}})
	if admission != nil || run.BudgetReservations[f.step.ID].Usage != (BudgetUsage{Turns: 1, Actions: 1}) {
		t.Fatalf("admission=%#v reservations=%#v", admission, run.BudgetReservations)
	}
	if stepReservationID(run, f.step) != f.step.ID {
		t.Fatal("reserved step should settle its own reservation")
	}
}

func TestRunLoopBudgetAdmissionRefusesStep(t *testing.T) {
	f := newStepBudgetFixture(t, &BudgetPolicy{MaxInputTokens: 10})
	before := *f.run
	run, admission := f.reserve(&TurnRunnerBinding{Producer: deterministicBudgetProducer{}, BudgetReservation: BudgetUsage{InputTokens: 11}})
	if admission == nil || admission.TurnID != f.step.ID || !admission.EvaluatedAt.Equal(f.now) ||
		admission.Dimension != "input_tokens" || admission.Reason != BudgetAdmissionReservation || admission.Remaining != 10 {
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
	if f.step.Status != AgentTurnStatusCanceled || f.step.FailureCode != StepFailureBudgetExhausted || f.step.BudgetAdmission == nil {
		t.Fatalf("step = %#v", f.step)
	}
	if run.Status != AgentRunStatusPaused || run.BudgetUsage != (BudgetUsage{}) || len(run.BudgetReservations) != 0 ||
		run.BudgetAdmission == nil || run.BudgetAdmission.TurnID != f.step.ID {
		t.Fatalf("paused run = %#v", run)
	}
	types := f.eventTypes()
	if types[len(types)-1] != "budget.exhausted" {
		t.Fatalf("events = %v", types)
	}
}

func TestRunLoopBudgetPlannerAdmission(t *testing.T) {
	f := newStepBudgetFixture(t, &BudgetPolicy{MaxInputTokens: 100})
	want := BudgetAdmission{Reason: BudgetAdmissionHostedInput, Dimension: "input_tokens", Required: 500, Remaining: 100}
	producer := &budgetPlanningProducer{err: &BudgetAdmissionError{Admission: want}}
	_, admission := f.reserve(&TurnRunnerBinding{Producer: producer})
	if admission == nil || admission.Dimension != "input_tokens" || admission.Required != 500 || admission.TurnID != f.step.ID {
		t.Fatalf("admission = %#v", admission)
	}

	producer.err = ErrBudgetExhausted
	_, admission = f.reserve(&TurnRunnerBinding{Producer: producer})
	if admission == nil || admission.Dimension != "unknown" || admission.Reason != BudgetAdmissionHostedInput {
		t.Fatalf("bare exhaustion admission = %#v", admission)
	}

	producer.err = errors.New("planner broke")
	if _, _, err := reserveStepBudget(t.Context(), f.activity, f.now, f.run, f.step, &TurnRunnerBinding{Producer: producer}, StepInput{}, "worker"); err == nil || !strings.Contains(err.Error(), "planner broke") {
		t.Fatalf("planner error = %v", err)
	}
}

func TestRunLoopDurationBudgetDeadline(t *testing.T) {
	parent := t.Context()

	unbounded, cancel, charge, ok := stepDurationDeadline(parent, &AgentRun{}, "step")
	defer cancel()
	if !ok || charge != 0 {
		t.Fatalf("unbounded: ok=%v charge=%d", ok, charge)
	}
	if _, has := unbounded.Deadline(); has {
		t.Fatal("unbounded step has a deadline")
	}

	spent := &AgentRun{
		Budget:      &BudgetPolicy{MaxDurationMS: 1000},
		BudgetUsage: BudgetUsage{DurationMS: 600},
		BudgetReservations: map[string]BudgetReservation{
			"other": {ID: "other", Usage: BudgetUsage{DurationMS: 400}},
			"step":  {ID: "step", Usage: BudgetUsage{DurationMS: 900}},
		},
	}
	if _, cancel, _, ok := stepDurationDeadline(parent, spent, "step"); ok {
		cancel()
		t.Fatal("no duration left must refuse the step before it starts")
	}
	finish := stepDurationExpiredFinish(&AgentTurn{Revision: 3}, "worker", TurnUsage{}, 0, 0)
	if finish.Status != AgentTurnStatusCanceled || finish.FailureCode != StepFailureDurationBudget ||
		finish.Usage != (TurnUsage{}) || finish.OutputSummary != stepDurationExpiredSummary || finish.ExpectedRevision != 3 {
		t.Fatalf("before-start finish = %#v", finish)
	}
	pause := stepBudgetPause(&AgentRun{Budget: spent.Budget, BudgetUsage: spent.BudgetUsage}, "worker", stepDurationExpiredSummary, nil, "duration_ms")
	if pause.Status != AgentRunStatusPaused || pause.EventType != "budget.exhausted" ||
		pause.Payload["dimension"] != "duration_ms" || pause.Payload["used"] != int64(600) || pause.Payload["limit"] != int64(1000) {
		t.Fatalf("duration pause = %#v", pause)
	}

	left := &AgentRun{
		Budget: &BudgetPolicy{MaxDurationMS: 30}, BudgetUsage: BudgetUsage{DurationMS: 10},
		BudgetReservations: map[string]BudgetReservation{"other": {ID: "other", Usage: BudgetUsage{DurationMS: 15}}},
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
	finish = stepDurationExpiredFinish(&AgentTurn{}, "worker", TurnUsage{InputTokens: 40, DurationMS: 2}, 3, charge)
	if finish.FailureCode != StepFailureDurationBudget || finish.Usage.DurationMS != 5 || finish.Usage.InputTokens != 40 ||
		finish.OutputSummary != stepDurationExceededSummary {
		t.Fatalf("mid-step finish = %#v", finish)
	}
	if finish := stepDurationExpiredFinish(&AgentTurn{}, "worker", TurnUsage{InputTokens: -1}, 9, 5); finish.Usage != (TurnUsage{DurationMS: 9}) {
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
	usage := TurnUsage{InputTokens: 3, OutputTokens: 4, Cost: 0.0000015, DurationMS: 7, NativeOperations: 2}
	want := BudgetUsage{Turns: 1, InputTokens: 3, OutputTokens: 4, CostMicros: 2, DurationMS: 7, Actions: 2}
	if got := budgetUsageForStep(usage, true); got != want {
		t.Fatalf("produced = %#v", got)
	}
	want.Turns = 0
	if got := budgetUsageForStep(usage, false); got != want {
		t.Fatalf("not produced = %#v", got)
	}

	req := RunTransitionRequest{Status: AgentRunStatusRunning}
	fillStepBudgetSettlement(&req, &AgentRun{}, &AgentTurn{ID: "s"}, false)
	if req.ActivityUsageDelta != nil || req.BudgetUsageDelta != nil || req.SettleBudgetReservationID != "" || req.EventType != "" {
		t.Fatalf("empty settlement = %#v", req)
	}
	fillStepBudgetSettlement(&req, &AgentRun{}, &AgentTurn{ID: "s", Usage: usage}, true)
	if req.ActivityUsageDelta == nil || req.ActivityUsageDelta.Turns != 1 || req.BudgetUsageDelta != nil {
		t.Fatalf("unbounded settlement = %#v", req)
	}
}

func TestPlanModelStepReservation(t *testing.T) {
	cases := []struct {
		name      string
		policy    BudgetPolicy
		remaining BudgetPolicy
		input     int64
		want      BudgetUsage
		dimension string
	}{
		{name: "unbounded tokens", policy: BudgetPolicy{MaxTurns: 3}, input: 900},
		{name: "input fits", policy: BudgetPolicy{MaxInputTokens: 1000}, remaining: BudgetPolicy{MaxInputTokens: 1000}, input: 900, want: BudgetUsage{InputTokens: 900}},
		{name: "input short", policy: BudgetPolicy{MaxInputTokens: 1000}, remaining: BudgetPolicy{MaxInputTokens: 899}, input: 900, dimension: "input_tokens"},
		{name: "total takes the rest", policy: BudgetPolicy{MaxTotalTokens: 2000}, remaining: BudgetPolicy{MaxTotalTokens: 1500}, input: 900, want: BudgetUsage{InputTokens: 900, OutputTokens: 600}},
		{name: "total short of minimum output", policy: BudgetPolicy{MaxTotalTokens: 2000}, remaining: BudgetPolicy{MaxTotalTokens: 900 + stepMinimumOutputTokens - 1}, input: 900, dimension: "total_tokens"},
		{name: "output caps total", policy: BudgetPolicy{MaxTotalTokens: 2000, MaxOutputTokens: 100}, remaining: BudgetPolicy{MaxTotalTokens: 1500, MaxOutputTokens: 100}, input: 900, want: BudgetUsage{InputTokens: 900, OutputTokens: 100}},
		{name: "output only", policy: BudgetPolicy{MaxOutputTokens: 500}, remaining: BudgetPolicy{MaxOutputTokens: 300}, input: 900, want: BudgetUsage{OutputTokens: 300}},
		{name: "output short", policy: BudgetPolicy{MaxOutputTokens: 500}, remaining: BudgetPolicy{MaxOutputTokens: stepMinimumOutputTokens - 1}, input: 900, dimension: "output_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planModelStepReservation(tc.policy, tc.remaining, tc.input)
			if tc.dimension != "" {
				var admission *BudgetAdmissionError
				if !errors.As(err, &admission) || !errors.Is(err, ErrBudgetExhausted) || admission.Admission.Dimension != tc.dimension ||
					admission.Admission.Reason != BudgetAdmissionHostedInput {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("reservation = %#v err=%v, want %#v", got, err, tc.want)
			}
		})
	}
}

func TestEstimateModelStepInputTokens(t *testing.T) {
	text := ModelStepRequest{
		APIVersion: ModelStepAPIVersion, StepID: "step",
		Entries: []SessionEntryView{{ID: "u", Type: SessionEntryUser, User: &UserEntryView{Content: []ContentPart{{Type: ContentPartText, Text: "hello"}}}}},
	}
	base, err := EstimateModelStepInputTokens(text)
	if err != nil || base <= stepProtocolInputReserveTokens {
		t.Fatalf("base estimate = %d err=%v", base, err)
	}

	image := text
	image.Entries = []SessionEntryView{text.Entries[0], {
		ID: "t", Type: SessionEntryTool, Tool: &ToolEntryView{CallID: "c", Name: "shot", Result: ToolResultView{Content: []ContentPart{
			{Type: ContentPartImage, MediaType: "image/png", DataBase64: strings.Repeat("A", 1<<20)},
		}}},
	}}
	withImage, err := EstimateModelStepInputTokens(image)
	if err != nil {
		t.Fatal(err)
	}
	if extra := withImage - base; extra < stepMediaReserveTokens || extra > stepMediaReserveTokens+200 {
		t.Fatalf("image estimate adds %d tokens; bytes must not be counted", extra)
	}
	if image.Entries[1].Tool.Result.Content[0].DataBase64 == "" {
		t.Fatal("estimate mutated the request")
	}

	plain, err := modelStepReservationInput(text, false)
	if err != nil || plain != base*stepMaximumProviderAttempts+stepRepairInputReserveTokens {
		t.Fatalf("reservation input = %d err=%v", plain, err)
	}
	due, err := modelStepReservationInput(text, true)
	if err != nil || due != 2*plain {
		t.Fatalf("compaction-due reservation input = %d, want %d", due, 2*plain)
	}
}
