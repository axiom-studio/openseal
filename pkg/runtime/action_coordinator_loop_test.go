package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const loopProposeWorker = "loop-worker"

// loopProposeFixture is one running Run of release-agent claimed by
// loopProposeWorker, proposing the governed release.deploy action through
// a coordinator with a switchable policy.
type loopProposeFixture struct {
	t           *testing.T
	ctx         context.Context
	store       KernelStore
	scope       Scope
	now         time.Time
	run         *AgentRun
	coordinator *ActionCoordinator
	decision    ActionPolicyDecision
	evaluations int
}

func newLoopProposeFixture(t *testing.T, store KernelStore, mutate func(*CreateAgentRunRequest), validators ...ActionProposalValidator) *loopProposeFixture {
	t.Helper()
	catalog, scope := governedActionCatalog(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := &loopProposeFixture{t: t, ctx: context.Background(), store: store, scope: scope, now: now, decision: ActionPolicyDecision{Disposition: ActionDispositionAllow}}
	f.run = claimLoopProposeRun(t, store, scope, now, loopProposeWorker, mutate)
	f.coordinator = NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		f.evaluations++
		return f.decision, nil
	}), validators...)
	f.coordinator.now = func() time.Time { return now.Add(time.Second) }
	return f
}

// claimLoopProposeRun creates a Run of release-agent and claims it for
// worker under an hour-long lease.
func claimLoopProposeRun(t *testing.T, store KernelStore, scope Scope, now time.Time, worker string, mutate func(*CreateAgentRunRequest)) *AgentRun {
	t.Helper()
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	req := CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective,
	}
	if mutate != nil {
		mutate(&req)
	}
	created, err := portfolio.CreateAgentRun(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), AgentRunClaim{Scope: scope, WorkerID: worker, Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != created.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return claimed
}

func (f *loopProposeFixture) request(toolCallID string) ProposeActionRequest {
	return loopProposeRequest(ProposeActionRequest{
		Scope: f.scope, RunID: f.run.ID, WorkerID: loopProposeWorker, DeploymentID: "release-agent",
		SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Arguments: map[string]interface{}{"environment": "staging"}, Summary: "Deploy the release to staging",
	}, toolCallID)
}

func (f *loopProposeFixture) propose(req ProposeActionRequest) *ActionProposalResult {
	f.t.Helper()
	result, err := f.coordinator.Propose(f.ctx, req)
	if err != nil {
		f.t.Fatalf("propose %s: %v", req.IdempotencyKey, err)
	}
	return result
}

func (f *loopProposeFixture) calls() []*ActionCall {
	f.t.Helper()
	calls, err := f.store.ListActionCalls(f.ctx, ActionFilter{Scope: f.scope, RunID: f.run.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	return calls
}

func (f *loopProposeFixture) requireRejection(err error, code string) {
	f.t.Helper()
	var rejection *ActionProposalRejection
	if !errors.As(err, &rejection) || rejection.Code != code || rejection.Message == "" || len(rejection.Message) > maximumProposalRejectionMessageBytes {
		f.t.Fatalf("error = %#v (%v), want rejection %s", err, err, code)
	}
}

func (f *loopProposeFixture) requireApproval() {
	f.decision = ActionPolicyDecision{
		Disposition: ActionDispositionRequireApproval, Reason: "deploys need review",
		EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "release-manager"}}, ApprovalTTL: time.Hour,
	}
}

