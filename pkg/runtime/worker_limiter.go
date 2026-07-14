package runtime

import (
	"context"
	"errors"
	"hash/fnv"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerLimiter is a process-wide admission boundary shared by otherwise
// independent scope and worker-family pools. Durable leases still coordinate
// replicas; this limiter prevents one process from multiplying local database
// and provider pressure by tenant count.
type WorkerLimiter struct {
	limit        int64
	permits      chan struct{}
	inUse        atomic.Int64
	maximumInUse atomic.Int64
	waitCount    atomic.Int64
	acquireCount atomic.Int64
}

func workerPollDelay(base time.Duration, consecutiveFailures int, seed string) time.Duration {
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	if consecutiveFailures < 0 {
		consecutiveFailures = 0
	}
	exponent := consecutiveFailures
	if exponent > 6 {
		exponent = 6
	}
	delay := base * time.Duration(1<<exponent)
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(seed))
	_, _ = hash.Write([]byte(strconv.Itoa(consecutiveFailures)))
	permille := int64(750 + hash.Sum32()%501)
	delay = time.Duration(int64(delay) * permille / 1000)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func waitForWorkerPoll(ctx context.Context, wake <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

type WorkerLimiterStats struct {
	Limit        int64
	InUse        int64
	MaximumInUse int64
	WaitCount    int64
	AcquireCount int64
}

func NewWorkerLimiter(limit int) (*WorkerLimiter, error) {
	if limit <= 0 || limit > 4096 {
		return nil, errors.New("worker concurrency limit must be between 1 and 4096")
	}
	return &WorkerLimiter{limit: int64(limit), permits: make(chan struct{}, limit)}, nil
}

func (l *WorkerLimiter) acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case l.permits <- struct{}{}:
	default:
		l.waitCount.Add(1)
		select {
		case l.permits <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	inUse := l.inUse.Add(1)
	l.acquireCount.Add(1)
	for maximum := l.maximumInUse.Load(); inUse > maximum && !l.maximumInUse.CompareAndSwap(maximum, inUse); maximum = l.maximumInUse.Load() {
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.inUse.Add(-1)
			<-l.permits
		})
	}, nil
}

func (l *WorkerLimiter) Stats() WorkerLimiterStats {
	if l == nil {
		return WorkerLimiterStats{}
	}
	return WorkerLimiterStats{
		Limit: l.limit, InUse: l.inUse.Load(), MaximumInUse: l.maximumInUse.Load(),
		WaitCount: l.waitCount.Load(), AcquireCount: l.acquireCount.Load(),
	}
}
