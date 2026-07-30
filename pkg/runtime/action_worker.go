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
	Resolve(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundAction, error)
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

// ActionCredentialLeaseIssuer creates a signed, one-time opaque credential
// lease for a remote execution host. It receives durable identity and opaque
// references only; implementations select exact fields and sign through the
// ActionCredentialLease contract.
type ActionCredentialLeaseIssuer interface {
	IssueActionCredentialLease(context.Context, ActionCredentialLeaseIssueRequest) (*SignedActionCredentialLease, error)
}

type ActionCredentialLeaseIssuerFunc func(context.Context, ActionCredentialLeaseIssueRequest) (*SignedActionCredentialLease, error)

func (f ActionCredentialLeaseIssuerFunc) IssueActionCredentialLease(ctx context.Context, request ActionCredentialLeaseIssueRequest) (*SignedActionCredentialLease, error) {
	return f(ctx, request)
}

type ActionCredentialLeaseIssueRequest struct {
	Call       *ActionCall
	Run        *AgentRun
	References map[string]skill.CredentialReference
	Transport  string
}

type ActionDispatchInput struct {
	Call *ActionCall
	// Run is the durable, kernel-owned execution context for the call. It is
	// deliberately unavailable to model arguments. Dispatchers use it to keep
	// authority ownership (for example, a Team Skill binding) separate from the
	// roster Agent that supplies execution placement.
	Run         *AgentRun
	Bound       *skill.BoundAction
	Arguments   map[string]interface{}
	Credentials map[string]string
	// CredentialLease and CredentialReferences are the remote-resolution path.
	// They are mutually exclusive with plaintext Credentials.
	CredentialLease      *SignedActionCredentialLease
	CredentialReferences map[string]skill.CredentialReference
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
	leaseIssuer ActionCredentialLeaseIssuer
	dispatcher  ActionDispatcher
	now         func() time.Time
	newID       func() string
}

func NewActionWorker(store KernelStore, catalog ActionExecutionCatalog, credentials CredentialResolver, dispatcher ActionDispatcher) *ActionWorker {
	return &ActionWorker{store: store, catalog: catalog, credentials: credentials, dispatcher: dispatcher, now: time.Now, newID: uuid.NewString}
}

