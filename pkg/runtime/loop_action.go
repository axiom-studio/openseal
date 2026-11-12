package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"go.uber.org/zap"
)

// GovernedToolCall is one governed tool call of a run loop step, as the
// kernel tool executor hands it to the governed executor.
type GovernedToolCall struct {
	// Run is the claimed Run (a clone): running, LeaseOwner == WorkerID.
	Run *AgentRun
	// Step is the running step row; its id is the call's TurnID.
	Step     *AgentTurn
	WorkerID string
	Binding  *TurnRunnerBinding
	// ToolCallID is the provider tool call id (LoopToolCall.ID).
	ToolCallID string
	// Name is the tool name, capability.ModelAction.Name.
	Name string
	// Arguments are the tool input as stored on the tool entry.
	Arguments json.RawMessage
	// Directive is set by deterministic producers only.
	Directive *ToolCallDirective
	// Narration is the step's assistant text; "" for deterministic producers.
	Narration string

	// chain is the Run as the executor's own writes left it during one
	// Execute or Resume; finish sets it.
	chain *governedRunChain
	// keyStepID is the step whose tool entry this call resumes, when that
	// is an earlier step than Step: the idempotency key stays the one the
	// call was first proposed under. "" means Step.
	keyStepID string
}

// idempotencyKey is the loop key of this tool call: under the step that
// first appended its tool entry, so a resume in a later step finds the
// same ActionCall and never proposes a duplicate.
func (call GovernedToolCall) idempotencyKey() string {
	stepID := call.keyStepID
	if stepID == "" {
		stepID = call.Step.ID
	}
	return loopIdempotencyKey(stepID, call.ToolCallID, call.Directive)
}

// governedRunChain follows the Run through the executor's own writes. A
// written Run is adopted only when it is the next revision of the Run the
// executor already knows: a gap means someone else wrote the Run in between
// (a held approval decision or card settle, section 03 A5), and handing the
// loop that revision would let its step-end park CAS succeed on a wait that
// was already decided. The loop's park then fails its CAS and re-plans.
type governedRunChain struct {
	known  *AgentRun
	broken bool
}

func (c *governedRunChain) adopt(written *AgentRun) {
	if c == nil || written == nil || c.broken {
		return
	}
	if written.Revision != c.known.Revision+1 {
		c.broken = true
		return
	}
	c.known = written
}

// GovernedToolExecutorConfig wires the governed executor.
type GovernedToolExecutorConfig struct {
	Actions   *ActionCoordinator
	Approvals *ApprovalCoordinator
	// Worker executes loop calls inline in the claiming replica.
	Worker *ActionWorker
	Store  KernelStore
	// Observer projects created proposals (optional).
	Observer ActionProposalObserver
	// Logger reports observer failures (optional).
	Logger *zap.SugaredLogger
	Now    func() time.Time
}

// GovernedToolExecutor proposes, executes inline and resumes the governed
// Skill and MCP tool calls of run loop steps (section 03 §8). It never parks
// the Run itself: awaiting outcomes carry the park the loop applies at step
// end (PlanPark, ReplanPark).
type GovernedToolExecutor struct {
	governedParker
	actions  *ActionCoordinator
	worker   *ActionWorker
	store    KernelStore
	observer ActionProposalObserver
	logger   *zap.SugaredLogger
	now      func() time.Time
}

// governedProposeAttempts bounds Propose retries on a Run revision conflict
// (only a budget reservation can conflict, with the Run lease heartbeat).
const governedProposeAttempts = 4

// governedNotClaimableRetry is how long the loop waits before it tries a call
// again that it could not claim twice in a row.
const governedNotClaimableRetry = 5 * time.Second

