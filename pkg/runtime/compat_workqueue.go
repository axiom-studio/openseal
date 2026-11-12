// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/workqueue. Remove once in-flight branches have landed.
//
// Re-exports of declarations moved to pkg/runtime/workqueue, kept so that
// importers of pkg/runtime continue to compile.

package runtime

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

type WorkOutboxStore = workqueue.WorkOutboxStore

type WorkQueue = workqueue.WorkQueue

type WorkScopeChecker = workqueue.WorkScopeChecker

type MemoryWorkQueue = workqueue.MemoryWorkQueue

// NewMemoryWorkQueue forwards to workqueue.NewMemoryWorkQueue.
func NewMemoryWorkQueue(opts ...models.ServiceOption) *workqueue.MemoryWorkQueue {
	return workqueue.NewMemoryWorkQueue(opts...)
}

type WorkSignalKind = workqueue.WorkSignalKind

const WorkSignalChanged = workqueue.WorkSignalChanged

const WorkSignalConnected = workqueue.WorkSignalConnected

const WorkSignalDisconnected = workqueue.WorkSignalDisconnected

type WorkSignal = workqueue.WorkSignal

type WorkSignalHealth = workqueue.WorkSignalHealth

type WorkSignalStatus = workqueue.WorkSignalStatus

// NewWorkSignalStatus forwards to workqueue.NewWorkSignalStatus.
func NewWorkSignalStatus(opts ...models.ServiceOption) *workqueue.WorkSignalStatus {
	return workqueue.NewWorkSignalStatus(opts...)
}

type WorkerLimiter = workqueue.WorkerLimiter

type WorkerLimiterStats = workqueue.WorkerLimiterStats

// NewWorkerLimiter forwards to workqueue.NewWorkerLimiter.
func NewWorkerLimiter(limit int) (*workqueue.WorkerLimiter, error) {
	return workqueue.NewWorkerLimiter(limit)
}

type WorkerScopeSource = workqueue.WorkerScopeSource

type WorkerScopeSourceFunc = workqueue.WorkerScopeSourceFunc
