package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ActionOrigin records who requested an ActionCall when it was not the model
// at the end of its Turn.
type ActionOrigin string

// ActionOriginVM marks a nested call made during a Turn by code running in the
// chat's VM (for example `seal call` inside workspace.run_command).
const ActionOriginVM ActionOrigin = "vm"

const (
	// MaxNestedActionCallsPerTurn bounds the nested calls one Turn may make.
	MaxNestedActionCallsPerTurn = 500
	// MaxConcurrentNestedActionCalls bounds the nested calls of one Turn that
	// may be unfinished at once (ready, waiting for approval, or running).
	MaxConcurrentNestedActionCalls = 4
)

var (
	// ErrNestedTurnNotActive reports that the Turn is not the Run's current
	// in-flight Turn hosted by this process.
	ErrNestedTurnNotActive = errors.New("turn is not the run's current in-flight turn")
	// ErrNestedActionNotAuthorized reports a capability the Turn did not offer.
	ErrNestedActionNotAuthorized = errors.New("capability is not authorized for this turn")
	// ErrNestedActionLimit reports that a per-Turn nested call bound is reached.
	ErrNestedActionLimit = errors.New("nested action call limit reached for this turn")
)

// NestedActionRequest asks the kernel to run one action during an in-flight
// Turn on behalf of code in the chat's VM. Capability is the model action name
// exactly as the Turn offered it (a Skill action or mcp.<binding>.<tool>), and
// Arguments are the model-facing input for it.
type NestedActionRequest struct {
	Scope      Scope                  `json:"scope"`
	RunID      string                 `json:"runId"`
	TurnID     string                 `json:"turnId"`
	Capability string                 `json:"capability"`
	Arguments  map[string]interface{} `json:"arguments,omitempty"`
	Origin     ActionOrigin           `json:"origin"`
	// OriginRef groups calls in activity, such as the command they ran under.
	OriginRef string `json:"originRef,omitempty"`
	// IdempotencyKey identifies this request within the Turn. Repeating a
	// request with the same key returns the same ActionCall.
	IdempotencyKey string `json:"idempotencyKey"`
	Summary        string `json:"summary,omitempty"`
}

// NestedActionResult is the current state of one nested call. Approval is set
// while (and after) the call needed a person's decision.
type NestedActionResult struct {
	Call     *ActionCall         `json:"call"`
	Approval *ApprovalCheckpoint `json:"approval,omitempty"`
	Created  bool                `json:"created"`
}

// Settled reports whether the call needs no more waiting by its caller: it
// finished, or it waits for a person's approval.
func (r *NestedActionResult) Settled() bool {
	if r == nil || r.Call == nil {
		return false
	}
	switch r.Call.Status {
	case ActionCallStatusReady, ActionCallStatusRunning:
		return false
	}
	return true
}

// NestedActionFence is checked by the store in the transaction that creates a
// nested ActionCall: the Run is running under WorkerID's live lease, the Turn
// is its running Turn under the same lease, and the Turn's bounds hold.
type NestedActionFence struct {
	WorkerID      string
	Now           time.Time
	MaxCalls      int
	MaxConcurrent int
}

// nestedActionTurnState is what a store observed inside its transaction.
type nestedActionTurnState struct {
	RunStatus       AgentRunStatus
	RunLeaseOwner   string
	RunLeaseExpiry  *time.Time
	Turn            *AgentTurn
	TotalCalls      int
	UnfinishedCalls int
}

func (f *NestedActionFence) admit(call *ActionCall, state nestedActionTurnState) error {
	turn := state.Turn
	if state.RunStatus != AgentRunStatusRunning || state.RunLeaseOwner != f.WorkerID || state.RunLeaseExpiry == nil || !state.RunLeaseExpiry.After(f.Now) ||
		turn == nil || turn.RunID != call.RunID || turn.Status != AgentTurnStatusRunning || turn.LeaseOwner != f.WorkerID ||
		turn.LeaseExpiresAt == nil || !turn.LeaseExpiresAt.After(f.Now) {
		return ErrNestedTurnNotActive
	}
	if state.TotalCalls >= f.MaxCalls {
		return fmt.Errorf("%w: at most %d calls", ErrNestedActionLimit, f.MaxCalls)
	}
	if state.UnfinishedCalls >= f.MaxConcurrent {
		return fmt.Errorf("%w: at most %d unfinished calls at once", ErrNestedActionLimit, f.MaxConcurrent)
	}
	return nil
}

