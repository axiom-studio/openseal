package runtime

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerLimiterBoundsConcurrentFamiliesAndExposesContention(t *testing.T) {
	limiter, err := NewWorkerLimiter(2)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	hold := make(chan struct{})
	var group sync.WaitGroup
	var active atomic.Int64
	var maximum atomic.Int64
	for index := 0; index < 20; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			release, acquireErr := limiter.acquire(t.Context())
			if acquireErr != nil {
				t.Error(acquireErr)
				return
			}
			current := active.Add(1)
			for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
			}
			<-hold
			active.Add(-1)
			release()
			release()
		}()
	}
	close(start)
	deadline := time.Now().Add(time.Second)
	for {
		stats := limiter.Stats()
		if stats.AcquireCount == 2 && stats.WaitCount == 18 {
			break
		}
		if time.Now().After(deadline) {
			close(hold)
			group.Wait()
			t.Fatalf("workers did not reach deterministic contention: %#v", stats)
		}
		time.Sleep(time.Millisecond)
	}
	close(hold)
	group.Wait()
	stats := limiter.Stats()
	if maximum.Load() != 2 || stats.Limit != 2 || stats.MaximumInUse != 2 || stats.InUse != 0 || stats.AcquireCount != 20 || stats.WaitCount < 18 {
		t.Fatalf("maximum=%d stats=%#v", maximum.Load(), stats)
	}
}

func TestWorkerLimiterRejectsInvalidLimits(t *testing.T) {
	for _, limit := range []int{-1, 0, 4097} {
		if _, err := NewWorkerLimiter(limit); err == nil {
			t.Fatalf("limit %d was accepted", limit)
		}
	}
}

func TestWorkerLimiterDoesNotStarveAWaitingScopeBehindHotWork(t *testing.T) {
	limiter, err := NewWorkerLimiter(1)
	if err != nil {
		t.Fatal(err)
	}
	initialRelease, err := limiter.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hotFirst, hotContinue, hotSecond := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		release, acquireErr := limiter.acquire(t.Context())
		if acquireErr != nil {
			return
		}
		close(hotFirst)
		<-hotContinue
		release()
		release, acquireErr = limiter.acquire(t.Context())
		if acquireErr != nil {
			return
		}
		close(hotSecond)
		release()
	}()
	waitForLimiterWaiters(t, limiter, 1)
	coolAcquired, coolRelease := make(chan struct{}), make(chan struct{})
	go func() {
		release, acquireErr := limiter.acquire(t.Context())
		if acquireErr != nil {
			return
		}
		close(coolAcquired)
		<-coolRelease
		release()
	}()
	waitForLimiterWaiters(t, limiter, 2)
	initialRelease()
	select {
	case <-hotFirst:
	case <-time.After(time.Second):
		t.Fatal("first hot scope acquisition timed out")
	}
	close(hotContinue)
	select {
	case <-coolAcquired:
	case <-hotSecond:
		t.Fatal("hot scope reacquired before an already-waiting scope")
	case <-time.After(time.Second):
		t.Fatal("waiting scope was starved")
	}
	close(coolRelease)
	select {
	case <-hotSecond:
	case <-time.After(time.Second):
		t.Fatal("hot scope did not continue after the waiting scope released")
	}
}

func waitForLimiterWaiters(t *testing.T, limiter *WorkerLimiter, count int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for limiter.Stats().WaitCount < count {
		if time.Now().After(deadline) {
			t.Fatalf("limiter did not observe %d waiters: %#v", count, limiter.Stats())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWorkerPollDelayJittersAndExponentiallyBacksOff(t *testing.T) {
	base := time.Second
	previous := time.Duration(0)
	for failures := 0; failures <= 8; failures++ {
		delay := workerPollDelay(base, failures, "tenant-7-agent-work")
		if delay <= previous && delay < 30*time.Second {
			t.Fatalf("failure %d delay=%s previous=%s", failures, delay, previous)
		}
		if delay > 30*time.Second || delay != workerPollDelay(base, failures, "tenant-7-agent-work") {
			t.Fatalf("failure %d delay=%s", failures, delay)
		}
		previous = delay
	}
	if workerPollDelay(base, 0, "tenant-a") == workerPollDelay(base, 0, "tenant-b") {
		t.Fatal("distinct worker identities received identical jitter")
	}
}
