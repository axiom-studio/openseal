package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type RunEventWaitWorkerOptions struct {
	WorkerID      string
	LeaseDuration time.Duration
	BatchSize     int
	Concurrency   int
}

type RunEventWaitWorkerStats struct {
	Claimed  uint64
	Matched  uint64
	TimedOut uint64
	Idle     uint64
	Errors   uint64
}

// RunEventWaitWorker performs only bounded database work. It allocates no
// goroutine or model session per outstanding wait. Leases coordinate replicas;
// each batch gets a fresh attempt identity so a reclaimed lease fences old work.
type RunEventWaitWorker struct {
	store    RunEventWaitStore
	options  RunEventWaitWorkerOptions
	now      func() time.Time
	claimed  atomic.Uint64
	matched  atomic.Uint64
	timedOut atomic.Uint64
	idle     atomic.Uint64
	errors   atomic.Uint64
	limiter  *WorkerLimiter
}

func (w *RunEventWaitWorker) SetWorkerLimiter(limiter *WorkerLimiter) { w.limiter = limiter }

func NewRunEventWaitWorker(store RunEventWaitStore, options RunEventWaitWorkerOptions) (*RunEventWaitWorker, error) {
	if store == nil {
		return nil, errors.New("run event wait store is required")
	}
	if !validOpaqueIdentifier(options.WorkerID, 180) || options.LeaseDuration < 3*time.Second || options.LeaseDuration > 5*time.Minute || options.BatchSize < 1 || options.BatchSize > 100 || options.Concurrency < 1 || options.Concurrency > 32 || options.Concurrency > options.BatchSize {
		return nil, errors.New("event wait worker requires bounded identity, lease, batch and concurrency")
	}
	return &RunEventWaitWorker{store: store, options: options, now: time.Now}, nil
}

func (w *RunEventWaitWorker) WorkScopes(ctx context.Context, limit int) ([]Scope, error) {
	if w == nil || w.store == nil {
		return nil, errors.New("event wait worker is not configured")
	}
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidRunEventWait
	}
	release, err := w.limiter.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return w.store.ListRunEventWaitWorkScopes(ctx, w.now().UTC(), limit)
}

func (w *RunEventWaitWorker) ProcessScope(ctx context.Context, scope Scope) (int, error) {
	if w == nil || w.store == nil {
		return 0, errors.New("event wait worker is not configured")
	}
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, w.options.LeaseDuration*2/3)
	defer cancel()
	workerID := w.options.WorkerID + ":" + uuid.NewString()
	release, err := w.limiter.acquire(ctx)
	if err != nil {
		return 0, err
	}
	waits, err := w.store.ClaimRunEventWaits(ctx, ClaimRunEventWaitsRequest{Scope: scope, WorkerID: workerID, Now: w.now().UTC(), LeaseDuration: w.options.LeaseDuration, Limit: w.options.BatchSize})
	release()
	if err != nil {
		w.errors.Add(1)
		return 0, err
	}
	w.claimed.Add(uint64(len(waits)))
	if len(waits) == 0 {
		return 0, nil
	}
	jobs := make(chan *RunEventWait)
	var group sync.WaitGroup
	var completed atomic.Int64
	var errorMu sync.Mutex
	var failures []error
	for i := 0; i < w.options.Concurrency; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for wait := range jobs {
				release, admissionErr := w.limiter.acquire(ctx)
				if admissionErr != nil {
					w.errors.Add(1)
					errorMu.Lock()
					failures = append(failures, admissionErr)
					errorMu.Unlock()
					continue
				}
				if wait.LeaseExpiresAt == nil {
					release()
					w.errors.Add(1)
					errorMu.Lock()
					failures = append(failures, ErrLeaseLost)
					errorMu.Unlock()
					continue
				}
				result, processErr := w.store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: wait.RunID, Key: wait.Spec.Key, WorkerID: workerID, Now: w.now().UTC(), LeaseExpiresAt: *wait.LeaseExpiresAt})
				release()
				if processErr != nil {
					w.errors.Add(1)
					errorMu.Lock()
					failures = append(failures, processErr)
					errorMu.Unlock()
					continue
				}
				if result == nil || result.Wait == nil {
					w.idle.Add(1)
					continue
				}
				switch result.Wait.Status {
				case RunEventWaitMatched:
					w.matched.Add(1)
					completed.Add(1)
				case RunEventWaitTimedOut:
					w.timedOut.Add(1)
					completed.Add(1)
				default:
					w.idle.Add(1)
				}
			}
		}()
	}
dispatch:
	for _, wait := range waits {
		select {
		case jobs <- wait:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	group.Wait()
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err())
	}
	return int(completed.Load()), errors.Join(failures...)
}

func (w *RunEventWaitWorker) PruneExpiredEvents(ctx context.Context, limit int) (int, error) {
	if w == nil || w.store == nil {
		return 0, errors.New("event wait worker is not configured")
	}
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalidRunEventWait
	}
	ctx, cancel := context.WithTimeout(ctx, w.options.LeaseDuration*2/3)
	defer cancel()
	release, err := w.limiter.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	return w.store.PruneExpiredRunEvents(ctx, w.now().UTC().Add(-RunEventRetention), limit)
}

func (w *RunEventWaitWorker) Stats() RunEventWaitWorkerStats {
	if w == nil {
		return RunEventWaitWorkerStats{}
	}
	return RunEventWaitWorkerStats{Claimed: w.claimed.Load(), Matched: w.matched.Load(), TimedOut: w.timedOut.Load(), Idle: w.idle.Load(), Errors: w.errors.Load()}
}
