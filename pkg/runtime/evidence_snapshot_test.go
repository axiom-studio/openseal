package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type evidenceProjectionStore interface {
	KernelStore
	InitiativeStore
	SourceMonitorStore
}

func TestScheduledInitiativeEvidenceSnapshotPersistsAcrossStoresAndReplay(t *testing.T) {
	cases := []struct {
		name string
		open func(*testing.T) (evidenceProjectionStore, func() evidenceProjectionStore)
	}{
		{name: "memory", open: func(t *testing.T) (evidenceProjectionStore, func() evidenceProjectionStore) {
			store := NewMemoryStore()
			return store, func() evidenceProjectionStore { return store }
		}},
		{name: "sqlite", open: func(t *testing.T) (evidenceProjectionStore, func() evidenceProjectionStore) {
			path := filepath.Join(t.TempDir(), "evidence.db")
			store, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return store, func() evidenceProjectionStore {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewSQLiteStore(path)
				if err != nil {
					t.Fatal(err)
				}
				store = reopened
				return reopened
			}
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		cases = append(cases, struct {
			name string
			open func(*testing.T) (evidenceProjectionStore, func() evidenceProjectionStore)
		}{name: "postgres", open: func(t *testing.T) (evidenceProjectionStore, func() evidenceProjectionStore) {
			schema := "evidence_projection_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
				_ = store.Close()
			})
			return store, func() evidenceProjectionStore {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema(schema))
				if err != nil {
					t.Fatal(err)
				}
				store = reopened
				return reopened
			}
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, restart := tc.open(t)
			scope := Scope{Kind: "tenant", ID: "evidence-" + tc.name}
			initiative, monitorRuns := seedExecutableMonitorInitiative(t, store, scope)
			now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
			monitorService := NewSourceMonitorService(store, store, store, nil)
			monitorService.now = func() time.Time { return now.Add(-time.Hour) }
			if _, err := monitorService.Ingest(ctx, sourceObservationRequest(scope, initiative.ID, "monitor-a", monitorRuns["monitor-a"], 0, "one", "thread-one", strings.Repeat("A", 20))); err != nil {
				t.Fatal(err)
			}
			if _, err := monitorService.Ingest(ctx, sourceObservationRequest(scope, initiative.ID, "monitor-b", monitorRuns["monitor-b"], 0, "two", "thread-two", "second finding")); err != nil {
				t.Fatal(err)
			}

			due := now.Add(-time.Minute)
			objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
				Scope: scope, Owner: initiative.Owner, Title: "Synthesize", Goal: "Synthesize cited findings", Status: ObjectiveStatusActive,
				NextEvaluationAt: &due, Cadence: &ObjectiveCadence{
					Type: ObjectiveCadenceInterval, IntervalSeconds: 300, AssignedAgentID: "researcher", MaximumConcurrent: 2,
					RunTemplate: &ObjectiveRunTemplate{EvidenceProjection: &ObjectiveEvidenceProjection{
						MaximumObservations: 2, MaximumSummaryRunes: 8, MaximumTotalRunes: 12,
					}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			refs := append(append([]string(nil), initiative.ObjectiveRefs...), objective.ID)
			initiative, _, err = NewInitiativeService(store, store).Patch(ctx, scope, initiative.ID, UpdateInitiativeRequest{
				ExpectedRevision: initiative.Revision, ObjectiveRefs: &refs, Actor: ActivityActor{Type: "user", ID: "operator"},
			})
			if err != nil {
				t.Fatal(err)
			}

			scheduler := NewObjectiveScheduler(store)
			scheduler.now = func() time.Time { return now }
			result, err := scheduler.ReconcileScope(ctx, scope, 20)
			if err != nil || result.Scheduled != 1 {
				t.Fatalf("schedule result=%#v err=%v", result, err)
			}
			runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Order: AgentRunOrderCreatedDesc})
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs=%#v err=%v", runs, err)
			}
			first := runs[0]
			if first.Context["initiativeId"] != initiative.ID {
				t.Fatalf("initiative lineage=%#v", first.Context)
			}
			firstSnapshot := requireEvidenceSnapshot(t, first.Context)
			if firstSnapshot.SelectedCount != 2 || !firstSnapshot.Truncated || firstSnapshot.ID == "" || len(firstSnapshot.Observations) != 2 {
				t.Fatalf("snapshot=%#v", firstSnapshot)
			}
			for _, observation := range firstSnapshot.Observations {
				if observation.ID == "" || observation.SourceURI == "" || observation.ContentDigest == "" || observation.ObservedAt.IsZero() || len([]rune(observation.Summary)) > 8 {
					t.Fatalf("observation projection=%#v", observation)
				}
			}
			if ref := evidenceSnapshotInputContextRef(first.Context); ref != evidenceSnapshotRefPrefix+firstSnapshot.ID {
				t.Fatalf("input context ref=%q", ref)
			}
			events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: first.ID, EventTypes: []string{"run.created"}, Limit: 10})
			if err != nil || len(events) != 1 || events[0].InitiativeID != initiative.ID || events[0].Payload["evidenceSnapshotId"] != firstSnapshot.ID {
				t.Fatalf("snapshot activity=%#v err=%v", events, err)
			}
			encoded, _ := json.Marshal(first.Context)
			for _, forbidden := range []string{"metadata", "actionCallId", "stableSourceId", "credential", "secret"} {
				if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(forbidden)) {
					t.Fatalf("snapshot leaked %q: %s", forbidden, encoded)
				}
			}

			// Simulate a restart after Run persistence but before cadence cursor
			// advancement by restoring the same due timestamp. New evidence must
			// not mutate the already materialized schedule occurrence.
			store = restart()
			loadedObjective, err := store.GetObjective(ctx, scope, objective.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = NewPortfolioService(store).UpdateObjective(ctx, scope, objective.ID, UpdateObjectiveRequest{
				ExpectedRevision: loadedObjective.Revision, NextEvaluationAt: &due, Actor: ActivityActor{Type: "service", ID: "test"},
			}); err != nil {
				t.Fatal(err)
			}
			scheduler = NewObjectiveScheduler(store)
			scheduler.now = func() time.Time { return now }
			replayed, err := scheduler.ReconcileScope(ctx, scope, 20)
			if err != nil || replayed.Replayed != 1 || replayed.Backpressured != 0 {
				t.Fatalf("replay=%#v err=%v", replayed, err)
			}
			persisted, err := store.GetAgentRun(ctx, scope, first.ID)
			if err != nil || requireEvidenceSnapshot(t, persisted.Context).ID != firstSnapshot.ID {
				t.Fatalf("persisted snapshot changed: %#v err=%v", persisted, err)
			}

			// A later schedule occurrence resolves evidence again and therefore
			// includes observations that arrived after the first immutable Run.
			monitorService = NewSourceMonitorService(store, store, store, nil)
			monitorService.now = func() time.Time { return now.Add(time.Hour) }
			request := sourceObservationRequest(scope, initiative.ID, "monitor-a", monitorRuns["monitor-a"], 1, "three", "thread-three", "new finding")
			request.ObservedAt = now.Add(time.Hour)
			if _, err := monitorService.Ingest(ctx, request); err != nil {
				t.Fatal(err)
			}
			scheduler = NewObjectiveScheduler(store)
			scheduler.now = func() time.Time { return due.Add(6 * time.Minute) }
			next, err := scheduler.ReconcileScope(ctx, scope, 20)
			if err != nil || next.Scheduled != 1 {
				t.Fatalf("next schedule=%#v err=%v", next, err)
			}
			runs, err = store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 10})
			if err != nil || len(runs) != 2 {
				t.Fatalf("next runs=%#v err=%v", runs, err)
			}
			var second *AgentRun
			for _, candidate := range runs {
				if candidate.ID != first.ID {
					second = candidate
				}
			}
			if second == nil || requireEvidenceSnapshot(t, second.Context).ID == firstSnapshot.ID {
				t.Fatalf("new schedule did not receive new evidence: %#v", second)
			}
		})
	}
}

