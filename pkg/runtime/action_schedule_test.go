package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestActionClaimsAreAtomicAndRecoverExpiredLeases(t *testing.T) {
	for _, testCase := range []struct {
		name string
		open func(*testing.T) (KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func()) { return NewMemoryStore(20), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "claims.db"))
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
			_, proposal := createRunnableAction(t, store, now)
			const contenders = 20
			results := make(chan *ActionCall, contenders)
			errs := make(chan error, contenders)
			var wg sync.WaitGroup
			for index := 0; index < contenders; index++ {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()
					call, err := store.ClaimNextAction(context.Background(), ActionClaim{Scope: proposal.Call.Scope, WorkerID: fmt.Sprintf("worker-%d", index), Now: now.Add(2 * time.Second), LeaseDuration: time.Minute})
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
			if _, err := store.RenewActionLease(context.Background(), proposal.Call.Scope, proposal.Call.ID, "stale", now.Add(3*time.Second), time.Minute); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("stale renewal error = %v", err)
			}
			renewed, err := store.RenewActionLease(context.Background(), proposal.Call.Scope, proposal.Call.ID, owner, now.Add(3*time.Second), time.Minute)
			if err != nil || renewed.LeaseExpiresAt == nil {
				t.Fatalf("renewal = %#v, %v", renewed, err)
			}
			recovered, err := store.ClaimNextAction(context.Background(), ActionClaim{Scope: proposal.Call.Scope, WorkerID: "recovery", Now: renewed.LeaseExpiresAt.Add(time.Millisecond), LeaseDuration: time.Minute})
			if err != nil || recovered == nil || recovered.ID != proposal.Call.ID || recovered.Attempt != 2 || recovered.LeaseOwner != "recovery" {
				t.Fatalf("recovery = %#v, %v", recovered, err)
			}
		})
	}
}
