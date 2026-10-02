package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These contracts use the same public commands and durable scheduling boundary
// across stores. They intentionally do not inspect wait tables or projections.
type eventWaitContractStore interface {
	RunCommandStore
	RunEventWaitStore
	AgentRunScheduleStore
}

type eventWaitContractFixture struct {
	store  eventWaitContractStore
	reopen func() eventWaitContractStore
}

func eventWaitContractFixtures(t *testing.T, test func(*testing.T, eventWaitContractFixture)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, eventWaitContractFixture{store: NewMemoryStore()}) })
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "contract.db")
		current, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		test(t, eventWaitContractFixture{store: current, reopen: func() eventWaitContractStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return current
		}})
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set OPENSEAL_TEST_POSTGRES_DSN for the shared PostgreSQL contract")
		}
		schema := "event_wait_contract_" + uuid.NewString()[:8]
		current, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = current.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+current.quotedSchema()+` CASCADE`)
			_ = current.Close()
		})
		test(t, eventWaitContractFixture{store: current, reopen: func() eventWaitContractStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			return current
		}})
	})
}

var eventWaitContractEpoch = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func eventWaitContractSpec(key, subject string, at time.Time) RunEventWaitSpec {
	return RunEventWaitSpec{
		Key: key, Type: "conversation.message.received", Source: RunEventBindingSource("agent-one", "connection-one", "messages"),
		Subject: subject, Attributes: map[string]interface{}{"externalParticipantId": "person-one", "externalThreadId": "thread-one"},
		After: at, Deadline: at.Add(time.Hour),
	}
}

func eventWaitContractEvent(scope Scope, spec RunEventWaitSpec, id string, occurredAt time.Time) EventEnvelope {
	attributes := make(map[string]interface{}, len(spec.Attributes)+3)
	for key, value := range spec.Attributes {
		attributes[key] = value
	}
	attributes["deploymentId"], attributes["bindingId"], attributes["adapterId"] = "agent-one", "connection-one", "messages"
	return EventEnvelope{
		ID: id, Scope: scope, Type: spec.Type, Source: spec.Source, Subject: spec.Subject, OccurredAt: occurredAt,
		Attributes: attributes, Payload: map[string]interface{}{"text": "My tasks are ready", "reply": map[string]interface{}{"priority": "high"}},
	}
}

func eventWaitContractCreate(t testing.TB, store eventWaitContractStore, scope Scope, spec RunEventWaitSpec, at time.Time) *AgentRun {
	return eventWaitContractCreateForAgent(t, store, scope, spec, "agent-one", at)
}

func eventWaitContractCreateForAgent(t testing.TB, store eventWaitContractStore, scope Scope, spec RunEventWaitSpec, agentID string, at time.Time) *AgentRun {
	t.Helper()
	commands := NewRunCommandService(store)
	commands.now = func() time.Time { return at }
	created, err := commands.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID}, AssignedAgentID: agentID,
		Goal: "Wait for the requested response, then report it", Source: RunSourceChat,
		WakeCondition:  &WakeCondition{Type: "event", EventWait: &spec},
		Checkpoint:     map[string]interface{}{"reportConversationId": "original-conversation", "nextStep": "report-reply"},
		IdempotencyKey: "event-wait-contract:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Run.Status != AgentRunStatusWaitingForEvent {
		t.Fatalf("creation queued blocked work: %s", created.Run.Status)
	}
	return created.Run
}