// succeedLoopCall completes call as its loop executes it: own claim, then success
// without a Run change.
func succeedLoopCall(t *testing.T, store KernelStore, call *ActionCall, worker string, at time.Time) *ActionCall {
	t.Helper()
	ctx := context.Background()
	claimed, err := store.ClaimNextAction(ctx, ActionClaim{Scope: call.Scope, WorkerID: worker, Now: at, LeaseDuration: LoopActionLease, OwnActionID: call.ID})
	if err != nil || claimed == nil {
		t.Fatalf("own claim = %#v, %v", claimed, err)
	}
	run, err := store.GetAgentRun(ctx, call.Scope, call.RunID)
	if err != nil {
		t.Fatal(err)
	}
	completed := cloneActionCall(claimed)
	completed.Status = ActionCallStatusSucceeded
	completed.Output = map[string]interface{}{"receipt": "external-42"}
	completed.CompletedAt = &at
	completed.LeaseOwner, completed.LeaseExpiresAt = "", nil
	completed.UpdatedAt = at
	completed.Revision++
	result, err := store.PersistActionExecution(ctx, ActionExecutionRecord{
		Call: completed, ExpectedCallRevision: claimed.Revision, ExpectedRunRevision: run.Revision, WorkerID: worker, Now: at,
		Event: &ActivityEvent{ID: "succeeded-" + call.ID, Scope: call.Scope, RunID: call.RunID, EventType: "action.succeeded", Summary: "Action succeeded", CreatedAt: at},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Call
}

func TestLoopProposeAllowCreatesReadyLoopCallAndLeavesRun(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopProposeFixture(t, store, nil)
		result := f.propose(f.request("call-1"))
		call := result.Call
		if !result.Created || result.Run != nil || result.Approval != nil || result.BudgetExhausted || call.Status != ActionCallStatusReady ||
			call.Origin != ActionOriginLoop || call.Execution != ActionExecutionLoop || call.TurnID != "step-1" || call.OriginRef != "" ||
			call.IdempotencyKey != "loop:step-1:call-1" || call.BindingID != "release-binding" {
			t.Fatalf("allowed loop proposal = %#v", result)
		}
		if result.Event.EventType != "action.proposed" || result.Event.TurnID != "step-1" || result.Event.Summary != "Deploy the release to staging" ||
			result.Event.Payload["origin"] != "loop" || result.Event.Payload["execution"] != "loop" {
			t.Fatalf("proposal event = %#v", result.Event)
		}
		assertLoopRunUntouched(t, store, f.run)
		// Only the loop runs its calls: the pool never takes it while the Run lives.
		if claimed, err := store.ClaimNextAction(f.ctx, ActionClaim{Scope: f.scope, WorkerID: "pool", Now: f.now.Add(time.Minute), LeaseDuration: time.Minute}); err != nil || claimed != nil {
			t.Fatalf("pool claimed a live loop call: %#v, %v", claimed, err)
		}
	})
}

func TestLoopProposeDenyLeavesRunRunning(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopProposeFixture(t, store, nil)
		f.decision = ActionPolicyDecision{Disposition: ActionDispositionDeny, Reason: "production freeze"}
		result := f.propose(f.request("call-1"))
		if result.Call.Status != ActionCallStatusDenied || result.Call.Error != "production freeze" || result.Call.CompletedAt == nil ||
			result.Approval != nil || result.Run != nil || result.Event.EventType != "action.denied" || result.Event.Payload["policyReason"] != "production freeze" {
			t.Fatalf("denied loop proposal = %#v", result)
		}
		assertLoopRunUntouched(t, store, f.run)
	})
}

func TestLoopProposeApprovalCreatesPendingApprovalWithoutRunChange(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopProposeFixture(t, store, nil)
		f.requireApproval()
		result := f.propose(f.request("call-1"))
		if result.Call.Status != ActionCallStatusWaitingApproval || result.Approval == nil || result.Call.ApprovalID != result.Approval.ID ||
			result.Approval.Status != ApprovalStatusPending || result.Approval.Summary != "Deploy the release to staging" || result.Run != nil ||
			result.Event.EventType != "action.approval_requested" {
			t.Fatalf("approval loop proposal = %#v", result)
		}
		approval, err := store.GetApproval(f.ctx, f.scope, result.Approval.ID)
		if err != nil || approval.Status != ApprovalStatusPending || approval.ActionCallID != result.Call.ID {
			t.Fatalf("stored approval = %#v, %v", approval, err)
		}
		assertLoopRunUntouched(t, store, f.run)
	})
}

