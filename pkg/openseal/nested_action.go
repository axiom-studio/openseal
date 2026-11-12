package openseal

import (
	"context"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
)

type (
	ActionOrigin            = models.ActionOrigin
	NestedActionRequest     = action.NestedActionRequest
	NestedActionResult      = action.NestedActionResult
	ActionExecution         = models.ActionExecution
	NestedHostActionRequest = action.NestedHostActionRequest
	NestedHostActionGrant   = action.NestedHostActionGrant
	NestedHostActionOutcome = action.NestedHostActionOutcome
)

const (
	ActionOriginVM                 = models.ActionOriginVM
	ActionOriginLoop               = models.ActionOriginLoop
	ActionExecutionHost            = models.ActionExecutionHost
	ActionExecutionLoop            = models.ActionExecutionLoop
	NestedHostActionLease          = action.NestedHostActionLease
	NestedHostActionReadyGrace     = models.NestedHostActionReadyGrace
	MaxNestedActionCallsPerTurn    = action.MaxNestedActionCallsPerTurn
	MaxConcurrentNestedActionCalls = action.MaxConcurrentNestedActionCalls
	// NestedActionWait bounds how long RunNestedAction waits for a call to
	// settle before returning its current state.
	NestedActionWait = 30 * time.Second
)

var (
	ErrNestedTurnNotActive       = models.ErrNestedTurnNotActive
	ErrNestedActionNotAuthorized = action.ErrNestedActionNotAuthorized
	ErrNestedActionLimit         = models.ErrNestedActionLimit
	ErrNestedHostActionNotReady  = action.ErrNestedHostActionNotReady
	ErrNestedHostActionClosed    = action.ErrNestedHostActionClosed
)

// RunNestedAction polls a call's state from nestedActionPollFirst, doubling
// up to nestedActionPollMax.
const (
	nestedActionPollFirst = 20 * time.Millisecond
	nestedActionPollMax   = 250 * time.Millisecond
)

// RunNestedAction runs one action during the in-flight Turn req.TurnID on
// behalf of code in the chat's VM, under the governance of a model-requested
// action of that Turn, without changing the Run's lifecycle. It must be called
// in the process running the step (the one whose StepHost is serving it);
// elsewhere it returns ErrNestedTurnNotActive.
//
// It returns once the call settles (see NestedActionResult.Settled) or after
// NestedActionWait. A call waiting for approval returns at once with its
// pending Approval. To keep waiting, call RunNestedAction again with the same
// IdempotencyKey, or poll GetNestedAction.
func (e *Engine) RunNestedAction(ctx context.Context, req NestedActionRequest) (*NestedActionResult, error) {
	if e == nil || e.actions == nil {
		return nil, errors.New("governed actions are not configured")
	}
	result, err := e.actions.ProposeNested(ctx, req)
	if err != nil {
		return nil, err
	}
	if result.Settled() {
		return result, nil
	}
	e.wakeActionWorkersForScope(req.Scope)
	timer := time.NewTimer(NestedActionWait)
	defer timer.Stop()
	interval := nestedActionPollFirst
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return result, nil
		case <-time.After(interval):
		}
		current, err := e.actions.GetNested(ctx, req.Scope, req.RunID, result.Call.ID)
		if err != nil {
			return nil, err
		}
		current.Created = result.Created
		result = current
		if result.Settled() {
			return result, nil
		}
		if interval *= 2; interval > nestedActionPollMax {
			interval = nestedActionPollMax
		}
	}
}

// GetNestedAction returns the current state of one nested call of a Run.
func (e *Engine) GetNestedAction(ctx context.Context, scope Scope, runID, actionCallID string) (*NestedActionResult, error) {
	if e == nil || e.actions == nil {
		return nil, errors.New("governed actions are not configured")
	}
	return e.actions.GetNested(ctx, scope, runID, actionCallID)
}

// ProposeNestedHost records work the host itself performs during the in-flight
// Turn req.TurnID for code in the chat's VM, such as a git fetch or push over
// HTTPS. It is governed, approved and recorded as the offered Skill action
// req.SkillID/req.Action with req.Arguments, but never dispatched to the
// Skill: the host claims it with AcquireNestedHostAction, which hands it a
// credential lease, performs the work, and ends it with
// FinishNestedHostAction. It returns at once: a call that needs approval
// waits for it (poll GetNestedAction); an allowed call is ready for the host.
// It must be called in the process hosting the Turn.
func (e *Engine) ProposeNestedHost(ctx context.Context, req NestedHostActionRequest) (*NestedActionResult, error) {
	if e == nil || e.actions == nil {
		return nil, errors.New("governed actions are not configured")
	}
	return e.actions.ProposeNestedHost(ctx, req)
}

// AcquireNestedHostAction claims (or renews the host's hold on) a ready host
// call of the in-flight Turn and issues a fresh single-use credential lease for
// it through issuer. The lease names opaque credential references only; the
// host resolves them for its own use and must never record the values. It
// returns ErrNestedHostActionNotReady while the call waits for approval and
// ErrNestedHostActionClosed once it was denied or ended.
func (e *Engine) AcquireNestedHostAction(ctx context.Context, scope Scope, runID, turnID, actionCallID string, issuer ActionCredentialLeaseIssuer) (*NestedHostActionGrant, error) {
	if e == nil || e.actions == nil {
		return nil, errors.New("governed actions are not configured")
	}
	return e.actions.AcquireNestedHostAction(ctx, scope, runID, turnID, actionCallID, issuer)
}

// FinishNestedHostAction records how a host call ended and releases it.
func (e *Engine) FinishNestedHostAction(ctx context.Context, outcome NestedHostActionOutcome) (*ActionCall, error) {
	if e == nil || e.actions == nil {
		return nil, errors.New("governed actions are not configured")
	}
	return e.actions.FinishNestedHostAction(ctx, outcome)
}