func eventWaitContractPublish(t testing.TB, store RunEventWaitStore, event EventEnvelope, receivedAt time.Time) bool {
	t.Helper()
	service := NewRunEventWaitService(store)
	service.now = func() time.Time { return receivedAt }
	inserted, err := service.Publish(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	return inserted
}

func eventWaitContractWorker(t testing.TB, store RunEventWaitStore, at time.Time, batch int) *RunEventWaitWorker {
	t.Helper()
	worker, err := NewRunEventWaitWorker(store, RunEventWaitWorkerOptions{
		WorkerID: "contract-worker", LeaseDuration: time.Minute, BatchSize: batch, Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return at }
	return worker
}

func eventWaitContractProcess(t testing.TB, store RunEventWaitStore, scope Scope, at time.Time, want int) {
	t.Helper()
	completed, err := eventWaitContractWorker(t, store, at, 100).ProcessScope(context.Background(), scope)
	if err != nil || completed != want {
		t.Fatalf("completed=%d error=%v, want %d", completed, err, want)
	}
}

func TestRunEventWaitContractEarlyReplyResumeSameExecutionOnce(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "early-contract"}
		spec := eventWaitContractSpec("reply-one", "dm-one", eventWaitContractEpoch)
		event := eventWaitContractEvent(scope, spec, "reply-one", eventWaitContractEpoch.Add(time.Second))
		if !eventWaitContractPublish(t, fixture.store, event, eventWaitContractEpoch.Add(2*time.Second)) {
			t.Fatal("first observed reply was discarded")
		}
		run := eventWaitContractCreate(t, fixture.store, scope, spec, eventWaitContractEpoch)
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		if eventWaitContractPublish(t, fixture.store, event, eventWaitContractEpoch.Add(3*time.Second)) {
			t.Fatal("provider redelivery was considered a second response")
		}
		changed := event
		changed.Payload = map[string]interface{}{"text": "a changed immutable response"}
		service := NewRunEventWaitService(fixture.store)
		if _, err := service.Publish(t.Context(), changed); !errors.Is(err, ErrRunEventConflict) {
			t.Fatalf("conflicting event identity accepted: %v", err)
		}
		now := eventWaitContractEpoch.Add(4 * time.Second)
		eventWaitContractProcess(t, fixture.store, scope, now, 1)
		eventWaitContractProcess(t, fixture.store, scope, now, 0)
		resumed, err := fixture.store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || resumed == nil || resumed.ID != run.ID || resumed.RootRunID != run.RootRunID || resumed.Status != AgentRunStatusQueued || resumed.Revision != run.Revision+1 {
			t.Fatalf("original execution was not resumed exactly once: %#v, %v", resumed, err)
		}
		if resumed.Checkpoint["reportConversationId"] != "original-conversation" || resumed.Checkpoint["nextStep"] != "report-reply" {
			t.Fatal("saved continuation/report destination was lost")
		}
		resolution, ok := resumed.Checkpoint["lastEventWait"].(map[string]interface{})
		if !ok || resolution["status"] != "matched" || resolution["key"] != spec.Key {
			t.Fatalf("missing continuation outcome: %#v", resumed.Checkpoint)
		}
		observed, ok := resolution["event"].(map[string]interface{})
		if !ok || observed["id"] != event.ID {
			t.Fatalf("wrong reply resumed the continuation: %#v", resolution)
		}
		payload, ok := observed["payload"].(map[string]interface{})
		if !ok || payload["text"] != "My tasks are ready" {
			t.Fatalf("reply content was not durable: %#v", observed)
		}
		// The ordinary agent scheduler sees one continuation, without a second
		// Run or duplicated outgoing/report opportunity on redelivery.
		claimed, err := fixture.store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "report-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("resumed work was not executable: %#v, %v", claimed, err)
		}
		second, err := fixture.store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "other-report-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if err != nil || second != nil {
			t.Fatalf("duplicate continuation became executable: %#v, %v", second, err)
		}
	})
}

func TestRunEventWaitContractCommandsBufferResumeAndCancel(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "commands-contract"}
		spec := eventWaitContractSpec("reply", "pause-dm", eventWaitContractEpoch)
		run := eventWaitContractCreate(t, fixture.store, scope, spec, eventWaitContractEpoch)
		commands := NewRunCommandService(fixture.store)
		commands.now = func() time.Time { return eventWaitContractEpoch.Add(time.Second) }
		paused, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: run.ID, Kind: AgentRunCommandPause, ExpectedRevision: run.Revision})
		if err != nil {
			t.Fatal(err)
		}
		event := eventWaitContractEvent(scope, spec, "while-paused", eventWaitContractEpoch.Add(2*time.Second))
		eventWaitContractPublish(t, fixture.store, event, event.OccurredAt)
		eventWaitContractProcess(t, fixture.store, scope, event.OccurredAt, 0)
		commands.now = func() time.Time { return eventWaitContractEpoch.Add(3 * time.Second) }
		resumed, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: run.ID, Kind: AgentRunCommandResume, ExpectedRevision: paused.Run.Revision})
		if err != nil || resumed.Run.ID != run.ID {
			t.Fatalf("resume: %#v, %v", resumed, err)
		}
		eventWaitContractProcess(t, fixture.store, scope, resumed.Run.UpdatedAt, 1)
		secondSpec := eventWaitContractSpec("canceled-reply", "cancel-dm", eventWaitContractEpoch)
		canceledRun := eventWaitContractCreate(t, fixture.store, scope, secondSpec, eventWaitContractEpoch)
		_, err = commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: canceledRun.ID, Kind: AgentRunCommandCancel, ExpectedRevision: canceledRun.Revision})
		if err != nil {
			t.Fatal(err)
		}
		eventWaitContractPublish(t, fixture.store, eventWaitContractEvent(scope, secondSpec, "after-cancel", resumed.Run.UpdatedAt), resumed.Run.UpdatedAt)
		eventWaitContractProcess(t, fixture.store, scope, resumed.Run.UpdatedAt, 0)
		stored, err := fixture.store.GetAgentRun(t.Context(), scope, canceledRun.ID)
		if err != nil || stored.Status != AgentRunStatusCanceled || stored.Checkpoint["lastEventWait"] != nil {
			t.Fatalf("a reply revived canceled work: %#v, %v", stored, err)
		}
	})
}

