package action_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestLoopProposalRequiresRunningRunAndLiveLease(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, loopProposalRequiresRunningRunAndLiveLease)
}

func loopProposalRequiresRunningRunAndLiveLease(t *testing.T, store models.KernelStore) {
	f := actiontest.NewLoopStore(t, store)
	expired := f.Proposal(f.LoopCall("expired-lease"))
	expired.Lease.Now = f.Now.Add(2 * time.Hour)
	if _, err := store.CreateActionProposal(f.Ctx, expired); !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("expired lease = %v", err)
	}
	f.RequireNoCall("expired-lease")
	foreign := f.Proposal(f.LoopCall("foreign-owner"))
	foreign.Lease.WorkerID = "other-worker"
	if _, err := store.CreateActionProposal(f.Ctx, foreign); !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("foreign owner = %v", err)
	}
	f.RequireNoCall("foreign-owner")
	unleased := f.Proposal(f.LoopCall("no-lease"))
	unleased.Lease = nil
	if _, err := store.CreateActionProposal(f.Ctx, unleased); err == nil {
		t.Fatal("a loop proposal without its Run lease was accepted")
	}
	f.RequireNoCall("no-lease")

	// A queued Run whose lease fields survived is still not running.
	current := f.CurrentRun()
	queued := models.CloneAgentRun(current)
	queued.Status = models.AgentRunStatusQueued
	queued.Revision++
	if _, err := store.UpdateAgentRunWithEvent(f.Ctx, queued, current.Revision, f.Event("queued", "run.status_changed"), nil); err != nil {
		t.Fatal(err)
	}
	f.Run = f.CurrentRun()
	if _, err := store.CreateActionProposal(f.Ctx, f.Proposal(f.LoopCall("queued-run"))); !errors.Is(err, models.ErrLeaseLost) {
		t.Fatalf("queued Run = %v", err)
	}
	f.RequireNoCall("queued-run")
}

func TestLoopProposalWithoutRunUpdateIgnoresRunRevision(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		observed := f.Run.Revision
		heartbeat, err := store.RenewAgentRunLease(f.Ctx, f.Scope, f.Run.ID, actiontest.LoopStoreWorker, f.Now, time.Hour)
		if err != nil || heartbeat.Revision == observed {
			t.Fatalf("heartbeat = %#v, %v", heartbeat, err)
		}
		proposal := f.Proposal(f.LoopCall("after-heartbeat"))
		proposal.ExpectedRunRevision = observed
		result, err := store.CreateActionProposal(f.Ctx, proposal)
		if err != nil || !result.Created || result.Call.ID != "after-heartbeat" {
			t.Fatalf("proposal = %#v, %v", result, err)
		}
		if run := f.CurrentRun(); run.Revision != heartbeat.Revision || run.Status != models.AgentRunStatusRunning {
			t.Fatalf("Run changed: %#v", run)
		}
	})
}

func TestLoopProposalBudgetReservationIsRevisionFenced(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		reserve := func(observed *models.AgentRun) *models.AgentRun {
			updated := models.CloneAgentRun(observed)
			updated.Revision++
			updated.UpdatedAt = f.Now
			return updated
		}
		observed := f.CurrentRun()
		if _, err := store.RenewAgentRunLease(f.Ctx, f.Scope, f.Run.ID, actiontest.LoopStoreWorker, f.Now, time.Hour); err != nil {
			t.Fatal(err)
		}
		stale := f.Proposal(f.LoopCall("stale-reservation"))
		stale.Run, stale.ExpectedRunRevision = reserve(observed), observed.Revision
		if _, err := store.CreateActionProposal(f.Ctx, stale); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("stale reservation = %v", err)
		}
		f.RequireNoCall("stale-reservation")
		current := f.CurrentRun()
		fresh := f.Proposal(f.LoopCall("fresh-reservation"))
		fresh.Run, fresh.ExpectedRunRevision = reserve(current), current.Revision
		if result, err := store.CreateActionProposal(f.Ctx, fresh); err != nil || !result.Created {
			t.Fatalf("fresh reservation = %#v, %v", result, err)
		}
		if run := f.CurrentRun(); run.Revision != current.Revision+1 {
			t.Fatalf("reserved Run revision = %d, want %d", run.Revision, current.Revision+1)
		}
	})
}

