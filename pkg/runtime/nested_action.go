package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	// MaxNestedActionCallsPerTurn bounds the nested calls one step may make.
	MaxNestedActionCallsPerTurn = 500
	// MaxConcurrentNestedActionCalls bounds the nested calls of one step that
	// may be unfinished at once (ready, waiting for approval, or running).
	MaxConcurrentNestedActionCalls = 4
)

var (
	// ErrNestedActionNotAuthorized reports a capability the step did not offer.
	ErrNestedActionNotAuthorized = errors.New("capability is not authorized for this step")
)

// NestedActionRequest asks the kernel to run one action during a running step
// on behalf of code in the chat's VM. TurnID is the step id. Capability is the
// model action name exactly as the step offered it (a Skill action or
// mcp.<binding>.<tool>), and Arguments are the model-facing input for it.
type NestedActionRequest struct {
	Scope      Scope                  `json:"scope"`
	RunID      string                 `json:"runId"`
	TurnID     string                 `json:"turnId"`
	Capability string                 `json:"capability"`
	Arguments  map[string]interface{} `json:"arguments,omitempty"`
	Origin     ActionOrigin           `json:"origin"`
	// OriginRef groups calls in activity, such as the command they ran under.
	OriginRef string `json:"originRef,omitempty"`
	// IdempotencyKey identifies this request within the step. Repeating a
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
	if r.Call.Execution == ActionExecutionHost {
		// No action worker moves a host call on: ready or running, it waits
		// for its host.
		return true
	}
	switch r.Call.Status {
	case ActionCallStatusReady, ActionCallStatusRunning:
		return false
	}
	return true
}

// nestedActionTurnLive reports whether a nested call's step is still a
// running step of its running Run, leased by the Run's lease owner. Execution
// checks it before dispatch; a call whose step has ended is canceled.
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

// nestedActionAuthority is the exact action set a worker offered to a running
// step. It lives only in the process hosting that step.
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

// open records the step's authority until the returned function is called.
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

// OpenNestedActionTurn makes a step's offered actions available to nested
// calls until the returned function is called. The run loop calls it when a
// step begins and closes it when the step ends, so nested calls and their
// bounds are scoped to one step.
func (c *ActionCoordinator) OpenNestedActionTurn(run *AgentRun, turn *AgentTurn, binding *TurnRunnerBinding) func() {
	if c == nil {
		return func() {}
	}
	return c.nested.open(run, turn, binding)
}

// ProposeNested authorizes and records one nested call for a running step
// exactly as a model-requested action of that step would be: the same offered
// action set, binding, credential references, policy and approvals. It never
// changes the Run. An allowed call is ready for action workers; a call that
// needs review waits for its approval; a denied call is returned denied.
func (c *ActionCoordinator) ProposeNested(ctx context.Context, req NestedActionRequest) (*NestedActionResult, error) {
	return c.proposeNested(ctx, req, "", nil)
}