func TestRunEventWaitContractCorrelationAndAgentIsolation(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "isolation-contract"}
		spec := eventWaitContractSpec("reply", "dm-one", eventWaitContractEpoch)
		run := eventWaitContractCreate(t, fixture.store, scope, spec, eventWaitContractEpoch)
		// Park the wait. Publishing unrelated observations must not poll or wake it.
		eventWaitContractProcess(t, fixture.store, scope, eventWaitContractEpoch, 0)
		now := eventWaitContractEpoch.Add(time.Minute)
		for _, mismatch := range []string{"scope", "source", "subject", "participant", "thread", "deployment", "binding", "adapter", "too-old"} {
			event := eventWaitContractEvent(scope, spec, mismatch, now)
			switch mismatch {
			case "scope":
				event.Scope.ID = "another-tenant"
			case "source":
				event.Source = RunEventBindingSource("agent-two", "connection-one", "messages")
				event.Attributes["deploymentId"] = "agent-two"
			case "subject":
				event.Subject = "dm-two"
			case "participant":
				event.Attributes["externalParticipantId"] = "person-two"
			case "thread":
				event.Attributes["externalThreadId"] = "thread-two"
			case "deployment":
				event.Attributes["deploymentId"] = "agent-two"
			case "binding":
				event.Attributes["bindingId"] = "connection-two"
			case "adapter":
				event.Attributes["adapterId"] = "other-adapter"
			case "too-old":
				event.OccurredAt = spec.After.Add(-time.Nanosecond)
			}
			if mismatch == "deployment" || mismatch == "binding" || mismatch == "adapter" {
				service := NewRunEventWaitService(fixture.store)
				service.now = func() time.Time { return now }
				if _, err := service.Publish(t.Context(), event); !errors.Is(err, ErrInvalidRunEventWait) {
					t.Fatalf("inconsistent binding identity was accepted (%s): %v", mismatch, err)
				}
			} else {
				eventWaitContractPublish(t, fixture.store, event, now)
			}
			eventWaitContractProcess(t, fixture.store, scope, now, 0)
		}
		stored, err := fixture.store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || stored.Status != AgentRunStatusWaitingForEvent || stored.Revision != run.Revision {
			t.Fatalf("unrelated or foreign input resumed work: %#v, %v", stored, err)
		}
		valid := eventWaitContractEvent(scope, spec, "valid", now)
		eventWaitContractPublish(t, fixture.store, valid, now)
		eventWaitContractProcess(t, fixture.store, scope, now, 1)
		foreign := eventWaitContractCreateForAgent(t, fixture.store, scope, spec, "agent-two", eventWaitContractEpoch)
		eventWaitContractProcess(t, fixture.store, scope, now, 0)
		stored, err = fixture.store.GetAgentRun(t.Context(), scope, foreign.ID)
		if err != nil || stored.Status != AgentRunStatusWaitingForEvent || stored.Checkpoint["lastEventWait"] != nil {
			t.Fatalf("another agent consumed the bound connector's observation: %#v, %v", stored, err)
		}
		idle := eventWaitContractWorker(t, fixture.store, now, 100)
		for attempt := 0; attempt < 10; attempt++ {
			if completed, err := idle.ProcessScope(t.Context(), scope); err != nil || completed != 0 {
				t.Fatalf("idle continuation unexpectedly became work: completed=%d error=%v", completed, err)
			}
		}
		if idle.Stats().Claimed != 0 {
			t.Fatalf("dormant waits consumed worker claims: %#v", idle.Stats())
		}
	})
}