func TestEvidenceSnapshotExcludesRetentionExpiredObservations(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "retention"}
	initiative, monitorRuns := seedExecutableMonitorInitiative(t, store, scope)
	base := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	expires := base.Add(time.Hour)
	service := NewSourceMonitorService(store, store, store, nil)
	service.now = func() time.Time { return base }
	request := sourceObservationRequest(scope, initiative.ID, "monitor-a", monitorRuns["monitor-a"], 0, "expired", "thread-expired", "retained only briefly")
	request.ObservedAt = base
	request.RetentionExpiresAt = &expires
	if _, err := service.Ingest(ctx, request); err != nil {
		t.Fatal(err)
	}
	snapshot, err := buildEvidenceSnapshot(ctx, store, scope, initiative.ID, expires, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ExpiredCount != 1 || snapshot.SelectedCount != 0 || len(snapshot.Observations) != 0 {
		t.Fatalf("expired evidence projected: %#v", snapshot)
	}
}

func TestObjectiveSchedulerDefersInactiveInitiativeSynthesis(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "paused-synthesis"}
	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"}, Title: "Synthesize", Goal: "Synthesize findings", Status: ObjectiveStatusActive,
		NextEvaluationAt: &due, Cadence: &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 300, AssignedAgentID: "analyst"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = NewInitiativeService(store, store).Create(ctx, CreateInitiativeRequest{Initiative: &Initiative{
		ID: "initiative-paused", Scope: scope, Owner: objective.Owner, Title: "Paused", Purpose: "Coordinate research", Status: InitiativeStatusPaused, ObjectiveRefs: []string{objective.ID},
	}}); err != nil {
		t.Fatal(err)
	}
	scheduler := NewObjectiveScheduler(store)
	scheduler.now = func() time.Time { return now }
	result, err := scheduler.ReconcileScope(ctx, scope, 10)
	if err != nil || result.Suspended != 1 || result.Scheduled != 0 {
		t.Fatalf("inactive result=%#v err=%v", result, err)
	}
}