func (c *ActionCoordinator) proposeNested(ctx context.Context, req NestedActionRequest, execution ActionExecution, choose func(*TurnRunnerBinding) (*capability.ModelAction, error)) (*NestedActionResult, error) {
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
	if authority.run.Kind == RunKindConversation && strings.TrimSpace(authority.run.AssignedAgentID) == "" {
		return nil, errors.New("nested action calls require the Run's acting Agent")
	}
	binding := authority.binding
	var selected *capability.ModelAction
	var err error
	if choose != nil {
		selected, err = choose(binding)
	} else {
		selected, err = selectTurnModelAction(binding.ModelActions, TurnAction{Capability: req.Capability})
	}
	if err != nil {
		return nil, err
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: %q", ErrNestedActionNotAuthorized, req.Capability)
	}
	deploymentID := actionDeploymentForSelection(binding, selected)
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = "Called " + selected.Name + " from the chat's VM"
	}
	idempotencyPrefix := "vm:"
	if execution == ActionExecutionHost {
		idempotencyPrefix = "vm-host:"
	}
	proposal, err := c.Propose(ctx, ProposeActionRequest{
		Scope: req.Scope, RunID: req.RunID, TurnID: req.TurnID, WorkerID: authority.workerID, DeploymentID: deploymentID,
		BindingID: selected.BindingID, BindingRevision: selected.BindingRevision,
		SkillID: selected.SkillID, SkillVersion: selected.Version, Action: selected.Action,
		Arguments:       selected.ActionInput(maps.Clone(req.Arguments)),
		PreparedRuntime: preparedRuntimeForAction(binding, selected),
		IdempotencyKey:  idempotencyPrefix + req.TurnID + ":" + strings.TrimSpace(req.IdempotencyKey),
		Summary:         summary, Actor: ActivityActor{Type: "worker", ID: authority.workerID},
		Origin: ActionOriginVM, OriginRef: strings.TrimSpace(req.OriginRef), CausationID: req.TurnID, Execution: execution,
	})
	if errors.Is(err, ErrLeaseLost) {
		// The worker that hosts this step no longer holds the Run's lease, so
		// the step is no longer the Run's running step.
		return nil, fmt.Errorf("%w: %w", ErrNestedTurnNotActive, err)
	}
	if err != nil {
		return nil, err
	}
	if proposal == nil || proposal.Call == nil {
		return nil, errors.New("nested action proposal returned no ActionCall")
	}
	if proposal.Call.Origin != ActionOriginVM || proposal.Call.TurnID != req.TurnID || proposal.Call.Execution != execution {
		return nil, ErrIdempotencyConflict
	}
	return &NestedActionResult{Call: proposal.Call, Approval: proposal.Approval, Created: proposal.Created}, nil
}

// actionDeploymentForSelection is the deployment whose bindings authorize the
// selected action: the action's own owner, else the binding's action
// deployment (a Team member Run resolves Team actions against the Team), else
// the Run's deployment.
func actionDeploymentForSelection(binding *TurnRunnerBinding, selected *capability.ModelAction) string {
	if selected != nil {
		if deploymentID := strings.TrimSpace(selected.DeploymentID); deploymentID != "" {
			return deploymentID
		}
	}
	if binding == nil {
		return ""
	}
	if deploymentID := strings.TrimSpace(binding.ActionDeploymentID); deploymentID != "" {
		return deploymentID
	}
	return binding.DeploymentID
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

// preparedRuntimeForAction is the activation-time runtime prepared for the
// selected action's exact binding, if any.
func preparedRuntimeForAction(binding *TurnRunnerBinding, selected *capability.ModelAction) *skill.PreparedRuntime {
	for runtimeIndex := range binding.PreparedRuntimes {
		prepared := &binding.PreparedRuntimes[runtimeIndex]
		if prepared.DeploymentID == selected.DeploymentID && prepared.BindingID == selected.BindingID && prepared.BindingRevision == selected.BindingRevision &&
			prepared.SkillID == selected.SkillID && prepared.SkillVersion == selected.Version {
			copy := prepared.Runtime
			copy.Executables = append([]string(nil), prepared.Runtime.Executables...)
			return &copy
		}
	}
	return nil
}

// selectTurnModelAction resolves a proposal exactly by name and, when the
// proposal carries one, binding. A name exposed twice is refused even when the
// proposal names a binding: a catalog with duplicate names is itself invalid.
func selectTurnModelAction(actions []capability.ModelAction, request TurnAction) (*capability.ModelAction, error) {
	var selected *capability.ModelAction
	matches := 0
	for index := range actions {
		candidate := &actions[index]
		if candidate.Name != request.Capability {
			continue
		}
		if matches++; matches > 1 {
			return nil, fmt.Errorf("requested capability %q is ambiguous: more than one binding exposes it", request.Capability)
		}
		if request.BindingID != "" && (candidate.BindingID != request.BindingID || candidate.BindingRevision != request.BindingRevision) {
			continue
		}
		selected = candidate
	}
	if request.ExpectedSkillVersion != "" && (selected == nil || selected.Version != request.ExpectedSkillVersion) {
		return nil, fmt.Errorf("%w: requested capability %q version %q is not authorized by the current binding", ErrRunSkillDependencyUnavailable, request.Capability, request.ExpectedSkillVersion)
	}
	return selected, nil
}

func portfolioKey(scope models.Scope, id string) string { return scope.Key() + ":" + id }
