package runtime

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestSQLiteRunTransitionLeaseUsesAbsoluteTimeAndExactExpiry(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "lease-offset.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scope := models.Scope{Kind: "local", ID: "lease-offset"}
	now := time.Date(2026, 10, 3, 0, 0, 0, 123456789, time.UTC)
	offset := time.FixedZone("caller", 5*60*60+30*60)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	at := now
	activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return at }))
	for _, tc := range []struct {
		name string
		at   time.Time
		lost bool
	}{
		{name: "one nanosecond before expiry in another offset", at: now.Add(time.Minute - time.Nanosecond).In(offset)},
		{name: "exact expiry", at: now.Add(time.Minute).In(offset), lost: true},
		{name: "after expiry", at: now.Add(time.Minute + time.Nanosecond).In(offset), lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: tc.name, Source: models.RunSourceManual})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: scope, WorkerID: "owner", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil || claimed.ID != run.ID {
				t.Fatalf("claim: %#v %v", claimed, err)
			}
			at = tc.at
			updated, _, err := activity.TransitionRun(t.Context(), scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: claimed.Revision, Status: models.AgentRunStatusWaitingForEvent, LeaseOwner: "owner", WakeCondition: &models.WakeCondition{Type: "event", Reference: "ready"}})
			if tc.lost {
				if !errors.Is(err, models.ErrLeaseLost) {
					t.Fatalf("expiry must reject transition: %#v %v", updated, err)
				}
				// Prevent the expired fixture from competing for the next claim.
				_, _, err = activity.TransitionRun(t.Context(), scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: claimed.Revision, Status: models.AgentRunStatusCanceled})
				if err != nil {
					t.Fatal(err)
				}
			} else if err != nil || updated.Status != models.AgentRunStatusWaitingForEvent || updated.LeaseOwner != "" {
				t.Fatalf("valid lease rejected: %#v %v", updated, err)
			}
		})
	}
}
