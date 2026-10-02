package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func benchmarkRunEventWaitSQLite(b *testing.B, idle int) (*SQLiteStore, Scope) {
	b.Helper()
	store, err := NewSQLiteStore(filepath.Join(b.TempDir(), "benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	scope := Scope{Kind: "tenant", ID: "benchmark"}
	for index := 0; index < idle; index++ {
		spec := eventWaitContractSpec("reply", fmt.Sprintf("idle-subject-%d", index), eventWaitContractEpoch)
		eventWaitContractCreate(b, store, scope, spec, eventWaitContractEpoch)
	}
	worker := eventWaitContractWorker(b, store, eventWaitContractEpoch, 100)
	for worker.Stats().Claimed < uint64(idle) {
		completed, err := worker.ProcessScope(context.Background(), scope)
		if err != nil || completed != 0 {
			b.Fatalf("park idle waits: completed=%d error=%v", completed, err)
		}
	}
	if scopes, err := store.ListRunEventWaitWorkScopes(context.Background(), eventWaitContractEpoch, 100); err != nil || len(scopes) != 0 {
		b.Fatalf("idle waits remain scheduled: scopes=%v error=%v", scopes, err)
	}
	return store, scope
}

// Setup and wait registration are excluded. Each measured operation durably
// ingests a new observation and resolves its matching continuation while the
// same tenant has many unrelated, dormant waits. This measures real SQLite
// transactions, not selector helpers or an in-memory mock.
func BenchmarkRunEventWaitSQLitePublishResolve(b *testing.B) {
	for _, idle := range []int{100, 10000} {
		b.Run(fmt.Sprintf("idle_%d", idle), func(b *testing.B) {
			store, scope := benchmarkRunEventWaitSQLite(b, idle)
			now := eventWaitContractEpoch.Add(time.Minute)
			worker := eventWaitContractWorker(b, store, now, 100)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				b.StopTimer()
				spec := eventWaitContractSpec("reply", fmt.Sprintf("active-subject-%d", index), eventWaitContractEpoch)
				eventWaitContractCreate(b, store, scope, spec, eventWaitContractEpoch)
				eventWaitContractProcess(b, store, scope, now, 0)
				event := eventWaitContractEvent(scope, spec, fmt.Sprintf("observation-%d", index), now)
				b.StartTimer()
				if !eventWaitContractPublish(b, store, event, now) {
					b.Fatal("fresh observation rejected")
				}
				if completed, err := worker.ProcessScope(context.Background(), scope); err != nil || completed != 1 {
					b.Fatalf("publish/resolution: completed=%d error=%v", completed, err)
				}
			}
		})
	}
}

// Dormant waits should not scale the work done by the host's due-scope scan.
func BenchmarkRunEventWaitSQLiteIdleDiscovery(b *testing.B) {
	for _, idle := range []int{100, 10000} {
		b.Run(fmt.Sprintf("idle_%d", idle), func(b *testing.B) {
			store, _ := benchmarkRunEventWaitSQLite(b, idle)
			now := eventWaitContractEpoch.Add(time.Minute)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				scopes, err := store.ListRunEventWaitWorkScopes(context.Background(), now, 100)
				if err != nil || len(scopes) != 0 {
					b.Fatalf("dormant waits discovered as work: %v, %v", scopes, err)
				}
			}
		})
	}
}

// One authentic observation can satisfy independent Runs, but processing stays
// bounded by the worker's batch size rather than spawning a goroutine per wait.
func BenchmarkRunEventWaitSQLiteFanout100(b *testing.B) {
	store, scope := benchmarkRunEventWaitSQLite(b, 100)
	now := eventWaitContractEpoch.Add(time.Minute)
	worker := eventWaitContractWorker(b, store, now, 25)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		b.StopTimer()
		spec := eventWaitContractSpec("reply", fmt.Sprintf("fanout-%d", index), eventWaitContractEpoch)
		for child := 0; child < 100; child++ {
			eventWaitContractCreate(b, store, scope, spec, eventWaitContractEpoch)
		}
		eventWaitContractProcess(b, store, scope, now, 0)
		event := eventWaitContractEvent(scope, spec, fmt.Sprintf("fanout-observation-%d", index), now)
		b.StartTimer()
		eventWaitContractPublish(b, store, event, now)
		for batch := 0; batch < 4; batch++ {
			if completed, err := worker.ProcessScope(context.Background(), scope); err != nil || completed != 25 {
				b.Fatalf("fanout exceeded/lost a bounded batch: completed=%d error=%v", completed, err)
			}
		}
	}
	b.ReportMetric(100, "resolutions/op")
}