func TestLoopProposeApprovalWithoutSummaryIsRejected(t *testing.T) {
	f := newLoopProposeFixture(t, NewMemoryStore(), nil)
	f.requireApproval()
	req := f.request("call-1")
	req.Summary = "   "
	_, err := f.coordinator.Propose(f.ctx, req)
	f.requireRejection(err, ProposalRejectionReviewSummaryRequired)
	if err.Error() != proposalRejectionReviewSummaryRequiredMessage {
		t.Fatalf("rejection text = %q", err.Error())
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("rejected proposal was recorded: %#v", calls)
	}
	// An allowed call needs no summary; activity keeps its audit fallback.
	f.decision = ActionPolicyDecision{Disposition: ActionDispositionAllow}
	result := f.propose(req)
	if result.Call.Status != ActionCallStatusReady || result.Event.Summary != "Proposed release.deploy" {
		t.Fatalf("allowed call without summary = %#v", result)
	}
}

func TestLoopProposeBudgetExhaustedDenies(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopProposeFixture(t, store, func(req *CreateAgentRunRequest) {
			req.Budget = &BudgetPolicy{MaxActions: 1}
		})
		first := f.propose(f.request("call-1"))
		if first.Call.Status != ActionCallStatusReady || first.BudgetExhausted || first.Run == nil || first.Run.Revision != f.run.Revision+1 ||
			first.Run.Status != AgentRunStatusRunning || first.Run.LeaseOwner != loopProposeWorker {
			t.Fatalf("first proposal = %#v", first)
		}
		if _, ok := first.Run.BudgetReservations[actionBudgetReservationID(first.Call.ID)]; !ok {
			t.Fatalf("budget was not reserved: %#v", first.Run.BudgetReservations)
		}
		reserved, err := store.GetAgentRun(f.ctx, f.scope, f.run.ID)
		if err != nil || reserved.Revision != f.run.Revision+1 {
			t.Fatalf("reserved Run = %#v, %v", reserved, err)
		}
		second := f.propose(f.request("call-2"))
		if !second.BudgetExhausted || second.Call.Status != ActionCallStatusDenied || second.Call.Error != "run action budget is exhausted" ||
			second.Event.EventType != "budget.exhausted" || second.Run != nil {
			t.Fatalf("exhausted proposal = %#v", second)
		}
		assertLoopRunUntouched(t, store, reserved)
	})
}

type failingProposalValidator struct{ err error }

func (v failingProposalValidator) ValidateActionProposal(_ context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if input.History == nil {
		return nil, errors.New("validator got no Run action history")
	}
	if input.Arguments["environment"] == "blocked" {
		return nil, v.err
	}
	return nil, nil
}

func TestLoopProposeValidationErrorsAreRejections(t *testing.T) {
	validatorErr := errors.New("the environment is not admitted for this Run")
	f := newLoopProposeFixture(t, NewMemoryStore(), nil, failingProposalValidator{err: validatorErr})

	missing := f.request("missing")
	missing.Action = "rollback"
	_, err := f.coordinator.Propose(f.ctx, missing)
	f.requireRejection(err, ProposalRejectionCapabilityUnavailable)

	invalid := f.request("invalid")
	invalid.Arguments = map[string]interface{}{"environment": 42}
	_, err = f.coordinator.Propose(f.ctx, invalid)
	f.requireRejection(err, ProposalRejectionToolArgumentsInvalid)

	blocked := f.request("blocked")
	blocked.Arguments = map[string]interface{}{"environment": "blocked"}
	_, err = f.coordinator.Propose(f.ctx, blocked)
	f.requireRejection(err, ProposalRejectionNotAdmitted)
	if err.Error() != validatorErr.Error() || !errors.Is(err, validatorErr) {
		t.Fatalf("validator rejection = %v", err)
	}
	if calls := f.calls(); len(calls) != 0 || f.evaluations != 0 {
		t.Fatalf("rejected proposals reached policy or the store: calls=%#v evaluations=%d", calls, f.evaluations)
	}
	assertLoopRunUntouched(t, f.store, f.run)
}

