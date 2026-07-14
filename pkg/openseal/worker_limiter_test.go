package openseal

import "testing"

func TestEngineConfiguresOneSharedWorkerConcurrencyBoundary(t *testing.T) {
	engine, err := New(WithWorkerConcurrencyLimit(7))
	if err != nil {
		t.Fatal(err)
	}
	stats := engine.WorkerConcurrencyStats()
	if stats.Limit != 7 || stats.InUse != 0 {
		t.Fatalf("worker concurrency stats = %#v", stats)
	}
	if _, err := New(WithWorkerConcurrencyLimit(0)); err == nil {
		t.Fatal("invalid worker concurrency limit was accepted")
	}
}
