package runloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

// Crash-resume scenarios of the run loop (D14, SPEC §3.7, section 02 §8).
// Each scenario drives a claim that dies at a chosen write (loopCrashStore armed:
// every later write fails, nothing is cleaned up), then lets the Run and step
// leases expire on the fake clock and drives a second claim by another worker.

const (
	resumeWorkerA = "resume-worker-a"
	resumeWorkerB = "resume-worker-b"
	resumeLease   = time.Minute
)

// resumeStore is the crash store with hooks on the step-row writes, so a
// scenario can crash right before RecordStepModel or right after FinishTurn.
type resumeStore struct {
	*loopCrashStore
	mu           sync.Mutex
	beforeRecord func()
	afterFinish  func(turn *models.AgentTurn)
}

func (s *resumeStore) RecordAgentTurnStepModel(ctx context.Context, turn *models.AgentTurn, expectedRevision int64, events []*models.ActivityEvent) error {
	s.mu.Lock()
	hook := s.beforeRecord
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.loopCrashStore.RecordAgentTurnStepModel(ctx, turn, expectedRevision, events)
}

func (s *resumeStore) UpdateAgentTurn(ctx context.Context, turn *models.AgentTurn, expectedRevision int64, workerID string) error {
	err := s.loopCrashStore.UpdateAgentTurn(ctx, turn, expectedRevision, workerID)
	s.mu.Lock()
	hook := s.afterFinish
	s.mu.Unlock()
	if err == nil && hook != nil && models.IsTerminalAgentTurnStatus(turn.Status) {
		hook(models.CloneAgentTurn(turn))
	}
	return err
}

// resumeProducer is a model producer answering from a script, one reply per
// call; before runs inside Produce (a scenario arms the crash there).
type resumeProducer struct {
	mu      sync.Mutex
	replies []*models.AssistantEntry
	steps   []string // step id of every call
	before  func(call int)
}

func (p *resumeProducer) Kind() models.StepProducerKind { return models.StepProducerModel }

func (p *resumeProducer) Produce(_ context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	p.mu.Lock()
	call := len(p.steps)
	p.steps = append(p.steps, in.Step.ID)
	before := p.before
	if call >= len(p.replies) {
		p.mu.Unlock()
		return agentrun.StepOutput{}, fmt.Errorf("resume producer: no reply for call %d", call+1)
	}
	reply := *p.replies[call]
	p.mu.Unlock()
	if before != nil {
		before(call)
	}
	return agentrun.StepOutput{Assistant: &reply, ModelProvider: string(reply.Provider), Model: reply.Model}, nil
}

func (p *resumeProducer) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.steps...)
}

func resumeSay(text string) *models.AssistantEntry {
	return &models.AssistantEntry{Provider: models.ModelStepProviderAnthropic, Model: loopModelName, Text: text, StopReason: models.ModelStopEnd}
}

func resumeAct(narration string, calls ...models.ModelToolCall) *models.AssistantEntry {
	return &models.AssistantEntry{Provider: models.ModelStepProviderAnthropic, Model: loopModelName, Text: narration, ToolCalls: calls, StopReason: models.ModelStopToolCalls}
}

func resumeCall(id, name string, args string) models.ModelToolCall {
	return models.ModelToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// resumeTools is a LoopToolExecutor over the real session log: ordinary
// calls complete inline, the final tool is a settle-phase call that yields
// the FinalAnswer. Hooks let a scenario crash inside a phase.
type resumeTools struct {
	h *resumeHarness

	mu            sync.Mutex
	executeCalls  int
	settleCalls   int
	resumeCalls   int
	resumeEntries [][]string // entry ids passed to each ResumeOpenTools
	beforeExecute func() error
	beforeSettle  func() error
}

func (x *resumeTools) plan(_ *agentrun.TurnRunnerBinding, session *models.AgentSession, step *models.AgentTurn, assistant *models.SessionEntry, _ []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
	var entries []*models.SessionEntry
	for _, call := range assistant.Assistant.ToolCalls {
		entries = append(entries, &models.SessionEntry{
			ID: models.SessionEntryID(session.ID, models.ToolEntryKey(step.ID, call.ID)), Scope: session.Scope, SessionID: session.ID,
			RunID: assistant.RunID, StepID: step.ID, Step: step.Sequence, Type: models.SessionEntryTool, State: models.ToolEntryPending,
			ToolCallID: call.ID, Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: models.ToolSideEffectRead},
		})
	}
	return entries, nil
}

