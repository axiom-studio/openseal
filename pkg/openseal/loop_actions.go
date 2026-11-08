package openseal

import (
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type (
	GovernedToolExecutor       = runtime.GovernedToolExecutor
	GovernedToolExecutorConfig = runtime.GovernedToolExecutorConfig
	GovernedToolCall           = runtime.GovernedToolCall
	GovernedToolOutcome        = runtime.GovernedToolOutcome
)

// buildGovernedToolExecutor builds the inline executor from the first
// action-worker spec: supervisor specs (sentinel) first, else pool specs
// (daemon). Called at the end of rebuildActionWorkerPools. With no spec the
// engine has no governed tools and GovernedTools returns nil.
func (e *Engine) buildGovernedToolExecutor() error {
	e.governedWorker = nil
	e.governedTools = nil
	var worker *runtime.ActionWorker
	switch {
	case len(e.actionSupervisorSpecs) > 0:
		spec := e.actionSupervisorSpecs[0]
		worker = newInlineActionWorker(e, spec.credentials, spec.leaseIssuer, spec.dispatcher)
	case len(e.actionPoolSpecs) > 0:
		spec := e.actionPoolSpecs[0]
		worker = newInlineActionWorker(e, spec.credentials, spec.leaseIssuer, spec.dispatcher)
	default:
		return nil
	}
	worker.SetCredentialAvailabilityChecker(e.credentialAvailability)
	executor, err := runtime.NewGovernedToolExecutor(runtime.GovernedToolExecutorConfig{
		Actions: e.actions, Approvals: e.approvals, Worker: worker, Store: e.store,
		Logger: e.logger,
	})
	if err != nil {
		return err
	}
	e.governedWorker = worker
	e.governedTools = executor
	return nil
}

// newInlineActionWorker is the action worker the claiming replica runs loop
// calls with; it holds the same credentials and dispatcher as the pools.
func newInlineActionWorker(e *Engine, credentials runtime.CredentialResolver, issuer runtime.ActionCredentialLeaseIssuer, dispatcher runtime.ActionDispatcher) *runtime.ActionWorker {
	if issuer != nil {
		return runtime.NewActionWorkerWithCredentialLeaseIssuer(e.store, e.skills, issuer, dispatcher)
	}
	return runtime.NewActionWorker(e.store, e.skills, credentials, dispatcher)
}

// GovernedTools returns the executor the RunLoop's tool executor uses, or nil
// when the engine has no action workers (governed calls are then refused as
// capability_unavailable).
func (e *Engine) GovernedTools() *runtime.GovernedToolExecutor {
	if e == nil {
		return nil
	}
	return e.governedTools
}
