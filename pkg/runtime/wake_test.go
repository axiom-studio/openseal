package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWakeSignalsRequeueRunsExactlyOnce(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "wake.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := test.store(t)
			ctx := context.Background()
			scope := Scope{Kind: "local", ID: "test"}
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			portfolio := NewPortfolioService(store)
			portfolio.now = func() time.Time { return now }
			run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
				Goal: "wait", Source: RunSourceEvent,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
				Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			activity := NewRunActivityService(store, store)
			activity.now = func() time.Time { return now.Add(time.Second) }
			waiting, _, err := activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
				ExpectedRevision: claimed.Revision, Status: AgentRunStatusWaitingForEvent, LeaseOwner: "worker",
				WakeCondition: &WakeCondition{Type: "event", Reference: "jobs", Predicate: map[string]interface{}{"state": "ready"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			wake := NewAgentRunWakeService(store, store)
			wrong, err := wake.Wake(ctx, WakeSignal{
				ID: "signal-wrong", Scope: scope, Type: "event", Reference: "jobs",
				Payload: map[string]interface{}{"state": "pending"}, At: now.Add(2 * time.Second),
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(wrong.Runs) != 0 {
				t.Fatalf("predicate mismatch woke a run: %#v", wrong)
			}

			const contenders = 12
			results := make(chan *WakeResult, contenders)
			errs := make(chan error, contenders)
			var wg sync.WaitGroup
			for i := 0; i < contenders; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					result, wakeErr := wake.Wake(ctx, WakeSignal{
						ID: "signal-ready", Scope: scope, Type: "event", Reference: "jobs",
						Payload: map[string]interface{}{"state": "ready", "caller": fmt.Sprintf("%d", i)},
						At:      now.Add(3 * time.Second),
					})
					results <- result
					errs <- wakeErr
				}(i)
			}
			wg.Wait()
			close(results)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			wokenCount := 0
			for result := range results {
				wokenCount += len(result.Runs)
			}
			if wokenCount != 1 {
				t.Fatalf("successful wake transitions = %d, want 1", wokenCount)
			}
			loaded, err := portfolio.GetAgentRun(ctx, scope, waiting.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Status != AgentRunStatusQueued || loaded.WakeCondition != nil || loaded.LastWakeSignalID != "signal-ready" ||
				!loaded.AvailableAt.Equal(now.Add(3*time.Second)) || loaded.LeaseOwner != "" {
				t.Fatalf("run was not requeued durably: %#v", loaded)
			}
			events, err := activity.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[1].EventType != "run.woken" || events[1].CorrelationID != "signal-ready" {
				t.Fatalf("unexpected wake activity: %#v", events)
			}
		})
	}
}

func TestWakeConditionHonorsDueTimeReferenceAndPredicate(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	condition := &WakeCondition{
		Type: "timer", WakeAt: timePointer(now), Reference: "review",
		Predicate: map[string]interface{}{"approved": true},
	}
	if wakeConditionMatches(condition, WakeSignal{
		Type: "timer", Reference: "review", Payload: map[string]interface{}{"approved": true}, At: now.Add(-time.Second),
	}) {
		t.Fatal("condition matched before its due time")
	}
	if !wakeConditionMatches(condition, WakeSignal{
		Type: "timer", Reference: "review", Payload: map[string]interface{}{"approved": true}, At: now,
	}) {
		t.Fatal("due matching condition did not match")
	}
}

func TestWakeSignalSurvivesSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wake-restart.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Goal: "sleep", Source: RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	due := now.Add(time.Hour)
	activity := NewRunActivityService(store, store)
	activity.now = func() time.Time { return now.Add(time.Second) }
	_, _, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: claimed.Revision, Status: AgentRunStatusSleeping, LeaseOwner: "worker",
		WakeCondition: &WakeCondition{Type: "timer", WakeAt: &due, Reference: "schedule"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := NewAgentRunWakeService(reopened, reopened).WakeDueTimers(ctx, scope, due)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].Run.ID != run.ID || result.Runs[0].Run.Status != AgentRunStatusQueued {
		t.Fatalf("sleeping run was not woken after restart: %#v", result)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
