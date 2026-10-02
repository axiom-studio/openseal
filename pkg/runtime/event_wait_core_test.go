package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunEventWaitBoundsNumericWork(t *testing.T) {
	spec := eventWaitContractSpec("bounded", "resource", eventWaitContractEpoch)
	for _, number := range []json.Number{"1e100000000", "1e-100000000", json.Number(strings.Repeat("9", 4097)), "NaN"} {
		spec.Attributes = map[string]interface{}{"number": number}
		if err := spec.Validate(); !errors.Is(err, ErrInvalidRunEventWait) {
			t.Fatalf("accepted unbounded number %q: %v", number, err)
		}
		if runEventScalarEqual(number, number) {
			t.Fatalf("unbounded number reached numeric comparison: %q", number)
		}
	}
	for _, number := range []json.Number{"9007199254740993", "1e4096", "1e-4096"} {
		spec.Attributes = map[string]interface{}{"number": number}
		if err := spec.Validate(); err != nil || !runEventScalarEqual(number, number) {
			t.Fatalf("bounded exact number rejected %q: %v", number, err)
		}
	}
}

func TestRunEventWaitObservationKeepsAcquisitionAndPrecision(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "precise-acquisition"}
		spec := eventWaitContractSpec("precise", "resource", eventWaitContractEpoch)
		spec.Attributes["sequence"] = json.Number("9007199254740993")
		run := eventWaitContractCreate(t, fixture.store, scope, spec, spec.After)
		event := eventWaitContractEvent(scope, spec, "precise-event", spec.After.Add(time.Second))
		event.Actor = ActivityActor{Type: "callback", ID: "registration-one"}
		event.Payload["sequence"] = json.Number("9007199254740993")
		service := NewRunEventWaitService(fixture.store)
		service.now = func() time.Time { return spec.Deadline.Add(time.Hour) }
		if inserted, err := service.PublishReceivedAt(t.Context(), event, spec.Deadline.Add(-time.Second)); err != nil || !inserted {
			t.Fatalf("on-time observation failed: %v", err)
		}
		event.Actor.ID = "registration-two"
		if inserted, err := service.PublishReceivedAt(t.Context(), event, spec.Deadline.Add(time.Hour)); err != nil || inserted {
			t.Fatalf("acquisition provenance changed immutable identity: inserted=%v err=%v", inserted, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		eventWaitContractProcess(t, fixture.store, scope, spec.Deadline.Add(time.Hour), 1)
		resumed, err := fixture.store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		resolution := resumed.Checkpoint[runEventWaitCheckpointKey].(map[string]interface{})
		if resolution["status"] != string(RunEventWaitMatched) {
			t.Fatalf("dispatch time changed on-time arrival: %#v", resolution)
		}
		observed := resolution["event"].(map[string]interface{})
		for _, section := range []string{"attributes", "payload"} {
			value := observed[section].(map[string]interface{})["sequence"]
			if fmt.Sprint(value) != "9007199254740993" {
				t.Fatalf("%s lost precision after durable continuation: %#v", section, value)
			}
		}
	})
}

type eventWaitAdmissionStore struct {
	*MemoryStore
	entered chan struct{}
	proceed chan struct{}
	active  atomic.Int64
	maximum atomic.Int64
}

func (s *eventWaitAdmissionStore) ClaimRunEventWaits(_ context.Context, request ClaimRunEventWaitsRequest) ([]*RunEventWait, error) {
	expires := request.Now.Add(request.LeaseDuration)
	waits := make([]*RunEventWait, 8)
	for i := range waits {
		waits[i] = &RunEventWait{RunID: fmt.Sprint(i), Spec: RunEventWaitSpec{Key: "wait"}, LeaseExpiresAt: &expires}
	}
	return waits, nil
}

func (s *eventWaitAdmissionStore) ProcessRunEventWait(ctx context.Context, _ ProcessRunEventWaitRequest) (*RunEventWaitResult, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for maximum := s.maximum.Load(); active > maximum && !s.maximum.CompareAndSwap(maximum, active); maximum = s.maximum.Load() {
	}
	s.entered <- struct{}{}
	select {
	case <-s.proceed:
		return &RunEventWaitResult{Wait: &RunEventWait{Status: RunEventWaitMatched}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRunEventWaitWorkersShareAdmissionAcrossScopes(t *testing.T) {
	store := &eventWaitAdmissionStore{MemoryStore: NewMemoryStore(), entered: make(chan struct{}, 32), proceed: make(chan struct{})}
	limiter, err := NewWorkerLimiter(2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var group sync.WaitGroup
	failures := make(chan error, 3)
	for i := 0; i < 3; i++ {
		worker, err := NewRunEventWaitWorker(store, RunEventWaitWorkerOptions{WorkerID: fmt.Sprint("worker-", i), LeaseDuration: time.Minute, BatchSize: 8, Concurrency: 8})
		if err != nil {
			t.Fatal(err)
		}
		worker.SetWorkerLimiter(limiter)
		group.Add(1)
		go func(i int) {
			defer group.Done()
			count, err := worker.ProcessScope(ctx, Scope{Kind: "tenant", ID: fmt.Sprint(i)})
			if err != nil || count != 8 {
				failures <- fmt.Errorf("completed=%d error=%v", count, err)
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-store.entered:
		case <-ctx.Done():
			t.Fatal("workers did not reach admission limit")
		}
	}
	close(store.proceed)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if maximum := store.maximum.Load(); maximum > 2 || maximum < 1 {
		t.Fatalf("worker families exceeded shared admission: %d", maximum)
	}
	if stats := limiter.Stats(); stats.MaximumInUse != 2 || stats.InUse != 0 {
		t.Fatalf("admission permits leaked or multiplied: %#v", stats)
	}
}
