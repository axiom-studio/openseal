package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skillerror"
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
	// onClaim is an immutable pool hook that hands off another claim before
	// this worker starts hydrating authority or waiting on the provider.
	onClaim func()
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
	if w.onClaim != nil {
		w.onClaim()
	}
	executionCtx, stopLease := w.holdActionLease(ctx, call, workerID, leaseDuration)
	// An ActionCall may outlive its parent dependency after cancellation or a
	// replacement continuation. Reconcile it before touching provider authority.
	sourceRun, executionErr := w.store.GetAgentRun(executionCtx, call.Scope, call.RunID)
	if executionErr == nil && sourceRun == nil {
		executionErr = ErrRunNotFound
	}
	if executionErr != nil || !runOwnsActionDependency(sourceRun, call) || runHasPausedActionDependency(sourceRun, call) {
		call, leaseErr := stopLease()
		if leaseErr != nil {
			return nil, leaseErr
		}
		if executionErr != nil {
			return nil, executionErr
		}
		return w.persistOutcome(ctx, call, nil, nil, nil, nil, workerID, false)
	}
	if w.correctionBlocked(executionCtx, sourceRun, call) {
		// Only the correction admitted for a returned failure may run; a stopped
		// attempt runs nothing. Stop before catalog hydration, credentials or dispatch.
		call, leaseErr := stopLease()
		if leaseErr != nil {
			return nil, leaseErr
		}
		return w.persistOutcome(ctx, call, nil, nil, nil, errActionNotAdmittedAfterFailure, workerID, false)
	}
	if call.RecoveredRunning && call.SideEffect != skill.SideEffectRead && call.SideEffect != skill.SideEffectNone {
		// The expired worker may have already dispatched this mutation. A new
		// lease does not prove that nothing happened, even with MaxAttempts=1.
		call, leaseErr := stopLease()
		if leaseErr != nil {
			return nil, leaseErr
		}
		return w.persistOutcome(ctx, call, nil, nil, nil,
			errors.New("The previous operation may already have taken effect. It was not sent again."), workerID, true)
	}
	if executionErr = actionSkillRuntimeMaintenanceError(executionCtx, w.store, call.Scope, call.SkillID); executionErr != nil {
		call, leaseErr := stopLease()
		if leaseErr != nil {
			return nil, leaseErr
		}
		return w.persistOutcome(ctx, call, nil, nil, nil, executionErr, workerID, false)
	}
	selection := []skill.BindingReference(nil)
	if call.BindingID != "" || call.BindingRevision != 0 {
		selection = append(selection, skill.BindingReference{ID: call.BindingID, Revision: call.BindingRevision})
	}
	bound, executionErr := w.catalog.Resolve(executionCtx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, call.DeploymentID, call.SkillID, call.SkillVersion, call.Action, selection...)
	if executionErr == nil {
		executionErr = w.catalog.ValidateInput(executionCtx, bound, call.Arguments)
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
	dispatched := false
	if executionErr == nil {
		// Catalog or credential resolution can race a pause/cancel command. The
		// second check keeps a known stale dependency out of the dispatcher.
		sourceRun, executionErr = w.store.GetAgentRun(executionCtx, call.Scope, call.RunID)
		if executionErr == nil && sourceRun == nil {
			executionErr = ErrRunNotFound
		}
	}
	if executionErr == nil {
		executionErr = actionSkillRuntimeMaintenanceError(executionCtx, w.store, call.Scope, call.SkillID)
	}
	if executionErr == nil && w.correctionBlocked(executionCtx, sourceRun, call) {
		executionErr = errActionNotAdmittedAfterFailure
	}
	if executionErr == nil && runOwnsActionDependency(sourceRun, call) && !runHasPausedActionDependency(sourceRun, call) {
		dispatchCtx := executionCtx
		cancel := func() {}
		if timeout := bound.Action.Timeout.Duration(); timeout > 0 {
			dispatchCtx, cancel = context.WithTimeout(executionCtx, timeout)
		}
		dispatched = true
		output, executionErr = w.dispatcher.DispatchAction(dispatchCtx, ActionDispatchInput{
			Call: cloneActionCall(call), Run: cloneAgentRun(sourceRun), Bound: bound, Arguments: cloneMap(call.Arguments), Credentials: credentials,
			CredentialLease: credentialLease, CredentialReferences: credentialReferences,
		})
		cancel()
	}
	if dispatched && executionErr == nil {
		executionErr = explicitActionResultFailure(output)
	}
	if dispatched && executionErr == nil {
		executionErr = w.catalog.ValidateOutput(executionCtx, bound, output)
	}
	call, leaseErr := stopLease()
	if leaseErr != nil {
		return nil, leaseErr
	}
	return w.persistOutcome(ctx, call, bound, credentials, output, executionErr, workerID, dispatched)
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

// Action stores may expose maintenance without requiring every custom action
// implementation to become a maintenance owner. Built-in stores additionally
// fence submission and claims inside their authoritative transaction.
func actionSkillRuntimeMaintenanceError(ctx context.Context, store interface{}, scope Scope, skillID string) error {
	getter, ok := store.(interface {
		GetSkillRuntimeMaintenance(context.Context, Scope, string) (*SkillRuntimeMaintenance, error)
	})
	if !ok {
		return nil
	}
	gate, err := getter.GetSkillRuntimeMaintenance(ctx, scope, skillID)
	if errors.Is(err, ErrSkillRuntimeMaintenanceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if gate != nil && gate.Active {
		return &SkillRuntimeMaintenanceError{Maintenance: *gate}
	}
	return nil
}

// Both the active and paused continuation own the same durable dependency.
// Checking identity prevents a late result from waking a newer continuation.
func runOwnsActionDependency(run *AgentRun, call *ActionCall) bool {
	if run == nil || call == nil || run.Scope != call.Scope || run.ID != call.RunID {
		return false
	}
	condition := run.WakeCondition
	if run.Status == AgentRunStatusPaused && run.PausedFrom == AgentRunStatusWaitingForDependency {
		condition = run.PausedWakeCondition
	} else if run.Status != AgentRunStatusWaitingForDependency {
		return false
	}
	return condition != nil && condition.Type == "action" && condition.Reference == call.ID
}

func runHasPausedActionDependency(run *AgentRun, call *ActionCall) bool {
	return run != nil && run.Status == AgentRunStatusPaused && runOwnsActionDependency(run, call)
}

func (w *ActionWorker) persistOutcome(ctx context.Context, call *ActionCall, bound *skill.BoundAction, credentials map[string]string, output map[string]interface{}, executionErr error, workerID string, dispatched bool) (*ActionExecutionResult, error) {
	// Retry the outcome, not the external operation, when an operator updates
	// the Run between our observation and its atomic completion transaction.
	const maximumPersistenceAttempts = 8
	humanInterventionID := ""
	if dispatched && executionErr == nil {
		if request := humanInterventionFromAction(&ActionCall{Output: output}, w.now().UTC(), w.newID); request != nil {
			humanInterventionID = request.ID
		}
	}
	eventID := w.newID()
	for attempt := 0; attempt < maximumPersistenceAttempts; attempt++ {
		sourceRun, err := w.store.GetAgentRun(ctx, call.Scope, call.RunID)
		if err != nil {
			return nil, err
		}
		if sourceRun == nil {
			return nil, ErrRunNotFound
		}
		record, err := w.prepareActionOutcome(ctx, call, sourceRun, bound, credentials, output, executionErr, workerID, dispatched, eventID, humanInterventionID)
		if err != nil {
			return nil, err
		}
		result, err := w.store.PersistActionExecution(ctx, record)
		if !errors.Is(err, ErrRevisionConflict) {
			return result, err
		}
		currentCall, err := w.store.GetActionCall(ctx, call.Scope, call.ID)
		if err != nil {
			return nil, err
		}
		if currentCall == nil || currentCall.Status != ActionCallStatusRunning || currentCall.LeaseOwner != workerID || currentCall.Attempt != call.Attempt || currentCall.LeaseExpiresAt == nil || !currentCall.LeaseExpiresAt.After(w.now().UTC()) {
			return nil, ErrLeaseLost
		}
		call = currentCall
	}
	return nil, ErrRevisionConflict
}

func (w *ActionWorker) prepareActionOutcome(ctx context.Context, call *ActionCall, sourceRun *AgentRun, bound *skill.BoundAction, credentials map[string]string, output map[string]interface{}, executionErr error, workerID string, dispatched bool, eventID, humanInterventionID string) (ActionExecutionRecord, error) {
	now := w.now().UTC()
	ownsDependency := runOwnsActionDependency(sourceRun, call)
	paused := runHasPausedActionDependency(sourceRun, call)
	updatedCall := cloneActionCall(call)
	updatedCall.Revision++
	updatedCall.UpdatedAt = now
	updatedCall.LeaseOwner = ""
	updatedCall.LeaseExpiresAt = nil
	eventType := "action.succeeded"
	summary := fmt.Sprintf("Completed %s.%s", call.SkillID, call.Action)
	var updatedRun *AgentRun
	if !dispatched && !ownsDependency {
		updatedCall.Status = ActionCallStatusCanceled
		updatedCall.CompletedAt = &now
		updatedCall.Error = "owning Run no longer waits for this action"
		eventType = "action.canceled"
		summary = fmt.Sprintf("Canceled stale %s.%s action", call.SkillID, call.Action)
	} else if !dispatched && (paused || executionErr == nil || errors.Is(executionErr, ErrSkillRuntimeMaintenance)) {
		updatedCall.Status = ActionCallStatusReady
		updatedCall.AvailableAt = now
		eventType = "action.deferred"
		summary = fmt.Sprintf("Deferred %s.%s after its Run changed before dispatch", call.SkillID, call.Action)
		if errors.Is(executionErr, ErrSkillRuntimeMaintenance) {
			summary = fmt.Sprintf("Deferred %s.%s during runtime maintenance", call.SkillID, call.Action)
		}

	} else {
		completedAt := now
		updatedCall.CompletedAt = &completedAt
		if executionErr == nil {
			updatedCall.Status = ActionCallStatusSucceeded
			var semanticArguments map[string]string
			if bound != nil {
				semanticArguments = bound.Action.SemanticArguments
			}
			updatedCall.Output = sanitizeActionOutput(annotateActionProgress(output, updatedCall, semanticArguments), credentials)
			updatedCall.Error = ""
			updatedCall.ErrorCode, updatedCall.ErrorDetails = "", nil
		} else {
			updatedCall.Status = ActionCallStatusFailed
			updatedCall.Error = sanitizeActionError(executionErr, credentials)
			updatedCall.ErrorCode, updatedCall.ErrorDetails = "", nil
			var failure *skillerror.ActionError
			if errors.As(executionErr, &failure) && failure != nil {
				if safe := skillerror.NewActionError(failure.Code(), "", failure.Details()); safe != nil {
					updatedCall.Error = safe.Error()
					updatedCall.ErrorCode, updatedCall.ErrorDetails = safe.Code(), safe.Details()
				}
			}
			updatedCall.FailurePhase = ActionFailureBeforeDispatch
			if dispatched {
				updatedCall.FailurePhase = ActionFailureAfterDispatch
			}
			eventType = "action.failed"
			summary = fmt.Sprintf("Failed %s.%s after %d attempts", call.SkillID, call.Action, call.Attempt)
		}
	}
	terminalOutcome := updatedCall.Status == ActionCallStatusSucceeded || updatedCall.Status == ActionCallStatusFailed
	if ownsDependency && terminalOutcome {
		updatedRun = cloneAgentRun(sourceRun)
		if paused {
			updatedRun.PausedFrom = AgentRunStatusQueued
			updatedRun.PausedWakeCondition = nil
		} else {
			updatedRun.Status = AgentRunStatusQueued
			updatedRun.WakeCondition = nil
			updatedRun.AvailableAt = now
			updatedRun.QueueEnteredAt = now
		}
		updatedRun.LastWakeSignalID = "action:" + call.ID + ":" + fmt.Sprint(updatedCall.Revision)
		updatedRun.Checkpoint = checkpointTerminalAction(updatedRun.Checkpoint, updatedCall, nil)
		if updatedCall.Status == ActionCallStatusFailed && !paused {
			// The failure is returned to the model as this action's result
			// (bounded by checkpointToolFeedbackFailure). The attempt ends when
			// that bound is reached, or for a typed source challenge without a
			// proven conversation for its bounded question.
			stop := requiresFinalFailureExplanation(updatedRun.Checkpoint)
			if !stop && hasCanonicalSourceAccessChallenge(updatedRun) {
				interaction, interactionErr := resolveSourceAccessChallengeInteraction(ctx, w.store, updatedRun)
				stop = interactionErr != nil || interaction == nil
			}
			if stop {
				updatedRun.Status = AgentRunStatusFailed
				updatedRun.Error = updatedCall.Error
				updatedRun.CompletedAt = &now
				updatedRun.LeaseOwner, updatedRun.LeaseExpiresAt = "", nil
			}
		}
		if updatedCall.Status == ActionCallStatusFailed && updatedCall.ApprovalID != "" {
			approval, approvalErr := w.store.GetApproval(ctx, updatedCall.Scope, updatedCall.ApprovalID)
			if approvalErr != nil {
				return ActionExecutionRecord{}, approvalErr
			}
			updatedRun.Checkpoint = checkpointApprovedActionFailure(updatedRun.Checkpoint, approval, updatedCall)
		} else if updatedCall.Status == ActionCallStatusSucceeded && updatedRun.Checkpoint[approvalRecoveryCheckpointKey] != nil {
			updatedRun.Checkpoint = clearApprovedActionFailure(updatedRun.Checkpoint)
		}
		if updatedCall.Status == ActionCallStatusSucceeded && updatedRun.Checkpoint[proposalRecoveryCheckpointKey] != nil {
			updatedRun.Checkpoint = clearProposalFailure(updatedRun.Checkpoint)
		}
		if updatedCall.Status == ActionCallStatusSucceeded {
			if request := humanInterventionFromAction(updatedCall, now, func() string { return humanInterventionID }); request != nil {
				condition := &WakeCondition{Type: "human_intervention", Reference: request.ID}
				if paused {
					updatedRun.PausedFrom = AgentRunStatusWaitingForEvent
					updatedRun.PausedWakeCondition = condition
				} else {
					updatedRun.Status = AgentRunStatusWaitingForEvent
					updatedRun.WakeCondition = condition
				}
				updatedRun.HumanInterventions = append(updatedRun.HumanInterventions, *request)
				eventType = "action.human_intervention_required"
				summary = request.Summary
			} else if request := pendingCredentialRequestFromAction(ctx, w.store, updatedCall); request != nil {
				// The work continues only after the user saves the credential in
				// the vault or dismisses the in-chat card.
				condition := &WakeCondition{Type: CredentialRequestWakeType, Reference: request.ID}
				if paused {
					updatedRun.PausedFrom = AgentRunStatusWaitingForEvent
					updatedRun.PausedWakeCondition = condition
				} else {
					updatedRun.Status = AgentRunStatusWaitingForEvent
					updatedRun.WakeCondition = condition
				}
				eventType = "action.credential_requested"
				summary = "Waiting for the user to save a requested credential"
			}
		}
	}
	// Cancellation before dispatch releases unused capacity. A result obtained
	// during cancellation settles its own reservation without changing the
	// canceled status or replacing a newer continuation's checkpoint.
	if sourceRun.Budget != nil && (terminalOutcome || updatedCall.Status == ActionCallStatusCanceled) {
		_, reserved := sourceRun.BudgetReservations[actionBudgetReservationID(call.ID)]
		if reserved || ownsDependency && terminalOutcome {
			if updatedRun == nil {
				updatedRun = cloneAgentRun(sourceRun)
			}
			var budgetErr error
			if updatedCall.Status == ActionCallStatusCanceled {
				budgetErr = releaseRunBudgetReservation(updatedRun, actionBudgetReservationID(call.ID))
			} else {
				budgetErr = settleRunBudgetReservation(updatedRun, actionBudgetReservationID(call.ID), BudgetUsage{Actions: 1})
			}
			if budgetErr != nil {
				return ActionExecutionRecord{}, budgetErr
			}
		}
	}
	if updatedRun != nil {
		updatedRun.UpdatedAt = now
		updatedRun.Revision++
	}
	event := &ActivityEvent{
		ID: eventID, Scope: call.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: call.DeploymentID, ObjectiveID: sourceRun.ObjectiveID, TeamID: teamIDForRun(sourceRun), RunID: call.RunID, TurnID: call.TurnID, Actor: ActivityActor{Type: "worker", ID: workerID},
		Summary: summary, Visibility: ActivityVisibilityScope, CausationID: call.ID, CreatedAt: now,
		Payload: map[string]interface{}{"actionCallId": call.ID, "bindingId": call.BindingID, "bindingRevision": call.BindingRevision, "skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action, "status": updatedCall.Status, "attempt": updatedCall.Attempt},
	}
	if eventType == "action.human_intervention_required" {
		event.Severity = ActivitySeverityWarning
		event.Payload["humanInterventionId"] = updatedRun.HumanInterventions[len(updatedRun.HumanInterventions)-1].ID
		event.Payload["challenge"] = append([]string(nil), updatedRun.HumanInterventions[len(updatedRun.HumanInterventions)-1].Challenge...)
	}
	if updatedCall.Status == ActionCallStatusSucceeded || updatedCall.Status == ActionCallStatusFailed {
		event.UsageDelta = &BudgetUsage{Actions: 1}
	}
	return ActionExecutionRecord{
		Call: updatedCall, ExpectedCallRevision: call.Revision, Run: updatedRun, ExpectedRunRevision: sourceRun.Revision,
		WorkerID: workerID, Now: now, Event: event,
	}, nil
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

var errActionNotAdmittedAfterFailure = errors.New("This operation was not run: a previous operation failed and this one was not admitted as its correction.")

func (w *ActionWorker) correctionBlocked(ctx context.Context, run *AgentRun, call *ActionCall) bool {
	return toolFeedbackActionBlocked(run.Checkpoint, call) && !sourceAccessChallengeActionAdmitted(ctx, w.store, run, call)
}
