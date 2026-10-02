package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSkillRuntimeMaintenanceMutationSerializesTakeoverAndRejectsStaleOwner(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(SkillRuntimeMaintenanceStore)
		request := skillRuntimeMaintenanceEmptyRequest()
		gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		mutationDone := make(chan error, 1)
		go func() {
			mutationDone <- store.WithSkillRuntimeMaintenanceMutation(t.Context(), gate, func(ctx context.Context) error {
				proof, ok := SkillRuntimeMaintenanceFromContext(ctx)
				if !ok || proof.Revision != gate.Revision || ValidateSkillRuntimeMaintenanceContext(ctx, gate, time.Now().UTC()) != nil {
					return ErrSkillRuntimeMaintenanceConflict
				}
				deadline, ok := ctx.Deadline()
				if !ok || deadline.After(time.Now().Add(15*time.Second)) || !deadline.Before(gate.LeaseExpiresAt) {
					return errors.New("external mutation did not receive a bounded owner context")
				}
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		select {
		case <-entered:
		case err := <-mutationDone:
			t.Fatalf("valid external mutation did not start: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("valid external mutation did not start")
		}
		takeoverStarted, takeoverDone := make(chan struct{}), make(chan error, 1)
		contender := request
		contender.Owner, contender.Now = "replacement-controller", gate.LeaseExpiresAt.Add(time.Second)
		go func() {
			close(takeoverStarted)
			_, err := store.AcquireSkillRuntimeMaintenance(t.Context(), contender)
			takeoverDone <- err
		}()
		<-takeoverStarted
		select {
		case err := <-takeoverDone:
			close(release)
			t.Fatalf("takeover passed a live external publication fence: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		close(release)
		if err := <-mutationDone; err != nil {
			t.Fatal(err)
		}
		if err := <-takeoverDone; err != nil {
			t.Fatal(err)
		}
		called := false
		err = store.WithSkillRuntimeMaintenanceMutation(t.Context(), gate, func(context.Context) error { called = true; return nil })
		if called || !errors.Is(err, ErrSkillRuntimeMaintenance) {
			t.Fatalf("stale owner reached external mutation: called=%v error=%v", called, err)
		}
	})
}

func TestSkillRuntimeMaintenanceMutationRechecksLeaseAfterWaitingForFence(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(SkillRuntimeMaintenanceStore)
		request := skillRuntimeMaintenanceEmptyRequest()
		request.LeaseDuration = 600 * time.Millisecond
		gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- store.WithSkillRuntimeMaintenanceMutation(t.Context(), gate, func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		select {
		case <-entered:
		case err := <-first:
			t.Fatalf("first mutation did not acquire: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("first mutation did not acquire")
		}
		called := false
		err = store.WithSkillRuntimeMaintenanceMutation(t.Context(), gate, func(context.Context) error { called = true; return nil })
		if called || err == nil {
			t.Fatalf("owner waiting past safe lease window reached external mutation: called=%v error=%v", called, err)
		}
		if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first callback exceeded its live lease: %v", err)
		}
		current, err := store.GetSkillRuntimeMaintenance(t.Context(), gate.Scope, gate.SkillID)
		if err != nil || !current.Active || current.Revision != gate.Revision {
			t.Fatalf("callback timeout reopened or changed the gate: %#v %v", current, err)
		}
	})
}
