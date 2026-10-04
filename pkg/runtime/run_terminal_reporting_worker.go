package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type RunTerminalReportingWorkerOptions struct {
	WorkerID      string
	LeaseDuration time.Duration
	BatchSize     int
	Concurrency   int
	ScopeAllowed  func(context.Context, Scope) (bool, error)
}

type RunTerminalReportingWorkerStats struct {
	Claimed   uint64
	Delivered uint64
	Retried   uint64
	Errors    uint64
}

type RunTerminalReportingWorker struct {
	store     RunTerminalReportingStore
	project   func(context.Context, *AgentRun) error
	options   RunTerminalReportingWorkerOptions
	limiter   *WorkerLimiter
	now       func() time.Time
	claimed   atomic.Uint64
	delivered atomic.Uint64
	retried   atomic.Uint64
	errors    atomic.Uint64
}

func NewRunTerminalReportingWorker(store interface {
	RunTerminalReportingStore
	ConversationStore
}, options RunTerminalReportingWorkerOptions) (*RunTerminalReportingWorker, error) {
	if store == nil || !validOpaqueIdentifier(options.WorkerID, 180) || options.LeaseDuration < 3*time.Second || options.LeaseDuration > 5*time.Minute || options.BatchSize < 1 || options.BatchSize > 100 || options.Concurrency < 1 || options.Concurrency > 32 || options.Concurrency > options.BatchSize {
		return nil, ErrInvalidRunTerminalReport
	}
	return &RunTerminalReportingWorker{
		store: store, options: options, now: time.Now,
		project: func(ctx context.Context, run *AgentRun) error { return projectTerminalRunReporting(ctx, store, run) },
	}, nil
}

func (w *RunTerminalReportingWorker) SetWorkerLimiter(limiter *WorkerLimiter) { w.limiter = limiter }

func (w *RunTerminalReportingWorker) ProcessBatch(ctx context.Context) (int, error) {
	return w.process(ctx, nil)
}

func (w *RunTerminalReportingWorker) ProcessScope(ctx context.Context, scope Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	return w.process(ctx, &scope)
}

func (w *RunTerminalReportingWorker) process(ctx context.Context, scope *Scope) (int, error) {
	if w == nil || w.store == nil || w.project == nil {
		return 0, ErrInvalidRunTerminalReport
	}
	ctx, cancel := context.WithTimeout(ctx, w.options.LeaseDuration*2/3)
	defer cancel()
	release, err := w.limiter.acquire(ctx)
	if err != nil {
		return 0, err
	}
	workerID := w.options.WorkerID + ":" + uuid.NewString()
	reports, err := w.store.ClaimRunTerminalReports(ctx, RunTerminalReportingClaim{
		Scope: scope, WorkerID: workerID, Now: w.now().UTC(), LeaseDuration: w.options.LeaseDuration, Limit: w.options.BatchSize,
	})
	release()
	if err != nil {
		w.errors.Add(1)
		return 0, err
	}
	w.claimed.Add(uint64(len(reports)))
	if len(reports) == 0 {
		return 0, nil
	}
	jobs := make(chan *RunTerminalReport)
	var group sync.WaitGroup
	var completed atomic.Int64
	var failureMu sync.Mutex
	var failures []error
	addFailure := func(err error) {
		w.errors.Add(1)
		failureMu.Lock()
		failures = append(failures, err)
		failureMu.Unlock()
	}
	for index := 0; index < w.options.Concurrency; index++ {
		group.Go(func() {
			for report := range jobs {
				release, err := w.limiter.acquire(ctx)
				if err != nil {
					addFailure(err)
					continue
				}
				if err := w.processReport(ctx, report); err != nil {
					addFailure(err)
				} else {
					completed.Add(1)
				}
				release()
			}
		})
	}
dispatch:
	for _, report := range reports {
		select {
		case jobs <- report:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	group.Wait()
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err())
	}
	return int(completed.Load()), errors.Join(failures...)
}

func (w *RunTerminalReportingWorker) processReport(ctx context.Context, report *RunTerminalReport) error {
	if report == nil || report.Run == nil || report.LeaseExpiresAt == nil || report.Run.Scope != report.Scope || report.Run.ID != report.RunID || report.Run.Status != report.Status || report.TerminalRevision != report.Run.Revision || !runNeedsTerminalReporting(report.Run) {
		return ErrInvalidRunTerminalReport
	}
	var err error
	if w.options.ScopeAllowed != nil {
		var allowed bool
		allowed, err = w.options.ScopeAllowed(ctx, report.Scope)
		if err == nil && !allowed {
			err = errors.New("terminal Run reporting scope is inactive")
		}
	}
	if err == nil {
		err = w.project(ctx, cloneAgentRun(report.Run))
	}
	now := w.now().UTC()
	completion := RunTerminalReportingCompletion{
		Scope: report.Scope, RunID: report.RunID, Status: report.Status, TerminalRevision: report.TerminalRevision, WorkerID: report.LeaseOwner,
		LeaseExpiresAt: *report.LeaseExpiresAt, Now: now,
	}
	if err != nil {
		delay := workerPollDelay(time.Second, report.Attempts-1, "terminal-report:"+report.RunID)
		if retryErr := w.store.RetryRunTerminalReport(ctx, RunTerminalReportingRetry{RunTerminalReportingCompletion: completion, AvailableAt: now.Add(delay)}); retryErr != nil {
			return errors.Join(err, retryErr)
		}
		w.retried.Add(1)
		return err
	}
	if err := w.store.CompleteRunTerminalReport(ctx, completion); err != nil {
		return err
	}
	w.delivered.Add(1)
	return nil
}

func (w *RunTerminalReportingWorker) Stats() RunTerminalReportingWorkerStats {
	if w == nil {
		return RunTerminalReportingWorkerStats{}
	}
	return RunTerminalReportingWorkerStats{Claimed: w.claimed.Load(), Delivered: w.delivered.Load(), Retried: w.retried.Load(), Errors: w.errors.Load()}
}
