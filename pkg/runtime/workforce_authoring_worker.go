package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const workforceAuthoringAgentID = "openseal.workforce-authoring"

type WorkforceAuthoringRunStore interface {
	KernelStore
	authoring.ChangeSetStore
	authoring.PendingChangeSetGenerationStore
}

type WorkforceAuthoringRunService struct {
	changeSets *authoring.ChangeSetService
	store      WorkforceAuthoringRunStore
	runs       *RunCommandService
}

func NewWorkforceAuthoringRunService(compiler *authoring.Compiler, store WorkforceAuthoringRunStore) (*WorkforceAuthoringRunService, error) {
	if store == nil {
		return nil, errors.New("workforce authoring run store is required")
	}
	changeSets, err := authoring.NewChangeSetService(compiler, store)
	if err != nil {
		return nil, err
	}
	return &WorkforceAuthoringRunService{changeSets: changeSets, store: store, runs: NewRunCommandService(store)}, nil
}

// Prepare persists the generation intent first, then idempotently schedules its
// canonical Run. RecoverPending repairs the narrow crash window between them.
func (s *WorkforceAuthoringRunService) Prepare(ctx context.Context, request authoring.CreateChangeSetRequest) (*authoring.ChangeSet, *AgentRun, bool, error) {
	changeSet, replay, err := s.changeSets.Prepare(ctx, request)
	if err != nil {
		return nil, nil, false, err
	}
	run, err := s.Enqueue(ctx, changeSet)
	if err != nil {
		return changeSet, nil, replay, err
	}
	changeSet, err = s.changeSets.Get(ctx, changeSet.Scope, changeSet.ID)
	return changeSet, run, replay, err
}

func (s *WorkforceAuthoringRunService) Enqueue(ctx context.Context, changeSet *authoring.ChangeSet) (*AgentRun, error) {
	if changeSet == nil || changeSet.Generation == nil || changeSet.Status != authoring.ChangeSetEvaluating {
		return nil, errors.New("evaluating workforce change set generation is required")
	}
	scope := Scope{Kind: changeSet.Scope.Kind, ID: changeSet.Scope.ID}
	result, err := s.runs.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindWorkforceAuthoring,
		Owner:           ObjectiveOwner{Type: OwnerTypeAgent, ID: workforceAuthoringAgentID},
		AssignedAgentID: workforceAuthoringAgentID,
		ConcurrencyKey:  "workforce-change-set:" + changeSet.ID,
		Goal:            "Generate governed workforce change set " + changeSet.ID,
		Source:          RunSourceRequest,
		Context: map[string]interface{}{
			"changeSetId": changeSet.ID,
		},
		Checkpoint:     map[string]interface{}{"phase": "queued", "changeSetId": changeSet.ID},
		IdempotencyKey: fmt.Sprintf("workforce-change-set-generation:%s:%d", changeSet.ID, changeSet.Generation.Attempt),
		Actor:          ActivityActor{Type: changeSet.Actor.Type, ID: changeSet.Actor.ID},
	})
	if err != nil {
		return nil, err
	}
	if changeSet.Generation.RunID == "" {
		next := cloneRuntimeChangeSetForAuthoring(changeSet)
		next.Generation.RunID = result.Run.ID
		next.Revision++
		next.UpdatedAt = time.Now().UTC()
		next.Lifecycle = append(next.Lifecycle, authoring.ChangeSetLifecycleEvent{
			Revision: next.Revision, From: authoring.ChangeSetEvaluating, To: authoring.ChangeSetEvaluating,
			Reason: "generation_run_scheduled", Actor: next.Actor, At: next.UpdatedAt,
		})
		if _, updateErr := s.store.UpdateChangeSet(ctx, next, changeSet.Revision); updateErr != nil && !errors.Is(updateErr, authoring.ErrChangeSetRevision) {
			return nil, updateErr
		}
	}
	return result.Run, nil
}

