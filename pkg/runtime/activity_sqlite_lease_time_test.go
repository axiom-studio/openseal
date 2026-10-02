package runtime

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteRunTransitionLeaseUsesAbsoluteTimeAndExactExpiry(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "lease-offset.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scope := Scope{Kind: "local", ID: "lease-offset"}
	now := time.Date(2026, 10, 3, 0, 0, 0, 123456789, time.UTC)
	offset := time.FixedZone("caller", 5*60*60+30*60)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	activity := NewRunActivityService(store, store)
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
			run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: tc.name, Source: RunSourceManual})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "owner", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
			if err != nil || claimed == nil || claimed.ID != run.ID {
				t.Fatalf("claim: %#v %v", claimed, err)
			}
			activity.now = func() time.Time { return tc.at }
			updated, _, err := activity.TransitionRun(t.Context(), scope, run.ID, RunTransitionRequest{ExpectedRevision: claimed.Revision, Status: AgentRunStatusWaitingForEvent, LeaseOwner: "owner", WakeCondition: &WakeCondition{Type: "event", Reference: "ready"}})
			if tc.lost {
				if !errors.Is(err, ErrLeaseLost) {
					t.Fatalf("expiry must reject transition: %#v %v", updated, err)
				}
				// Prevent the expired fixture from competing for the next claim.
				_, _, err = activity.TransitionRun(t.Context(), scope, run.ID, RunTransitionRequest{ExpectedRevision: claimed.Revision, Status: AgentRunStatusCanceled})
				if err != nil {
					t.Fatal(err)
				}
			} else if err != nil || updated.Status != AgentRunStatusWaitingForEvent || updated.LeaseOwner != "" {
				t.Fatalf("valid lease rejected: %#v %v", updated, err)
			}
		})
	}
}