func TestEvidenceSnapshotProjectsToHostedInputWithoutRawSourceState(t *testing.T) {
	snapshot := &EvidenceSnapshot{
		APIVersion: evidenceSnapshotAPIVersion, InitiativeID: "initiative-one",
		ObservationLimit: 25, SummaryRuneLimit: 1000, TotalSummaryRuneLimit: 20000, SelectedCount: 1,
		Observations: []EvidenceSnapshotObservation{{
			ID: "observation-one", SourceURI: "https://forum.example/thread/1", ObservedAt: time.Now().UTC(),
			ContentDigest: "sha256:digest", Summary: "A bounded finding",
		}},
	}
	identity, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ID = hashBytes(identity)
	projected, err := evidenceSnapshotContext(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn-one", ModelProvider: "test", Model: "test-model",
		OutputSummary: "done", NextRunStatus: AgentRunStatusCompleted,
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{
		AgentID: "analyst", DefinitionID: "researcher", DefinitionVersion: "1", ModelProvider: "test", Model: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.RunTurn(context.Background(), TurnExecutionContext{
		Run:  &AgentRun{ID: "run-one", Scope: Scope{Kind: "tenant", ID: "host"}, AssignedAgentID: "analyst", Goal: "Synthesize", Context: map[string]interface{}{EvidenceSnapshotContextKey: projected}},
		Turn: &AgentTurn{ID: "turn-one"},
	}); err != nil {
		t.Fatal(err)
	}
	if requireEvidenceSnapshot(t, host.request.InputContext).ID != snapshot.ID {
		t.Fatalf("host input=%#v", host.request.InputContext)
	}
}

func TestAgentRunWorkerRecordsEvidenceSnapshotInputReference(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "snapshot-ref"}
	snapshot := &EvidenceSnapshot{APIVersion: evidenceSnapshotAPIVersion, ID: "sha256:stable", InitiativeID: "initiative-one", ObservationLimit: 25, SummaryRuneLimit: 1000, TotalSummaryRuneLimit: 20000, Observations: []EvidenceSnapshotObservation{}}
	projected, err := evidenceSnapshotContext(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, AssignedAgentID: "analyst", Goal: "Synthesize", Source: RunSourceSchedule,
		Context: map[string]interface{}{EvidenceSnapshotContextKey: projected},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan []string, 1)
	shared := &TurnRunnerBinding{DefinitionID: "analyst", DefinitionVersion: "1", InputContextRefs: []string{"definition:analyst@1"}}
	shared.Runner = TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		seen <- append([]string(nil), input.Turn.InputContextRefs...)
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "done"}, nil
	})
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return shared, nil
	}), nil, AgentRunWorkerConfig{Scope: scope, AssignedAgentID: "analyst", PollInterval: 10 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()
	select {
	case refs := <-seen:
		want := evidenceSnapshotRefPrefix + snapshot.ID
		if !containsString(refs, "definition:analyst@1") || !containsString(refs, want) {
			t.Fatalf("turn input refs=%#v", refs)
		}
		if len(shared.InputContextRefs) != 1 || shared.InputContextRefs[0] != "definition:analyst@1" {
			t.Fatalf("resolver binding was mutated across runs: %#v", shared.InputContextRefs)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("worker did not execute Run %s", run.ID)
	}
}

func TestObjectiveSchedulerFailsClosedForAmbiguousInitiativeMembership(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "ambiguous"}
	due := time.Now().Add(-time.Minute).UTC()
	objective, err := NewPortfolioService(store).CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, Title: "Synthesize", Goal: "Synthesize", Status: ObjectiveStatusActive,
		NextEvaluationAt: &due, Cadence: &ObjectiveCadence{Type: ObjectiveCadenceInterval, IntervalSeconds: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"initiative-a", "initiative-b"} {
		if _, _, err = NewInitiativeService(store, store).Create(ctx, CreateInitiativeRequest{Initiative: &Initiative{
			ID: id, Scope: scope, Owner: objective.Owner, Title: id, Purpose: "Coordinate", Status: InitiativeStatusActive, ObjectiveRefs: []string{objective.ID},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := NewObjectiveScheduler(store).ReconcileScope(ctx, scope, 10)
	if err == nil || !strings.Contains(err.Error(), "multiple Initiatives") || result.Scheduled != 0 {
		t.Fatalf("ambiguous result=%#v err=%v", result, err)
	}
	runs, _ := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, ObjectiveID: objective.ID})
	if len(runs) != 0 {
		t.Fatalf("ambiguous membership scheduled runs: %#v", runs)
	}
}

func requireEvidenceSnapshot(t *testing.T, contextValues map[string]interface{}) EvidenceSnapshot {
	t.Helper()
	encoded, err := json.Marshal(contextValues[EvidenceSnapshotContextKey])
	if err != nil {
		t.Fatal(err)
	}
	var snapshot EvidenceSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.APIVersion != evidenceSnapshotAPIVersion {
		t.Fatalf("snapshot apiVersion=%q", snapshot.APIVersion)
	}
	return snapshot
}
