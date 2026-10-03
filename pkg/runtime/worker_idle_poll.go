package runtime

import (
	"sync"
)

// workerPollSignal broadcasts latency hints without losing a notification
// between an empty durable claim and the following wait. Periodic polling is
// still authoritative for missed notifications and process restarts.
type workerPollSignal struct {
	mu         sync.Mutex
	generation uint64
	wake       chan struct{}
}

func newWorkerPollSignal() *workerPollSignal {
	return &workerPollSignal{wake: make(chan struct{})}
}

func (p *workerPollSignal) snapshot() (uint64, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.generation, p.wake
}

func (p *workerPollSignal) notify() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.generation++
	close(p.wake)
	p.wake = make(chan struct{})
}
