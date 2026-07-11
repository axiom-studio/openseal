package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type ActionExecutionCatalog interface {
	Resolve(context.Context, skill.ScopeReference, string, string, string, string) (*skill.BoundAction, error)
	ValidateInput(context.Context, *skill.BoundAction, map[string]interface{}) error
	ValidateOutput(context.Context, *skill.BoundAction, map[string]interface{}) error
}

type CredentialResolver interface {
	ResolveCredentials(context.Context, CredentialResolutionRequest) (map[string]string, error)
}

// CredentialResolutionRequest provides auditable action identity while keeping
// opaque references separate from their ephemeral resolved values.
type CredentialResolutionRequest struct {
	Scope        Scope
	DeploymentID string
	SkillID      string
	SkillVersion string
	Action       string
	ActionCallID string
	RunID        string
	References   map[string]skill.CredentialReference
}

type CredentialResolverFunc func(context.Context, CredentialResolutionRequest) (map[string]string, error)

func (f CredentialResolverFunc) ResolveCredentials(ctx context.Context, request CredentialResolutionRequest) (map[string]string, error) {
	return f(ctx, request)
}

type ActionDispatchInput struct {
	Call        *ActionCall
	Bound       *skill.BoundAction
	Arguments   map[string]interface{}
	Credentials map[string]string
}

type ActionDispatcher interface {
	DispatchAction(context.Context, ActionDispatchInput) (map[string]interface{}, error)
}

type ActionDispatcherFunc func(context.Context, ActionDispatchInput) (map[string]interface{}, error)

func (f ActionDispatcherFunc) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	return f(ctx, input)
}

type ActionWorker struct {
	store       KernelStore
	catalog     ActionExecutionCatalog
	credentials CredentialResolver
	dispatcher  ActionDispatcher
	now         func() time.Time
	newID       func() string
}

func NewActionWorker(store KernelStore, catalog ActionExecutionCatalog, credentials CredentialResolver, dispatcher ActionDispatcher) *ActionWorker {
	return &ActionWorker{store: store, catalog: catalog, credentials: credentials, dispatcher: dispatcher, now: time.Now, newID: uuid.NewString}
}

func (w *ActionWorker) RunOnce(ctx context.Context, scope Scope, workerID string, leaseDuration time.Duration) (*ActionExecutionResult, error) {
	if w == nil || w.store == nil || w.catalog == nil || w.dispatcher == nil {
		return nil, errors.New("action worker is not configured")
	}
	if leaseDuration <= 0 {
		leaseDuration = time.Minute
	}
	now := w.now().UTC()
	call, err := w.store.ClaimNextAction(ctx, ActionClaim{Scope: scope, WorkerID: workerID, Now: now, LeaseDuration: leaseDuration})
	if err != nil || call == nil {
		return nil, err
	}
	executionCtx, stopLease := w.holdActionLease(ctx, call, workerID, leaseDuration)
	bound, executionErr := w.catalog.Resolve(executionCtx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, call.DeploymentID, call.SkillID, call.SkillVersion, call.Action)
	if executionErr == nil {
		executionErr = w.catalog.ValidateInput(executionCtx, bound, call.Arguments)
	}
	credentials := map[string]string(nil)
	if executionErr == nil && len(call.CredentialRefs) > 0 {
		if w.credentials == nil {
			executionErr = errors.New("action credentials cannot be resolved")
		} else {
			credentials, executionErr = w.credentials.ResolveCredentials(executionCtx, CredentialResolutionRequest{
				Scope: call.Scope, DeploymentID: call.DeploymentID, SkillID: call.SkillID, SkillVersion: call.SkillVersion,
				Action: call.Action, ActionCallID: call.ID, RunID: call.RunID, References: cloneCredentialReferences(call.CredentialRefs),
			})
			if executionErr == nil {
				for name := range call.CredentialRefs {
					if credentials[name] == "" {
						executionErr = fmt.Errorf("credential %s was not resolved", name)
						break
					}
				}
			}
		}
	}
	var output map[string]interface{}
	if executionErr == nil {
		dispatchCtx := executionCtx
		cancel := func() {}
		if timeout := bound.Action.Timeout.Duration(); timeout > 0 {
			dispatchCtx, cancel = context.WithTimeout(executionCtx, timeout)
		}
		output, executionErr = w.dispatcher.DispatchAction(dispatchCtx, ActionDispatchInput{
			Call: cloneActionCall(call), Bound: bound, Arguments: cloneMap(call.Arguments), Credentials: credentials,
		})
		cancel()
	}
	if executionErr == nil {
		executionErr = w.catalog.ValidateOutput(executionCtx, bound, output)
	}
	call, leaseErr := stopLease()
	if leaseErr != nil {
		return nil, leaseErr
	}
	return w.persistOutcome(ctx, call, bound, credentials, output, executionErr, workerID)
}

func (w *ActionWorker) holdActionLease(parent context.Context, call *ActionCall, workerID string, leaseDuration time.Duration) (context.Context, func() (*ActionCall, error)) {
	executionCtx, cancelExecution := context.WithCancel(parent)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
	interval := leaseDuration / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	var mu sync.Mutex
	current := cloneActionCall(call)
	var leaseErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-parent.Done():
				mu.Lock()
				leaseErr = parent.Err()
				mu.Unlock()
				cancelExecution()
				return
			case <-ticker.C:
				mu.Lock()
				id := current.ID
				scope := current.Scope
				mu.Unlock()
				renewed, err := w.store.RenewActionLease(heartbeatCtx, scope, id, workerID, w.now().UTC(), leaseDuration)
				if err != nil {
					mu.Lock()
					leaseErr = err
					mu.Unlock()
					cancelExecution()
					return
				}
				mu.Lock()
				current = renewed
				mu.Unlock()
			}
		}
	}()
	var once sync.Once
	stop := func() (*ActionCall, error) {
		once.Do(func() {
			cancelHeartbeat()
			<-done
			cancelExecution()
		})
		mu.Lock()
		defer mu.Unlock()
		return cloneActionCall(current), leaseErr
	}
	return executionCtx, stop
}

