package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestActionClaimsAreAtomicAndRecoverExpiredLeases(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (models.KernelStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.KernelStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "claims.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, cleanup := testCase.open(t)
			defer cleanup()
			now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
			proposal := createDispatchableNestedAction(t, store, now)
			const contenders = 20
			results := make(chan *models.ActionCall, contenders)
			errs := make(chan error, contenders)
			var wg sync.WaitGroup
			for index := 0; index < contenders; index++ {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()
					call, err := store.ClaimNextAction(context.Background(), models.ActionClaim{Scope: proposal.Call.Scope, WorkerID: fmt.Sprintf("worker-%d", index), Now: now.Add(2 * time.Second), LeaseDuration: time.Minute})
					results <- call
					errs <- err
				}(index)
			}
			wg.Wait()
			close(results)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			successes := 0
			var owner string
			for call := range results {
				if call != nil {
					successes++
					owner = call.LeaseOwner
				}
			}
			if successes != 1 {
				t.Fatalf("successful claims = %d, want 1", successes)
			}
			if _, err := store.RenewActionLease(context.Background(), proposal.Call.Scope, proposal.Call.ID, "stale", now.Add(3*time.Second), time.Minute); !errors.Is(err, models.ErrLeaseLost) {
				t.Fatalf("stale renewal error = %v", err)
			}
			renewed, err := store.RenewActionLease(context.Background(), proposal.Call.Scope, proposal.Call.ID, owner, now.Add(3*time.Second), time.Minute)
			if err != nil || renewed.LeaseExpiresAt == nil {
				t.Fatalf("renewal = %#v, %v", renewed, err)
			}
			recovered, err := store.ClaimNextAction(context.Background(), models.ActionClaim{Scope: proposal.Call.Scope, WorkerID: "recovery", Now: renewed.LeaseExpiresAt.Add(time.Millisecond), LeaseDuration: time.Minute})
			if err != nil || recovered == nil || recovered.ID != proposal.Call.ID || recovered.Attempt != 2 || recovered.LeaseOwner != "recovery" {
				t.Fatalf("recovery = %#v, %v", recovered, err)
			}
		})
	}
}

// createDispatchableNestedAction records release.deploy as a ready nested
// call of step-1, as ProposeNested does for code in the chat's VM, for a Run
// that agent-worker holds for a minute. Action workers dispatch nested calls;
// loop calls are only ever claimed by their own loop.
func createDispatchableNestedAction(t *testing.T, store models.KernelStore, now time.Time) *models.ActionProposalResult {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "claims"}
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	at := now.Add(time.Second)
	stepLease := now.Add(time.Minute)
	if _, err := store.CreateAgentTurn(ctx, &models.AgentTurn{
		ID: "step-1", Scope: scope, RunID: run.ID, Status: models.AgentTurnStatusRunning, Producer: models.StepProducerModel, LeaseOwner: "agent-worker", LeaseExpiresAt: &stepLease,
		Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	call := &models.ActionCall{
		ID: "call", Scope: scope, RunID: run.ID, TurnID: "step-1", DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0",
		Action: "deploy", Origin: models.ActionOriginVM, OriginRef: "command-1", Status: models.ActionCallStatusReady,
		Arguments: map[string]interface{}{"environment": "production"}, MaxAttempts: 1, AvailableAt: at, Revision: 1, CreatedAt: at, UpdatedAt: at,
	}
	result, err := store.CreateActionProposal(ctx, models.ActionProposalRecord{
		Call: call, Lease: &models.AgentRunLeaseGuard{WorkerID: "agent-worker", Now: at},
		Nested: &models.NestedActionFence{WorkerID: "agent-worker", Now: at, MaxCalls: action.MaxNestedActionCallsPerTurn, MaxConcurrent: action.MaxConcurrentNestedActionCalls},
		Event:  &models.ActivityEvent{ID: "proposed-call", Scope: scope, RunID: run.ID, TurnID: "step-1", EventType: "action.proposed", Summary: "Proposed deploy", CreatedAt: at},
	})
	if err != nil || !result.Created {
		t.Fatalf("nested proposal = %#v, %v", result, err)
	}
	return result
}