func TestOriginlessProposalIsRejected(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		call := f.LoopCall("originless")
		call.Origin, call.Execution = "", ""
		proposal := f.Proposal(call)
		updated := models.CloneAgentRun(f.Run)
		updated.Revision++
		proposal.Run = updated
		if _, err := store.CreateActionProposal(f.Ctx, proposal); err == nil || !strings.Contains(err.Error(), "action origin is invalid") {
			t.Fatalf("originless proposal = %v", err)
		}
		f.RequireNoCall("originless")
	})
}

func TestLoopActionCallValidation(t *testing.T) {
	f := &actiontest.LoopStore{Scope: models.Scope{Kind: "tenant", ID: "one"}, Now: time.Now().UTC(), Run: &models.AgentRun{ID: "run"}}
	if err := f.LoopCall("valid").Validate(); err != nil {
		t.Fatalf("loop call = %v", err)
	}
	for name, mutate := range map[string]func(*models.ActionCall){
		"no origin":       func(c *models.ActionCall) { c.Origin, c.Execution = "", "" },
		"unknown origin":  func(c *models.ActionCall) { c.Origin = "model" },
		"no step":         func(c *models.ActionCall) { c.TurnID = "" },
		"worker executed": func(c *models.ActionCall) { c.Execution = "" },
		"host executed":   func(c *models.ActionCall) { c.Execution = models.ActionExecutionHost },
		"origin ref":      func(c *models.ActionCall) { c.OriginRef = "command-1" },
		"vm loop execute": func(c *models.ActionCall) { c.Origin = models.ActionOriginVM },
	} {
		call := f.LoopCall("invalid")
		mutate(call)
		if err := call.Validate(); err == nil {
			t.Errorf("%s: invalid loop call validated", name)
		}
	}
	vm := f.LoopCall("vm")
	vm.Origin, vm.Execution, vm.OriginRef = models.ActionOriginVM, models.ActionExecutionHost, "command-1"
	if err := vm.Validate(); err != nil {
		t.Fatalf("vm host call = %v", err)
	}
	vm.Execution = ""
	if err := vm.Validate(); err != nil {
		t.Fatalf("vm worker call = %v", err)
	}
}

func TestRefusedStatusValidatesWithDuplicateOf(t *testing.T) {
	f := &actiontest.LoopStore{Scope: models.Scope{Kind: "tenant", ID: "one"}, Now: time.Now().UTC(), Run: &models.AgentRun{ID: "run"}}
	refused := f.LoopCall("refused")
	refused.Status = models.ActionCallStatusRefused
	refused.DuplicateOfActionCallID = "first-call"
	refused.ErrorCode = "repeat_refused"
	refused.ErrorDetails = map[string]string{"step": "2"}
	refused.CompletedAt = &f.Now
	if err := refused.Validate(); err != nil {
		t.Fatalf("refused duplicate = %v", err)
	}
	failed := f.LoopCall("failed")
	failed.Status = models.ActionCallStatusFailed
	failed.DuplicateOfActionCallID = "first-call"
	if err := failed.Validate(); err == nil {
		t.Fatal("a non-refused duplicate validated without an external operation digest")
	}
	// refused.Validate passing above shows refused is a valid status.
	if models.ExternalOperationProtects(models.ActionCallStatusRefused) {
		t.Fatal("refused must be a valid status that never protects an external operation")
	}
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		row := f.LoopCall("refused-row")
		row.Status, row.DuplicateOfActionCallID, row.ErrorCode, row.CompletedAt = models.ActionCallStatusRefused, "first-call", "duplicate_suppressed", &f.Now
		f.Propose(row)
		if claimed := f.PoolClaim(f.Now.Add(time.Hour)); claimed != nil {
			t.Fatalf("refused call claimed: %#v", claimed)
		}
		f.SetRunStatus(models.AgentRunStatusCanceled)
		if claimed := f.PoolClaim(f.Now.Add(time.Hour)); claimed != nil {
			t.Fatalf("refused call of ended Run claimed: %#v", claimed)
		}
		stored, err := store.GetActionCall(f.Ctx, f.Scope, "refused-row")
		if err != nil || stored.Status != models.ActionCallStatusRefused || stored.DuplicateOfActionCallID != "first-call" || stored.ErrorCode != "duplicate_suppressed" {
			t.Fatalf("stored refused row = %#v, %v", stored, err)
		}
	})
}