func NewActionWorkerWithCredentialLeaseIssuer(store KernelStore, catalog ActionExecutionCatalog, issuer ActionCredentialLeaseIssuer, dispatcher ActionDispatcher) *ActionWorker {
	return &ActionWorker{store: store, catalog: catalog, leaseIssuer: issuer, dispatcher: dispatcher, now: time.Now, newID: uuid.NewString}
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
	selection := []skill.BindingReference(nil)
	if call.BindingID != "" || call.BindingRevision != 0 {
		selection = append(selection, skill.BindingReference{ID: call.BindingID, Revision: call.BindingRevision})
	}
	bound, executionErr := w.catalog.Resolve(executionCtx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, call.DeploymentID, call.SkillID, call.SkillVersion, call.Action, selection...)
	if executionErr == nil {
		executionErr = w.catalog.ValidateInput(executionCtx, bound, call.Arguments)
	}
	var sourceRun *AgentRun
	if executionErr == nil {
		sourceRun, executionErr = w.store.GetAgentRun(executionCtx, call.Scope, call.RunID)
		if executionErr == nil && sourceRun == nil {
			executionErr = ErrRunNotFound
		}
	}
	credentials := map[string]string(nil)
	var credentialLease *SignedActionCredentialLease
	credentialReferences := map[string]skill.CredentialReference(nil)
	if executionErr == nil && len(call.CredentialRefs) > 0 {
		if w.leaseIssuer != nil {
			credentialTransport, transportErr := boundActionToolTransport(bound)
			if transportErr != nil {
				executionErr = fmt.Errorf("delegated credential lease transport: %w", transportErr)
			} else {
				credentialReferences = cloneCredentialReferences(call.CredentialRefs)
				credentialLease, executionErr = w.leaseIssuer.IssueActionCredentialLease(executionCtx, ActionCredentialLeaseIssueRequest{
					Call: cloneActionCall(call), Run: cloneAgentRun(sourceRun), References: cloneCredentialReferences(call.CredentialRefs), Transport: credentialTransport,
				})
			}
			if executionErr == nil {
				executionErr = MatchActionCredentialLeaseReferences(credentialLease, call, sourceRun, credentialTransport)
			}
		} else if w.credentials != nil {
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
		} else {
			executionErr = errors.New("action credentials cannot be resolved")
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
			Call: cloneActionCall(call), Run: cloneAgentRun(sourceRun), Bound: bound, Arguments: cloneMap(call.Arguments), Credentials: credentials,
			CredentialLease: credentialLease, CredentialReferences: credentialReferences,
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
			updatedCall.Output = sanitizeActionOutput(annotateActionProgress(output, updatedCall, bound.Action.SemanticArguments), credentials)
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
		updatedRun.Checkpoint = checkpointTerminalAction(updatedRun.Checkpoint, updatedCall, nil)
		if updatedCall.Status == ActionCallStatusFailed && updatedCall.ApprovalID != "" {
			approval, approvalErr := w.store.GetApproval(ctx, updatedCall.Scope, updatedCall.ApprovalID)
			if approvalErr != nil {
				return nil, approvalErr
			}
			updatedRun.Checkpoint = checkpointApprovedActionFailure(updatedRun.Checkpoint, approval, updatedCall)
		} else if updatedCall.Status == ActionCallStatusSucceeded && updatedRun.Checkpoint[approvalRecoveryCheckpointKey] != nil {
			updatedRun.Checkpoint = clearApprovedActionFailure(updatedRun.Checkpoint)
		}
		if updatedCall.Status == ActionCallStatusSucceeded && updatedRun.Checkpoint[proposalRecoveryCheckpointKey] != nil {
			updatedRun.Checkpoint = clearProposalFailure(updatedRun.Checkpoint)
		}
		if updatedCall.Status == ActionCallStatusSucceeded {
			if request := humanInterventionFromAction(updatedCall, now, w.newID); request != nil {
				updatedRun.Status = AgentRunStatusWaitingForEvent
				updatedRun.WakeCondition = &WakeCondition{Type: "human_intervention", Reference: request.ID}
				updatedRun.HumanInterventions = append(updatedRun.HumanInterventions, *request)
				eventType = "action.human_intervention_required"
				summary = request.Summary
			}
		}
	}
	event := &ActivityEvent{
		ID: w.newID(), Scope: call.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: call.DeploymentID, ObjectiveID: sourceRun.ObjectiveID, TeamID: teamIDForRun(sourceRun), RunID: call.RunID, TurnID: call.TurnID, Actor: ActivityActor{Type: "worker", ID: workerID},
		Summary: summary, Visibility: ActivityVisibilityScope, CausationID: call.ID, CreatedAt: now,
		Payload: map[string]interface{}{"actionCallId": call.ID, "bindingId": call.BindingID, "bindingRevision": call.BindingRevision, "skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action, "status": updatedCall.Status, "attempt": updatedCall.Attempt},
	}
	if updatedRun != nil && updatedRun.WakeCondition != nil && updatedRun.WakeCondition.Type == "human_intervention" {
		event.Severity = ActivitySeverityWarning
		event.Payload["humanInterventionId"] = updatedRun.WakeCondition.Reference
		event.Payload["challenge"] = append([]string(nil), updatedRun.HumanInterventions[len(updatedRun.HumanInterventions)-1].Challenge...)
	}
	if updatedCall.Status == ActionCallStatusSucceeded || updatedCall.Status == ActionCallStatusFailed {
		event.UsageDelta = &BudgetUsage{Actions: 1}
	}
	return w.store.PersistActionExecution(ctx, ActionExecutionRecord{
		Call: updatedCall, ExpectedCallRevision: call.Revision, Run: updatedRun, ExpectedRunRevision: expectedRunRevision,
		WorkerID: workerID, Now: now, Event: event,
	})
}

func humanInterventionFromAction(call *ActionCall, now time.Time, newID func() string) *HumanInterventionRequest {
	if call == nil || call.Output == nil {
		return nil
	}
	requiresHuman, _ := call.Output["requiresHuman"].(bool)
	if !requiresHuman {
		return nil
	}
	challenge := boundedChallengeKinds(call.Output["challenges"])
	return &HumanInterventionRequest{
		ID: newID(), Kind: "capability_challenge", Status: HumanInterventionStatusPending,
		ActionCallID: call.ID, Summary: "Human intervention is required before automation can continue",
		Challenge: challenge, CreatedAt: now,
	}
}

func boundedChallengeKinds(raw interface{}) []string {
	values := make([]string, 0, 4)
	appendValue := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || len(value) > 64 {
			return
		}
		for _, existing := range values {
			if existing == value {
				return
			}
		}
		if len(values) < 4 {
			values = append(values, value)
		}
	}
	switch typed := raw.(type) {
	case []string:
		for _, value := range typed {
			appendValue(value)
		}
	case []interface{}:
		for _, value := range typed {
			if text, ok := value.(string); ok {
				appendValue(text)
			}
		}
	}
	return values
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

// sanitizeActionOutput prevents an endpoint that echoes an injected credential
// from persisting or returning that secret to a later model turn. Credential
// values are ephemeral and are never part of the canonical action arguments.
func sanitizeActionOutput(output map[string]interface{}, credentials map[string]string) map[string]interface{} {
	if output == nil {
		return nil
	}
	redact := func(value string) string {
		for _, secret := range credentials {
			if secret != "" {
				value = strings.ReplaceAll(value, secret, "[REDACTED]")
			}
		}
		return value
	}
	var sanitize func(interface{}) interface{}
	sanitize = func(value interface{}) interface{} {
		switch typed := value.(type) {
		case string:
			return redact(typed)
		case map[string]interface{}:
			result := make(map[string]interface{}, len(typed))
			for key, child := range typed {
				result[key] = sanitize(child)
			}
			return result
		case []interface{}:
			result := make([]interface{}, len(typed))
			for index, child := range typed {
				result[index] = sanitize(child)
			}
			return result
		default:
			return typed
		}
	}
	return sanitize(output).(map[string]interface{})
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