func TestLoopProposeRejectionMessageIsBounded(t *testing.T) {
	long := strings.Repeat("é", maximumProposalRejectionMessageBytes)
	err := rejectProposal(ProposalRejectionNotAdmitted, errors.New(long))
	var rejection *ActionProposalRejection
	if !errors.As(err, &rejection) || len(rejection.Message) > maximumProposalRejectionMessageBytes || !strings.HasPrefix(long, rejection.Message) {
		t.Fatalf("bounded rejection = %#v", err)
	}
	if again := rejectProposal(ProposalRejectionToolArgumentsInvalid, err); again != err {
		t.Fatalf("existing rejection was rewrapped: %#v", again)
	}
	if rejectProposal(ProposalRejectionNotAdmitted, nil) != nil {
		t.Fatal("nil error became a rejection")
	}
}

func TestLoopProposeCrossRunExternalReceiptSuppressesWithoutRunChange(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopProposeFixture(t, store, nil)
		identity := &ExternalOperationIdentity{Resource: "https://Forum.Example/topics/42?b=2&a=1#reply", Operation: "comment:create"}
		first := f.request("comment")
		first.IdempotencyKey = "routine:comment-42"
		first.ExternalOperation = identity
		proposed := f.propose(first)
		succeedLoopCall(t, store, proposed.Call, loopProposeWorker, f.now.Add(2*time.Second))

		secondRun := claimLoopProposeRun(t, store, f.scope, f.now.Add(3*time.Second), "loop-worker-2", nil)
		second := f.request("comment")
		second.RunID, second.WorkerID = secondRun.ID, "loop-worker-2"
		second.IdempotencyKey = "routine:comment-42"
		second.ExternalOperation = &ExternalOperationIdentity{Resource: "https://forum.example/topics/42?a=1&b=2", Operation: "COMMENT:CREATE"}
		suppressed := f.propose(second)
		call := suppressed.Call
		if call.Status != ActionCallStatusSucceeded || call.DuplicateOfActionCallID != proposed.Call.ID || call.Origin != ActionOriginLoop ||
			call.Execution != ActionExecutionLoop || call.TurnID != "step-1" || call.Output["duplicateSuppressed"] != true ||
			call.Output["priorActionCallId"] != proposed.Call.ID || call.Output["priorRunId"] != f.run.ID || suppressed.Run != nil ||
			suppressed.Approval != nil || suppressed.Event.EventType != "action.duplicate_suppressed" || suppressed.Event.Payload["origin"] != "loop" {
			t.Fatalf("suppressed proposal = %#v", suppressed)
		}
		if f.evaluations != 1 {
			t.Fatalf("policy evaluations = %d, the duplicate is suppressed before policy", f.evaluations)
		}
		assertLoopRunUntouched(t, store, secondRun)
		receipt, err := store.GetActionCallByExternalOperation(f.ctx, f.scope, proposed.Call.ExternalOperationDigest)
		if err != nil || receipt.ID != proposed.Call.ID {
			t.Fatalf("authoritative receipt = %#v, %v", receipt, err)
		}
		replay := f.propose(second)
		if replay.Created || replay.Call.ID != call.ID {
			t.Fatalf("replayed suppression = %#v", replay)
		}
	})
}

func TestLoopProposeInFlightExternalOperationConflicts(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopProposeFixture(t, store, nil)
		identity := &ExternalOperationIdentity{Resource: "https://forum.example/topics/42", Operation: "comment:create"}
		first := f.request("comment")
		first.IdempotencyKey = "routine:comment-42"
		first.ExternalOperation = identity
		proposed := f.propose(first)

		secondRun := claimLoopProposeRun(t, store, f.scope, f.now.Add(time.Second), "loop-worker-2", nil)
		second := first
		second.RunID, second.WorkerID = secondRun.ID, "loop-worker-2"
		_, err := f.coordinator.Propose(f.ctx, second)
		var conflict *ExternalOperationConflictError
		if !errors.As(err, &conflict) || conflict.Prior == nil || conflict.Prior.ID != proposed.Call.ID {
			t.Fatalf("in-flight conflict = %v", err)
		}
		calls, err := store.ListActionCalls(f.ctx, ActionFilter{Scope: f.scope, RunID: secondRun.ID})
		if err != nil || len(calls) != 0 {
			t.Fatalf("conflicting proposal was recorded: %#v, %v", calls, err)
		}
		assertLoopRunUntouched(t, store, secondRun)
	})
}

