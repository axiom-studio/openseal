package openseal

import (
	"errors"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type (
	RunTerminalReportingWorker        = runtime.RunTerminalReportingWorker
	RunTerminalReportingWorkerOptions = runtime.RunTerminalReportingWorkerOptions
	RunTerminalReportingWorkerStats   = runtime.RunTerminalReportingWorkerStats
)

// NewRunTerminalReportingWorker retries persisted completion reports using the
// same admission boundary as other Engine workers.
func (e *Engine) NewRunTerminalReportingWorker(options RunTerminalReportingWorkerOptions) (*RunTerminalReportingWorker, error) {
	if e == nil {
		return nil, errors.New("OpenSeal engine is required")
	}
	store, ok := e.store.(interface {
		runtime.RunTerminalReportingStore
		runtime.ConversationStore
	})
	if !ok {
		return nil, errors.New("persistent store does not support durable completion reporting")
	}
	worker, err := runtime.NewRunTerminalReportingWorker(store, options)
	if err != nil {
		return nil, err
	}
	worker.SetWorkerLimiter(e.workerLimiter)
	// Routine reports reach the external conversation (Slack, for example)
	// the routine was created from when the store has external conversations.
	if _, external := e.store.(runtime.ExternalConversationStore); external && e.skills != nil {
		if err := worker.SetExternalConversationDelivery(e.skills); err != nil {
			return nil, err
		}
	}
	return worker, nil
}
