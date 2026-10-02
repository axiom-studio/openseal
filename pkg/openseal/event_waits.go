package openseal

import (
	"errors"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type (
	RunbookEventMatch                 = runbook.EventMatch
	RunEventWaitSpec                  = runtime.RunEventWaitSpec
	RunEventWait                      = runtime.RunEventWait
	RunEventWaitWorker                = runtime.RunEventWaitWorker
	RunEventWaitWorkerOptions         = runtime.RunEventWaitWorkerOptions
	RunEventWaitWorkerStats           = runtime.RunEventWaitWorkerStats
	RunTerminalReportingWorker        = runtime.RunTerminalReportingWorker
	RunTerminalReportingWorkerOptions = runtime.RunTerminalReportingWorkerOptions
	RunTerminalReportingWorkerStats   = runtime.RunTerminalReportingWorkerStats
)

// NewRunEventWaitWorker shares the Engine's process-wide worker admission.
// The host owns worker lifecycle; the same persistent Store coordinates replicas.
func (e *Engine) NewRunEventWaitWorker(options RunEventWaitWorkerOptions) (*RunEventWaitWorker, error) {
	if e == nil {
		return nil, errors.New("OpenSeal engine is required")
	}
	store, ok := e.store.(runtime.RunEventWaitStore)
	if !ok {
		return nil, errors.New("persistent store does not support durable event waits")
	}
	worker, err := runtime.NewRunEventWaitWorker(store, options)
	if err != nil {
		return nil, err
	}
	worker.SetWorkerLimiter(e.workerLimiter)
	return worker, nil
}

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
	return worker, nil
}
