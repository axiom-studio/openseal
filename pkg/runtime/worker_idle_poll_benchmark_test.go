package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// The reference reproduces the former independent idle polling shape using
// the same canonical RunOnce path. These are claim-count measurements, not
// serving CPU or conversation-latency benchmarks.
func BenchmarkActionWorkerIdleClaims(b *testing.B) {
	const interval = 5 * time.Millisecond
	const observation = 120 * time.Millisecond
	for _, workers := range []int{1, 8, 32} {
		for _, coalesced := range []bool{false, true} {
			mode := "independent_reference"
			if coalesced {
				mode = "coalesced"
			}
			b.Run(fmt.Sprintf("%s/workers_%d", mode, workers), func(b *testing.B) {
				var totalClaims, totalHydration int64
				var observed time.Duration
				for range b.N {
					store := &countingActionPoolStore{MemoryStore: NewMemoryStore()}
					catalog := skill.NewCatalog()
					dispatcher := ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
						return nil, errors.New("idle benchmark cannot dispatch")
					})
					scope := Scope{Kind: "tenant", ID: "idle-benchmark"}
					ctx, cancel := context.WithCancel(b.Context())
					var stop func()
					started := time.Now()
					if coalesced {
						pool, err := NewActionWorkerPool(store, catalog, nil, dispatcher, nil,
							ActionWorkerConfig{Scope: scope, Concurrency: workers, PollInterval: interval})
						if err != nil {
							b.Fatal(err)
						}
						pool.Start(ctx)
						stop = pool.Stop
					} else {
						worker := NewActionWorker(store, catalog, nil, dispatcher)
						var wg sync.WaitGroup
						for index := range workers {
							wg.Add(1)
							go func() {
								defer wg.Done()
								workerID := fmt.Sprintf("reference-%d", index)
								for ctx.Err() == nil {
									if _, err := worker.RunOnce(ctx, scope, workerID, time.Second); err != nil && ctx.Err() == nil {
										b.Error(err)
										return
									}
									if !waitForWorkerPoll(ctx, nil, workerPollDelay(interval, 0, workerID)) {
										return
									}
								}
							}()
						}
						stop = wg.Wait
					}
					time.Sleep(observation)
					cancel()
					stop()
					observed += time.Since(started)
					totalClaims += store.claims.Load()
					totalHydration += store.hydration.Load()
				}
				b.ReportMetric(float64(totalClaims)/observed.Seconds(), "claims/s")
				b.ReportMetric(float64(totalHydration)/observed.Seconds(), "hydrations/s")
			})
		}
	}
}
