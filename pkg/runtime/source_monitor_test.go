package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSourceMonitorObservationIngestDeduplicatesAndSurvivesRestart(t *testing.T) {
	for _, fixture := range []struct {
		name string
		open func(*testing.T) (KernelStore, func() KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func() KernelStore, func()) {
			store := NewMemoryStore()
			return store, func() KernelStore { return store }, func() {}
		}},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func() KernelStore, func()) {
			path := filepath.Join(t.TempDir(), "monitors.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return store, func() KernelStore {
				_ = store.Close()
				reopened, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				store = reopened
				return store
			}, func() { _ = store.Close() }
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store, restart, closeStore := fixture.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "research"}
			initiative, runs := seedExecutableMonitorInitiative(t, store, scope)
			service := NewSourceMonitorService(store.(SourceMonitorStore), store.(InitiativeStore), store, store.(ArtifactStore))
			first, err := service.Ingest(ctx, sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-7", "Finding one"))
			if err != nil || first.Replayed || first.Event == nil || first.Checkpoint.Revision != 1 || first.Checkpoint.ObservationCount != 1 {
				t.Fatalf("first=%#v err=%v", first, err)
			}
			replay, err := service.Ingest(ctx, sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 1, "cursor-2", "thread-7", "Finding one"))
			if err != nil || !replay.Replayed || replay.Event != nil || replay.Observation.ID != first.Observation.ID || replay.Checkpoint.Revision != 2 || replay.Checkpoint.Cursor != "cursor-2" || replay.Checkpoint.ObservationCount != 1 {
				t.Fatalf("replay=%#v err=%v", replay, err)
			}
			other, err := service.Ingest(ctx, sourceObservationRequest(scope, initiative.ID, "monitor-b", runs["monitor-b"], 0, "cursor-b", "thread-7", "Finding one"))
			if err != nil || other.Replayed || other.Observation.ID == first.Observation.ID {
				t.Fatalf("other=%#v err=%v", other, err)
			}
			store = restart()
			service = NewSourceMonitorService(store.(SourceMonitorStore), store.(InitiativeStore), store, store.(ArtifactStore))
			checkpoint, err := service.GetCheckpoint(ctx, scope, initiative.ID, "monitor-a")
			values, listErr := service.List(ctx, SourceObservationFilter{Scope: scope, InitiativeID: initiative.ID, Limit: 10})
			if err != nil || listErr != nil || checkpoint.Revision != 2 || checkpoint.ObservationCount != 1 || len(values) != 2 {
				t.Fatalf("checkpoint=%#v values=%#v err=%v listErr=%v", checkpoint, values, err, listErr)
			}
		})
	}
}

func TestSourceMonitorReadsExcludeExpiredEvidenceWithoutDeletingAuditRecords(t *testing.T) {
	for _, fixture := range []struct {
		name string
		open func(*testing.T) (KernelStore, func() KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func() KernelStore, func()) {
			store := NewMemoryStore()
			return store, func() KernelStore { return store }, func() {}
		}},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func() KernelStore, func()) {
			path := filepath.Join(t.TempDir(), "retention.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return store, func() KernelStore {
				_ = store.Close()
				reopened, openErr := NewSQLiteStore(path)
				if openErr != nil {
					t.Fatal(openErr)
				}
				store = reopened
				return store
			}, func() { _ = store.Close() }
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store, restart, closeStore := fixture.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "retention"}
			initiative, runs := seedExecutableMonitorInitiative(t, store, scope)
			service := NewSourceMonitorService(store.(SourceMonitorStore), store.(InitiativeStore), store, store.(ArtifactStore))
			base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
			service.now = func() time.Time { return base }

			expires := base.Add(time.Hour)
			expiredRequest := sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-expired", "thread-expired", "Expired finding")
			expiredRequest.RetentionExpiresAt = &expires
			expired, err := service.Ingest(ctx, expiredRequest)
			if err != nil {
				t.Fatal(err)
			}
			activeRequest := sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 1, "cursor-active", "thread-active", "Retained finding")
			active, err := service.Ingest(ctx, activeRequest)
			if err != nil {
				t.Fatal(err)
			}

			store = restart()
			service = NewSourceMonitorService(store.(SourceMonitorStore), store.(InitiativeStore), store, store.(ArtifactStore))
			service.now = func() time.Time { return expires }
			values, err := service.List(ctx, SourceObservationFilter{Scope: scope, InitiativeID: initiative.ID, Limit: 1})
			if err != nil || len(values) != 1 || values[0].ID != active.Observation.ID {
				t.Fatalf("retained values=%#v err=%v", values, err)
			}
			if _, err = service.Get(ctx, scope, expired.Observation.ID); !errors.Is(err, ErrSourceObservationNotFound) {
				t.Fatalf("expired product read err=%v", err)
			}
			if _, err = store.(SourceMonitorStore).GetSourceObservation(ctx, scope, expired.Observation.ID); err != nil {
				t.Fatalf("immutable audit record was deleted: %v", err)
			}
		})
	}
}