func (x *resumeTools) complete(ctx context.Context, in agentrun.StepToolsInput, entry *models.SessionEntry) error {
	now := x.h.clock.Now()
	_, err := x.h.sessions().TransitionToolEntry(ctx, models.ToolEntryTransitionRequest{
		Scope: in.Run.Scope, EntryID: entry.ID, RunID: in.Run.ID, Lease: models.AgentRunLeaseGuard{WorkerID: in.WorkerID, Now: now},
		From: entry.State, ExpectedRevision: entry.Revision, To: models.ToolEntryCompleted, Now: now,
		Result: &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{{Type: models.ContentPartText, Text: "ok: " + entry.Tool.Name}}},
	})
	return err
}

func (x *resumeTools) finalAnswer(entry *models.SessionEntry) *agentrun.FinalAnswer {
	var args struct {
		Answer string `json:"answer"`
	}
	_ = json.Unmarshal(entry.Tool.Arguments, &args)
	return &agentrun.FinalAnswer{
		StepID: entry.StepID, ToolEntryID: entry.ID, ToolCallID: entry.ToolCallID, Answer: args.Answer,
		Output: map[string]interface{}{"summary": args.Answer},
	}
}

// run settles entries in order: the final tool ends the walk (inline: as a
// settle-phase call; resumed: with its answer).
func (x *resumeTools) run(ctx context.Context, in agentrun.StepToolsInput, entries []*models.SessionEntry, inline bool) (agentrun.StepToolsOutcome, error) {
	for _, entry := range entries {
		if entry == nil || entry.Tool == nil || !entry.State.Open() {
			continue
		}
		if control.IsFinalTool(models.ControlToolName(entry.Tool.Name)) {
			if inline {
				return agentrun.StepToolsOutcome{SettlePending: true}, nil
			}
			return agentrun.StepToolsOutcome{Final: x.finalAnswer(entry)}, nil
		}
		if err := x.complete(ctx, in, entry); err != nil {
			return agentrun.StepToolsOutcome{}, err
		}
	}
	return agentrun.StepToolsOutcome{}, nil
}

func (x *resumeTools) ExecuteStepTools(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	x.mu.Lock()
	x.executeCalls++
	hook := x.beforeExecute
	x.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return agentrun.StepToolsOutcome{}, err
		}
	}
	return x.run(ctx, in, in.Entries, true)
}

func (x *resumeTools) SettleStep(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	x.mu.Lock()
	x.settleCalls++
	hook := x.beforeSettle
	x.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return agentrun.StepToolsOutcome{}, err
		}
	}
	entries, err := x.h.sessions().ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: in.Run.Scope, StepID: in.Step.ID, Types: []models.SessionEntryType{models.SessionEntryTool},
	})
	if err != nil {
		return agentrun.StepToolsOutcome{}, err
	}
	return x.run(ctx, in, entries, false)
}

func (x *resumeTools) ResumeOpenTools(ctx context.Context, in agentrun.StepToolsInput) (agentrun.StepToolsOutcome, error) {
	x.mu.Lock()
	x.resumeCalls++
	var ids []string
	for _, entry := range in.Entries {
		ids = append(ids, entry.ID)
	}
	x.resumeEntries = append(x.resumeEntries, ids)
	x.mu.Unlock()
	return x.run(ctx, in, in.Entries, false)
}

func (x *resumeTools) ReplanPark(context.Context, agentrun.StepToolsInput) (*agentrun.LoopPark, error) {
	return nil, nil
}

func (x *resumeTools) counts() (execute, settle, resume int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.executeCalls, x.settleCalls, x.resumeCalls
}

// resumeFinal posts one reply per Run (the conversation key is per Run and
// trigger): a second Deliver for the same Run is a replay of the posted
// message, as CommitChannelMessage does for an existing key.
type resumeFinal struct {
	mu        sync.Mutex
	calls     int
	posted    map[string]string // run id -> answer
	afterPost func()
}

