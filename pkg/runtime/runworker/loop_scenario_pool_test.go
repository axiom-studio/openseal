package runworker

// Worker pool scenarios (run-loop section 12, tests-T17a): the pool claims a
// Run, resolves its binding and drives the RunLoop until the Run is at rest
// (terminal, parked, or yielded back to the queue). These replace the old
// per-Turn worker tests: claim/yield/attempt limit/lease renewal, budget
// planning and settlement per step, and terminal handoff resolution.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimecollaboration "github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

// poolFixturePool is a worker pool over the loop fixture's store whose
// resolver hands out the fixture's production binding. Claims are driven
// one at a time with executeClaim, as the fixture's worker.
func poolFixturePool(t *testing.T, f *looptest.Fixture, maxSteps int) *AgentRunWorkerPool {
	t.Helper()
	if maxSteps <= 0 {
		maxSteps = runloop.RunLoopMaxStepsPerClaim
	}
	pool, err := newAgentRunWorkerPool(f.Store, conversation.TurnRunnerResolverFunc(func(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return f.Binding(run), nil
	}), nil, AgentRunWorkerConfig{Scope: f.Scope, LeaseDuration: looptest.FixtureLease, TurnLeaseDuration: looptest.FixtureLease}, maxSteps, models.WithClock(f.Clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	pool.SetActionCoordinator(f.Actions)
	return pool
}

// poolClaim claims runID through the pool's scheduler as the fixture worker
// and executes the claim. It returns the Run as stored afterwards.
func poolClaim(t *testing.T, f *looptest.Fixture, pool *AgentRunWorkerPool, runID string) *models.AgentRun {
	t.Helper()
	claimed, err := pool.scheduler.ClaimNext(f.Ctx, agentrun.AgentRunClaimRequest{
		Scope: f.Scope, WorkerID: f.WorkerID, LeaseDuration: looptest.FixtureLease,
	})
	if err != nil || claimed == nil || claimed.ID != runID {
		t.Fatalf("claim %s: %#v %v", runID, claimed, err)
	}
	pool.executeClaim(f.Ctx, f.WorkerID, claimed)
	return f.MustRun(runID)
}

// lookupSteps answers a lookup of a distinct issue on each of the first n
// steps, then gives the final answer.
func lookupSteps(n int) []*looptest.ModelRule {
	return []*looptest.ModelRule{
		{Name: "lookup", When: func(v looptest.StepView) bool { return v.Step() <= n },
			Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": fmt.Sprint(6800 + v.Step())}))
			}},
		{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Done.") }},
	}
}

// One claim drives the Run through all its steps to completion: the pool
// resolves once, the loop runs a read step and the final step, and nothing
// yields.
func TestLoopPoolClaimsAndRunsToRest(t *testing.T) {
	looptest.EachStore(t, "loop_pool_rest", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(lookupSteps(2)...))
		var resolved atomic.Int32
		pool, err := NewAgentRunWorkerPool(f.Store, conversation.TurnRunnerResolverFunc(func(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
			resolved.Add(1)
			return f.Binding(run), nil
		}), nil, AgentRunWorkerConfig{Scope: f.Scope, LeaseDuration: looptest.FixtureLease, TurnLeaseDuration: looptest.FixtureLease}, models.WithClock(f.Clock.Now))
		if err != nil {
			t.Fatal(err)
		}
		pool.SetActionCoordinator(f.Actions)
		run := f.StartRun(models.RunKindAgentWork, "Check two issues")

		done := poolClaim(t, f, pool, run.ID)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if resolved.Load() != 1 {
			t.Fatalf("resolutions = %d, want 1", resolved.Load())
		}
		if steps := f.Steps(run.ID); len(steps) != 3 || done.LastAppliedTurn != 3 {
			t.Fatalf("steps = %d, last applied %d; want 3", len(steps), done.LastAppliedTurn)
		}
		if f.Dispatch.Count("lookup") != 2 {
			t.Fatalf("lookup dispatched %d times, want 2", f.Dispatch.Count("lookup"))
		}
		if claims, yields := len(f.Events(run.ID, "run.claimed")), len(f.Events(run.ID, "run.yielded")); claims != 1 || yields != 0 {
			t.Fatalf("claimed %d, yielded %d; want one claim and no yield", claims, yields)
		}
	})
}

