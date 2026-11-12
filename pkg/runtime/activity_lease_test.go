package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestRunTransitionsRejectStaleLeaseOwners(t *testing.T) {
	tests := []struct {
		name  string
		store func(*testing.T) models.KernelStore
	}{
		{name: "sqlite", store: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "lease.db"))
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
			scope := models.Scope{Kind: "local", ID: "test"}
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
			run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
				AssignedAgentID: "agent", Goal: "work", Source: models.RunSourceObjective,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
				Scope: scope, WorkerID: "owner", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return now.Add(10 * time.Second) }))
			wake := &models.WakeCondition{Type: "event", Reference: "work.ready"}
			_, _, err = activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{
				ExpectedRevision: claimed.Revision, Status: models.AgentRunStatusWaitingForEvent,
				WakeCondition: wake, LeaseOwner: "stale",
			})
			if !errors.Is(err, models.ErrLeaseLost) {
				t.Fatalf("stale transition error = %v", err)
			}
			waiting, event, err := activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{
				ExpectedRevision: claimed.Revision, Status: models.AgentRunStatusWaitingForEvent,
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