func TestLoopProposeReplayWithSameKeyReturnsSameCall(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopProposeFixture(t, store, nil)
		req := f.request("call-1")
		req.ExternalOperation = &ExternalOperationIdentity{Resource: "https://forum.example/topics/42", Operation: "comment:create"}
		first := f.propose(req)
		replay := f.propose(req)
		if replay.Created || replay.Call.ID != first.Call.ID || replay.Call.Status != ActionCallStatusReady {
			t.Fatalf("replay of a ready call = %#v", replay)
		}
		succeedLoopCall(t, store, first.Call, loopProposeWorker, f.now.Add(2*time.Second))
		done := f.propose(req)
		if done.Created || done.Call.ID != first.Call.ID || done.Call.Status != ActionCallStatusSucceeded || done.Call.DuplicateOfActionCallID != "" {
			t.Fatalf("replay of a succeeded call = %#v", done)
		}
		changed := req
		changed.Arguments = map[string]interface{}{"environment": "production"}
		if _, err := f.coordinator.Propose(f.ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("changed replay error = %v", err)
		}
		if calls := f.calls(); len(calls) != 1 {
			t.Fatalf("replays recorded more calls: %#v", calls)
		}
	})
}

func TestLoopProposeRequiresLiveLeaseOfRunningRun(t *testing.T) {
	f := newLoopProposeFixture(t, NewMemoryStore(), nil)
	foreign := f.request("foreign")
	foreign.WorkerID = "other-worker"
	if _, err := f.coordinator.Propose(f.ctx, foreign); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign worker error = %v", err)
	}
	expired := f.request("expired")
	f.coordinator.now = func() time.Time { return f.now.Add(2 * time.Hour) }
	if _, err := f.coordinator.Propose(f.ctx, expired); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease error = %v", err)
	}
	for name, mutate := range map[string]func(*ProposeActionRequest){
		"no origin":      func(req *ProposeActionRequest) { req.Origin, req.Execution = "", "" },
		"host execution": func(req *ProposeActionRequest) { req.Execution = ActionExecutionHost },
		"no step":        func(req *ProposeActionRequest) { req.TurnID = "" },
		"origin ref":     func(req *ProposeActionRequest) { req.OriginRef = "command-1" },
	} {
		req := f.request("shape")
		mutate(&req)
		var rejection *ActionProposalRejection
		if _, err := f.coordinator.Propose(f.ctx, req); err == nil || errors.As(err, &rejection) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("refused proposals were recorded: %#v", calls)
	}
}