func validateNestedActionFence(proposal ActionProposalRecord) error {
	fence := proposal.Nested
	if fence == nil {
		return nil
	}
	if proposal.Run != nil || proposal.Call.Origin != ActionOriginVM || strings.TrimSpace(fence.WorkerID) == "" || fence.Now.IsZero() ||
		fence.MaxCalls < 1 || fence.MaxConcurrent < 1 {
		return errors.New("nested action proposal requires a vm call, no Run update, a worker, a time, and bounds")
	}
	return nil
}

// nestedActionUnfinished lists the statuses that count against the
// concurrency bound.
var nestedActionUnfinished = []ActionCallStatus{ActionCallStatusReady, ActionCallStatusWaitingApproval, ActionCallStatusRunning}

// nestedActionTurnLive reports whether a nested call's Turn is still the
// in-flight Turn of its running Run. Execution checks it before dispatch.
func nestedActionTurnLive(ctx context.Context, turns AgentTurnStore, run *AgentRun, call *ActionCall, now time.Time) (bool, error) {
	if run == nil || call == nil || run.Scope != call.Scope || run.ID != call.RunID || run.Status != AgentRunStatusRunning ||
		run.LeaseOwner == "" || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
		return false, nil
	}
	turn, err := turns.GetAgentTurn(ctx, call.Scope, call.TurnID)
	if err != nil || turn == nil {
		return false, err
	}
	return turn.RunID == run.ID && turn.Status == AgentTurnStatusRunning && turn.LeaseOwner == run.LeaseOwner &&
		turn.LeaseExpiresAt != nil && turn.LeaseExpiresAt.After(now), nil
}

// nestedActionAuthority is the exact action set a worker offered to an
// in-flight Turn. It lives only in the process hosting that Turn.
type nestedActionAuthority struct {
	runID    string
	workerID string
	run      *AgentRun
	binding  *TurnRunnerBinding
}

type nestedActionTurns struct {
	mu    sync.RWMutex
	turns map[string]*nestedActionAuthority
}

func nestedActionTurnKey(scope Scope, turnID string) string {
	return portfolioKey(scope, turnID)
}

// open records the Turn's authority until the returned function is called.
func (t *nestedActionTurns) open(run *AgentRun, turn *AgentTurn, binding *TurnRunnerBinding) func() {
	if t == nil || run == nil || turn == nil || binding == nil || turn.RunID != run.ID || turn.LeaseOwner == "" {
		return func() {}
	}
	key := nestedActionTurnKey(turn.Scope, turn.ID)
	authority := &nestedActionAuthority{runID: run.ID, workerID: turn.LeaseOwner, run: cloneAgentRun(run), binding: binding}
	t.mu.Lock()
	if t.turns == nil {
		t.turns = make(map[string]*nestedActionAuthority)
	}
	t.turns[key] = authority
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		if t.turns[key] == authority {
			delete(t.turns, key)
		}
		t.mu.Unlock()
	}
}

func (t *nestedActionTurns) lookup(scope Scope, turnID string) *nestedActionAuthority {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.turns[nestedActionTurnKey(scope, turnID)]
}

// OpenNestedActionTurn makes a Turn's offered actions available to nested
// calls until the returned function is called. Agent Run workers call it
// around every Turn they host.
func (c *ActionCoordinator) OpenNestedActionTurn(run *AgentRun, turn *AgentTurn, binding *TurnRunnerBinding) func() {
	if c == nil {
		return func() {}
	}
	return c.nested.open(run, turn, binding)
}