func NewGovernedToolExecutor(config GovernedToolExecutorConfig) (*GovernedToolExecutor, error) {
	if config.Actions == nil || config.Approvals == nil || config.Worker == nil || config.Store == nil {
		return nil, errors.New("governed tool executor requires actions, approvals, an action worker and a store")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &GovernedToolExecutor{
		governedParker: newGovernedParker(config.Store, config.Approvals, now),
		actions:        config.Actions, worker: config.Worker, store: config.Store,
		observer: config.Observer, logger: logger, now: now,
	}, nil
}

// IsGoverned reports whether name is a governed action of the binding.
func (x *GovernedToolExecutor) IsGoverned(binding *TurnRunnerBinding, name string) bool {
	if binding == nil || name == "" {
		return false
	}
	for index := range binding.ModelActions {
		if binding.ModelActions[index].Name == name {
			return true
		}
	}
	return false
}

// loopIdempotencyKey is stable for one tool call of one step, so a crash
// replay of the same call reuses its ActionCall and never collapses two
// different calls.
func loopIdempotencyKey(stepID, toolCallID string, directive *ToolCallDirective) string {
	if directive != nil && strings.TrimSpace(directive.IdempotencyKey) != "" {
		return strings.TrimSpace(directive.IdempotencyKey)
	}
	return "loop:" + stepID + ":" + toolCallID
}

// governedDirective is the governance part of a producer directive.
func governedDirective(directive *ToolCallDirective) *governedInputDirective {
	if directive == nil {
		return nil
	}
	return &governedInputDirective{Summary: directive.Summary, ExternalOperation: directive.ExternalOperation, ReviewContext: directive.ReviewContext}
}

// Execute proposes the governed tool call and, when it may run, executes it
// inline under the Run lease. A call already proposed under the same key (a
// crash replay) is settled from its row without validating it again.
func (x *GovernedToolExecutor) Execute(ctx context.Context, call GovernedToolCall) (GovernedToolOutcome, error) {
	if err := validateGovernedToolCall(call); err != nil {
		return GovernedToolOutcome{}, err
	}
	existing, err := x.callByKey(ctx, call)
	if err != nil {
		return GovernedToolOutcome{}, err
	}
	if existing != nil {
		return x.finish(call, func(call GovernedToolCall) (GovernedToolOutcome, error) {
			return x.settle(ctx, call, existing, nil, false)
		})
	}
	return x.finish(call, func(call GovernedToolCall) (GovernedToolOutcome, error) { return x.propose(ctx, call) })
}

// Resume settles an open governed tool entry of an earlier step (or of this
// step after a crash): it executes calls that never got a row or are ready,
// re-parks waiting ones, renders terminal ones with their settled card text,
// and supersedes approvals a newer message already replaced in a sibling.
func (x *GovernedToolExecutor) Resume(ctx context.Context, call GovernedToolCall, entry *SessionEntry, settled []AgentRunIntervention) (GovernedToolOutcome, error) {
	if err := validateGovernedToolCall(call); err != nil {
		return GovernedToolOutcome{}, err
	}
	if entry != nil && entry.StepID != "" {
		if entry.ToolCallID != call.ToolCallID {
			return GovernedToolOutcome{}, fmt.Errorf("tool entry %q does not belong to tool call %q", entry.ToolCallID, call.ToolCallID)
		}
		call.keyStepID = entry.StepID
	}
	var current *ActionCall
	var err error
	if entry != nil && entry.ActionCallID != "" {
		current, err = x.store.GetActionCall(ctx, call.Run.Scope, entry.ActionCallID)
		if err != nil {
			return GovernedToolOutcome{}, err
		}
	} else if current, err = x.callByKey(ctx, call); err != nil {
		return GovernedToolOutcome{}, err
	}
	if current == nil {
		// Never proposed: nothing was dispatched, so reads and writes both run.
		return x.Execute(ctx, call)
	}
	if current.RunID != call.Run.ID || current.Origin != ActionOriginLoop {
		return GovernedToolOutcome{}, fmt.Errorf("action call %s is not a loop call of Run %s", current.ID, call.Run.ID)
	}
	return x.finish(call, func(call GovernedToolCall) (GovernedToolOutcome, error) {
		if current.Status == ActionCallStatusWaitingApproval {
			if replaced, ok, err := x.supersedeSibling(ctx, call, current, settled); err != nil || ok {
				return replaced, err
			}
		}
		return x.settle(ctx, call, current, settled, false)
	})
}

func validateGovernedToolCall(call GovernedToolCall) error {
	switch {
	case call.Run == nil || call.Step == nil:
		return errors.New("governed tool call requires its Run and step")
	case strings.TrimSpace(call.WorkerID) == "" || call.Run.LeaseOwner != call.WorkerID:
		return ErrLeaseLost
	case strings.TrimSpace(call.ToolCallID) == "":
		return errors.New("governed tool call requires a tool call id")
	}
	return nil
}

// callByKey is the Run's call for this tool call's idempotency key, nil when
// none was proposed.
func (x *GovernedToolExecutor) callByKey(ctx context.Context, call GovernedToolCall) (*ActionCall, error) {
	key := call.idempotencyKey()
	existing, err := x.store.GetActionCallByIdempotencyKey(ctx, call.Run.Scope, call.Run.ID, key)
	if errors.Is(err, ErrActionNotFound) {
		return nil, nil
	}
	return existing, err
}

// finish runs body and reports the Run as the executor's own writes left it.
// It never reads the Run again: a read taken after the wait target was
// inspected could carry a held decision's revision bump (governedRunChain).
func (x *GovernedToolExecutor) finish(call GovernedToolCall, body func(GovernedToolCall) (GovernedToolOutcome, error)) (GovernedToolOutcome, error) {
	call.chain = &governedRunChain{known: call.Run}
	outcome, err := body(call)
	if err != nil {
		return GovernedToolOutcome{}, err
	}
	outcome.ToolCallID = call.ToolCallID
	outcome.Run = nil
	if call.chain.known != call.Run {
		outcome.Run = call.chain.known
	}
	return outcome, nil
}

// refused is a refusal before any row exists.
func refusedOutcome(state ToolEntryState, result *ToolResult) (GovernedToolOutcome, error) {
	return GovernedToolOutcome{State: state, Result: result}, nil
}

func (x *GovernedToolExecutor) propose(ctx context.Context, call GovernedToolCall) (GovernedToolOutcome, error) {
	action, err := selectGovernedAction(call.Binding, call.Name, call.Directive)
	if err != nil {
		return GovernedToolOutcome{}, err
	}
	if action == nil {
		return refusedOutcome(renderGovernedRejection(&ActionProposalRejection{
			Code: ProposalRejectionUnknownTool, Message: fmt.Sprintf("No tool named %q is available here.", call.Name),
		}))
	}
	input, err := ParseGovernedToolInput(*action, call.Arguments, governedDirective(call.Directive))
	if err != nil {
		var rejection *ActionProposalRejection
		if errors.As(err, &rejection) {
			return refusedOutcome(renderGovernedRejection(rejection))
		}
		return GovernedToolOutcome{}, err
	}
	key := call.idempotencyKey()
	injectGovernedIdempotencyKey(*action, input.Arguments, key)
	summary := input.ReviewSummary
	if summary == "" {
		summary = narrationSummary(call.Narration)
	}
	req := ProposeActionRequest{
		Scope: call.Run.Scope, RunID: call.Run.ID, TurnID: call.Step.ID, WorkerID: call.WorkerID,
		DeploymentID: actionDeploymentForSelection(call.Binding, action),
		BindingID:    action.BindingID, BindingRevision: action.BindingRevision,
		SkillID: action.SkillID, SkillVersion: action.Version, Action: action.Action,
		Arguments:       action.ActionInput(input.Arguments),
		PreparedRuntime: preparedRuntimeForAction(call.Binding, action),
		IdempotencyKey:  key, Summary: summary,
		Actor:             ActivityActor{Type: "worker", ID: call.WorkerID},
		EvidenceRefs:      append([]string(nil), input.EvidenceRefs...),
		ExternalOperation: input.ExternalOperation, ReviewContext: input.ReviewContext,
		Origin: ActionOriginLoop, Execution: ActionExecutionLoop, CausationID: call.Step.ID,
	}
	var proposal *ActionProposalResult
	for attempt := 0; attempt < governedProposeAttempts; attempt++ {
		proposal, err = x.actions.Propose(ctx, req)
		if !errors.Is(err, ErrRevisionConflict) {
			break
		}
		if ctx.Err() != nil {
			return GovernedToolOutcome{}, ctx.Err()
		}
	}
	if err != nil {
		var rejection *ActionProposalRejection
		var maintenance *SkillRuntimeMaintenanceError
		var inFlight *ExternalOperationConflictError
		switch {
		case errors.As(err, &rejection):
			return refusedOutcome(renderGovernedRejection(rejection))
		case errors.As(err, &maintenance):
			return refusedOutcome(renderGovernedMaintenanceRefusal())
		case errors.As(err, &inFlight) && inFlight.Prior != nil:
			return refusedOutcome(renderGovernedInFlight(inFlight.Prior.ID))
		}
		return GovernedToolOutcome{}, err
	}
	if proposal == nil || proposal.Call == nil {
		return GovernedToolOutcome{}, errors.New("governed proposal returned no action call")
	}
	call.chain.adopt(proposal.Run)
	if proposal.Created && x.observer != nil {
		observed := call.Run
		if proposal.Run != nil {
			observed = proposal.Run
		}
		if observeErr := x.observer.ObserveActionProposal(ctx, observed, call.Step, proposal); observeErr != nil {
			x.logger.Warnw("governed action projection will require reconciliation", "runId", call.Run.ID, "stepId", call.Step.ID, "actionCallId", proposal.Call.ID, "error", observeErr)
		}
	}
	if proposal.BudgetExhausted {
		state, result := renderGovernedBudgetExhausted(proposal.Call)
		return GovernedToolOutcome{State: state, Result: result, ActionCallID: proposal.Call.ID, BudgetExhausted: true}, nil
	}
	return x.settle(ctx, call, proposal.Call, nil, false)
}

// injectGovernedIdempotencyKey fills the action's own idempotencyKey
// argument, when its schema declares one and the model left it empty, with
// the loop key: the kernel owns idempotency, the model never writes it.
func injectGovernedIdempotencyKey(action capability.ModelAction, arguments map[string]interface{}, key string) {
	if arguments == nil {
		return
	}
	properties, _ := action.InputSchema["properties"].(map[string]interface{})
	if _, declared := properties["idempotencyKey"]; !declared {
		return
	}
	if value, exists := arguments["idempotencyKey"]; exists {
		if text, ok := value.(string); !ok || strings.TrimSpace(text) != "" {
			return
		}
	}
	arguments["idempotencyKey"] = key
}

// settle turns the call's current row into its outcome, executing a ready
// (or lease-expired running) call inline. retried is set after one
// not-claimable re-read.
func (x *GovernedToolExecutor) settle(ctx context.Context, call GovernedToolCall, current *ActionCall, settled []AgentRunIntervention, retried bool) (GovernedToolOutcome, error) {
	switch current.Status {
	case ActionCallStatusReady:
		return x.executeInline(ctx, call, current, settled, retried)
	case ActionCallStatusRunning:
		if current.LeaseExpiresAt != nil && current.LeaseExpiresAt.After(x.now().UTC()) {
			at := current.LeaseExpiresAt.UTC()
			return GovernedToolOutcome{State: ToolEntryRunning, ActionCallID: current.ID, RetryAt: &at}, nil
		}
		return x.executeInline(ctx, call, current, settled, retried)
	case ActionCallStatusWaitingApproval:
		approval, err := x.store.GetApproval(ctx, call.Run.Scope, current.ApprovalID)
		if err != nil {
			return GovernedToolOutcome{}, err
		}
		return governedApprovalOutcome(call.ToolCallID, current, approval), nil
	}
	run := call.chain.known
	if requiresHuman, _ := current.Output["requiresHuman"].(bool); requiresHuman && !runHasHumanInterventionFor(run, current.ID) {
		// Inspection only: the intervention was written by a write the chain
		// did not adopt. This Run is never handed to the loop.
		latest, err := x.store.GetAgentRun(ctx, call.Run.Scope, call.Run.ID)
		if err != nil {
			return GovernedToolOutcome{}, err
		}
		if latest != nil {
			run = latest
		}
	}
	wake, summary, err := x.governedCardWait(ctx, run, current)
	if err != nil {
		return GovernedToolOutcome{}, err
	}
	if wake != nil {
		return governedCardOutcome(call.ToolCallID, current, wake, summary), nil
	}
	if isAgentTrialCall(current) {
		// The trial's reply is the tool result, not the started-trial output.
		output, err := x.agentTrialOutput(ctx, run, current, settled)
		if err != nil {
			return GovernedToolOutcome{}, err
		}
		return GovernedToolOutcome{
			State: ToolEntryCompleted, ActionCallID: current.ID, Output: maps.Clone(output),
			Result: governedJSONResult(ToolResultSucceeded, map[string]interface{}{"evidenceRef": GovernedEvidenceRef(current.ID), "output": output}),
		}, nil
	}
	var approval *ApprovalCheckpoint
	if current.ApprovalID != "" {
		if approval, err = x.store.GetApproval(ctx, call.Run.Scope, current.ApprovalID); err != nil {
			return GovernedToolOutcome{}, err
		}
	}
	state, result := renderGovernedResult(current, approval, x.settledText(ctx, run, current, settled))
	if result == nil {
		return GovernedToolOutcome{}, fmt.Errorf("action call %s has status %s, which a governed tool entry cannot take", current.ID, current.Status)
	}
	outcome := GovernedToolOutcome{State: state, Result: result, ActionCallID: current.ID}
	if current.Status == ActionCallStatusSucceeded && current.DuplicateOfActionCallID == "" {
		outcome.Output = maps.Clone(current.Output)
	}
	if current.Status == ActionCallStatusDenied && current.Error == actionBudgetExhaustedReason {
		outcome.BudgetExhausted = true
	}
	return outcome, nil
}

// executeInline claims the call for the Run's lease owner and executes it in
// this replica.
func (x *GovernedToolExecutor) executeInline(ctx context.Context, call GovernedToolCall, current *ActionCall, settled []AgentRunIntervention, retried bool) (GovernedToolOutcome, error) {
	result, err := x.worker.ExecuteOwnLoopCall(ctx, call.Run.Scope, current.ID, call.WorkerID)
	var maintenance *LoopActionMaintenanceError
	switch {
	case errors.As(err, &maintenance):
		return governedMaintenanceOutcome(call.ToolCallID, current, maintenance.RetryAt), nil
	case errors.Is(err, ErrLoopActionNotClaimable):
		if retried {
			at := x.now().UTC().Add(governedNotClaimableRetry)
			return GovernedToolOutcome{State: ToolEntryRunning, ActionCallID: current.ID, RetryAt: &at}, nil
		}
		reread, readErr := x.store.GetActionCall(ctx, call.Run.Scope, current.ID)
		if readErr != nil {
			return GovernedToolOutcome{}, readErr
		}
		return x.settle(ctx, call, reread, settled, true)
	case err != nil:
		return GovernedToolOutcome{}, err
	}
	if result == nil || result.Call == nil {
		return GovernedToolOutcome{}, errors.New("inline execution returned no action call")
	}
	call.chain.adopt(result.Run)
	if !isTerminalGovernedStatus(result.Call.Status) {
		return GovernedToolOutcome{}, fmt.Errorf("inline execution left action call %s %s", result.Call.ID, result.Call.Status)
	}
	return x.settle(ctx, call, result.Call, settled, retried)
}

func runHasHumanInterventionFor(run *AgentRun, actionCallID string) bool {
	for i := range run.HumanInterventions {
		if run.HumanInterventions[i].ActionCallID == actionCallID {
			return true
		}
	}
	return false
}

func isTerminalGovernedStatus(status ActionCallStatus) bool {
	switch status {
	case ActionCallStatusSucceeded, ActionCallStatusFailed, ActionCallStatusDenied, ActionCallStatusCanceled,
		ActionCallStatusRefused, ActionCallStatusCompensating, ActionCallStatusCompensated:
		return true
	}
	return false
}

// supersedeSibling resolves the pending approval of current as superseded
// when a newer message already superseded another approval of this Run and
// current's approval was created before that message (section 03 §9.3). ok
// is false when current stays waiting.
func (x *GovernedToolExecutor) supersedeSibling(ctx context.Context, call GovernedToolCall, current *ActionCall, settled []AgentRunIntervention) (GovernedToolOutcome, bool, error) {
	approval, err := x.store.GetApproval(ctx, call.Run.Scope, current.ApprovalID)
	if err != nil {
		return GovernedToolOutcome{}, false, err
	}
	if approval.Status != ApprovalStatusPending {
		return GovernedToolOutcome{}, false, nil
	}
	superseded, err := x.store.ListApprovals(ctx, ApprovalFilter{Scope: call.Run.Scope, RunID: call.Run.ID, Status: []ApprovalStatus{ApprovalStatusSuperseded}})
	if err != nil {
		return GovernedToolOutcome{}, false, err
	}
	messageID := ""
	for _, other := range superseded {
		if other == nil || other.ID == approval.ID || other.SupersededByMessageID == "" {
			continue
		}
		at, known := supersedingMessageTime(call.Run, other)
		if known && approval.CreatedAt.Before(at) {
			messageID = other.SupersededByMessageID
			break
		}
	}
	if messageID == "" {
		return GovernedToolOutcome{}, false, nil
	}
	result, err := x.approvals.ResolveSupersededByMessage(ctx, call.Run.Scope, approval.ID, approval.Revision, messageID)
	if errors.Is(err, ErrApprovalResolved) || errors.Is(err, ErrApprovalExpired) || errors.Is(err, ErrRevisionConflict) {
		// Someone else decided it first; settle from the fresh row.
		fresh, readErr := x.store.GetActionCall(ctx, call.Run.Scope, current.ID)
		if readErr != nil {
			return GovernedToolOutcome{}, false, readErr
		}
		outcome, settleErr := x.settle(ctx, call, fresh, settled, false)
		return outcome, true, settleErr
	}
	if err != nil {
		return GovernedToolOutcome{}, false, err
	}
	call.chain.adopt(result.Run)
	state, rendered := renderGovernedResult(result.Call, result.Approval, "")
	if rendered == nil {
		return GovernedToolOutcome{}, false, nil
	}
	return GovernedToolOutcome{State: state, Result: rendered, ActionCallID: result.Call.ID}, true, nil
}

// supersedingMessageTime is when the message that superseded approval
// reached the Run: its delivered intervention, else the supersede decision.
func supersedingMessageTime(run *AgentRun, approval *ApprovalCheckpoint) (time.Time, bool) {
	for i := len(run.PendingInterventions) - 1; i >= 0; i-- {
		message := run.PendingInterventions[i].ConversationMessage
		if message != nil && message.MessageID == approval.SupersededByMessageID {
			return run.PendingInterventions[i].CreatedAt, true
		}
	}
	if approval.DecidedAt != nil {
		return *approval.DecidedAt, true
	}
	return time.Time{}, false
}

// GovernedBudgetExhaustion is the loop's budget exit for an outcome denied
// because the Run's action budget is used up; nil otherwise.
func GovernedBudgetExhaustion(outcome GovernedToolOutcome) *BudgetExhaustion {
	if !outcome.BudgetExhausted {
		return nil
	}
	return &BudgetExhaustion{Summary: governedBudgetExhaustedReason, Dimensions: []string{"actions"}, ActionCallID: outcome.ActionCallID}
}

// ActionProposalObserver projects a committed governed ActionCall into other
// durable kernel resources. Projection failures never roll back or redispatch
// the authoritative action; observers must be idempotent and repairable from
// the persisted proposal.
type ActionProposalObserver interface {
	ObserveActionProposal(context.Context, *AgentRun, *AgentTurn, *ActionProposalResult) error
}