func (f *resumeFinal) Deliver(_ context.Context, d agentrun.FinalDelivery) (agentrun.FinalDeliveryResult, error) {
	f.mu.Lock()
	f.calls++
	if f.posted == nil {
		f.posted = map[string]string{}
	}
	replayed := true
	if _, ok := f.posted[d.Run.ID]; !ok {
		f.posted[d.Run.ID] = d.Answer.Answer
		replayed = false
	}
	answer := f.posted[d.Run.ID]
	hook := f.afterPost
	f.mu.Unlock()
	if hook != nil && !replayed {
		hook()
	}
	return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryComplete, Output: map[string]interface{}{
		"summary": answer, "messageId": "message-" + d.Run.ID, "replayed": replayed,
	}}, nil
}

func (f *resumeFinal) counts() (calls, posts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, len(f.posted)
}

type resumeHarness struct {
	t        *testing.T
	raw      models.KernelStore
	store    *resumeStore
	clock    *loopClock
	scope    models.Scope
	runID    string
	loop     *Loop
	producer *resumeProducer
	tools    *resumeTools
	final    *resumeFinal
}

func (h *resumeHarness) sessions() models.AgentSessionStore { return h.store }

func newResumeHarness(t *testing.T, raw models.KernelStore, budget *models.BudgetPolicy, stepLease time.Duration, replies ...*models.AssistantEntry) *resumeHarness {
	t.Helper()
	ctx := context.Background()
	h := &resumeHarness{
		t: t, raw: raw, store: &resumeStore{loopCrashStore: newLoopCrashStore(t, raw)},
		clock:    newLoopClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)),
		scope:    models.Scope{Kind: "tenant", ID: "resume-" + strings.ReplaceAll(t.Name(), "/", "-")},
		producer: &resumeProducer{replies: replies}, final: &resumeFinal{},
	}
	h.tools = &resumeTools{h: h}
	portfolio := portfolio.NewService(raw, models.WithClock(h.clock.Now))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: h.scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-a"}, AssignedAgentID: "agent-a",
		Goal: "resume work", Source: models.RunSourceObjective, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.runID = run.ID
	loop, err := New(h.store, nil, nil, Config{
		StepLeaseDuration: stepLease, Now: h.clock.Now, PlanToolEntries: h.tools.plan,
		PlainTextFinal: func(_ context.Context, cc agentrun.ControlContext, text string) (control.Result, error) {
			return control.Result{Kind: control.Final, Final: &agentrun.FinalAnswer{
				StepID: cc.Step.ID, PlainText: true, Answer: text, Output: map[string]interface{}{"summary": text},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.loop = loop
	return h
}

// drive claims the Run as worker and drives one claim. The first claim
// creates the Run's session.
func (h *resumeHarness) drive(worker string) Exit {
	h.t.Helper()
	ctx := context.Background()
	claimed, err := h.raw.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: h.scope, WorkerID: worker, Now: h.clock.Now(), LeaseDuration: resumeLease, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != h.runID {
		h.t.Fatalf("claim by %s = %#v, %v", worker, claimed, err)
	}
	sessions := h.raw.(models.AgentSessionStore)
	if claimed.SessionID == "" {
		if _, err := sessions.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{
			Scope: h.scope, OwnerRunID: claimed.ID, AgentID: "agent-a",
			Lease: models.AgentRunLeaseGuard{WorkerID: worker, Now: h.clock.Now()}, Now: h.clock.Now(),
		}); err != nil {
			h.t.Fatal(err)
		}
		if claimed, err = h.raw.GetAgentRun(ctx, h.scope, claimed.ID); err != nil {
			h.t.Fatal(err)
		}
	}
	session, err := sessions.GetAgentSession(ctx, h.scope, claimed.SessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.loop.Drive(ctx, Claim{Run: claimed, WorkerID: worker, Binding: &agentrun.TurnRunnerBinding{
		Producer: h.producer, Session: session, Tools: h.tools, Final: h.final,
	}})
}

// crashed checks that the first claim died on the armed store, then revives
// the store and lets every lease expire.
func (h *resumeHarness) crashed(exit Exit) {
	h.t.Helper()
	if exit.Reason != ExitFailed || !errors.Is(exit.Err, errLoopCrash) {
		h.t.Fatalf("crashed claim exit = %s, %v; want the simulated crash", exit.Reason, exit.Err)
	}
	h.store.disarm()
	h.store.mu.Lock()
	h.store.beforeRecord, h.store.afterFinish = nil, nil
	h.store.mu.Unlock()
}

func (h *resumeHarness) runNow() *models.AgentRun {
	h.t.Helper()
	run, err := h.raw.GetAgentRun(context.Background(), h.scope, h.runID)
	if err != nil {
		h.t.Fatal(err)
	}
	return run
}

func (h *resumeHarness) rows() []*models.AgentTurn {
	h.t.Helper()
	rows, err := h.raw.ListAgentTurns(context.Background(), models.AgentTurnFilter{Scope: h.scope, RunID: h.runID})
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func (h *resumeHarness) entries(types ...models.SessionEntryType) []*models.SessionEntry {
	h.t.Helper()
	entries, err := h.raw.(models.AgentSessionStore).ListAgentSessionEntries(context.Background(), models.SessionEntryFilter{
		Scope: h.scope, RunID: h.runID, Types: types,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return entries
}

func (h *resumeHarness) expectCompleted(exit Exit, answer string) *models.AgentRun {
	h.t.Helper()
	if exit.Err != nil || exit.Reason != ExitFinal {
		h.t.Fatalf("exit = %s, %v; want final", exit.Reason, exit.Err)
	}
	run := h.runNow()
	if run.Status != models.AgentRunStatusCompleted || run.Output["summary"] != answer {
		h.t.Fatalf("run = %s output %#v; want completed with %q", run.Status, run.Output, answer)
	}
	if calls, posts := h.final.counts(); posts != 1 || calls < 1 {
		h.t.Fatalf("deliveries = %d, posts = %d; want one posted reply", calls, posts)
	}
	return run
}

func TestRunLoopResumeRunningStepWithoutAssistant(t *testing.T) {
	eachLoopStore(t, "resume_no_assistant", func(t *testing.T, store models.KernelStore) {
		h := newResumeHarness(t, store, nil, resumeLease, resumeSay("first try"), resumeSay("done"))
		h.producer.before = func(call int) {
			if call == 0 {
				h.store.arm() // dies after the model call, before the append
			}
		}
		h.crashed(h.drive(resumeWorkerA))
		if got := h.entries(models.SessionEntryAssistant); len(got) != 0 {
			t.Fatalf("assistant entries after the crash = %d; want none", len(got))
		}
		h.clock.Advance(2 * resumeLease)
		h.expectCompleted(h.drive(resumeWorkerB), "done")
		calls := h.producer.calls()
		if len(calls) != 2 || calls[0] != calls[1] {
			t.Fatalf("model calls = %v; want the same step called twice", calls)
		}
		rows := h.rows()
		if len(rows) != 1 || rows[0].ID != calls[0] || rows[0].Status != models.AgentTurnStatusCompleted || rows[0].LeaseOwner != "" && rows[0].LeaseOwner != resumeWorkerB {
			t.Fatalf("rows = %#v; want the one step row completed by the second worker", rows)
		}
	})
}

func TestRunLoopRedoModelCallReusesStepRow(t *testing.T) {
	eachLoopStore(t, "resume_redo", func(t *testing.T, store models.KernelStore) {
		h := newResumeHarness(t, store, &models.BudgetPolicy{MaxTurns: 5}, resumeLease, resumeSay("first try"), resumeSay("done"))
		h.producer.before = func(call int) {
			if call == 0 {
				h.store.arm()
			}
		}
		h.crashed(h.drive(resumeWorkerA))
		crashedRun := h.runNow()
		calls := h.producer.calls()
		if _, ok := crashedRun.BudgetReservations[calls[0]]; !ok || len(crashedRun.BudgetReservations) != 1 {
			t.Fatalf("reservations after the crash = %#v; want one keyed by step %s", crashedRun.BudgetReservations, calls[0])
		}
		h.clock.Advance(2 * resumeLease)
		run := h.expectCompleted(h.drive(resumeWorkerB), "done")
		calls = h.producer.calls()
		if len(calls) != 2 || calls[0] != calls[1] {
			t.Fatalf("model calls = %v; want the same step id redone", calls)
		}
		if rows := h.rows(); len(rows) != 1 || rows[0].Sequence != 1 || run.LastAppliedTurn != 1 {
			t.Fatalf("rows = %d, LastAppliedTurn = %d; want one row applied once", len(rows), run.LastAppliedTurn)
		}
		if run.BudgetUsage.Turns != 1 || len(run.BudgetReservations) != 0 {
			t.Fatalf("budget usage turns = %d, reservations = %#v; want the reused reservation settled as one turn", run.BudgetUsage.Turns, run.BudgetReservations)
		}
	})
}

func TestRunLoopResumeAfterAssistantBeforeRecord(t *testing.T) {
	eachLoopStore(t, "resume_before_record", func(t *testing.T, store models.KernelStore) {
		h := newResumeHarness(t, store, nil, resumeLease, resumeSay("done"))
		h.store.beforeRecord = func() { h.store.arm() }
		h.crashed(h.drive(resumeWorkerA))
		assistants := h.entries(models.SessionEntryAssistant)
		if len(assistants) != 1 {
			t.Fatalf("assistant entries after the crash = %d; want the appended one", len(assistants))
		}
		if rows := h.rows(); len(rows) != 1 || rows[0].AssistantEntryID != "" || rows[0].Status != models.AgentTurnStatusRunning {
			t.Fatalf("row after the crash = %#v; want running with no recorded model output", rows)
		}
		h.clock.Advance(2 * resumeLease)
		h.expectCompleted(h.drive(resumeWorkerB), "done")
		if calls := h.producer.calls(); len(calls) != 1 {
			t.Fatalf("model calls = %d; want no second call", len(calls))
		}
		rows := h.rows()
		if len(rows) != 1 || rows[0].AssistantEntryID != assistants[0].ID || !rows[0].Final || rows[0].StopReason != string(models.ModelStopEnd) {
			t.Fatalf("row = %#v; want RecordStepModel backfilled from the entry", rows[0])
		}
	})
}

func TestRunLoopResumeRunningToolsDelegatesToExecutor(t *testing.T) {
	eachLoopStore(t, "resume_tools", func(t *testing.T, store models.KernelStore) {
		h := newResumeHarness(t, store, nil, resumeLease,
			resumeAct("Looking it up", resumeCall("c1", "lookup", `{"q":"x"}`)), resumeSay("done"))
		h.tools.beforeExecute = func() error {
			h.store.arm()
			return errLoopCrash
		}
		h.crashed(h.drive(resumeWorkerA))
		h.tools.beforeExecute = nil
		tools := h.entries(models.SessionEntryTool)
		if len(tools) != 1 || tools[0].State != models.ToolEntryPending {
			t.Fatalf("tool entries after the crash = %#v; want one pending", tools)
		}
		h.clock.Advance(2 * resumeLease)
		h.expectCompleted(h.drive(resumeWorkerB), "done")
		execute, _, resume := h.tools.counts()
		if execute != 1 || resume != 1 {
			t.Fatalf("execute = %d, resume = %d; want the crashed call resumed through ResumeOpenTools", execute, resume)
		}
		if got := h.tools.resumeEntries[0]; len(got) != 1 || got[0] != tools[0].ID {
			t.Fatalf("resumed entries = %v; want %s", got, tools[0].ID)
		}
		calls := h.producer.calls()
		rows := h.rows()
		if len(calls) != 2 || len(rows) != 2 || calls[0] != rows[0].ID || calls[1] != rows[1].ID {
			t.Fatalf("model calls = %v, rows = %d; want step 1 resumed without a model call, then step 2", calls, len(rows))
		}
		if tools = h.entries(models.SessionEntryTool); tools[0].State != models.ToolEntryCompleted {
			t.Fatalf("tool entry = %s; want completed", tools[0].State)
		}
	})
}

func TestRunLoopResumeStaleStepLeaseRequeues(t *testing.T) {
	eachLoopStore(t, "resume_stale_lease", func(t *testing.T, store models.KernelStore) {
		stepLease := 10 * time.Minute
		h := newResumeHarness(t, store, nil, stepLease, resumeSay("first try"), resumeSay("done"))
		h.producer.before = func(call int) {
			if call == 0 {
				h.store.arm()
			}
		}
		h.crashed(h.drive(resumeWorkerA))
		rows := h.rows()
		if len(rows) != 1 || rows[0].LeaseExpiresAt == nil {
			t.Fatalf("rows after the crash = %#v; want one leased running row", rows)
		}
		expires := *rows[0].LeaseExpiresAt
		h.clock.Advance(2 * resumeLease) // the Run lease expired, the step lease did not
		exit := h.drive(resumeWorkerB)
		if exit.Err != nil || exit.Reason != ExitRequeued {
			t.Fatalf("exit = %s, %v; want requeued", exit.Reason, exit.Err)
		}
		run := h.runNow()
		if run.Status != models.AgentRunStatusQueued || !run.AvailableAt.Equal(expires) {
			t.Fatalf("run = %s available %v; want queued at the step lease expiry %v", run.Status, run.AvailableAt, expires)
		}
		if calls := h.producer.calls(); len(calls) != 1 {
			t.Fatalf("model calls = %d; want none while the dead step lease is live", len(calls))
		}
		// Once the step lease expires the next claim resumes the same row.
		h.clock.Advance(stepLease)
		h.expectCompleted(h.drive(resumeWorkerB), "done")
		if rows := h.rows(); len(rows) != 1 {
			t.Fatalf("rows = %d; want the one resumed row", len(rows))
		}
	})
}

func TestRunLoopResumeAppliesFinishedUnappliedStep(t *testing.T) {
	eachLoopStore(t, "resume_unapplied", func(t *testing.T, store models.KernelStore) {
		h := newResumeHarness(t, store, &models.BudgetPolicy{MaxTurns: 5}, resumeLease,
			resumeAct("Looking it up", resumeCall("c1", "lookup", `{}`)), resumeSay("done"))
		h.store.afterFinish = func(turn *models.AgentTurn) {
			if turn.Sequence == 1 {
				h.store.arm() // dies between FinishTurn and applyStep
			}
		}
		h.crashed(h.drive(resumeWorkerA))
		crashedRun := h.runNow()
		if rows := h.rows(); len(rows) != 1 || rows[0].Status != models.AgentTurnStatusCompleted || crashedRun.LastAppliedTurn != 0 {
			t.Fatalf("after the crash: rows %#v, LastAppliedTurn %d; want one finished unapplied row", rows, crashedRun.LastAppliedTurn)
		}
		h.clock.Advance(2 * resumeLease)
		run := h.expectCompleted(h.drive(resumeWorkerB), "done")
		if run.LastAppliedTurn != 2 || run.BudgetUsage.Turns != 2 || len(run.BudgetReservations) != 0 {
			t.Fatalf("LastAppliedTurn = %d, turns = %d, reservations = %#v; want each step applied once", run.LastAppliedTurn, run.BudgetUsage.Turns, run.BudgetReservations)
		}
		if _, _, resume := h.tools.counts(); resume != 0 {
			t.Fatalf("resume calls = %d; want none (the step's tools had finished)", resume)
		}
		events, err := store.ListActivity(context.Background(), models.ActivityFilter{Scope: h.scope, RunID: h.runID})
		if err != nil {
			t.Fatal(err)
		}
		applied := map[int64]int{}
		for _, event := range events {
			if seq, ok := event.Payload["turnSequence"]; ok {
				applied[toInt64(seq)]++
			}
		}
		if applied[1] != 1 || applied[2] != 1 {
			t.Fatalf("step applications = %v; want each step applied once", applied)
		}
	})
}

func TestRunLoopResumeFinalToolStepDeliversOnce(t *testing.T) {
	finalCall := resumeCall("c1", string(models.ControlToolFinalAnswer), `{"answer":"all done"}`)
	t.Run("crash before settle", func(t *testing.T) {
		eachLoopStore(t, "resume_final_settle", func(t *testing.T, store models.KernelStore) {
			h := newResumeHarness(t, store, nil, resumeLease, resumeAct("", finalCall))
			h.tools.beforeSettle = func() error {
				h.store.arm()
				return errLoopCrash
			}
			h.crashed(h.drive(resumeWorkerA))
			h.tools.beforeSettle = nil
			if calls, _ := h.final.counts(); calls != 0 {
				t.Fatalf("deliveries before the crash = %d; want none", calls)
			}
			h.clock.Advance(2 * resumeLease)
			h.expectCompleted(h.drive(resumeWorkerB), "all done")
			if calls := h.producer.calls(); len(calls) != 1 {
				t.Fatalf("model calls = %d; want the pending final settled without a model call", len(calls))
			}
			if _, _, resume := h.tools.counts(); resume != 1 {
				t.Fatalf("resume calls = %d; want the final entry settled in phase 2", resume)
			}
			assertFinalEntryDelivered(t, h)
		})
	})
	t.Run("crash after the reply was posted", func(t *testing.T) {
		eachLoopStore(t, "resume_final_posted", func(t *testing.T, store models.KernelStore) {
			h := newResumeHarness(t, store, nil, resumeLease, resumeAct("", finalCall))
			h.final.afterPost = func() { h.store.arm() }
			h.crashed(h.drive(resumeWorkerA))
			h.final.afterPost = nil
			h.clock.Advance(2 * resumeLease)
			h.expectCompleted(h.drive(resumeWorkerB), "all done")
			if calls, posts := h.final.counts(); calls != 2 || posts != 1 {
				t.Fatalf("deliveries = %d, posts = %d; want the second delivery to replay the posted reply", calls, posts)
			}
			if calls := h.producer.calls(); len(calls) != 1 {
				t.Fatalf("model calls = %d; want no second model call", len(calls))
			}
			if run := h.runNow(); run.Output["messageId"] != "message-"+h.runID {
				t.Fatalf("output = %#v; want the posted message", run.Output)
			}
			assertFinalEntryDelivered(t, h)
		})
	})
}

// TestRunLoopResumePlainFinalAfterFinish crashes a plain-text final between
// its FinishTurn and its terminal transition: the next claim completes it
// through the final chain (a replayed delivery), not a new model call.
func TestRunLoopResumePlainFinalAfterFinish(t *testing.T) {
	eachLoopStore(t, "resume_plain_final", func(t *testing.T, store models.KernelStore) {
		h := newResumeHarness(t, store, nil, resumeLease, resumeSay("done"))
		h.store.afterFinish = func(*models.AgentTurn) { h.store.arm() }
		h.crashed(h.drive(resumeWorkerA))
		if rows := h.rows(); len(rows) != 1 || rows[0].Status != models.AgentTurnStatusCompleted || h.runNow().LastAppliedTurn != 0 {
			t.Fatalf("after the crash: rows %#v; want the final row finished, unapplied", rows)
		}
		h.clock.Advance(2 * resumeLease)
		run := h.expectCompleted(h.drive(resumeWorkerB), "done")
		if calls := h.producer.calls(); len(calls) != 1 {
			t.Fatalf("model calls = %d; want no second model call", len(calls))
		}
		if calls, posts := h.final.counts(); calls != 2 || posts != 1 {
			t.Fatalf("deliveries = %d, posts = %d; want one post and one replay", calls, posts)
		}
		if run.LastAppliedTurn != 1 || len(h.rows()) != 1 {
			t.Fatalf("LastAppliedTurn = %d, rows = %d; want the one step applied by the terminal transition", run.LastAppliedTurn, len(h.rows()))
		}
	})
}

func assertFinalEntryDelivered(t *testing.T, h *resumeHarness) {
	t.Helper()
	tools := h.entries(models.SessionEntryTool)
	if len(tools) != 1 || tools[0].State != models.ToolEntryCompleted || tools[0].Tool.Result == nil ||
		len(tools[0].Tool.Result.Content) != 1 || !strings.Contains(string(tools[0].Tool.Result.Content[0].Value), finalDeliveredStatus) {
		t.Fatalf("final entry = %#v; want completed delivered", tools)
	}
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return -1
}
