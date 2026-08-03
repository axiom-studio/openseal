package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRunCommandsAreIdempotentAuditedAndRevisionSafe(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) RunCommandStore
	}{
		{name: "memory", store: func(*testing.T) RunCommandStore { return NewMemoryStore() }},
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
				Scope: scope, Owner: owner, AssignedAgentID: owner.ID, ConcurrencyKey: "agent-1:operations", Goal: "Operate the service",
				Source: RunSourceManual, IdempotencyKey: "run-one", Actor: ActivityActor{Type: "user", ID: "7"},
			}
			created, err := service.CreateAgentRun(ctx, create)
			if err != nil {
				t.Fatal(err)
			}
			if created.Event == nil || created.Event.EventType != "run.created" || created.Event.Sequence != 1 {
				t.Fatalf("creation event = %#v", created.Event)
			}
			if created.Run.Kind != RunKindAgentWork || created.Run.ConcurrencyKey != "agent-1:operations" {
				t.Fatalf("created run = %#v", created.Run)
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
			changed = create
			changed.Kind = RunKindConversation
			if _, err := service.CreateAgentRun(ctx, changed); !errors.Is(err, ErrRunIdempotency) {
				t.Fatalf("run kind replay conflict = %v", err)
			}
			changed = create
			changed.ConcurrencyKey = "agent-1:other"
			if _, err := service.CreateAgentRun(ctx, changed); !errors.Is(err, ErrRunIdempotency) {
				t.Fatalf("concurrency key replay conflict = %v", err)
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

func TestCancelingParentRunCascadesToAllNonTerminalDescendants(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) RunCommandStore
	}{
		{name: "memory", store: func(*testing.T) RunCommandStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) RunCommandStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "cascade.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := t.Context()
			store := fixture.store(t)
			service := NewRunCommandService(store)
			scope := Scope{Kind: "tenant", ID: "cascade"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
			create := func(goal, parentID string) *AgentRun {
				result, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
					Scope: scope, ParentRunID: parentID, Owner: owner, AssignedAgentID: owner.ID,
					Goal: goal, Source: RunSourceHandoff,
				})
				if err != nil {
					t.Fatal(err)
				}
				return result.Run
			}
			root := create("root work", "")
			child := create("delegated work", root.ID)
			grandchild := create("nested work", child.ID)
			completed := create("already complete", root.ID)
			activity := NewRunActivityService(store, store)
			running, _, err := activity.TransitionRun(ctx, scope, completed.ID, RunTransitionRequest{
				ExpectedRevision: completed.Revision, Status: AgentRunStatusRunning,
			})
			if err != nil {
				t.Fatal(err)
			}
			completed, _, err = activity.TransitionRun(ctx, scope, completed.ID, RunTransitionRequest{
				ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted,
			})

			result, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
				Scope: scope, RunID: root.ID, ExpectedRevision: root.Revision, Kind: AgentRunCommandCancel,
				Actor: ActivityActor{Type: "user", ID: "operator"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Run.Status != AgentRunStatusCanceled {
				t.Fatalf("root status = %s", result.Run.Status)
			}
			for _, id := range []string{child.ID, grandchild.ID} {
				run, err := store.GetAgentRun(ctx, scope, id)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status != AgentRunStatusCanceled || run.CompletedAt == nil || run.LeaseOwner != "" || run.LeaseExpiresAt != nil {
					t.Fatalf("descendant %s was not fully canceled: %#v", id, run)
				}
				events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: id})
				cancellations := 0
				for _, event := range events {
					if event.EventType == "run.canceled" {
						cancellations++
					}
				}
				if err != nil || cancellations != 1 {
					t.Fatalf("descendant %s cancellation events = %d, err = %v", id, cancellations, err)
				}
			}
			unchanged, err := store.GetAgentRun(ctx, scope, completed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.Status != AgentRunStatusCompleted || unchanged.Revision != completed.Revision {
				t.Fatalf("completed descendant changed: %#v", unchanged)
			}
			if err := service.CascadeTerminalRun(ctx, result.Run); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveHumanInterventionQueuesRunAndPreservesAudit(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	service := NewRunCommandService(store)
	scope := Scope{Kind: "tenant", ID: "human"}
	created, err := service.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "browser-agent"}, AssignedAgentID: "browser-agent",
		Goal: "Continue only after a human completes the challenge", Source: RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, created.Run.ID, RunTransitionRequest{ExpectedRevision: created.Run.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 27, 7, 15, 0, 0, time.UTC)
	request := HumanInterventionRequest{
		ID: "human-request", Kind: "capability_challenge", Status: HumanInterventionStatusPending,
		ActionCallID: "browser-click", Summary: "Human intervention required", Challenge: []string{"captcha"}, CreatedAt: now,
	}
	waiting, _, err := activity.TransitionRun(ctx, scope, created.Run.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusWaitingForEvent,
		WakeCondition: &WakeCondition{Type: "human_intervention", Reference: request.ID}, HumanInterventions: []HumanInterventionRequest{request},
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := ActivityActor{Type: "user", ID: "operator"}
	resolved, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: waiting.ID, ExpectedRevision: waiting.Revision, Kind: AgentRunCommandResolveHumanIntervention,
		HumanInterventionID: request.ID, Instruction: "CAPTCHA completed in the retained Browser session", Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Run.Status != AgentRunStatusQueued || resolved.Run.WakeCondition != nil || resolved.Event.EventType != "run.human_intervention_resolved" {
		t.Fatalf("resolved run = %#v", resolved)
	}
	if len(resolved.Run.HumanInterventions) != 1 || resolved.Run.HumanInterventions[0].Status != HumanInterventionStatusResolved || resolved.Run.HumanInterventions[0].ResolvedBy == nil || resolved.Run.HumanInterventions[0].ResolvedBy.ID != actor.ID {
		t.Fatalf("resolved intervention = %#v", resolved.Run.HumanInterventions)
	}
	if len(resolved.Run.PendingInterventions) != 1 || resolved.Run.PendingInterventions[0].Instruction == "" {
		t.Fatalf("operator context was not queued for the next turn: %#v", resolved.Run.PendingInterventions)
	}
	if _, err := service.CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: resolved.Run.ID, ExpectedRevision: resolved.Run.Revision, Kind: AgentRunCommandResolveHumanIntervention,
		HumanInterventionID: request.ID, Instruction: "replay", Actor: actor,
	}); err == nil {
		t.Fatal("resolved human intervention was accepted twice")
	}
}

func TestRunPausePreservesWaitingCondition(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
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

func TestRunCreationRejectsCredentialStateButAllowsTokenBudgets(t *testing.T) {
	service := NewRunCommandService(NewMemoryStore())
	base := CreateAgentRunRequest{
		Scope: Scope{Kind: "tenant", ID: "safe"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Goal: "Operate safely", Source: RunSourceManual,
	}
	unsafe := base
	unsafe.Context = map[string]interface{}{"api_token": "resolved-secret"}
	if _, err := service.CreateAgentRun(context.Background(), unsafe); !errors.Is(err, ErrUnsafeSharedContext) || !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("credential context error = %v", err)
	}
	safe := base
	safe.Context = map[string]interface{}{"tokenBudget": 4096, "budget_tokens": 2048}
	if _, err := service.CreateAgentRun(context.Background(), safe); err != nil {
		t.Fatalf("non-secret token budget rejected: %v", err)
	}
}