func TestPoolNeverClaimsLoopCallOfLiveRun(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, poolNeverClaimsLoopCallOfLiveRun)
}

func poolNeverClaimsLoopCallOfLiveRun(t *testing.T, store models.KernelStore) {
	for _, status := range []models.AgentRunStatus{
		models.AgentRunStatusQueued, models.AgentRunStatusRunning, models.AgentRunStatusWaitingForApproval, models.AgentRunStatusWaitingForEvent, models.AgentRunStatusPaused,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := actiontest.NewLoopStore(t, store)
			f.Propose(f.LoopCall("ready-call"))
			f.Propose(f.LoopCall("abandoned-call"))
			if claimed := f.OwnClaim("abandoned-call", f.Now); claimed == nil || claimed.Status != models.ActionCallStatusRunning {
				t.Fatalf("own claim = %#v", claimed)
			}
			if status != models.AgentRunStatusRunning {
				f.SetRunStatus(status)
			}
			// Well past the loop's call lease: the running call is expired too.
			if claimed := f.PoolClaim(f.Now.Add(10 * models.LoopActionLease)); claimed != nil {
				t.Fatalf("pool claimed loop call %s of a %s Run", claimed.ID, status)
			}
		})
	}
}

func TestPoolClaimsLoopCallOfEndedRun(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		f.Propose(f.LoopCall("ready-call"))
		f.SetRunStatus(models.AgentRunStatusCanceled)
		claimed := f.PoolClaim(f.Now.Add(time.Second))
		if claimed == nil || claimed.ID != "ready-call" || claimed.Status != models.ActionCallStatusRunning || claimed.LeaseOwner != "pool-worker" {
			t.Fatalf("pool claim of ended Run's call = %#v", claimed)
		}
	})
}

func TestOwnClaimTakesReadyOrExpiredRunningLoopCall(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		f.Propose(f.LoopCall("own-call"))
		f.Propose(f.LoopCall("other-call"))
		first := f.OwnClaim("own-call", f.Now)
		if first == nil || first.ID != "own-call" || first.Status != models.ActionCallStatusRunning || first.LeaseOwner != actiontest.LoopStoreWorker || first.Attempt != 1 || first.RecoveredRunning {
			t.Fatalf("own claim of ready call = %#v", first)
		}
		if live := f.OwnClaim("own-call", f.Now.Add(models.LoopActionLease/2)); live != nil {
			t.Fatalf("own claim took a running call under a live lease: %#v", live)
		}
		recovered := f.OwnClaim("own-call", f.Now.Add(2*models.LoopActionLease))
		if recovered == nil || recovered.ID != "own-call" || recovered.Attempt != 2 || !recovered.RecoveredRunning {
			t.Fatalf("own claim of expired running call = %#v", recovered)
		}
		if missing := f.OwnClaim("unknown-call", f.Now); missing != nil {
			t.Fatalf("own claim of an unknown id = %#v", missing)
		}
	})
}

