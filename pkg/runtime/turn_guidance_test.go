package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestGuidanceCommandReplaySurvivesRevisionChanges(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "guidance"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, Goal: "Study"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewRunCommandService(store)
	req := AgentRunCommandRequest{Scope: scope, RunID: run.ID, ExpectedRevision: run.Revision, Kind: AgentRunCommandIntervene, InterventionID: uuid.NewString(), Instruction: "Keep uncertainty explicit", Actor: ActivityActor{Type: "user", ID: "local-operator"}}
	first, err := service.CommandAgentRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := service.CommandAgentRun(ctx, req)
	if err != nil || len(repeated.Run.PendingInterventions) != 1 || repeated.Run.Revision != first.Run.Revision {
		t.Fatalf("duplicate guidance: %+v %v", repeated, err)
	}
	req.Instruction = "Different instruction"
	if _, err = service.CommandAgentRun(ctx, req); !errors.Is(err, ErrInvalidRunCommand) {
		t.Fatalf("identity reused for other text: %v", err)
	}
	req.Instruction = "Keep uncertainty explicit"
	req.Actor.ID = "other"
	if _, err = service.CommandAgentRun(ctx, req); !errors.Is(err, ErrInvalidRunCommand) {
		t.Fatalf("identity reused for other actor: %v", err)
	}
	req.Actor.ID = "local-operator"
	req.InterventionID = uuid.NewString()
	if _, err = service.CommandAgentRun(ctx, req); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("new guidance ignored revision: %v", err)
	}
}

func TestGuidanceSupersedesUnappliedTurnAndSurvivesRecovery(t *testing.T) {
	for _, timing := range []string{"during-provider", "after-persist"} {
		t.Run(timing, func(t *testing.T) {
			ctx := context.Background()
			db := filepath.Join(t.TempDir(), "state.db")
			store, err := NewSQLiteStore(db)
			if err != nil {
				t.Fatal(err)
			}
			scope := Scope{Kind: "local", ID: "guidance"}
			run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, Goal: "Study"})
			if err != nil {
				t.Fatal(err)
			}
			add := func() {
				current, err := store.GetAgentRun(ctx, scope, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = NewRunCommandService(store).CommandAgentRun(ctx, AgentRunCommandRequest{Scope: scope, RunID: run.ID, ExpectedRevision: current.Revision, Kind: AgentRunCommandIntervene, InterventionID: uuid.NewString(), Instruction: "Do not publish; explain uncertainty", Actor: ActivityActor{Type: "user", ID: "local-operator"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			runner := TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
				calls++
				if calls == 1 {
					if timing == "during-provider" {
						add()
					}
					return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Old completion", ProposedActions: []TurnAction{{Type: "skill_action", Capability: "publish", Summary: "Old action"}}, RunOutput: map[string]interface{}{"reply": "old output"}, Usage: TurnUsage{OutputTokens: 10}}, nil
				}
				if len(input.Run.PendingInterventions) != 1 || len(input.Turn.InputInterventionIDs) != 1 {
					t.Fatalf("guidance missing: %+v", input)
				}
				return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"reply": "uncertainty explained"}}, nil
			})
			coordinator := NewTurnCoordinator(store, store, store)
			if timing == "after-persist" {
				coordinator.afterTurnPersisted = func() error { add(); return errors.New("simulated crash") }
			}
			first, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, runner)
			if timing == "after-persist" {
				if err == nil {
					t.Fatal("expected crash")
				}
				store.Close()
				store, err = NewSQLiteStore(db)
				if err != nil {
					t.Fatal(err)
				}
				coordinator = NewTurnCoordinator(store, store, store)
				first, err = coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, runner)
			}
			defer store.Close()
			if err != nil {
				t.Fatal(err)
			}
			if first.Run.Status != AgentRunStatusRunning || len(first.Turn.RequestedActions) != 0 || len(first.Run.Output) != 0 || first.Turn.Usage.OutputTokens != 10 || calls != 1 {
				t.Fatalf("old intent applied: calls=%d result=%+v turn=%+v", calls, first, first.Turn)
			}
			second, err := coordinator.Advance(ctx, AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, runner)
			if err != nil {
				t.Fatal(err)
			}
			if second.Run.Status != AgentRunStatusCompleted || second.Run.Output["reply"] != "uncertainty explained" || calls != 2 {
				t.Fatalf("guidance not considered: %+v", second)
			}
		})
	}
}