// ProposeNested authorizes and records one nested call for an in-flight Turn
// exactly as a model-requested action of that Turn would be: the same offered
// action set, binding, credential references, policy and approvals. It never
// changes the Run. An allowed call is ready for action workers; a call that
// needs review waits for its approval; a denied call is returned denied.
func (c *ActionCoordinator) ProposeNested(ctx context.Context, req NestedActionRequest) (*NestedActionResult, error) {
	if c == nil {
		return nil, errors.New("action coordinator is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.Origin != ActionOriginVM {
		return nil, fmt.Errorf("unsupported nested action origin %q", req.Origin)
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.TurnID) == "" || strings.TrimSpace(req.Capability) == "" || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, errors.New("nested action requires run, turn, capability, and idempotency key")
	}
	authority := c.nested.lookup(req.Scope, req.TurnID)
	if authority == nil || authority.runID != req.RunID {
		return nil, ErrNestedTurnNotActive
	}
	if authority.run.Kind == RunKindConversation && authority.run.Owner.Type == OwnerTypeTeam && strings.TrimSpace(authority.run.AssignedAgentID) == "" {
		return nil, errors.New("nested action calls are not available in Team conversations")
	}
	binding := authority.binding
	selected, err := selectTurnModelAction(binding.ModelActions, TurnAction{Capability: req.Capability})
	if err != nil {
		return nil, err
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: %q", ErrNestedActionNotAuthorized, req.Capability)
	}
	deploymentID := strings.TrimSpace(binding.ActionDeploymentID)
	if strings.TrimSpace(selected.DeploymentID) != "" {
		deploymentID = strings.TrimSpace(selected.DeploymentID)
	}
	if deploymentID == "" {
		deploymentID = binding.DeploymentID
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = "Called " + selected.Name + " from the chat's VM"
	}
	proposal, err := c.Propose(ctx, ProposeActionRequest{
		Scope: req.Scope, RunID: req.RunID, TurnID: req.TurnID, WorkerID: authority.workerID, DeploymentID: deploymentID,
		BindingID: selected.BindingID, BindingRevision: selected.BindingRevision,
		SkillID: selected.SkillID, SkillVersion: selected.Version, Action: selected.Action,
		Arguments:       selected.ActionInput(cloneMap(req.Arguments)),
		PreparedRuntime: preparedRuntimeForAction(binding, selected),
		IdempotencyKey:  "vm:" + req.TurnID + ":" + strings.TrimSpace(req.IdempotencyKey),
		Summary:         summary, Actor: ActivityActor{Type: "worker", ID: authority.workerID},
		Origin: ActionOriginVM, OriginRef: strings.TrimSpace(req.OriginRef), CausationID: req.TurnID,
	})
	if err != nil {
		return nil, err
	}
	if proposal == nil || proposal.Call == nil {
		return nil, errors.New("nested action proposal returned no ActionCall")
	}
	if proposal.Call.Origin != ActionOriginVM || proposal.Call.TurnID != req.TurnID {
		return nil, ErrIdempotencyConflict
	}
	return &NestedActionResult{Call: proposal.Call, Approval: proposal.Approval, Created: proposal.Created}, nil
}

// GetNested returns the current state of one nested call of a Run.
func (c *ActionCoordinator) GetNested(ctx context.Context, scope Scope, runID, actionCallID string) (*NestedActionResult, error) {
	if c == nil || c.actions == nil {
		return nil, errors.New("action coordinator is not configured")
	}
	call, err := c.actions.GetActionCall(ctx, scope, actionCallID)
	if err != nil {
		return nil, err
	}
	if call == nil || call.RunID != runID || call.Origin != ActionOriginVM {
		return nil, ErrActionNotFound
	}
	result := &NestedActionResult{Call: call}
	if call.ApprovalID != "" {
		if result.Approval, err = c.actions.GetApproval(ctx, scope, call.ApprovalID); err != nil {
			return nil, err
		}
	}
	return result, nil
}