func TestSourceMonitorObservationConcurrentCASCannotSkipEvidence(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "race"}
	initiative, runs := seedExecutableMonitorInitiative(t, store, scope)
	service := NewSourceMonitorService(store, store, store, store)
	requests := []IngestSourceObservationRequest{
		sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-a", "thread-a", "Finding A"),
		sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-b", "thread-b", "Finding B"),
	}
	var wg sync.WaitGroup
	results := make(chan *SourceObservationIngestResult, 2)
	errs := make(chan error, 2)
	for index := range requests {
		wg.Add(1)
		go func(request IngestSourceObservationRequest) {
			defer wg.Done()
			result, err := service.Ingest(context.Background(), request)
			results <- result
			errs <- err
		}(requests[index])
	}
	wg.Wait()
	close(results)
	close(errs)
	succeeded, conflicted := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
		} else if err == ErrSourceMonitorCheckpoint {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	checkpoint, err := service.GetCheckpoint(context.Background(), scope, initiative.ID, "monitor-a")
	if err != nil {
		t.Fatal(err)
	}
	var retry IngestSourceObservationRequest
	for _, request := range requests {
		if request.Cursor != checkpoint.Cursor {
			retry = request
			break
		}
	}
	retry.ExpectedCheckpointRevision = checkpoint.Revision
	if _, err = service.Ingest(context.Background(), retry); err != nil {
		t.Fatal(err)
	}
	values, err := service.List(context.Background(), SourceObservationFilter{Scope: scope, InitiativeID: initiative.ID, MonitorID: "monitor-a"})
	if err != nil || len(values) != 2 {
		t.Fatalf("values=%#v err=%v", values, err)
	}
}

func TestSourceMonitorNoChangeCheckpointIsDurableAndReplaySafe(t *testing.T) {
	for _, fixture := range []struct {
		name string
		open func(*testing.T) (KernelStore, func() KernelStore, func())
	}{
		{name: "memory", open: func(*testing.T) (KernelStore, func() KernelStore, func()) {
			store := NewMemoryStore()
			return store, func() KernelStore { return store }, func() {}
		}},
		{name: "sqlite", open: func(t *testing.T) (KernelStore, func() KernelStore, func()) {
			path := filepath.Join(t.TempDir(), "no-change.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return store, func() KernelStore {
				_ = store.Close()
				reopened, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				store = reopened
				return store
			}, func() { _ = store.Close() }
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store, restart, closeStore := fixture.open(t)
			defer closeStore()
			scope := Scope{Kind: "tenant", ID: "no-change"}
			initiative, runs := seedExecutableMonitorInitiative(t, store, scope)
			service := NewSourceMonitorService(store.(SourceMonitorStore), store.(InitiativeStore), store, store.(ArtifactStore))
			first, err := service.Ingest(context.Background(), sourceObservationRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-1", "Finding one"))
			if err != nil {
				t.Fatal(err)
			}
			request := checkpointRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 1, "cursor-1", "no-change-action")
			advanced, err := service.AdvanceCheckpoint(context.Background(), request)
			if err != nil || advanced.Replayed || advanced.Event == nil || advanced.Checkpoint.Revision != 2 || advanced.Checkpoint.ObservationCount != 1 || advanced.Checkpoint.LastObservationID != first.Observation.ID {
				t.Fatalf("advanced=%#v err=%v", advanced, err)
			}
			replay, err := service.AdvanceCheckpoint(context.Background(), request)
			if err != nil || !replay.Replayed || replay.Event != nil || replay.Checkpoint.Revision != 2 {
				t.Fatalf("replay=%#v err=%v", replay, err)
			}
			store = restart()
			service = NewSourceMonitorService(store.(SourceMonitorStore), store.(InitiativeStore), store, store.(ArtifactStore))
			checkpoint, err := service.GetCheckpoint(context.Background(), scope, initiative.ID, "monitor-a")
			if err != nil || checkpoint.Revision != 2 || checkpoint.ObservationCount != 1 || checkpoint.LastActionCallID != "no-change-action" || checkpoint.Cursor != "cursor-1" {
				t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
			}
		})
	}
}

