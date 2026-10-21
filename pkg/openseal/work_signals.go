package openseal

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// WorkSignalHealth reports the work queue consumer's state.
type WorkSignalHealth = runtime.WorkSignalHealth

// WorkItem asks for one pass over a family's durable work in a scope.
type WorkItem = runtime.WorkItem

// WorkQueue is the shared schedule of scope passes. See runtime.WorkQueue.
type WorkQueue = runtime.WorkQueue

// WorkSignal is one delivery or lifecycle event from a WorkQueue.
type WorkSignal = runtime.WorkSignal

// WorkFamily names the worker loops a WorkItem wakes.
type WorkFamily = runtime.WorkFamily

// WorkFamilyAuthoring is delivered for workforce change sets and authoring
// Runs. Hosts that run authoring workers subscribe with HandleWork.
const WorkFamilyAuthoring = runtime.WorkFamilyAuthoring

const (
	WorkSignalChanged      = runtime.WorkSignalChanged
	WorkSignalConnected    = runtime.WorkSignalConnected
	WorkSignalDisconnected = runtime.WorkSignalDisconnected
)

const (
	// workQueueRetry bounds how quickly a failed consumer is restarted.
	workQueueRetry = 5 * time.Second
	// workOutboxRelayBatch bounds one relay transaction.
	workOutboxRelayBatch = 500
	// workOutboxRelaySpacing coalesces relays under a steady commit rate:
	// at most one outbox read per spacing per process.
	workOutboxRelaySpacing = 20 * time.Millisecond
	// workOutboxOrphanInterval relays rows a crashed process committed but
	// never relayed. Any later commit in any process relays them sooner.
	workOutboxOrphanInterval = 5 * time.Minute
	// workBackstopInterval republishes pending durable work, in case an item
	// was lost between delivery and its pass. It reads only small partial
	// indexes of active rows.
	workBackstopInterval = 15 * time.Minute
)

// WithWorkQueue schedules worker passes through queue, shared by every
// replica, for example Valkey streams. Without it a PostgreSQL store uses a
// process-local queue: each replica then handles the work its own commits
// create, plus the pending work sweeps. Other stores keep polling.
func WithWorkQueue(queue runtime.WorkQueue) Option {
	return func(e *Engine) error {
		e.workQueue = queue
		return nil
	}
}

// HandleWork calls handle with the scope of every delivered item of family,
// for workers the host owns, such as workforce authoring. Handlers must not
// block; a delivery is a hint to look at durable state in that scope.
func (e *Engine) HandleWork(family runtime.WorkFamily, handle func(runtime.Scope)) {
	if e == nil || handle == nil {
		return
	}
	e.workHandlersMu.Lock()
	defer e.workHandlersMu.Unlock()
	if e.workHandlers == nil {
		e.workHandlers = make(map[runtime.WorkFamily][]func(runtime.Scope))
	}
	e.workHandlers[family] = append(e.workHandlers[family], handle)
}

// WorkSignalHealth reports whether the work queue is consuming. While it is
// not, every worker loop polls at its configured interval.
func (e *Engine) WorkSignalHealth() WorkSignalHealth {
	if e == nil {
		return WorkSignalHealth{}
	}
	return e.workSignals.Snapshot()
}

func (e *Engine) workOutbox() runtime.WorkOutboxStore {
	outbox, _ := e.store.(runtime.WorkOutboxStore)
	return outbox
}

// applyWorkSignals hands the shared queue status to every worker loop and
// binds the queue. It runs before the loops start; without an outbox store
// no queue is bound, so the loops keep polling exactly as configured.
func (e *Engine) applyWorkSignals() {
	status := e.workSignals
	if e.workOutbox() != nil && e.workPublish == nil {
		if e.workQueue == nil {
			e.workQueue = runtime.NewMemoryWorkQueue()
		}
		e.workPublish = status.BindQueue(e.workQueue, e.logger)
	}
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

// startWorkQueue runs the queue's four process loops: the scheduled pass
// publisher, the outbox relay, the consumer and the pending work backstop.
func (e *Engine) startWorkQueue(parent context.Context) {
	outbox := e.workOutbox()
	if outbox == nil || e.workPublish == nil || e.workQueueCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	e.workQueueCancel = cancel
	e.workRelayRequests = make(chan struct{}, 1)
	e.workRebuilds = make(chan struct{}, 1)
	loops := []func(context.Context){
		e.workPublish,
		func(ctx context.Context) { e.relayWorkOutbox(ctx, outbox) },
		e.consumeWork,
		func(ctx context.Context) { e.backstopWork(ctx, outbox) },
	}
	e.workQueueDone = make(chan struct{}, len(loops))
	for _, loop := range loops {
		go func() {
			defer func() { e.workQueueDone <- struct{}{} }()
			loop(ctx)
		}()
	}
	e.requestWorkRelay()
}

func (e *Engine) stopWorkQueue() {
	if e.workQueueCancel == nil {
		return
	}
	e.workQueueCancel()
	for range cap(e.workQueueDone) {
		<-e.workQueueDone
	}
	e.workQueueCancel, e.workQueueDone = nil, nil
}

func (e *Engine) requestWorkRelay() {
	select {
	case e.workRelayRequests <- struct{}{}:
	default:
	}
}

// relayWorkOutbox moves committed outbox rows into the queue right after
// this process commits, and rarely otherwise.
func (e *Engine) relayWorkOutbox(ctx context.Context, outbox runtime.WorkOutboxStore) {
	orphans := time.NewTicker(workOutboxOrphanInterval)
	defer orphans.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-outbox.WorkCommitted():
		case <-e.workRelayRequests:
		case <-orphans.C:
		}
		for {
			relayed, err := outbox.RelayWorkOutbox(ctx, workOutboxRelayBatch, e.workQueue.PublishWork)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				e.logger.Warnw("work outbox relay failed", "error", err)
				// Rows stay in the outbox; retry after a bounded backoff.
				select {
				case <-ctx.Done():
					return
				case <-time.After(min(time.Duration(failures)*time.Second, 30*time.Second)):
				}
				e.requestWorkRelay()
				break
			}
			failures = 0
			if relayed < workOutboxRelayBatch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(workOutboxRelaySpacing):
		}
	}
}

