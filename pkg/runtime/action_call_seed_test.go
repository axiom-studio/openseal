package runtime

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// actionCallSeedStore is the public store surface seedActionCall writes
// through.
type actionCallSeedStore interface {
	ActionStore
	CreateAgentRun(ctx context.Context, run *AgentRun) error
	GetAgentRun(ctx context.Context, scope Scope, id string) (*AgentRun, error)
	CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error)
	GetAgentTurn(ctx context.Context, scope Scope, turnID string) (*AgentTurn, error)
}

// actionSeedWorker leases the Runs and steps seedActionCall creates.
const actionSeedWorker = "action-seed-worker"

// seedActionCall stores a copy of call, with its status, timestamps and
// output, through the store's proposal command, for feature tests that read
// a Run's recorded calls back. The command only records a call under its
// Run's lease (and a vm call under its running step's), so:
//   - a Run that does not exist yet is created running under
//     actionSeedWorker's lease; an existing Run must be running and leased;
//   - a vm call's step is created running under the Run's lease if it does
//     not exist yet;
//   - identity fields a stored call must name (deployment, skill version,
//     action, step, revision, attempts, loop execution) get placeholders when
//     the test leaves them empty.
//
// The command also appends one activity event to the Run.
func seedActionCall(t testing.TB, store actionCallSeedStore, call *ActionCall) {
	t.Helper()
	ctx := context.Background()
	seeded := cloneActionCall(call)
	fillSeededActionCall(seeded)
	run := seedActionRun(t, store, seeded)
	proposal := ActionProposalRecord{Call: seeded, Event: &ActivityEvent{
		ID: "seeded-" + seeded.ID, Scope: seeded.Scope, RunID: seeded.RunID, EventType: "action.seeded",
		Summary: "Seeded action call " + seeded.ID, CreatedAt: seeded.CreatedAt,
	}}
	// Any instant inside the Run's lease admits the call.
	now := run.LeaseExpiresAt.Add(-time.Second)
	if seeded.Origin == ActionOriginVM {
		seedActionStep(t, store, run, seeded.TurnID)
		proposal.Nested = &NestedActionFence{WorkerID: run.LeaseOwner, Now: now, MaxCalls: math.MaxInt32, MaxConcurrent: math.MaxInt32}
	} else {
		proposal.Lease = &AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now}
	}
	if _, err := store.CreateActionProposal(ctx, proposal); err != nil {
		t.Fatalf("seed action call %s: %v", seeded.ID, err)
	}
}

func fillSeededActionCall(call *ActionCall) {
	if call.Origin == "" {
		call.Origin = ActionOriginLoop
	}
	if call.Origin == ActionOriginLoop && call.Execution == "" {
		call.Execution = ActionExecutionLoop
	}
	if call.TurnID == "" {
		call.TurnID = "seeded-step"
	}
	if call.DeploymentID == "" {
		call.DeploymentID = "seeded-agent"
	}
	if call.SkillID == "" {
		call.SkillID = "seeded-skill"
	}
	if call.SkillVersion == "" {
		call.SkillVersion = "1.0.0"
	}
	if call.Action == "" {
		call.Action = "seeded-action"
	}
	if call.Revision < 1 {
		call.Revision = 1
	}
	if call.MaxAttempts < 1 {
		call.MaxAttempts = 1
	}
}

// seedActionRun returns call's Run, creating it running and leased when it
// does not exist.
func seedActionRun(t testing.TB, store actionCallSeedStore, call *ActionCall) *AgentRun {
	t.Helper()
	ctx := context.Background()
	// Stores answer a missing Run with ErrRunNotFound or a nil Run.
	run, err := store.GetAgentRun(ctx, call.Scope, call.RunID)
	if err != nil && !errors.Is(err, ErrRunNotFound) {
		t.Fatal(err)
	}
	if run == nil {
		expires := call.CreatedAt.Add(24 * time.Hour)
		run = &AgentRun{
			ID: call.RunID, Scope: call.Scope, RootRunID: call.RunID, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: call.DeploymentID},
			AssignedAgentID: call.DeploymentID, Goal: "seeded run", Status: AgentRunStatusRunning, LeaseOwner: actionSeedWorker,
			LeaseExpiresAt: &expires, Revision: 1, CreatedAt: call.CreatedAt, UpdatedAt: call.CreatedAt,
		}
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatalf("seed run %s: %v", run.ID, err)
		}
		return run
	}
	if run.Status != AgentRunStatusRunning || run.LeaseOwner == "" || run.LeaseExpiresAt == nil {
		t.Fatalf("seeded action call %s needs its Run running under a lease: %#v", call.ID, run)
	}
	return run
}

// seedActionStep creates the Run's running step id under the Run's lease when
// it does not exist.
func seedActionStep(t testing.TB, store actionCallSeedStore, run *AgentRun, id string) {
	t.Helper()
	ctx := context.Background()
	// Stores answer a missing step with ErrTurnNotFound or a nil step.
	step, err := store.GetAgentTurn(ctx, run.Scope, id)
	if err != nil && !errors.Is(err, ErrTurnNotFound) {
		t.Fatal(err)
	}
	if step != nil {
		return
	}
	if _, err := store.CreateAgentTurn(ctx, &AgentTurn{
		ID: id, Scope: run.Scope, RunID: run.ID, Status: AgentTurnStatusRunning, Producer: StepProducerModel,
		LeaseOwner: run.LeaseOwner, LeaseExpiresAt: run.LeaseExpiresAt, Revision: 1, CreatedAt: run.CreatedAt, UpdatedAt: run.CreatedAt, StartedAt: run.CreatedAt,
	}); err != nil {
		t.Fatalf("seed step %s: %v", id, err)
	}
}
