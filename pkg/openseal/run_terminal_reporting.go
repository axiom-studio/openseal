package openseal

import (
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
)

type (
	RunTerminalReportingWorker        = externalconversation.RunTerminalReportingWorker
	RunTerminalReportingWorkerOptions = externalconversation.RunTerminalReportingWorkerOptions
	RunTerminalReportingWorkerStats   = externalconversation.RunTerminalReportingWorkerStats
)

// NewRunTerminalReportingWorker retries persisted completion reports using the
// same admission boundary as other Engine workers.
func (e *Engine) NewRunTerminalReportingWorker(options RunTerminalReportingWorkerOptions) (*RunTerminalReportingWorker, error) {
	if e == nil {
		return nil, errors.New("OpenSeal engine is required")
	}
	store, ok := e.store.(interface {
		models.RunTerminalReportingStore
		models.ConversationStore
	})
	if !ok {
		return nil, errors.New("persistent store does not support durable completion reporting")
	}
	worker, err := externalconversation.NewRunTerminalReportingWorker(store, options)
	if err != nil {
		return nil, err
	}
	worker.SetWorkerLimiter(e.workerLimiter)
	// Routine reports reach the external conversation (Slack, for example)
	// the routine was created from when the store has external conversations.
	if _, external := e.store.(models.ExternalConversationStore); external && e.skills != nil {
		if err := worker.SetExternalConversationDelivery(e.skills); err != nil {
			return nil, err
		}
	}
	return worker, nil
}