func TestGetActionCallByIdempotencyKey(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		f.Propose(f.LoopCall("keyed-call"))
		found, err := store.GetActionCallByIdempotencyKey(f.Ctx, f.Scope, f.Run.ID, "tool-call:keyed-call")
		if err != nil || found == nil || found.ID != "keyed-call" || found.Origin != models.ActionOriginLoop || found.Execution != models.ActionExecutionLoop {
			t.Fatalf("lookup = %#v, %v", found, err)
		}
		for name, lookup := range map[string]func() (*models.ActionCall, error){
			"other key": func() (*models.ActionCall, error) {
				return store.GetActionCallByIdempotencyKey(f.Ctx, f.Scope, f.Run.ID, "tool-call:missing")
			},
			"other run": func() (*models.ActionCall, error) {
				return store.GetActionCallByIdempotencyKey(f.Ctx, f.Scope, "other-run", "tool-call:keyed-call")
			},
			"other scope": func() (*models.ActionCall, error) {
				return store.GetActionCallByIdempotencyKey(f.Ctx, models.Scope{Kind: "tenant", ID: "other"}, f.Run.ID, "tool-call:keyed-call")
			},
			"empty key": func() (*models.ActionCall, error) {
				return store.GetActionCallByIdempotencyKey(f.Ctx, f.Scope, f.Run.ID, "")
			},
		} {
			if call, err := lookup(); !errors.Is(err, models.ErrActionNotFound) {
				t.Errorf("%s = %#v, %v; want ErrActionNotFound", name, call, err)
			}
		}
	})
}

func TestLoopApprovalResolutionWithoutRunIsValid(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		call := f.LoopCall("reviewed-call")
		call.Action, call.Risk, call.SideEffect = "push", skill.RiskLevelExternal, skill.SideEffectExternal
		call.Status, call.ApprovalID = models.ActionCallStatusWaitingApproval, "approval-1"
		call.InvocationDigest = models.ComputeActionInvocationDigest(call)
		call.SemanticDigest = models.ComputeActionSemanticDigest(call)
		approval := &models.ApprovalCheckpoint{
			ID: "approval-1", Scope: f.Scope, RunID: f.Run.ID, ActionCallID: call.ID, Status: models.ApprovalStatusPending,
			Risk: skill.RiskLevelExternal, Summary: "Push the branch", ProposedAction: map[string]interface{}{"path": call.ID},
			EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "user"}}, ExpiresAt: f.Now.Add(time.Hour), Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now,
		}
		proposal := f.Proposal(call)
		proposal.Approval = approval
		if result, err := store.CreateActionProposal(f.Ctx, proposal); err != nil || !result.Created {
			t.Fatalf("proposal = %#v, %v", result, err)
		}
		before := f.CurrentRun()
		decided := models.CloneApprovalCheckpoint(approval)
		decided.Status, decided.DecisionID, decided.DecisionBy, decided.DecidedAt = models.ApprovalStatusApproved, "decision-1", &models.ApprovalPrincipal{Type: "user", ID: "user"}, &f.Now
		decided.Revision++
		decided.UpdatedAt = f.Now
		ready := models.CloneActionCall(call)
		ready.Status = models.ActionCallStatusReady
		ready.Revision++
		ready.UpdatedAt = f.Now
		result, err := store.ResolveApproval(f.Ctx, models.ApprovalResolutionRecord{
			Approval: decided, ExpectedApprovalRevision: approval.Revision, Call: ready, ExpectedCallRevision: call.Revision,
			Event: f.Event("approval-decided", "approval.approved"),
		})
		if err != nil || !result.Resolved || result.Call.Status != models.ActionCallStatusReady {
			t.Fatalf("resolution = %#v, %v", result, err)
		}
		if after := f.CurrentRun(); after.Revision != before.Revision || after.Status != models.AgentRunStatusRunning {
			t.Fatalf("resolution without a Run update changed the Run: %#v", after)
		}
	})
}