func (w *ActionWorker) persistOutcome(ctx context.Context, call *ActionCall, bound *skill.BoundAction, credentials map[string]string, output map[string]interface{}, executionErr error, workerID string) (*ActionExecutionResult, error) {
	now := w.now().UTC()
	sourceRun, err := w.store.GetAgentRun(ctx, call.Scope, call.RunID)
	if err != nil {
		return nil, err
	}
	if sourceRun == nil {
		return nil, ErrRunNotFound
	}
	updatedCall := cloneActionCall(call)
	updatedCall.Revision++
	updatedCall.UpdatedAt = now
	updatedCall.LeaseOwner = ""
	updatedCall.LeaseExpiresAt = nil
	eventType := "action.succeeded"
	summary := fmt.Sprintf("Completed %s.%s", call.SkillID, call.Action)
	var updatedRun *AgentRun
	var expectedRunRevision int64
	if executionErr != nil && call.Attempt < call.MaxAttempts {
		updatedCall.Status = ActionCallStatusReady
		updatedCall.Error = sanitizeActionError(executionErr, credentials)
		var retryPolicy skill.ActionRetryPolicy
		if bound != nil {
			retryPolicy = bound.Action.Retry
		}
		updatedCall.AvailableAt = now.Add(actionRetryDelay(retryPolicy, call.Attempt))
		eventType = "action.retry_scheduled"
		summary = fmt.Sprintf("Scheduled retry %d of %d for %s.%s", call.Attempt+1, call.MaxAttempts, call.SkillID, call.Action)
	} else {
		completedAt := now
		updatedCall.CompletedAt = &completedAt
		if executionErr == nil {
			updatedCall.Status = ActionCallStatusSucceeded
			updatedCall.Output = cloneMap(output)
			updatedCall.Error = ""
		} else {
			updatedCall.Status = ActionCallStatusFailed
			updatedCall.Error = sanitizeActionError(executionErr, credentials)
			eventType = "action.failed"
			summary = fmt.Sprintf("Failed %s.%s after %d attempts", call.SkillID, call.Action, call.Attempt)
		}
		if sourceRun.Status != AgentRunStatusWaitingForDependency || sourceRun.WakeCondition == nil || sourceRun.WakeCondition.Type != "action" || sourceRun.WakeCondition.Reference != call.ID {
			return nil, fmt.Errorf("%w: run is not waiting on action %s", ErrInvalidRunTransition, call.ID)
		}
		expectedRunRevision = sourceRun.Revision
		updatedRun = cloneAgentRun(sourceRun)
		updatedRun.Status = AgentRunStatusQueued
		updatedRun.WakeCondition = nil
		updatedRun.AvailableAt = now
		updatedRun.QueueEnteredAt = now
		updatedRun.UpdatedAt = now
		updatedRun.Revision++
		if updatedRun.Budget != nil {
			if err := settleRunBudgetReservation(updatedRun, actionBudgetReservationID(call.ID), BudgetUsage{Actions: 1}); err != nil {
				return nil, err
			}
		}
		updatedRun.LastWakeSignalID = "action:" + call.ID + ":" + fmt.Sprint(updatedCall.Revision)
		checkpoint := cloneMap(updatedRun.Checkpoint)
		if checkpoint == nil {
			checkpoint = make(map[string]interface{})
		}
		checkpoint["lastAction"] = map[string]interface{}{"actionCallId": call.ID, "status": updatedCall.Status}
		updatedRun.Checkpoint = checkpoint
	}
	event := &ActivityEvent{
		ID: w.newID(), Scope: call.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: call.DeploymentID, ObjectiveID: sourceRun.ObjectiveID, TeamID: teamIDForRun(sourceRun), RunID: call.RunID, TurnID: call.TurnID, Actor: ActivityActor{Type: "worker", ID: workerID},
		Summary: summary, Visibility: ActivityVisibilityScope, CausationID: call.ID, CreatedAt: now,
		Payload: map[string]interface{}{"actionCallId": call.ID, "skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action, "status": updatedCall.Status, "attempt": updatedCall.Attempt},
	}
	return w.store.PersistActionExecution(ctx, ActionExecutionRecord{
		Call: updatedCall, ExpectedCallRevision: call.Revision, Run: updatedRun, ExpectedRunRevision: expectedRunRevision,
		WorkerID: workerID, Now: now, Event: event,
	})
}

func actionRetryDelay(policy skill.ActionRetryPolicy, attempt int) time.Duration {
	delay := policy.InitialBackoff.Duration()
	if delay <= 0 {
		delay = time.Second
	}
	for current := 1; current < attempt; current++ {
		if delay > (1<<62)/2 {
			break
		}
		delay *= 2
	}
	if maximum := policy.MaxBackoff.Duration(); maximum > 0 && delay > maximum {
		return maximum
	}
	return delay
}

func sanitizeActionError(err error, credentials map[string]string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, value := range credentials {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	if len(message) > 1024 {
		message = message[:1024]
	}
	return message
}

func cloneCredentialReferences(input map[string]skill.CredentialReference) map[string]skill.CredentialReference {
	if input == nil {
		return nil
	}
	result := make(map[string]skill.CredentialReference, len(input))
	for name, reference := range input {
		result[name] = reference
	}
	return result
}
