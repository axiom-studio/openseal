package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

type turnStepBackend struct {
	name string
	open func(*testing.T) models.KernelStore
}

func turnStepBackends() []turnStepBackend {
	return []turnStepBackend{
		{name: "sqlite", open: func(t *testing.T) models.KernelStore {
			store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "turn-steps.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
		{name: "postgres", open: func(t *testing.T) models.KernelStore {
			pgSchema := storetest.NewPostgresSchema(t, "openseal_turn_steps_")
			dsn := pgSchema.DSN
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			store, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(pgSchema.Name))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
				_ = store.Close()
			})
			return store
		}},
	}
}

func forEachTurnStepBackend(t *testing.T, test func(t *testing.T, store models.KernelStore)) {
	for _, backend := range turnStepBackends() {
		t.Run(backend.name, func(t *testing.T) { test(t, backend.open(t)) })
	}
}

func runningStepRun(t *testing.T, store models.KernelStore, scope models.Scope) *models.AgentRun {
	t.Helper()
	ctx := t.Context()
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "work in steps", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err = activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func beginModelStep(t *testing.T, turns *activity.AgentTurnService, scope models.Scope, runID string) *models.AgentTurn {
	t.Helper()
	turn, err := turns.BeginTurn(t.Context(), activity.BeginAgentTurnRequest{
		Scope: scope, RunID: runID, SessionID: "session-1", Producer: models.StepProducerModel,
		WorkerID: "worker-a", LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func narrationEvents(t *testing.T, store models.KernelStore, scope models.Scope, runID string) []*models.ActivityEvent {
	t.Helper()
	events, err := store.ListActivity(t.Context(), models.ActivityFilter{Scope: scope, RunID: runID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var result []*models.ActivityEvent
	for _, event := range events {
		if event.EventType == activity.ActivityEventTurnNarration {
			result = append(result, event)
		}
	}
	return result
}

func TestRecordStepModelEmitsNarrationEvent(t *testing.T) {
	forEachTurnStepBackend(t, func(t *testing.T, store models.KernelStore) {
		ctx := t.Context()
		scope := models.Scope{Kind: "local", ID: "steps"}
		run := runningStepRun(t, store, scope)
		turns := activity.NewAgentTurnService(store, store)
		step := beginModelStep(t, turns, scope, run.ID)
		if step.Producer != models.StepProducerModel || step.SessionID != "session-1" {
			t.Fatalf("begin did not keep producer/session: %+v", step)
		}
		recorded, err := turns.RecordStepModel(ctx, scope, step.ID, activity.StepModelRecord{
			ExpectedRevision: step.Revision, WorkerID: "worker-a", ModelProvider: "openai", Model: "gpt-test",
			AssistantEntryID: "assistant:" + step.ID, OutputSummary: "  Checking the open issues on the tracker  ",
			ToolCallCount: 2, StopReason: "tool_calls", Usage: models.TurnUsage{InputTokens: 10, OutputTokens: 4},
		})
		if err != nil {
			t.Fatal(err)
		}
		if recorded.Revision != step.Revision+1 || recorded.Status != models.AgentTurnStatusRunning || recorded.LeaseOwner != "worker-a" {
			t.Fatalf("record changed lifecycle: %+v", recorded)
		}
		stored, err := turns.GetTurn(ctx, scope, step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.OutputSummary != "Checking the open issues on the tracker" || stored.ToolCallCount != 2 ||
			stored.StopReason != "tool_calls" || stored.Final || stored.AssistantEntryID != "assistant:"+step.ID ||
			stored.ModelProvider != "openai" || stored.Model != "gpt-test" || stored.Usage.InputTokens != 10 ||
			stored.Revision != recorded.Revision {
			t.Fatalf("stored step = %+v", stored)
		}
		events := narrationEvents(t, store, scope, run.ID)
		if len(events) != 1 {
			t.Fatalf("narration events = %d, want 1", len(events))
		}
		event := events[0]
		if event.TurnID != step.ID || event.Summary != stored.OutputSummary || event.Visibility != models.ActivityVisibilityScope ||
			event.Severity != models.ActivitySeverityInfo || event.AgentID != "agent" || event.Sequence == 0 ||
			fmt.Sprint(event.Payload["stepSequence"]) != fmt.Sprint(step.Sequence) {
			t.Fatalf("narration event = %+v", event)
		}

		// Finishing keeps the recorded narration.
		finished, err := turns.FinishTurn(ctx, scope, step.ID, activity.FinishAgentTurnRequest{
			ExpectedRevision: recorded.Revision, Status: models.AgentTurnStatusCompleted, WorkerID: "worker-a",
			Usage: models.TurnUsage{InputTokens: 10, OutputTokens: 4, NativeOperations: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		if finished.OutputSummary != stored.OutputSummary || finished.FailureCode != "" {
			t.Fatalf("finish lost the narration: %+v", finished)
		}

		// A step without tool calls is final and has no narration event.
		final := beginModelStep(t, turns, scope, run.ID)
		recordedFinal, err := turns.RecordStepModel(ctx, scope, final.ID, activity.StepModelRecord{
			ExpectedRevision: final.Revision, WorkerID: "worker-a", AssistantEntryID: "assistant:" + final.ID, StopReason: "end",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !recordedFinal.Final || recordedFinal.OutputSummary != "" {
			t.Fatalf("final step = %+v", recordedFinal)
		}
		if got := len(narrationEvents(t, store, scope, run.ID)); got != 1 {
			t.Fatalf("final step emitted narration, events = %d", got)
		}
		if _, err := turns.RecordStepModel(ctx, scope, final.ID, activity.StepModelRecord{
			ExpectedRevision: recordedFinal.Revision, WorkerID: "worker-a", OutputSummary: "text", StopReason: "end",
		}); err == nil {
			t.Fatal("narration without tool calls was accepted")
		}
		failed, err := turns.FinishTurn(ctx, scope, final.ID, activity.FinishAgentTurnRequest{
			ExpectedRevision: recordedFinal.Revision, Status: models.AgentTurnStatusFailed, WorkerID: "worker-a", FailureCode: "producer_failed",
		})
		if err != nil {
			t.Fatal(err)
		}
		if failed.FailureCode != "producer_failed" {
			t.Fatalf("failure code = %q", failed.FailureCode)
		}
	})
}

func TestRecordStepModelLeaseLost(t *testing.T) {
	forEachTurnStepBackend(t, func(t *testing.T, store models.KernelStore) {
		ctx := t.Context()
		scope := models.Scope{Kind: "local", ID: "steps"}
		run := runningStepRun(t, store, scope)
		turns := activity.NewAgentTurnService(store, store)
		step := beginModelStep(t, turns, scope, run.ID)
		record := activity.StepModelRecord{
			ExpectedRevision: step.Revision, WorkerID: "worker-a", OutputSummary: "Looking it up", ToolCallCount: 1, StopReason: "tool_calls",
		}

		foreign := record
		foreign.WorkerID = "worker-b"
		if _, err := turns.RecordStepModel(ctx, scope, step.ID, foreign); !errors.Is(err, models.ErrTurnLeaseHeld) {
			t.Fatalf("foreign worker record error = %v, want ErrTurnLeaseHeld", err)
		}

		late := activity.NewAgentTurnService(store, store, models.WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) }))
		if _, err := late.RecordStepModel(ctx, scope, step.ID, record); !errors.Is(err, models.ErrTurnLeaseHeld) {
			t.Fatalf("expired lease record error = %v, want ErrTurnLeaseHeld", err)
		}

		// The store fences the lease inside its transaction too: another worker
		// took the expired step over after the caller read it.
		next := models.CloneAgentTurn(step)
		next.OutputSummary, next.ToolCallCount = "Looking it up", 1
		next.UpdatedAt = time.Now().Add(2 * time.Minute)
		taken, err := store.ClaimAgentTurn(ctx, scope, step.ID, "worker-b", time.Now().Add(2*time.Minute), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		next.Revision = taken.Revision + 1
		event := &models.ActivityEvent{ID: uuid.NewString(), Scope: scope, RunID: run.ID, TurnID: step.ID, EventType: activity.ActivityEventTurnNarration,
			Severity: models.ActivitySeverityInfo, Summary: "Looking it up", Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now()}
		if err := store.RecordAgentTurnStepModel(ctx, next, taken.Revision, []*models.ActivityEvent{event}); !errors.Is(err, models.ErrTurnLeaseHeld) {
			t.Fatalf("store record after takeover error = %v, want ErrTurnLeaseHeld", err)
		}
		if got := len(narrationEvents(t, store, scope, run.ID)); got != 0 {
			t.Fatalf("lease-lost record wrote %d narration events", got)
		}
		current, err := turns.GetTurn(ctx, scope, step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.OutputSummary != "" || current.LeaseOwner != "worker-b" {
			t.Fatalf("lease-lost record changed the step: %+v", current)
		}
	})
}

func TestRecordStepModelRevisionConflict(t *testing.T) {
	forEachTurnStepBackend(t, func(t *testing.T, store models.KernelStore) {
		ctx := t.Context()
		scope := models.Scope{Kind: "local", ID: "steps"}
		run := runningStepRun(t, store, scope)
		turns := activity.NewAgentTurnService(store, store)
		step := beginModelStep(t, turns, scope, run.ID)
		renewed, err := turns.RenewTurn(ctx, scope, step.ID, "worker-a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := turns.RecordStepModel(ctx, scope, step.ID, activity.StepModelRecord{
			ExpectedRevision: step.Revision, WorkerID: "worker-a", OutputSummary: "Stale", ToolCallCount: 1, StopReason: "tool_calls",
		}); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("stale revision record error = %v, want ErrRevisionConflict", err)
		}

		// Store-level CAS: a heartbeat landed between the caller's read and write.
		next := models.CloneAgentTurn(step)
		next.OutputSummary, next.ToolCallCount = "Stale", 1
		next.UpdatedAt = time.Now()
		next.Revision = step.Revision + 1
		event := &models.ActivityEvent{ID: uuid.NewString(), Scope: scope, RunID: run.ID, TurnID: step.ID, EventType: activity.ActivityEventTurnNarration,
			Severity: models.ActivitySeverityInfo, Summary: "Stale", Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now()}
		if err := store.RecordAgentTurnStepModel(ctx, next, step.Revision, []*models.ActivityEvent{event}); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("store stale record error = %v, want ErrRevisionConflict", err)
		}
		if got := len(narrationEvents(t, store, scope, run.ID)); got != 0 {
			t.Fatalf("conflicting record wrote %d narration events", got)
		}
		if _, err := turns.RecordStepModel(ctx, scope, step.ID, activity.StepModelRecord{
			ExpectedRevision: renewed.Revision, WorkerID: "worker-a", OutputSummary: "Fresh", ToolCallCount: 1, StopReason: "tool_calls",
		}); err != nil {
			t.Fatal(err)
		}
		if got := narrationEvents(t, store, scope, run.ID); len(got) != 1 || got[0].Summary != "Fresh" {
			t.Fatalf("narration after fresh record = %+v", got)
		}
	})
}

func TestRecordStepModelTruncates2000(t *testing.T) {
	forEachTurnStepBackend(t, func(t *testing.T, store models.KernelStore) {
		ctx := t.Context()
		scope := models.Scope{Kind: "local", ID: "steps"}
		run := runningStepRun(t, store, scope)
		turns := activity.NewAgentTurnService(store, store)
		step := beginModelStep(t, turns, scope, run.ID)
		// "€" is three bytes, so a byte cut at 2000 lands mid-rune.
		narration := "a" + strings.Repeat("€", 1000)
		recorded, err := turns.RecordStepModel(ctx, scope, step.ID, activity.StepModelRecord{
			ExpectedRevision: step.Revision, WorkerID: "worker-a", OutputSummary: narration, ToolCallCount: 1, StopReason: "tool_calls",
		})
		if err != nil {
			t.Fatal(err)
		}
		const narrationLimit = 2000 // the step narration bound
		if len(recorded.OutputSummary) > narrationLimit || len(recorded.OutputSummary) < narrationLimit-3 ||
			!utf8.ValidString(recorded.OutputSummary) || !strings.HasPrefix(narration, recorded.OutputSummary) {
			t.Fatalf("narration was not cut on a rune boundary within 2000 bytes: %d bytes", len(recorded.OutputSummary))
		}
		stored, err := turns.GetTurn(ctx, scope, step.ID)
		if err != nil {
			t.Fatal(err)
		}
		events := narrationEvents(t, store, scope, run.ID)
		if stored.OutputSummary != recorded.OutputSummary || len(events) != 1 || events[0].Summary != recorded.OutputSummary {
			t.Fatalf("stored narration and event summary differ from the cut narration")
		}
	})
}

func TestListAgentTurnsDescending(t *testing.T) {
	forEachTurnStepBackend(t, func(t *testing.T, store models.KernelStore) {
		ctx := t.Context()
		scope := models.Scope{Kind: "local", ID: "steps"}
		run := runningStepRun(t, store, scope)
		turns := activity.NewAgentTurnService(store, store)
		for i := 0; i < 4; i++ {
			step := beginModelStep(t, turns, scope, run.ID)
			if _, err := turns.FinishTurn(ctx, scope, step.ID, activity.FinishAgentTurnRequest{
				ExpectedRevision: step.Revision, Status: models.AgentTurnStatusFailed, WorkerID: "worker-a", FailureCode: "host_unavailable",
			}); err != nil {
				t.Fatal(err)
			}
		}
		sequences := func(filter models.AgentTurnFilter) []int64 {
			t.Helper()
			filter.Scope, filter.RunID = scope, run.ID
			listed, err := turns.ListTurns(ctx, filter)
			if err != nil {
				t.Fatal(err)
			}
			result := make([]int64, 0, len(listed))
			for _, turn := range listed {
				result = append(result, turn.Sequence)
			}
			return result
		}
		if got := fmt.Sprint(sequences(models.AgentTurnFilter{Descending: true, Limit: 3})); got != "[4 3 2]" {
			t.Fatalf("descending = %s", got)
		}
		if got := fmt.Sprint(sequences(models.AgentTurnFilter{Descending: true, AfterSequence: 2})); got != "[4 3]" {
			t.Fatalf("descending after 2 = %s", got)
		}
		if got := fmt.Sprint(sequences(models.AgentTurnFilter{Limit: 3})); got != "[1 2 3]" {
			t.Fatalf("ascending = %s", got)
		}
		if got := fmt.Sprint(sequences(models.AgentTurnFilter{Descending: true, Statuses: []models.AgentTurnStatus{models.AgentTurnStatusFailed}, Limit: 1})); got != "[4]" {
			t.Fatalf("descending failed = %s", got)
		}
	})
}