// consumeWork delivers due queue items to the worker loops, restarting the
// consumer whenever it ends.
func (e *Engine) consumeWork(ctx context.Context) {
	for {
		err := e.workQueue.ConsumeWork(ctx, e.handleWorkSignal)
		// Whatever ended the consumer, stop relying on it.
		e.handleWorkSignal(runtime.WorkSignal{Kind: runtime.WorkSignalDisconnected})
		if ctx.Err() != nil {
			return
		}
		e.logger.Warnw("work queue consumer stopped; workers poll until it reconnects", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(workQueueRetry):
		}
	}
}

// backstopWork republishes pending durable work periodically, and in full
// whenever the queue reports that it lost its schedule.
func (e *Engine) backstopWork(ctx context.Context, outbox runtime.WorkOutboxStore) {
	ticker := time.NewTicker(workBackstopInterval)
	defer ticker.Stop()
	for {
		rebuild := false
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-e.workRebuilds:
			rebuild = true
		}
		items, err := outbox.ListPendingWork(ctx, rebuild)
		if err == nil && len(items) > 0 {
			err = e.workQueue.PublishWork(ctx, items)
		}
		if err != nil && ctx.Err() == nil {
			e.logger.Warnw("pending work sweep failed", "rebuild", rebuild, "error", err)
			if rebuild {
				e.requestWorkRebuild()
			}
		}
	}
}

func (e *Engine) requestWorkRebuild() {
	select {
	case e.workRebuilds <- struct{}{}:
	default:
	}
}

// handleWorkSignal records queue health and wakes the loops that claim or
// reconcile the delivered family in its scope.
func (e *Engine) handleWorkSignal(signal runtime.WorkSignal) {
	e.workSignals.Observe(signal)
	switch signal.Kind {
	case runtime.WorkSignalConnected:
		if signal.Rebuild {
			e.requestWorkRebuild()
		}
		e.requestWorkRelay()
		return
	case runtime.WorkSignalDisconnected:
		// Loops observe the health change and resume polling on their own.
		return
	}
	scope := signal.Scope
	e.workHandlersMu.RLock()
	handlers := e.workHandlers[signal.Family]
	e.workHandlersMu.RUnlock()
	for _, handle := range handlers {
		handle(scope)
	}
	switch signal.Family {
	case runtime.WorkFamilyAction:
		e.wakeActionWorkersForScope(scope)
	case runtime.WorkFamilyRun, runtime.WorkFamilyRunDue:
		e.wakeAgentWorkersForScope(scope)
	case runtime.WorkFamilyMaintenance:
		e.wakeActionWorkersForScope(scope)
		e.wakeAgentWorkersForScope(scope)
	}
	switch signal.Family {
	case runtime.WorkFamilyRun, runtime.WorkFamilyConversation, runtime.WorkFamilyApproval, runtime.WorkFamilyExternal, runtime.WorkFamilyConversationDue:
		if e.conversationRunReconciler != nil {
			e.conversationRunReconciler.WakeScope(scope)
		}
	}
	switch signal.Family {
	case runtime.WorkFamilyRun, runtime.WorkFamilyConversation, runtime.WorkFamilyApproval, runtime.WorkFamilyExternal,
		runtime.WorkFamilyActivity, runtime.WorkFamilyExternalDue:
		if e.externalConversations.supervisor != nil {
			e.externalConversations.supervisor.WakeScope(scope)
		}
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
