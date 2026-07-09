package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRunTransitionsRejectStaleLeaseOwners(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore(10) }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "lease.db"))
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
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
				AssignedAgentID: "agent", Goal: "work", Source: RunSourceObjective,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
				Scope: scope, WorkerID: "owner", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			activity := NewRunActivityService(store, store)
			activity.now = func() time.Time { return now.Add(10 * time.Second) }
			wake := &WakeCondition{Type: "event", Reference: "work.ready"}
			_, _, err = activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
				ExpectedRevision: claimed.Revision, Status: AgentRunStatusWaitingForEvent,
				WakeCondition: wake, LeaseOwner: "stale",
			})
			if !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("stale transition error = %v", err)
			}
			waiting, event, err := activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
				ExpectedRevision: claimed.Revision, Status: AgentRunStatusWaitingForEvent,
				WakeCondition: wake, LeaseOwner: "owner",
			})
			if err != nil {
				t.Fatal(err)
			}
			if waiting.LeaseOwner != "" || waiting.LeaseExpiresAt != nil || event.Sequence != 1 {
				t.Fatalf("waiting transition did not release lease atomically: run=%#v event=%#v", waiting, event)
			}
		})
	}
}