func (s *WorkforceAuthoringRunService) Retry(ctx context.Context, request authoring.RetryChangeSetGenerationRequest) (*authoring.ChangeSet, *AgentRun, error) {
	changeSet, err := s.changeSets.RetryGeneration(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	run, err := s.Enqueue(ctx, changeSet)
	if err != nil {
		return changeSet, nil, err
	}
	changeSet, err = s.changeSets.Get(ctx, changeSet.Scope, changeSet.ID)
	return changeSet, run, err
}

func (s *WorkforceAuthoringRunService) RecoverPending(ctx context.Context, scope Scope, limit int) ([]*AgentRun, error) {
	values, err := s.store.ListPendingChangeSetGenerations(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, limit)
	if err != nil {
		return nil, err
	}
	runs := make([]*AgentRun, 0, len(values))
	for _, value := range values {
		run, enqueueErr := s.Enqueue(ctx, value)
		if enqueueErr != nil {
			return runs, enqueueErr
		}
		runs = append(runs, run)
	}
	return runs, nil
}

type WorkforceAuthoringWorkerConfig struct {
	Scope             Scope
	WorkerID          string
	LeaseDuration     time.Duration
	PollInterval      time.Duration
	RecoveryLimit     int
	GenerationTimeout time.Duration
}

type WorkforceAuthoringWorker struct {
	config    WorkforceAuthoringWorkerConfig
	service   *WorkforceAuthoringRunService
	scheduler *AgentRunScheduler
	activity  *RunActivityService
	logger    *zap.SugaredLogger
	wake      chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewWorkforceAuthoringWorker(service *WorkforceAuthoringRunService, logger *zap.SugaredLogger, config WorkforceAuthoringWorkerConfig) (*WorkforceAuthoringWorker, error) {
	if service == nil {
		return nil, errors.New("workforce authoring run service is required")
	}
	if err := config.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.WorkerID) == "" {
		config.WorkerID = "workforce-authoring-" + uuid.NewString()
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = 15 * time.Minute
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	if config.RecoveryLimit <= 0 {
		config.RecoveryLimit = 1000
	}
	if config.GenerationTimeout <= 0 {
		config.GenerationTimeout = 10 * time.Minute
	}
	if config.LeaseDuration < config.GenerationTimeout+30*time.Second {
		return nil, errors.New("workforce authoring lease duration must exceed generation timeout by at least 30 seconds")
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &WorkforceAuthoringWorker{
		config: config, service: service, scheduler: NewAgentRunScheduler(service.store),
		activity: NewRunActivityService(service.store, service.store), logger: logger, wake: make(chan struct{}, 1),
	}, nil
}

// Start reconciles persisted generation intents before claiming work, so a
// process restart does not depend on the original request reaching enqueue.
func (w *WorkforceAuthoringWorker) Start(ctx context.Context) error {
	var startErr error
	w.startOnce.Do(func() {
		if _, err := w.service.RecoverPending(ctx, w.config.Scope, w.config.RecoveryLimit); err != nil {
			startErr = err
			return
		}
		workerCtx, cancel := context.WithCancel(ctx)
		w.cancel = cancel
		w.wg.Add(1)
		go w.loop(workerCtx)
		w.Wake()
	})
	return startErr
}

func (w *WorkforceAuthoringWorker) Stop() {
	w.stopOnce.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
		w.wg.Wait()
	})
}

func (w *WorkforceAuthoringWorker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *WorkforceAuthoringWorker) loop(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()
	for {
		worked, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.logger.Errorw("workforce authoring run failed", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

func (w *WorkforceAuthoringWorker) RunOnce(ctx context.Context) (bool, error) {
	run, err := w.scheduler.ClaimNext(ctx, AgentRunClaimRequest{
		Scope: w.config.Scope, Kind: RunKindWorkforceAuthoring, WorkerID: w.config.WorkerID,
		LeaseDuration: w.config.LeaseDuration, AgingInterval: time.Minute, MaxActiveForAgent: 1,
		MaxActiveForConcurrencyKey: 1,
	})
	if err != nil || run == nil {
		return false, err
	}
	return true, w.execute(ctx, run)
}

func (w *WorkforceAuthoringWorker) execute(ctx context.Context, run *AgentRun) error {
	changeSetID, _ := run.Context["changeSetId"].(string)
	if strings.TrimSpace(changeSetID) == "" {
		return w.finishRun(ctx, run, AgentRunStatusFailed, nil, "authoring Run has no changeSetId", "run.failed")
	}
	scope := capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	changeSet, err := w.service.changeSets.Get(ctx, scope, changeSetID)
	if err != nil {
		return w.finishRun(ctx, run, AgentRunStatusFailed, nil, err.Error(), "run.failed")
	}
	if changeSet.Status != authoring.ChangeSetEvaluating {
		return w.finishRun(ctx, run, AgentRunStatusCompleted, map[string]interface{}{
			"changeSetId": changeSet.ID, "changeSetStatus": changeSet.Status, "candidateDigest": changeSet.CandidateDigest,
		}, "", "workforce.generation.reconciled")
	}
	_, _ = w.activity.AppendActivity(ctx, &ActivityEvent{
		Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID,
		EventType: "workforce.generation.started", Summary: "Generating workforce candidate",
		Actor: ActivityActor{Type: "worker", ID: w.config.WorkerID}, Visibility: ActivityVisibilityScope,
		Payload: map[string]interface{}{"changeSetId": changeSet.ID, "attempt": changeSet.Generation.Attempt + 1},
	})
	generationCtx, cancel := context.WithTimeout(ctx, w.config.GenerationTimeout)
	defer cancel()
	generated, generateErr := w.service.changeSets.GeneratePrepared(generationCtx, scope, changeSet.ID, changeSet.Revision)
	if generateErr != nil {
		if errors.Is(generateErr, context.Canceled) && ctx.Err() != nil {
			yieldCtx, yieldCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer yieldCancel()
			yieldErr := w.yieldInterruptedRun(yieldCtx, run)
			return errors.Join(generateErr, yieldErr)
		}
		if errors.Is(generateErr, authoring.ErrChangeSetRevision) {
			current, getErr := w.service.changeSets.Get(ctx, scope, changeSet.ID)
			if getErr == nil && current.Status != authoring.ChangeSetEvaluating {
				return w.finishRun(ctx, run, AgentRunStatusCompleted, map[string]interface{}{
					"changeSetId": current.ID, "changeSetStatus": current.Status, "candidateDigest": current.CandidateDigest,
				}, "", "workforce.generation.reconciled")
			}
		}
		publicError := "Workforce generation failed"
		if current, getErr := w.service.changeSets.Get(ctx, scope, changeSet.ID); getErr == nil && current.Generation != nil && current.Generation.LastError != "" {
			publicError = current.Generation.LastError
		}
		w.logger.Warnw("workforce generation provider call failed", "changeSetId", changeSet.ID, "runId", run.ID, "failure", publicError)
		finishErr := w.finishRun(ctx, run, AgentRunStatusFailed, nil, publicError, "workforce.generation.failed")
		return errors.Join(generateErr, finishErr)
	}
	return w.finishRun(ctx, run, AgentRunStatusCompleted, map[string]interface{}{
		"changeSetId": generated.ID, "changeSetStatus": generated.Status, "candidateDigest": generated.CandidateDigest,
	}, "", "workforce.generation.completed")
}

func (w *WorkforceAuthoringWorker) yieldInterruptedRun(ctx context.Context, claimed *AgentRun) error {
	current, err := w.service.store.GetAgentRun(ctx, claimed.Scope, claimed.ID)
	if err != nil {
		return err
	}
	if current == nil || current.Status != AgentRunStatusRunning {
		return nil
	}
	_, _, err = w.activity.TransitionRun(ctx, current.Scope, current.ID, RunTransitionRequest{
		ExpectedRevision: current.Revision, Status: AgentRunStatusQueued, LeaseOwner: w.config.WorkerID,
		Checkpoint: map[string]interface{}{"phase": "interrupted", "changeSetId": claimed.Context["changeSetId"]},
		EventType:  "workforce.generation.interrupted", Summary: "Workforce generation yielded during worker shutdown",
		Actor: ActivityActor{Type: "worker", ID: w.config.WorkerID}, Severity: ActivitySeverityWarning,
	})
	return err
}

func cloneRuntimeChangeSetForAuthoring(value *authoring.ChangeSet) *authoring.ChangeSet {
	payload, _ := json.Marshal(value)
	var cloned authoring.ChangeSet
	_ = json.Unmarshal(payload, &cloned)
	return &cloned
}

func (w *WorkforceAuthoringWorker) finishRun(ctx context.Context, claimed *AgentRun, status AgentRunStatus, output map[string]interface{}, runError, eventType string) error {
	current, err := w.service.store.GetAgentRun(ctx, claimed.Scope, claimed.ID)
	if err != nil {
		return err
	}
	if current == nil || isTerminalAgentRunStatus(current.Status) {
		return nil
	}
	_, _, err = w.activity.TransitionRun(ctx, current.Scope, current.ID, RunTransitionRequest{
		ExpectedRevision: current.Revision, Status: status, Output: output, Error: runError,
		Checkpoint: map[string]interface{}{"phase": string(status), "changeSetId": claimed.Context["changeSetId"]},
		LeaseOwner: w.config.WorkerID, EventType: eventType,
		Summary: fmt.Sprintf("Workforce candidate generation %s", status),
		Actor:   ActivityActor{Type: "worker", ID: w.config.WorkerID},
		Severity: func() ActivitySeverity {
			if status == AgentRunStatusFailed {
				return ActivitySeverityError
			}
			return ActivitySeverityInfo
		}(),
	})
	return err
}
