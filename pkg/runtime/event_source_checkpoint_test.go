package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEventSourceCheckpointIsBoundedAndSurvivesRestart(t *testing.T) {
	for _, fixture := range []struct {
		name string
		open func(*testing.T) (EventSourceCheckpointStore, func() EventSourceCheckpointStore, func())
	}{
		{name: "memory", open: func(*testing.T) (EventSourceCheckpointStore, func() EventSourceCheckpointStore, func()) {
			store := NewMemoryStore(10)
			return store, func() EventSourceCheckpointStore { return store }, func() {}
		}},
		{name: "sqlite", open: func(t *testing.T) (EventSourceCheckpointStore, func() EventSourceCheckpointStore, func()) {
			path := filepath.Join(t.TempDir(), "event-source.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return store, func() EventSourceCheckpointStore {
				_ = store.Close()
				store, err = NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				return store
			}, func() { _ = store.Close() }
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store, restart, closeStore := fixture.open(t)
			defer closeStore()
			service := NewEventSourceCheckpointService(store)
			scope := Scope{Kind: "tenant", ID: "acme"}
			first, err := service.Advance(context.Background(), AdvanceEventSourceCheckpointRequest{
				Scope: scope, Source: "kubernetes:cluster:7", SubscriptionID: "namespace:production",
				EventIDs: []string{"uid-a:1", "uid-a:1"}, Cursor: "rv-1", Watermark: time.Unix(20, 0),
			})
			if err != nil || first.Revision != 1 || len(first.RecentEventIDs) != 1 {
				t.Fatalf("first=%#v err=%v", first, err)
			}

			store = restart()
			service = NewEventSourceCheckpointService(store)
			loaded, err := service.Get(context.Background(), scope, first.Source, first.SubscriptionID)
			if err != nil || loaded.Revision != 1 || loaded.Cursor != "rv-1" || len(loaded.RecentEventIDs) != 1 {
				t.Fatalf("loaded=%#v err=%v", loaded, err)
			}

			ids := make([]string, MaximumEventSourceRecentIDs+5)
			for index := range ids {
				ids[index] = eventCheckpointTestID(index)
			}
			next, err := service.Advance(context.Background(), AdvanceEventSourceCheckpointRequest{
				Scope: scope, Source: first.Source, SubscriptionID: first.SubscriptionID, ExpectedRevision: 1,
				EventIDs: ids, Watermark: time.Unix(10, 0),
			})
			if err != nil || next.Revision != 2 || len(next.RecentEventIDs) != MaximumEventSourceRecentIDs || next.Watermark != first.Watermark || next.Cursor != "rv-1" {
				t.Fatalf("next revision=%v recent=%d watermark=%v cursor=%q err=%v", next.Revision, len(next.RecentEventIDs), next.Watermark, next.Cursor, err)
			}
			if next.RecentEventIDs[0] != eventCheckpointTestID(5) {
				t.Fatalf("oldest retained id = %q", next.RecentEventIDs[0])
			}
		})
	}
}

func TestEventSourceCheckpointCASSerializesReplicas(t *testing.T) {
	store := NewMemoryStore(10)
	service := NewEventSourceCheckpointService(store)
	scope := Scope{Kind: "tenant", ID: "race"}
	requests := []AdvanceEventSourceCheckpointRequest{
		{Scope: scope, Source: "webhook:billing", SubscriptionID: "fraud", EventIDs: []string{"event-a"}},
		{Scope: scope, Source: "webhook:billing", SubscriptionID: "fraud", EventIDs: []string{"event-b"}},
	}
	var wait sync.WaitGroup
	errs := make(chan error, len(requests))
	for _, request := range requests {
		wait.Add(1)
		go func(req AdvanceEventSourceCheckpointRequest) {
			defer wait.Done()
			_, err := service.Advance(context.Background(), req)
			errs <- err
		}(request)
	}
	wait.Wait()
	close(errs)
	succeeded, conflicted := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrEventSourceCheckpointConflict):
			conflicted++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func TestEventSourceCheckpointRejectsUnsafeIdentity(t *testing.T) {
	service := NewEventSourceCheckpointService(NewMemoryStore(10))
	_, err := service.Advance(context.Background(), AdvanceEventSourceCheckpointRequest{
		Scope: Scope{Kind: "tenant", ID: "acme"}, Source: "webhook", SubscriptionID: "bad\nidentity", EventIDs: []string{"event"},
	})
	if !errors.Is(err, ErrInvalidEventSourceCheckpoint) {
		t.Fatalf("unsafe identity error = %v", err)
	}
}

func eventCheckpointTestID(index int) string {
	const digits = "0123456789"
	value := "event-"
	if index == 0 {
		return value + "0"
	}
	var reversed [20]byte
	position := len(reversed)
	for index > 0 {
		position--
		reversed[position] = digits[index%10]
		index /= 10
	}
	return value + string(reversed[position:])
}