func TestLoopProposeValidatorsReadRunActionHistory(t *testing.T) {
	store := NewMemoryStore()
	var seen []*ActionCall
	f := newLoopProposeFixture(t, store, nil, proposalValidatorFunc(func(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
		calls, err := input.History.Calls(ctx)
		seen = calls
		return nil, err
	}))
	first := f.propose(f.request("call-1"))
	succeedLoopCall(t, store, first.Call, loopProposeWorker, f.now.Add(2*time.Second))
	store.actions[portfolioKey(f.scope, "vm-call")] = &ActionCall{
		ID: "vm-call", Scope: f.scope, RunID: f.run.ID, TurnID: "step-1", DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Origin: ActionOriginVM, Status: ActionCallStatusSucceeded, MaxAttempts: 1, AvailableAt: f.now, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.propose(f.request("call-2"))
	if len(seen) != 1 || seen[0].ID != first.Call.ID || seen[0].Status != ActionCallStatusSucceeded {
		t.Fatalf("history = %#v", seen)
	}
}

func TestRunActionHistoryListsTerminalNonVMCallsNewestFirst(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	base := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	add := func(id, runID string, origin ActionOrigin, status ActionCallStatus, completed time.Duration) {
		call := &ActionCall{ID: id, Scope: scope, RunID: runID, TurnID: "step-1", Origin: origin, Status: status, CreatedAt: base, UpdatedAt: base}
		if completed > 0 {
			at := base.Add(completed)
			call.CompletedAt = &at
		}
		store.actions[portfolioKey(scope, id)] = call
	}
	add("old", "run", ActionOriginLoop, ActionCallStatusSucceeded, time.Minute)
	add("new", "run", ActionOriginLoop, ActionCallStatusFailed, 3*time.Minute)
	add("refused", "run", ActionOriginLoop, ActionCallStatusRefused, 2*time.Minute)
	add("denied", "run", ActionOriginLoop, ActionCallStatusDenied, 90*time.Second)
	add("ready", "run", ActionOriginLoop, ActionCallStatusReady, 0)
	add("waiting", "run", ActionOriginLoop, ActionCallStatusWaitingApproval, 0)
	add("vm", "run", ActionOriginVM, ActionCallStatusSucceeded, 4*time.Minute)
	add("other-run", "other", ActionOriginLoop, ActionCallStatusSucceeded, 5*time.Minute)
	history := newRunActionHistory(store, scope, "run")
	calls, err := history.Calls(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		ids = append(ids, call.ID)
	}
	if strings.Join(ids, ",") != "new,refused,denied,old" {
		t.Fatalf("history = %v", ids)
	}
	// One read per Propose: later rows are not seen, and callers get copies.
	add("later", "run", ActionOriginLoop, ActionCallStatusSucceeded, 6*time.Minute)
	calls[0].Status = ActionCallStatusCanceled
	again, err := history.Calls(context.Background())
	if err != nil || len(again) != 4 || again[0].ID != "new" || again[0].Status != ActionCallStatusFailed {
		t.Fatalf("memoized history = %#v, %v", again, err)
	}
}

func TestNestedProposeHasNoCheckpointGuards(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	f := newNestedActionFixture(t, store)
	current, err := store.GetAgentRun(ctx, f.scope, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated := cloneAgentRun(current)
	updated.Checkpoint = map[string]interface{}{
		FinalFailureExplanationCheckpointKey: map[string]interface{}{"kind": "execution", "message": "failed"},
		ToolFeedbackCorrectionCheckpointKey:  map[string]interface{}{"sourceAccessChallenge": true},
	}
	updated.Revision++
	event := &ActivityEvent{ID: "old-checkpoint", Scope: f.scope, RunID: f.run.ID, EventType: "run.status_changed", Severity: ActivitySeverityInfo,
		Actor: ActivityActor{Type: "worker", ID: "agent-worker"}, Summary: "Old checkpoint keys", Visibility: ActivityVisibilityScope, CreatedAt: f.now}
	if _, err := store.UpdateAgentRunWithEvent(ctx, updated, current.Revision, event, nil); err != nil {
		t.Fatal(err)
	}
	result, err := f.coordinator.ProposeNested(ctx, f.request("repo.read_file", "bridge-guards"))
	if err != nil || !result.Created || result.Call.Status != ActionCallStatusReady || result.Call.Origin != ActionOriginVM {
		t.Fatalf("nested proposal with old checkpoint keys = %#v, %v", result, err)
	}
}

func TestLoopProposePersonCredentialApproverIsActingUser(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	catalog, scope := personActionCatalog(t)
	run := claimedPersonChatRun(t, store, scope, now, connectAccountTestUser)
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}, {Type: "user", ID: "9"}}, ApprovalTTL: time.Hour}, nil
	}))
	coordinator.now = func() time.Time { return now }
	proposal, err := coordinator.Propose(context.Background(), loopProposeRequest(ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: "release-agent",
		SkillID: "mail", SkillVersion: "1.0.0", Action: "send", Arguments: map[string]interface{}{"to": "ada@example.com"},
		Summary: "Send the weekly update to Ada",
	}, "send-1"))
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || len(proposal.Approval.EligibleApprovers) != 1 || proposal.Approval.EligibleApprovers[0] != (ApprovalPrincipal{Type: "user", ID: connectAccountTestUser}) {
		t.Fatalf("approval = %#v", proposal.Approval)
	}
	assertLoopRunUntouched(t, store, run)
}

// proposalValidatorFunc adapts a function to ActionProposalValidator.
type proposalValidatorFunc func(context.Context, ActionProposalValidationInput) (map[string]interface{}, error)

func (f proposalValidatorFunc) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	return f(ctx, input)
}
