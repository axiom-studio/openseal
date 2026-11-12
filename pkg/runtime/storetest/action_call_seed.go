package storetest

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// ActionCallSeedStore is the public store surface SeedActionCall writes
// through.
type ActionCallSeedStore interface {
	models.ActionStore
	CreateAgentRun(ctx context.Context, run *models.AgentRun) error
	GetAgentRun(ctx context.Context, scope models.Scope, id string) (*models.AgentRun, error)
	CreateAgentTurn(ctx context.Context, turn *models.AgentTurn) (*models.AgentTurn, error)
	GetAgentTurn(ctx context.Context, scope models.Scope, turnID string) (*models.AgentTurn, error)
}

// actionSeedWorker leases the Runs and steps SeedActionCall creates.
const actionSeedWorker = "action-seed-worker"

// SeedActionCall stores a copy of call, with its status, timestamps and
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
func SeedActionCall(t testing.TB, store ActionCallSeedStore, call *models.ActionCall) {
	t.Helper()
	ctx := context.Background()
	seeded := models.CloneActionCall(call)
	fillSeededActionCall(seeded)
	run := seedActionRun(t, store, seeded)
	proposal := models.ActionProposalRecord{Call: seeded, Event: &models.ActivityEvent{
		ID: "seeded-" + seeded.ID, Scope: seeded.Scope, RunID: seeded.RunID, EventType: "action.seeded",
		Summary: "Seeded action call " + seeded.ID, CreatedAt: seeded.CreatedAt,
	}}
	// Any instant inside the Run's lease admits the call.
	now := run.LeaseExpiresAt.Add(-time.Second)
	if seeded.Origin == models.ActionOriginVM {
		seedActionStep(t, store, run, seeded.TurnID)
		proposal.Nested = &models.NestedActionFence{WorkerID: run.LeaseOwner, Now: now, MaxCalls: math.MaxInt32, MaxConcurrent: math.MaxInt32}
	} else {
		proposal.Lease = &models.AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now}
	}
	if _, err := store.CreateActionProposal(ctx, proposal); err != nil {
		t.Fatalf("seed action call %s: %v", seeded.ID, err)
	}
}

func fillSeededActionCall(call *models.ActionCall) {
	if call.Origin == "" {
		call.Origin = models.ActionOriginLoop
	}
	if call.Origin == models.ActionOriginLoop && call.Execution == "" {
		call.Execution = models.ActionExecutionLoop
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
func seedActionRun(t testing.TB, store ActionCallSeedStore, call *models.ActionCall) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	// Stores answer a missing Run with ErrRunNotFound or a nil Run.
	run, err := store.GetAgentRun(ctx, call.Scope, call.RunID)
	if err != nil && !errors.Is(err, models.ErrRunNotFound) {
		t.Fatal(err)
	}
	if run == nil {
		expires := call.CreatedAt.Add(24 * time.Hour)
		run = &models.AgentRun{
			ID: call.RunID, Scope: call.Scope, RootRunID: call.RunID, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: call.DeploymentID},
			AssignedAgentID: call.DeploymentID, Goal: "seeded run", Status: models.AgentRunStatusRunning, LeaseOwner: actionSeedWorker,
			LeaseExpiresAt: &expires, Revision: 1, CreatedAt: call.CreatedAt, UpdatedAt: call.CreatedAt,
		}
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatalf("seed run %s: %v", run.ID, err)
		}
		return run
	}
	if run.Status != models.AgentRunStatusRunning || run.LeaseOwner == "" || run.LeaseExpiresAt == nil {
		t.Fatalf("seeded action call %s needs its Run running under a lease: %#v", call.ID, run)
	}
	return run
}

// seedActionStep creates the Run's running step id under the Run's lease when
// it does not exist.
func seedActionStep(t testing.TB, store ActionCallSeedStore, run *models.AgentRun, id string) {
	t.Helper()
	ctx := context.Background()
	// Stores answer a missing step with ErrTurnNotFound or a nil step.
	step, err := store.GetAgentTurn(ctx, run.Scope, id)
	if err != nil && !errors.Is(err, models.ErrTurnNotFound) {
		t.Fatal(err)
	}
	if step != nil {
		return
	}
	if _, err := store.CreateAgentTurn(ctx, &models.AgentTurn{
		ID: id, Scope: run.Scope, RunID: run.ID, Status: models.AgentTurnStatusRunning, Producer: models.StepProducerModel,
		LeaseOwner: run.LeaseOwner, LeaseExpiresAt: run.LeaseExpiresAt, Revision: 1, CreatedAt: run.CreatedAt, UpdatedAt: run.CreatedAt, StartedAt: run.CreatedAt,
	}); err != nil {
		t.Fatalf("seed step %s: %v", id, err)
	}
}