func TestSourceMonitorNoChangeCheckpointConcurrentCAS(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "no-change-race"}
	initiative, runs := seedExecutableMonitorInitiative(t, store, scope)
	service := NewSourceMonitorService(store, store, store, store)
	requests := []AdvanceSourceMonitorCheckpointRequest{
		checkpointRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-a", "action-a"),
		checkpointRequest(scope, initiative.ID, "monitor-a", runs["monitor-a"], 0, "cursor-b", "action-b"),
	}
	var wg sync.WaitGroup
	errorsOut := make(chan error, 2)
	for _, request := range requests {
		wg.Add(1)
		go func(candidate AdvanceSourceMonitorCheckpointRequest) {
			defer wg.Done()
			_, err := service.AdvanceCheckpoint(context.Background(), candidate)
			errorsOut <- err
		}(request)
	}
	wg.Wait()
	close(errorsOut)
	succeeded, conflicted := 0, 0
	for err := range errorsOut {
		if err == nil {
			succeeded++
		} else if err == ErrSourceMonitorCheckpoint {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func seedExecutableMonitorInitiative(t *testing.T, store KernelStore, scope Scope) (*Initiative, map[string]string) {
	t.Helper()
	ctx := context.Background()
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"}
	portfolio := NewPortfolioService(store)
	objectiveIDs := make([]string, 0, 2)
	monitors := make([]SourceMonitorReference, 0, 2)
	runs := map[string]string{}
	for _, monitorID := range []string{"monitor-a", "monitor-b"} {
		objective, err := portfolio.CreateObjective(ctx, CreateObjectiveRequest{Scope: scope, Owner: owner, Title: monitorID, Goal: "Monitor approved sources", Status: ObjectiveStatusActive, Cadence: &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 60, AssignedAgentID: "researcher", RunTemplate: &ObjectiveRunTemplate{Context: map[string]interface{}{"initiativeId": "initiative-research", "sourceMonitorId": monitorID}, Policy: map[string]interface{}{"sourcePolicyRef": "approved-forums"}, Capability: &ObjectiveCapabilityInvocation{SkillID: "forum-reader", SkillVersion: "1.0.0", Action: "search", Inputs: map[string]interface{}{"query": "pain points"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		objectiveIDs = append(objectiveIDs, objective.ID)
		monitors = append(monitors, SourceMonitorReference{ID: monitorID, ObjectiveID: objective.ID, AssignedAgentID: "researcher", SkillID: "forum-reader", SkillVersion: "1.0.0", Action: "search", SourcePolicyRef: "approved-forums", Deduplication: SourceMonitorDeduplicateStableSourceAndContent})
		run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: "researcher", Goal: objective.Goal, Source: RunSourceSchedule, Context: map[string]interface{}{"initiativeId": "initiative-research", "sourceMonitorId": monitorID}})
		if err != nil {
			t.Fatal(err)
		}
		runs[monitorID] = run.ID
	}
	initiative, _, err := NewInitiativeService(store.(InitiativeStore), store).Create(ctx, CreateInitiativeRequest{Initiative: &Initiative{ID: "initiative-research", Scope: scope, Title: "Research", Purpose: "Collect cited evidence", Status: InitiativeStatusActive, Owner: owner, ObjectiveRefs: objectiveIDs, SourceMonitors: monitors}, Actor: ActivityActor{Type: "user", ID: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	return initiative, runs
}

func sourceObservationRequest(scope Scope, initiativeID, monitorID, runID string, expected int64, cursor, stableSourceID, summary string) IngestSourceObservationRequest {
	digest := sha256.Sum256([]byte(summary))
	return IngestSourceObservationRequest{Scope: scope, InitiativeID: initiativeID, MonitorID: monitorID, ExpectedCheckpointRevision: expected, Cursor: cursor, StableSourceID: stableSourceID, SourceURI: "https://forum.example/threads/" + stableSourceID, ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Summary: summary, ObservedAt: time.Now().UTC(), RunID: runID, AgentID: "researcher", SkillID: "forum-reader", SkillVersion: "1.0.0", Action: "search", ActionCallID: "action-" + cursor, Metadata: map[string]interface{}{"confidence": .8}}
}

func checkpointRequest(scope Scope, initiativeID, monitorID, runID string, expected int64, cursor, actionCallID string) AdvanceSourceMonitorCheckpointRequest {
	return AdvanceSourceMonitorCheckpointRequest{Scope: scope, InitiativeID: initiativeID, MonitorID: monitorID, ExpectedCheckpointRevision: expected, Cursor: cursor, RunID: runID, AgentID: "researcher", SkillID: "forum-reader", SkillVersion: "1.0.0", Action: "search", ActionCallID: actionCallID}
}
