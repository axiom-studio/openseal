package openseal

import (
	"context"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type (
	ActionOrigin        = runtime.ActionOrigin
	NestedActionRequest = runtime.NestedActionRequest
	NestedActionResult  = runtime.NestedActionResult
)

const (
	ActionOriginVM                 = runtime.ActionOriginVM
	MaxNestedActionCallsPerTurn    = runtime.MaxNestedActionCallsPerTurn
	MaxConcurrentNestedActionCalls = runtime.MaxConcurrentNestedActionCalls
	// NestedActionWait bounds how long RunNestedAction waits for a call to
	// settle before returning its current state.
	NestedActionWait = 30 * time.Second
)

var (
	ErrNestedTurnNotActive       = runtime.ErrNestedTurnNotActive
	ErrNestedActionNotAuthorized = runtime.ErrNestedActionNotAuthorized
	ErrNestedActionLimit         = runtime.ErrNestedActionLimit
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
// in the process hosting the Turn (the one whose TurnHost is serving it);
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