// A claim ends after its step slice: the Run goes back to the queue with a
// run.yielded event, and the next claim continues from the log.
func TestLoopPoolYieldsAfterItsStepSlice(t *testing.T) {
	looptest.EachStore(t, "loop_pool_yield", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(lookupSteps(1)...))
		pool := poolFixturePool(t, f, 1)
		run := f.StartRun(models.RunKindAgentWork, "Check one issue")

		yielded := poolClaim(t, f, pool, run.ID)
		f.MustStatus(yielded, models.AgentRunStatusQueued)
		if yielded.LastAppliedTurn != 1 || len(f.Events(run.ID, "run.yielded")) != 1 {
			t.Fatalf("first claim: last applied %d, yields %d", yielded.LastAppliedTurn, len(f.Events(run.ID, "run.yielded")))
		}
		done := poolClaim(t, f, pool, run.ID)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if done.Attempt != 2 || done.LastAppliedTurn != 2 || len(f.Model.Calls(models.ModelStepPurposeStep)) != 2 {
			t.Fatalf("second claim: attempt %d, last applied %d, model calls %d", done.Attempt, done.LastAppliedTurn, len(f.Model.Calls(models.ModelStepPurposeStep)))
		}
		for _, event := range f.Events(run.ID, "run.claimed") {
			if event.UsageDelta != nil {
				t.Fatalf("run.claimed duplicated scheduler-owned attempt usage: %#v", event)
			}
		}
		if claims := len(f.Events(run.ID, "run.claimed")); claims != 2 {
			t.Fatalf("claims = %d, want 2", claims)
		}
	})
}

// A Run with one autonomous attempt pauses at the end of its step slice
// instead of going back to the queue: the model is never called past the
// attempt limit, and the paused Run is not claimed again.
func TestLoopPoolNeverChargesPastAttemptLimit(t *testing.T) {
	f := looptest.NewFixture(t, conversationtest.NewStore(t), looptest.WithRules(lookupSteps(4)...))
	pool := poolFixturePool(t, f, 1)
	run, err := f.Portfolio.CreateAgentRun(f.Ctx, models.CreateAgentRunRequest{
		Scope: f.Scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: looptest.FixtureAgent}, AssignedAgentID: looptest.FixtureAgent,
		Goal: "use exactly one autonomous attempt", Source: models.RunSourceObjective, Budget: &models.BudgetPolicy{MaxAttempts: 1, MaxTurns: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused := poolClaim(t, f, pool, run.ID)
	f.MustStatus(paused, models.AgentRunStatusPaused)
	if next, err := pool.scheduler.ClaimNext(f.Ctx, agentrun.AgentRunClaimRequest{Scope: f.Scope, WorkerID: f.WorkerID, LeaseDuration: looptest.FixtureLease}); err != nil || next != nil {
		t.Fatalf("paused Run was claimed again: %#v %v", next, err)
	}
	if paused.BudgetUsage.Attempts != 1 || len(f.Model.Calls(models.ModelStepPurposeStep)) != 1 || len(f.Steps(run.ID)) != 1 {
		t.Fatalf("attempt ceiling: attempts %d, model calls %d, steps %d", paused.BudgetUsage.Attempts, len(f.Model.Calls(models.ModelStepPurposeStep)), len(f.Steps(run.ID)))
	}
	if len(f.Events(run.ID, "budget.exhausted")) != 1 {
		t.Fatalf("budget.exhausted events = %d, want 1", len(f.Events(run.ID, "budget.exhausted")))
	}
}

// Each step reserves one Turn of an explicit Turns budget. Three steps fit a
// budget of three, with a host outage inside the second step costing no Turn
// (its retry is the same step's work); a fourth step is refused at admission
// and pauses the Run.
func TestLoopTurnsBudgetCountsModelSteps(t *testing.T) {
	for _, c := range []struct {
		name    string
		lookups int
		want    models.AgentRunStatus
	}{
		{name: "three_steps_fit", lookups: 2, want: models.AgentRunStatusCompleted},
		{name: "fourth_step_refused", lookups: 3, want: models.AgentRunStatusPaused},
	} {
		t.Run(c.name, func(t *testing.T) {
			looptest.EachStore(t, "loop_turns_budget", func(t *testing.T, store models.KernelStore) {
				outage := &looptest.ModelRule{Name: "outage", Times: 1, When: func(v looptest.StepView) bool { return v.Step() == 2 }, Err: modelstep.ErrTurnHostUnavailable}
				f := looptest.NewFixture(t, store, looptest.WithRules(append([]*looptest.ModelRule{outage}, lookupSteps(c.lookups)...)...))
				run, err := f.Portfolio.CreateAgentRun(f.Ctx, models.CreateAgentRunRequest{
					Scope: f.Scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: looptest.FixtureAgent},
					AssignedAgentID: looptest.FixtureAgent, Goal: "Check the issues", Source: models.RunSourceObjective, Budget: &models.BudgetPolicy{MaxTurns: 3},
				})
				if err != nil {
					t.Fatal(err)
				}
				done := f.DriveToRest(run.ID, 5)
				f.MustStatus(done, c.want)
				if outage.Used != 1 {
					t.Fatalf("outage rule used %d times, want 1", outage.Used)
				}
				if done.BudgetUsage.Turns != 3 {
					t.Fatalf("budget usage = %#v, want 3 Turns", done.BudgetUsage)
				}
				if done.Status == models.AgentRunStatusPaused {
					if len(f.Events(run.ID, "budget.exhausted")) != 1 || len(f.Model.Calls(models.ModelStepPurposeStep)) != 4 {
						t.Fatalf("exhausted: events %d, model calls %d (3 steps + 1 outage)",
							len(f.Events(run.ID, "budget.exhausted")), len(f.Model.Calls(models.ModelStepPurposeStep)))
					}
				}
				if done.Status == models.AgentRunStatusCompleted && len(done.BudgetReservations) != 0 {
					t.Fatalf("completed Run kept reservations: %#v", done.BudgetReservations)
				}
			})
		})
	}
}

// Chat Runs are unbounded: five steps reserve nothing and leave no budget.
func TestLoopChatRunsAcquireNoBudget(t *testing.T) {
	looptest.EachStore(t, "loop_chat_budget", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", When: func(v looptest.StepView) bool { return v.Step() <= 4 },
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": fmt.Sprint(6800 + v.Step())}))
				}},
			&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("All four are open.") }},
		))
		_, run := f.StartChat("check issues 6801 to 6804")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if steps := f.Steps(run.ID); len(steps) != 5 {
			t.Fatalf("steps = %d, want 5", len(steps))
		}
		if done.Budget != nil || len(done.BudgetReservations) != 0 {
			t.Fatalf("chat Run acquired a budget: %#v %#v", done.Budget, done.BudgetReservations)
		}
	})
}

