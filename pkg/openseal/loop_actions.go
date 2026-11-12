package openseal

import (
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

type (
	GovernedToolExecutor       = runloop.GovernedToolExecutor
	GovernedToolExecutorConfig = runloop.GovernedToolExecutorConfig
	GovernedToolCall           = runloop.GovernedToolCall
	GovernedToolOutcome        = runloop.GovernedToolOutcome
)

// buildGovernedToolExecutor builds the inline executor from the first
// action-worker spec: supervisor specs (sentinel) first, else pool specs
// (daemon). Called at the end of rebuildActionWorkerPools. With no spec the
// engine has no governed tools and GovernedTools returns nil.
func (e *Engine) buildGovernedToolExecutor() error {
	e.governedWorker = nil
	e.governedTools = nil
	var worker *action.Worker
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
	executor, err := runloop.NewGovernedToolExecutor(runloop.GovernedToolExecutorConfig{
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
func newInlineActionWorker(e *Engine, credentials action.CredentialResolver, issuer action.CredentialLeaseIssuer, dispatcher agentrun.ActionDispatcher) *action.Worker {
	if issuer != nil {
		return action.NewWorkerWithCredentialLeaseIssuer(e.store, e.skills, issuer, dispatcher)
	}
	return action.NewWorker(e.store, e.skills, credentials, dispatcher)
}

// GovernedTools returns the executor the RunLoop's tool executor uses, or nil
// when the engine has no action workers (governed calls are then refused as
// capability_unavailable).
func (e *Engine) GovernedTools() *runloop.GovernedToolExecutor {
	if e == nil {
		return nil
	}
	return e.governedTools
}
