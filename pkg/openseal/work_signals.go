package openseal

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// WorkSignalHealth reports the store's work signal listener state.
type WorkSignalHealth = runtime.WorkSignalHealth

// workSignalRetry bounds how quickly a failed listener setup is retried.
const workSignalRetry = 5 * time.Second

// WorkSignalHealth reports whether the store's work signal listener is
// connected. While it is not, every worker loop polls at its configured
// interval. Stores without work signals always report unhealthy.
func (e *Engine) WorkSignalHealth() WorkSignalHealth {
	if e == nil {
		return WorkSignalHealth{}
	}
	return e.workSignals.Snapshot()
}

func (e *Engine) workSignalSource() runtime.WorkSignalSource {
	source, _ := e.store.(runtime.WorkSignalSource)
	return source
}

// applyWorkSignals hands the shared listener health to every worker loop.
// It runs before the loops start; without a signal source it is never
// healthy, so the loops keep polling exactly as configured.
func (e *Engine) applyWorkSignals() {
	status := e.workSignals
	if e.conversationRunReconciler != nil {
		e.conversationRunReconciler.SetWorkSignals(status)
	}
	if e.externalConversations.supervisor != nil {
		e.externalConversations.supervisor.SetWorkSignals(status)
	}
	for _, pool := range e.agentPools {
		pool.SetWorkSignals(status)
	}
	for _, supervisor := range e.agentSupervisors {
		supervisor.SetWorkSignals(status)
	}
	for _, pool := range e.actionPools {
		pool.SetWorkSignals(status)
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.SetWorkSignals(status)
	}
}

func (e *Engine) startWorkSignalListener(parent context.Context) {
	source := e.workSignalSource()
	if source == nil || e.workSignalCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	e.workSignalCancel, e.workSignalDone = cancel, done
	go func() {
		defer close(done)
		for {
			err := source.ListenForWork(ctx, e.handleWorkSignal)
			// Whatever ended the listener, stop relying on it.
			e.handleWorkSignal(runtime.WorkSignal{Kind: runtime.WorkSignalDisconnected})
			if ctx.Err() != nil {
				return
			}
			e.logger.Warnw("work signal listener stopped; workers poll until it reconnects", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(workSignalRetry):
			}
		}
	}()
}

func (e *Engine) stopWorkSignalListener() {
	if e.workSignalCancel == nil {
		return
	}
	e.workSignalCancel()
	<-e.workSignalDone
	e.workSignalCancel, e.workSignalDone = nil, nil
}

// handleWorkSignal records listener health and wakes the loops that claim or
// reconcile the signalled family in its scope. A (re)connect wakes every loop
// once to catch up on changes missed while disconnected.
func (e *Engine) handleWorkSignal(signal runtime.WorkSignal) {
	e.workSignals.Observe(signal)
	switch signal.Kind {
	case runtime.WorkSignalConnected:
		e.wakeAllWork()
		return
	case runtime.WorkSignalDisconnected:
		// Loops observe the health change and resume polling on their own.
		return
	}
	scope := signal.Scope
	switch signal.Family {
	case runtime.WorkFamilyAction:
		e.wakeActionWorkersForScope(scope)
	case runtime.WorkFamilyRun:
		e.wakeAgentWorkersForScope(scope)
	case runtime.WorkFamilyMaintenance:
		e.wakeActionWorkersForScope(scope)
		e.wakeAgentWorkersForScope(scope)
	}
	switch signal.Family {
	case runtime.WorkFamilyRun, runtime.WorkFamilyConversation, runtime.WorkFamilyApproval, runtime.WorkFamilyExternal:
		if e.conversationRunReconciler != nil {
			e.conversationRunReconciler.WakeScope(scope)
		}
	}
	if signal.Family != runtime.WorkFamilyMaintenance && e.externalConversations.supervisor != nil {
		e.externalConversations.supervisor.WakeScope(scope)
	}
}

// wakeAllWork probes every loop once, for example after the listener
// reconnects and may have missed commits.
func (e *Engine) wakeAllWork() {
	for _, supervisor := range e.agentSupervisors {
		supervisor.RefreshScopes()
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.RefreshScopes()
	}
	e.WakeAgentWorkers()
	e.WakeActionWorkers()
	if e.conversationRunReconciler != nil {
		e.conversationRunReconciler.Wake()
	}
	if e.externalConversations.supervisor != nil {
		e.externalConversations.supervisor.Wake()
	}
}

// wakeExternalConversationsForReceived reconciles only the scopes that just
// received ingress instead of every listed scope.
func (e *Engine) wakeExternalConversationsForReceived(received []*runtime.ReceiveExternalConversationEventResult) {
	supervisor := e.externalConversations.supervisor
	if supervisor == nil {
		return
	}
	scoped := false
	for _, result := range received {
		if result != nil && result.Item != nil {
			supervisor.WakeScope(result.Item.Scope)
			scoped = true
		}
	}
	if !scoped {
		supervisor.Wake()
	}
}