func TestRunEventWaitContractTimeoutRejectsLateReceiptAndNeverReusesReply(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		scope := Scope{Kind: "tenant", ID: "deadline-contract"}
		spec := eventWaitContractSpec("first", "deadline-dm", eventWaitContractEpoch)
		run := eventWaitContractCreate(t, fixture.store, scope, spec, eventWaitContractEpoch)
		event := eventWaitContractEvent(scope, spec, "reply", spec.After.Add(time.Second))
		eventWaitContractPublish(t, fixture.store, event, spec.Deadline.Add(time.Nanosecond))
		eventWaitContractProcess(t, fixture.store, scope, spec.Deadline.Add(time.Second), 1)
		stored, err := fixture.store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || stored.Checkpoint["lastEventWait"].(map[string]interface{})["status"] != "timed_out" {
			t.Fatalf("late observation beat the deadline: %#v, %v", stored, err)
		}
		// Independently exercise a loop: a fresh wait key must wait for a fresh
		// observation even when it intentionally keeps the same selector/window.
		loopSpec := eventWaitContractSpec("first", "loop-dm", eventWaitContractEpoch)
		loop := eventWaitContractCreate(t, fixture.store, scope, loopSpec, eventWaitContractEpoch)
		now := eventWaitContractEpoch.Add(time.Minute)
		eventWaitContractPublish(t, fixture.store, eventWaitContractEvent(scope, loopSpec, "loop-reply", now), now)
		eventWaitContractProcess(t, fixture.store, scope, now, 1)
		loop, err = fixture.store.GetAgentRun(t.Context(), scope, loop.ID)
		if err != nil {
			t.Fatal(err)
		}
		loopSpec.Key = "second"
		loop.Status, loop.WakeCondition = AgentRunStatusWaitingForEvent, &WakeCondition{Type: "event", EventWait: &loopSpec}
		expected := loop.Revision
		loop.Revision++
		loop.UpdatedAt = now
		_, err = fixture.store.UpdateAgentRunWithEvent(t.Context(), loop, expected, &ActivityEvent{ID: uuid.NewString(), Scope: scope, RunID: loop.ID, EventType: "run.waiting", Summary: "Await another response", CreatedAt: now}, nil)
		if err != nil {
			t.Fatal(err)
		}
		eventWaitContractProcess(t, fixture.store, scope, now, 0)
		wait, err := fixture.store.GetRunEventWait(t.Context(), scope, loop.ID, loopSpec.Key)
		if err != nil || wait.Status != RunEventWaitPending {
			t.Fatalf("a previously consumed reply satisfied the next step: %#v, %v", wait, err)
		}
	})
}

func TestRunEventWaitContractConcurrentClaimRecoveryAndScopeFairness(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		// A busy tenant must not repeatedly occupy a bounded discovery page.
		for index := 0; index < 5; index++ {
			scope := Scope{Kind: "tenant", ID: fmt.Sprintf("fair-%d", index)}
			eventWaitContractCreate(t, fixture.store, scope, eventWaitContractSpec("reply", "dm", eventWaitContractEpoch), eventWaitContractEpoch)
		}
		seen := map[Scope]bool{}
		for iteration := 0; iteration < 5; iteration++ {
			scopes, err := fixture.store.ListRunEventWaitWorkScopes(t.Context(), eventWaitContractEpoch, 1)
			if err != nil || len(scopes) != 1 || seen[scopes[0]] {
				t.Fatalf("due scope starved behind a busy scope: %#v, %v", scopes, err)
			}
			seen[scopes[0]] = true
		}
		scope := Scope{Kind: "tenant", ID: "claim-contract"}
		spec := eventWaitContractSpec("reply", "dm", eventWaitContractEpoch)
		run := eventWaitContractCreate(t, fixture.store, scope, spec, eventWaitContractEpoch)
		eventWaitContractPublish(t, fixture.store, eventWaitContractEvent(scope, spec, "reply", eventWaitContractEpoch), eventWaitContractEpoch)
		var group sync.WaitGroup
		claims := make(chan *RunEventWait, 8)
		failures := make(chan error, 8)
		for index := 0; index < 8; index++ {
			group.Go(func() {
				leased, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: fmt.Sprintf("contender-%d", index), Now: eventWaitContractEpoch, LeaseDuration: time.Minute, Limit: 1})
				if err != nil {
					failures <- err
				}
				for _, claim := range leased {
					claims <- claim
				}
			})
		}
		group.Wait()
		close(claims)
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
		var initial *RunEventWait
		for claim := range claims {
			if initial != nil {
				t.Fatal("two workers owned the same durable continuation")
			}
			initial = claim
		}
		if initial == nil {
			t.Fatal("no worker acquired due work")
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
		}
		now := eventWaitContractEpoch.Add(2 * time.Minute)
		recovered, err := fixture.store.ClaimRunEventWaits(t.Context(), ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "recovery", Now: now, LeaseDuration: time.Minute, Limit: 1})
		if err != nil || len(recovered) != 1 {
			t.Fatalf("expired claim was not recovered: %#v, %v", recovered, err)
		}
		_, err = fixture.store.ProcessRunEventWait(t.Context(), ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: initial.LeaseOwner, Now: now, LeaseExpiresAt: *initial.LeaseExpiresAt})
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("old worker was not fenced after recovery: %v", err)
		}
		resolved, err := fixture.store.ProcessRunEventWait(t.Context(), ProcessRunEventWaitRequest{Scope: scope, RunID: run.ID, Key: spec.Key, WorkerID: recovered[0].LeaseOwner, Now: now, LeaseExpiresAt: *recovered[0].LeaseExpiresAt})
		if err != nil || resolved.Run == nil || resolved.Run.ID != run.ID || resolved.Wait.Status != RunEventWaitMatched {
			t.Fatalf("recovered execution lost its observation: %#v, %v", resolved, err)
		}
	})
}
