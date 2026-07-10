package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRunCommandsAreIdempotentAuditedAndRevisionSafe(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) RunCommandStore
	}{
		{name: "memory", store: func(*testing.T) RunCommandStore { return NewMemoryStore(100) }},
		{name: "sqlite", store: func(t *testing.T) RunCommandStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "commands.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "one"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"}
			service := NewRunCommandService(fixture.store(t))
			create := CreateAgentRunRequest{
				Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Operate the service",
				Source: RunSourceManual, IdempotencyKey: "run-one", Actor: ActivityActor{Type: "user", ID: "7"},
			}
			created, err := service.CreateAgentRun(ctx, create)
			if err != nil {
				t.Fatal(err)
			}
			if created.Event == nil || created.Event.EventType != "run.created" || created.Event.Sequence != 1 {
				t.Fatalf("creation event = %#v", created.Event)
			}
			replayed, err := service.CreateAgentRun(ctx, create)
			if err != nil {
				t.Fatal(err)
			}
			if replayed.Run.ID != created.Run.ID || replayed.Event != nil {
				t.Fatalf("replay = %#v, created = %#v", replayed, created)
			}
			changed := create
			changed.Goal = "Do different work"
			if _, err := service.CreateAgentRun(ctx, changed); !errors.Is(err, ErrRunIdempotency) {
				t.Fatalf("conflicting replay error = %v", err)
			}

			paused, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
				Scope: scope, RunID: created.Run.ID, ExpectedRevision: created.Run.Revision, Kind: AgentRunCommandPause,
				Actor: ActivityActor{Type: "user", ID: "7"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if paused.Run.Status != AgentRunStatusPaused || paused.Run.PausedFrom != AgentRunStatusQueued || paused.Event.EventType != "run.paused" {
				t.Fatalf("paused result = %#v", paused)
			}
			if _, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
				Scope: scope, RunID: created.Run.ID, ExpectedRevision: created.Run.Revision, Kind: AgentRunCommandResume,
			}); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("stale command error = %v", err)
			}

			intervened, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
				Scope: scope, RunID: paused.Run.ID, ExpectedRevision: paused.Run.Revision, Kind: AgentRunCommandIntervene,
				Actor: ActivityActor{Type: "user", ID: "7"}, Instruction: "Prioritize the database check",
			})
			if err != nil {
				t.Fatal(err)
			}
			if intervened.Run.Status != AgentRunStatusPaused || intervened.Run.PausedFrom != AgentRunStatusQueued || len(intervened.Run.PendingInterventions) != 1 {
				t.Fatalf("intervened result = %#v", intervened)
			}

			resumed, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
				Scope: scope, RunID: paused.Run.ID, ExpectedRevision: intervened.Run.Revision, Kind: AgentRunCommandResume,
			})
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Run.Status != AgentRunStatusQueued || resumed.Run.PausedFrom != "" || resumed.Event.EventType != "run.resumed" {
				t.Fatalf("resumed result = %#v", resumed)
			}

			canceled, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
				Scope: scope, RunID: resumed.Run.ID, ExpectedRevision: resumed.Run.Revision, Kind: AgentRunCommandCancel,
			})
			if err != nil {
				t.Fatal(err)
			}
			if canceled.Run.Status != AgentRunStatusCanceled || canceled.Run.CompletedAt == nil || canceled.Event.EventType != "run.canceled" {
				t.Fatalf("canceled result = %#v", canceled)
			}
		})
	}
}

func TestRunPausePreservesWaitingCondition(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(10)
	service := NewRunCommandService(store)
	scope := Scope{Kind: "local", ID: "wait"}
	created, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "ops"}, Goal: "Wait safely", Source: RunSourceEvent,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, created.Run.ID, RunTransitionRequest{ExpectedRevision: 1, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	waiting, _, err := activity.TransitionRun(ctx, scope, created.Run.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusWaitingForEvent,
		WakeCondition: &WakeCondition{Type: "event", Reference: "cluster.changed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{Scope: scope, RunID: waiting.ID, ExpectedRevision: waiting.Revision, Kind: AgentRunCommandPause})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{Scope: scope, RunID: waiting.ID, ExpectedRevision: paused.Run.Revision, Kind: AgentRunCommandResume})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Run.Status != AgentRunStatusWaitingForEvent || resumed.Run.WakeCondition == nil || resumed.Run.WakeCondition.Reference != "cluster.changed" {
		t.Fatalf("resumed waiting run = %#v", resumed.Run)
	}
}

func TestRunCreationReplaySurvivesSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	request := CreateAgentRunRequest{
		Scope: Scope{Kind: "tenant", ID: "restart"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "Survive restart", Source: RunSourceManual, IdempotencyKey: "stable-request",
	}
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewRunCommandService(store).CreateAgentRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed, err := NewRunCommandService(reopened).CreateAgentRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Run.ID != created.Run.ID || replayed.Event != nil {
		t.Fatalf("restart replay = %#v, created = %#v", replayed, created)
	}
	events, err := reopened.ListActivity(ctx, ActivityFilter{Scope: request.Scope, RunID: created.Run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != "run.created" {
		t.Fatalf("restart activity = %#v", events)
	}
}