// plannedPoolProducer plans its step's budget and reports usage equal to the
// reservation it sees.
type plannedPoolProducer struct {
	planned models.BudgetUsage
	seen    chan models.BudgetUsage
}

func (*plannedPoolProducer) Kind() models.StepProducerKind { return models.StepProducerDeterministic }

func (p *plannedPoolProducer) PlanStepBudget(context.Context, agentrun.StepInput) (models.BudgetUsage, error) {
	return p.planned, nil
}

func (p *plannedPoolProducer) Produce(_ context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	reservation := in.Run.BudgetReservations[in.Step.ID].Usage
	p.seen <- reservation
	return agentrun.StepOutput{
		Usage: models.TurnUsage{InputTokens: int(reservation.InputTokens), OutputTokens: int(reservation.OutputTokens)},
		Final: &agentrun.LoopFinal{Answer: "bounded work complete", Output: map[string]interface{}{"summary": "bounded work complete"}},
	}, nil
}

// The pool reserves the producer's planned usage for the step and settles
// exactly the step's usage when the Run completes.
func TestLoopPoolPreservesProducerBudgetPlanning(t *testing.T) {
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "7"}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher",
		Goal: "Synthesize bounded evidence", Source: models.RunSourceManual,
		Budget: &models.BudgetPolicy{MaxTurns: 2, MaxInputTokens: 8000, MaxOutputTokens: 2000, MaxTotalTokens: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	producer := &plannedPoolProducer{planned: models.BudgetUsage{InputTokens: 5000, OutputTokens: 1000}, seen: make(chan models.BudgetUsage, 1)}
	pool, err := NewAgentRunWorkerPool(store, conversation.TurnRunnerResolverFunc(func(context.Context, *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return &agentrun.TurnRunnerBinding{DefinitionID: "researcher", DefinitionVersion: "1", Producer: producer}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "researcher", Concurrency: 1,
		PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	select {
	case reservation := <-producer.seen:
		if reservation.Turns != 1 || reservation.InputTokens != 5000 || reservation.OutputTokens != 1000 {
			t.Fatalf("worker discarded the planned reservation: %#v", reservation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not run the bounded step")
	}
	completed := waitForRunStatus(t, store, scope, run.ID, models.AgentRunStatusCompleted)
	if completed.BudgetUsage.Turns != 1 || completed.BudgetUsage.InputTokens != 5000 || completed.BudgetUsage.OutputTokens != 1000 || len(completed.BudgetReservations) != 0 {
		t.Fatalf("planned reservation was not settled exactly: %#v", completed)
	}
}

// slowPoolProducer finishes after a step longer than the lease.
type slowPoolProducer struct{ wait time.Duration }

func (*slowPoolProducer) Kind() models.StepProducerKind { return models.StepProducerDeterministic }

func (p *slowPoolProducer) Produce(ctx context.Context, _ agentrun.StepInput) (agentrun.StepOutput, error) {
	select {
	case <-ctx.Done():
		return agentrun.StepOutput{}, ctx.Err()
	case <-time.After(p.wait):
		return agentrun.StepOutput{Final: &agentrun.LoopFinal{Answer: "Done", Output: map[string]interface{}{"summary": "Done"}}}, nil
	}
}

// A step that outlasts the Run and step leases keeps its claim: the pool's
// heartbeat renews the Run lease and the loop renews the step lease, so the
// step completes once on the original worker.
func TestLoopPoolRenewsLeaseDuringLongStep(t *testing.T) {
	store := conversationtest.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := models.Scope{Kind: "local", ID: "test"}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "finish a long bounded step", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewAgentRunWorkerPool(store, conversation.TurnRunnerResolverFunc(func(context.Context, *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return &agentrun.TurnRunnerBinding{DefinitionID: "agent", DefinitionVersion: "1", Producer: &slowPoolProducer{wait: 220 * time.Millisecond}}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "agent", PollInterval: 5 * time.Millisecond,
		LeaseDuration: 90 * time.Millisecond, TurnLeaseDuration: 90 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.Start(ctx)
	defer pool.Stop()
	completed := waitForRunStatus(t, store, scope, run.ID, models.AgentRunStatusCompleted)
	turns, err := store.ListAgentTurns(ctx, models.AgentTurnFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if completed.LastAppliedTurn != 1 || completed.Attempt != 1 || len(turns) != 1 || turns[0].Status != models.AgentTurnStatusCompleted {
		t.Fatalf("long step was not protected by renewable ownership: run=%#v turns=%#v", completed, turns)
	}
}

// handoffPoolProducer completes the recipient's child Run with its output.
type handoffPoolProducer struct{}

func (handoffPoolProducer) Kind() models.StepProducerKind { return models.StepProducerDeterministic }

func (handoffPoolProducer) Produce(context.Context, agentrun.StepInput) (agentrun.StepOutput, error) {
	return agentrun.StepOutput{Final: &agentrun.LoopFinal{Answer: "Launch follow-up ready",
		Output: map[string]interface{}{"followUp": "Publish the evidence-backed launch note"}}}, nil
}

// A handoff child whose source already ended still completes its request
// when the recipient's Run finishes: the pool resolves the terminal child
// into the AgentRequest and the source's output.
func TestLoopPoolCompletesArtifactFreeHandoffFromTerminalChild(t *testing.T) {
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "7"}
	portfolio := portfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "developer"}, AssignedAgentID: "developer",
		Goal: "Build the feature", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	collaboration := runtimecollaboration.NewCollaborationService(store)
	created, err := collaboration.CreateAgentRequest(t.Context(), runtimecollaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindHandoff, Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "developer"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"}, SourceRunID: source.ID,
		Goal: "Draft the launch follow-up", AcceptanceCriteria: map[string]interface{}{"required": "one follow-up"},
		IdempotencyKey: "handoff-worker-completion",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := collaboration.RespondAgentRequest(t.Context(), runtimecollaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: runtimecollaboration.AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marketing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Terminal maintenance can observe the completed source before the
	// recipient claims its child. Acceptance transfers cancellation ownership
	// even though the child's historical parent lineage remains unchanged.
	if err := agentrun.NewRunCommandService(store).CascadeTerminalRun(t.Context(), accepted.Source); err != nil {
		t.Fatal(err)
	}
	pool, err := NewAgentRunWorkerPool(store, conversation.TurnRunnerResolverFunc(func(context.Context, *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return &agentrun.TurnRunnerBinding{DefinitionID: "marketing", DefinitionVersion: "1", Producer: handoffPoolProducer{}}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, AssignedAgentID: "marketing", Concurrency: 1,
		PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	deadline := time.Now().Add(3 * time.Second)
	var completed *models.AgentRequest
	for time.Now().Before(deadline) {
		completed, err = collaboration.GetAgentRequest(ctx, scope, created.Request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == models.AgentRequestStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed == nil || completed.Status != models.AgentRequestStatusCompleted || completed.ChildRunID != accepted.Child.ID ||
		completed.CompletionSummary != "Publish the evidence-backed launch note" || completed.AcceptanceEvidence["runOutput"] == nil {
		t.Fatalf("completed handoff = %#v", completed)
	}
	child, err := portfolio.GetAgentRun(ctx, scope, accepted.Child.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshedSource, err := portfolio.GetAgentRun(ctx, scope, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != models.AgentRunStatusCompleted || refreshedSource.Status != models.AgentRunStatusCompleted ||
		refreshedSource.Output["handoffRequestId"] != completed.ID {
		t.Fatalf("source=%#v child=%#v", refreshedSource, child)
	}
}
