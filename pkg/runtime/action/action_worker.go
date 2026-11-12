package action

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skillerror"
	"github.com/google/uuid"
)

type CredentialResolver interface {
	ResolveCredentials(context.Context, CredentialResolutionRequest) (map[string]string, error)
}

// CredentialResolutionRequest provides auditable action identity while keeping
// opaque references separate from their ephemeral resolved values.
type CredentialResolutionRequest struct {
	Scope        models.Scope
	DeploymentID string
	SkillID      string
	SkillVersion string
	Action       string
	ActionCallID string
	RunID        string
	References   map[string]skill.CredentialReference
	// Umbrella is the run's umbrella and ActingUser the person it acts for
	// ("" for no person). Requirements are the bound action's credential
	// requirements by slot.
	Umbrella     models.ObjectiveOwner
	ActingUser   string
	Requirements map[string]skill.CredentialRequirement
}

// CredentialAvailabilityRequest asks the host, before a credential lease is
// issued, whether every referenced slot of a bound action can be resolved
// for the run's umbrella and acting person.
type CredentialAvailabilityRequest struct {
	Scope        models.Scope
	Umbrella     models.ObjectiveOwner
	ActingUser   string
	DeploymentID string
	BindingID    string
	References   map[string]skill.CredentialReference
	Requirements map[string]skill.CredentialRequirement
}

// CredentialGap is why one slot cannot be resolved.
type CredentialGap string

const (
	// CredentialGapPersonSignIn: the acting person has not connected an
	// account for a per_person slot.
	CredentialGapPersonSignIn CredentialGap = "person_sign_in"
	// CredentialGapSharedSignIn: no usable shared sign-in fills the slot.
	CredentialGapSharedSignIn CredentialGap = "shared_sign_in"
	// CredentialGapNoPerson: a per_person slot, and no person asked for the run.
	CredentialGapNoPerson CredentialGap = "no_person"
)

// CredentialAvailability maps each slot that cannot be resolved to its gap.
// An empty map means every slot can be resolved.
type CredentialAvailability struct{ Gaps map[string]CredentialGap }

// CredentialAvailabilityChecker is the host hook the action worker asks
// before it issues a credential lease.
type CredentialAvailabilityChecker interface {
	CheckCredentialAvailability(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error)
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
	IssueActionCredentialLease(context.Context, ActionCredentialLeaseIssueRequest) (*models.SignedActionCredentialLease, error)
}

type ActionCredentialLeaseIssuerFunc func(context.Context, ActionCredentialLeaseIssueRequest) (*models.SignedActionCredentialLease, error)

func (f ActionCredentialLeaseIssuerFunc) IssueActionCredentialLease(ctx context.Context, request ActionCredentialLeaseIssueRequest) (*models.SignedActionCredentialLease, error) {
	return f(ctx, request)
}

type ActionCredentialLeaseIssueRequest struct {
	Call       *models.ActionCall
	Run        *models.AgentRun
	References map[string]skill.CredentialReference
	Transport  string
}

type ActionDispatcherFunc func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error)

func (f ActionDispatcherFunc) DispatchAction(ctx context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
	return f(ctx, input)
}

type ActionWorker struct {
	store       models.KernelStore
	catalog     agentrun.ActionExecutionCatalog
	credentials CredentialResolver
	leaseIssuer ActionCredentialLeaseIssuer
	dispatcher  agentrun.ActionDispatcher
	now         func() time.Time
	newID       func() string
	// onClaim is an immutable pool hook that hands off another claim before
	// this worker starts hydrating authority or waiting on the provider.
	onClaim func()
	// hostSummary, when set, describes a host call's outcome in activity.
	hostSummary string
	// availability is the host's pre-dispatch credential check; teams reads
	// the rosters RunUmbrella checks.
	availability atomic.Pointer[credentialAvailabilityBox]
	teams        models.TeamRosterReader
	// loopLease is the call lease of inline loop execution; zero means
	// LoopActionLease.
	loopLease time.Duration
}

func NewActionWorker(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, credentials CredentialResolver, dispatcher agentrun.ActionDispatcher, opts ...models.ServiceOption) *ActionWorker {
	return &ActionWorker{store: store, catalog: catalog, credentials: credentials, dispatcher: dispatcher, now: models.ClockFrom(opts), newID: uuid.NewString, teams: models.TeamRosterReaderOf(store)}
}

func NewActionWorkerWithCredentialLeaseIssuer(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, issuer ActionCredentialLeaseIssuer, dispatcher agentrun.ActionDispatcher, opts ...models.ServiceOption) *ActionWorker {
	return &ActionWorker{store: store, catalog: catalog, leaseIssuer: issuer, dispatcher: dispatcher, now: models.ClockFrom(opts), newID: uuid.NewString, teams: models.TeamRosterReaderOf(store)}
}

// ErrLoopActionNotClaimable is returned by ExecuteOwnLoopCall when the call is
// neither ready nor a running call whose lease expired.
var ErrLoopActionNotClaimable = errors.New("loop action call is not claimable now")

// errLoopActionAbandoned closes a loop call whose Run ended before its loop
// finished it.
var errLoopActionAbandoned = errors.New("The run that made this call ended before it ran.")

// errActionInterrupted fails a write recovered from a lost execution: it may
// already have reached its provider, so it is never sent again.
var errActionInterrupted = errors.New("The previous operation may already have taken effect. It was not sent again.")

// loopActionMaintenanceRetry is how long a loop call held by Skill runtime
// maintenance waits before its loop tries it again.
const loopActionMaintenanceRetry = 30 * time.Second

// LoopActionMaintenanceError reports that a loop call was not dispatched
// because its Skill is under runtime maintenance. The call is ready again (or
// was never claimed); the loop parks its tool entry until RetryAt and then
// executes the same call.
type LoopActionMaintenanceError struct {
	Call        *models.ActionCall
	Maintenance models.SkillRuntimeMaintenance
	RetryAt     time.Time
}

func (e *LoopActionMaintenanceError) Error() string { return models.ErrSkillRuntimeMaintenance.Error() }
func (e *LoopActionMaintenanceError) Unwrap() error { return models.ErrSkillRuntimeMaintenance }

// RunOnce is the pool entry point. A pool worker executes nested calls of a
// running step; it closes, without dispatch, the host calls their host
// abandoned and the loop calls whose Run ended.
func (w *ActionWorker) RunOnce(ctx context.Context, scope models.Scope, workerID string, leaseDuration time.Duration) (*models.ActionExecutionResult, error) {
	if w == nil || w.store == nil || w.catalog == nil || w.dispatcher == nil {
		return nil, errors.New("action worker is not configured")
	}
	if leaseDuration <= 0 {
		leaseDuration = time.Minute
	}
	now := w.now().UTC()
	call, err := w.store.ClaimNextAction(ctx, models.ActionClaim{Scope: scope, WorkerID: workerID, Now: now, LeaseDuration: leaseDuration})
	if err != nil || call == nil {
		return nil, err
	}
	if w.onClaim != nil {
		w.onClaim()
	}
	switch call.Execution {
	case models.ActionExecutionHost:
		// Only its host performs a host call. A worker gets one only after
		// the host abandoned it, and closes it without dispatch; a call that
		// was running may already have reached its provider.
		return w.persistOutcome(ctx, call, nil, nil, nil, errNestedHostActionAbandoned, workerID, call.RecoveredRunning)
	case models.ActionExecutionLoop:
		// Only its loop runs a loop call. A worker gets one only after its Run
		// ended: a ready call is canceled, a running one may already have
		// reached its provider and fails without being sent again.
		return w.persistOutcome(ctx, call, nil, nil, nil, errLoopActionAbandoned, workerID, call.RecoveredRunning)
	}
	return w.executeClaimed(ctx, call, workerID, leaseDuration)
}

// ExecuteOwnLoopCall claims exactly this loop call for the Run's lease owner
// workerID and executes it inline under its call lease (LoopActionLease),
// which it renews while the call runs. It returns
// ErrLoopActionNotClaimable when the call is not ready and not a running call
// with an expired lease, and *LoopActionMaintenanceError when Skill runtime
// maintenance holds it.
func (w *ActionWorker) ExecuteOwnLoopCall(ctx context.Context, scope models.Scope, actionCallID, workerID string) (*models.ActionExecutionResult, error) {
	if w == nil || w.store == nil || w.catalog == nil || w.dispatcher == nil {
		return nil, errors.New("action worker is not configured")
	}
	current, err := w.store.GetActionCall(ctx, scope, actionCallID)
	if err != nil {
		return nil, err
	}
	if current.Origin != models.ActionOriginLoop || current.Execution != models.ActionExecutionLoop {
		return nil, errors.New("action call is not a loop call")
	}
	lease := w.loopLease
	if lease <= 0 {
		lease = models.LoopActionLease
	}
	claimed, err := w.store.ClaimNextAction(ctx, models.ActionClaim{Scope: scope, WorkerID: workerID, Now: w.now().UTC(), LeaseDuration: lease, OwnActionID: actionCallID})
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		// Stores do not claim a call whose Skill is under maintenance.
		if current.Status == models.ActionCallStatusReady {
			if maintenanceErr := actionSkillRuntimeMaintenanceError(ctx, w.store, scope, current.SkillID); maintenanceErr != nil {
				return nil, w.loopMaintenance(current, maintenanceErr)
			}
		}
		return nil, ErrLoopActionNotClaimable
	}
	result, err := w.executeClaimed(ctx, claimed, workerID, lease)
	var held *models.SkillRuntimeMaintenanceError
	if errors.As(err, &held) && result != nil {
		return nil, w.loopMaintenance(result.Call, err)
	}
	return result, err
}

func (w *ActionWorker) loopMaintenance(call *models.ActionCall, cause error) error {
	var held *models.SkillRuntimeMaintenanceError
	if !errors.As(cause, &held) {
		return cause
	}
	return &LoopActionMaintenanceError{Call: models.CloneActionCall(call), Maintenance: held.Maintenance, RetryAt: w.now().UTC().Add(loopActionMaintenanceRetry)}
}

// executeClaimed runs the governance body for a call this worker holds:
// ownership, the recovered-running guard, maintenance, Resolve and
// ValidateInput, credential availability, the credential lease or plaintext
// credentials, the re-check, the per-action timeout, dispatch, explicit result
// failures, ValidateOutput and the persisted outcome.
//
// A loop call held by maintenance is deferred back to ready and its result
// is returned together with the *SkillRuntimeMaintenanceError.
func (w *ActionWorker) executeClaimed(ctx context.Context, call *models.ActionCall, workerID string, leaseDuration time.Duration) (*models.ActionExecutionResult, error) {
	executionCtx, stopLease := w.holdActionLease(ctx, call, workerID, leaseDuration)
	// An ActionCall may outlive the Run step or wait that made it. Reconcile it
	// before touching provider authority.
	sourceRun, executionErr := w.store.GetAgentRun(executionCtx, call.Scope, call.RunID)
	if executionErr == nil && sourceRun == nil {
		executionErr = models.ErrRunNotFound
	}
	owns := false
	if executionErr == nil {
		owns, executionErr = w.callOwnedByRun(executionCtx, sourceRun, call)
	}
	if executionErr != nil || !owns || models.RunHasPausedActionDependency(sourceRun, call) {
		call, leaseErr := stopLease()
		if leaseErr != nil {
			return nil, leaseErr
		}
		if executionErr != nil {
			return nil, executionErr
		}
		return w.persistOutcome(ctx, call, nil, nil, nil, nil, workerID, false)
	}
	if call.RecoveredRunning && call.SideEffect != skill.SideEffectRead && call.SideEffect != skill.SideEffectNone {
		// The expired worker may have already dispatched this mutation. A new
		// lease does not prove that nothing happened, even with MaxAttempts=1.
		call, leaseErr := stopLease()
		if leaseErr != nil {
			return nil, leaseErr
		}
		return w.persistOutcome(ctx, call, nil, nil, nil, errActionInterrupted, workerID, true)
	}
	if executionErr = actionSkillRuntimeMaintenanceError(executionCtx, w.store, call.Scope, call.SkillID); executionErr != nil {
		call, leaseErr := stopLease()
		if leaseErr != nil {
			return nil, leaseErr
		}
		return w.persistMaintenanceOutcome(ctx, call, executionErr, workerID)
	}
	selection := []skill.BindingReference(nil)
	if call.BindingID != "" || call.BindingRevision != 0 {
		selection = append(selection, skill.BindingReference{ID: call.BindingID, Revision: call.BindingRevision})
	}
	bound, executionErr := w.catalog.Resolve(skill.WithRunActor(executionCtx, models.RunSkillActor(sourceRun)), skill.ScopeReference{Kind: call.Scope.Kind, ID: call.Scope.ID}, call.DeploymentID, call.SkillID, call.SkillVersion, call.Action, selection...)
	if executionErr == nil {
		executionErr = w.catalog.ValidateInput(executionCtx, bound, call.Arguments)
	}
	credentials := map[string]string(nil)
	var credentialLease *models.SignedActionCredentialLease
	credentialReferences := map[string]skill.CredentialReference(nil)
	var umbrella models.ObjectiveOwner
	if executionErr == nil && len(call.CredentialRefs) > 0 {
		// Fail closed before any lease: the run must act under a valid
		// umbrella, a per_person account needs a person, and a missing
		// sign-in becomes a card instead of a failed dispatch.
		umbrella, executionErr = w.checkCredentialAvailability(executionCtx, sourceRun, call, bound)
	}
	if executionErr == nil && len(call.CredentialRefs) > 0 {
		if w.leaseIssuer != nil {
			credentialTransport, transportErr := BoundActionToolTransport(bound)
			if transportErr != nil {
				executionErr = fmt.Errorf("delegated credential lease transport: %w", transportErr)
			} else {
				credentialReferences = maps.Clone(call.CredentialRefs)
				credentialLease, executionErr = w.leaseIssuer.IssueActionCredentialLease(executionCtx, ActionCredentialLeaseIssueRequest{
					Call: models.CloneActionCall(call), Run: models.CloneAgentRun(sourceRun), References: maps.Clone(call.CredentialRefs), Transport: credentialTransport,
				})
			}
			if executionErr == nil {
				executionErr = MatchActionCredentialLeaseReferences(credentialLease, call, sourceRun, credentialTransport)
			}
		} else if w.credentials != nil {
			credentials, executionErr = w.credentials.ResolveCredentials(executionCtx, CredentialResolutionRequest{
				Scope: call.Scope, DeploymentID: call.DeploymentID, SkillID: call.SkillID, SkillVersion: call.SkillVersion,
				Action: call.Action, ActionCallID: call.ID, RunID: call.RunID, References: maps.Clone(call.CredentialRefs),
				Umbrella: umbrella, ActingUser: sourceRun.ActingUser, Requirements: skillbinding.ActionCredentialRequirements(bound),
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
			executionErr = models.ErrRunNotFound
		}
	}
	if executionErr == nil {
		executionErr = actionSkillRuntimeMaintenanceError(executionCtx, w.store, call.Scope, call.SkillID)
	}
	owns = false
	if executionErr == nil {
		owns, executionErr = w.callOwnedByRun(executionCtx, sourceRun, call)
	}
	if executionErr == nil && owns && !models.RunHasPausedActionDependency(sourceRun, call) {
		dispatchCtx := executionCtx
		cancel := func() {}
		if timeout := bound.Action.Timeout.Duration(); timeout > 0 {
			dispatchCtx, cancel = context.WithTimeout(executionCtx, timeout)
		}
		dispatched = true
		output, executionErr = w.dispatcher.DispatchAction(dispatchCtx, agentrun.ActionDispatchInput{
			Call: models.CloneActionCall(call), Run: models.CloneAgentRun(sourceRun), Bound: bound, Arguments: maps.Clone(call.Arguments), Credentials: credentials,
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
	if !dispatched && errors.Is(executionErr, models.ErrSkillRuntimeMaintenance) {
		return w.persistMaintenanceOutcome(ctx, call, executionErr, workerID)
	}
	return w.persistOutcome(ctx, call, bound, credentials, output, executionErr, workerID, dispatched)
}

// persistMaintenanceOutcome defers a call that maintenance held before
// dispatch. A deferred loop call also returns the maintenance error, so its
// loop parks the tool entry instead of waiting on the pool.
func (w *ActionWorker) persistMaintenanceOutcome(ctx context.Context, call *models.ActionCall, cause error, workerID string) (*models.ActionExecutionResult, error) {
	result, err := w.persistOutcome(ctx, call, nil, nil, nil, cause, workerID, false)
	if err != nil || call.Origin != models.ActionOriginLoop || result == nil || result.Call == nil || result.Call.Status != models.ActionCallStatusReady {
		return result, err
	}
	return result, cause
}

func (w *ActionWorker) holdActionLease(parent context.Context, call *models.ActionCall, workerID string, leaseDuration time.Duration) (context.Context, func() (*models.ActionCall, error)) {
	executionCtx, cancelExecution := context.WithCancel(parent)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
	interval := leaseDuration / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	var mu sync.Mutex
	current := models.CloneActionCall(call)
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
	stop := func() (*models.ActionCall, error) {
		once.Do(func() {
			cancelHeartbeat()
			<-done
			cancelExecution()
		})
		mu.Lock()
		defer mu.Unlock()
		return models.CloneActionCall(current), leaseErr
	}
	return executionCtx, stop
}

// Action stores may expose maintenance without requiring every custom action
// implementation to become a maintenance owner. Built-in stores additionally
// fence submission and claims inside their authoritative transaction.
func actionSkillRuntimeMaintenanceError(ctx context.Context, store interface{}, scope models.Scope, skillID string) error {
	getter, ok := store.(interface {
		GetSkillRuntimeMaintenance(context.Context, models.Scope, string) (*models.SkillRuntimeMaintenance, error)
	})
	if !ok {
		return nil
	}
	gate, err := getter.GetSkillRuntimeMaintenance(ctx, scope, skillID)
	if errors.Is(err, models.ErrSkillRuntimeMaintenanceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if gate != nil && gate.Active {
		return &models.SkillRuntimeMaintenanceError{Maintenance: *gate}
	}
	return nil
}

// callOwnedByRun reports whether the Run still wants call executed: a loop
// call needs its Run running under the lease that claimed the call; a nested
// call needs its step to still be the Run's running step. Calls of any other
// origin have no owner.
func (w *ActionWorker) callOwnedByRun(ctx context.Context, run *models.AgentRun, call *models.ActionCall) (bool, error) {
	if call == nil {
		return false, nil
	}
	switch call.Origin {
	case models.ActionOriginLoop:
		return loopActionOwned(run, call, w.now().UTC()), nil
	case models.ActionOriginVM:
		return nestedActionTurnLive(ctx, w.store, run, call, w.now().UTC())
	}
	return false, nil
}

// loopActionOwned reports whether call is held by the live lease of its
// running Run: the loop executing it inline is that Run's claim.
func loopActionOwned(run *models.AgentRun, call *models.ActionCall, now time.Time) bool {
	return run != nil && call != nil && run.Scope == call.Scope && run.ID == call.RunID && run.Status == models.AgentRunStatusRunning &&
		run.LeaseOwner != "" && run.LeaseOwner == call.LeaseOwner && run.LeaseExpiresAt != nil && run.LeaseExpiresAt.After(now)
}

func (w *ActionWorker) persistOutcome(ctx context.Context, call *models.ActionCall, bound *skill.BoundAction, credentials map[string]string, output map[string]interface{}, executionErr error, workerID string, dispatched bool) (*models.ActionExecutionResult, error) {
	// Retry the outcome, not the external operation, when an operator updates
	// the Run between our observation and its atomic completion transaction.
	const maximumPersistenceAttempts = 8
	humanInterventionID := ""
	if dispatched && executionErr == nil {
		if request := humanInterventionFromAction(&models.ActionCall{Output: output}, w.now().UTC(), w.newID); request != nil {
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
			return nil, models.ErrRunNotFound
		}
		record, err := w.prepareActionOutcome(ctx, call, sourceRun, bound, credentials, output, executionErr, workerID, dispatched, eventID, humanInterventionID)
		if err != nil {
			return nil, err
		}
		result, err := w.store.PersistActionExecution(ctx, record)
		if !errors.Is(err, models.ErrRevisionConflict) {
			return result, err
		}
		currentCall, err := w.store.GetActionCall(ctx, call.Scope, call.ID)
		if err != nil {
			return nil, err
		}
		if currentCall == nil || currentCall.Status != models.ActionCallStatusRunning || currentCall.LeaseOwner != workerID || currentCall.Attempt != call.Attempt || currentCall.LeaseExpiresAt == nil || !currentCall.LeaseExpiresAt.After(w.now().UTC()) {
			return nil, models.ErrLeaseLost
		}
		call = currentCall
	}
	return nil, models.ErrRevisionConflict
}

func (w *ActionWorker) prepareActionOutcome(ctx context.Context, call *models.ActionCall, sourceRun *models.AgentRun, bound *skill.BoundAction, credentials map[string]string, output map[string]interface{}, executionErr error, workerID string, dispatched bool, eventID, humanInterventionID string) (models.ActionExecutionRecord, error) {
	now := w.now().UTC()
	nested := call.Origin == models.ActionOriginVM
	ownsDependency, err := w.callOwnedByRun(ctx, sourceRun, call)
	if err != nil {
		return models.ActionExecutionRecord{}, err
	}
	paused := models.RunHasPausedActionDependency(sourceRun, call)
	var parked *connectAccountParked
	errors.As(executionErr, &parked)
	updatedCall := models.CloneActionCall(call)
	updatedCall.Revision++
	updatedCall.UpdatedAt = now
	updatedCall.LeaseOwner = ""
	updatedCall.LeaseExpiresAt = nil
	eventType := "action.succeeded"
	summary := fmt.Sprintf("Completed %s.%s", call.SkillID, call.Action)
	var updatedRun *models.AgentRun
	if !dispatched && !ownsDependency {
		updatedCall.Status = models.ActionCallStatusCanceled
		updatedCall.CompletedAt = &now
		updatedCall.Error = "owning Run no longer waits for this action"
		if nested {
			updatedCall.Error = "the turn that made this call has ended"
		} else if executionErr != nil && call.Origin == models.ActionOriginLoop {
			updatedCall.Error = SanitizeActionError(executionErr, credentials)
		}
		eventType = "action.canceled"
		summary = fmt.Sprintf("Canceled stale %s.%s action", call.SkillID, call.Action)
	} else if !dispatched && parked == nil && (paused || executionErr == nil || errors.Is(executionErr, models.ErrSkillRuntimeMaintenance)) {
		updatedCall.Status = models.ActionCallStatusReady
		updatedCall.AvailableAt = now
		eventType = "action.deferred"
		summary = fmt.Sprintf("Deferred %s.%s after its Run changed before dispatch", call.SkillID, call.Action)
		if errors.Is(executionErr, models.ErrSkillRuntimeMaintenance) {
			summary = fmt.Sprintf("Deferred %s.%s during runtime maintenance", call.SkillID, call.Action)
		}

	} else {
		completedAt := now
		updatedCall.CompletedAt = &completedAt
		if parked != nil {
			// Held for a sign-in: failed before dispatch, carrying its card,
			// and without its external operation identity so the retry after
			// the account is connected is admitted as a new operation.
			updatedCall.Status = models.ActionCallStatusFailed
			updatedCall.FailurePhase = models.ActionFailureBeforeDispatch
			updatedCall.Error = parked.Error()
			updatedCall.ErrorCode, updatedCall.ErrorDetails = "", nil
			projection, projectionErr := skillbinding.SkillSetupRequestResult(parked.request)
			if projectionErr != nil {
				return models.ActionExecutionRecord{}, projectionErr
			}
			projection["message"] = parked.request.Reason
			updatedCall.Output = projection
			updatedCall.ExternalOperationDigest = ""
			eventType = "action.connection_requested"
			summary = skillbinding.ConnectAccountWaitSummary(parked.request)
		} else if executionErr == nil {
			updatedCall.Status = models.ActionCallStatusSucceeded
			updatedCall.Output = sanitizeActionOutput(output, credentials)
			updatedCall.Error = ""
			updatedCall.ErrorCode, updatedCall.ErrorDetails = "", nil
		} else {
			updatedCall.Status = models.ActionCallStatusFailed
			updatedCall.Error = SanitizeActionError(executionErr, credentials)
			updatedCall.ErrorCode, updatedCall.ErrorDetails = "", nil
			var unavailable *skillbinding.PersonUnavailableError
			if errors.As(executionErr, &unavailable) {
				updatedCall.ErrorCode = skillbinding.CredentialErrorPersonUnavailable
			}
			if errors.Is(executionErr, errActionInterrupted) {
				updatedCall.ErrorCode = models.ActionErrorCodeInterrupted
			}
			var failure *skillerror.ActionError
			if errors.As(executionErr, &failure) && failure != nil {
				if safe := skillerror.NewActionError(failure.Code(), "", failure.Details()); safe != nil {
					updatedCall.Error = safe.Error()
					updatedCall.ErrorCode, updatedCall.ErrorDetails = safe.Code(), safe.Details()
				}
			}
			updatedCall.FailurePhase = models.ActionFailureBeforeDispatch
			if dispatched {
				updatedCall.FailurePhase = models.ActionFailureAfterDispatch
			}
			eventType = "action.failed"
			summary = fmt.Sprintf("Failed %s.%s after %d attempts", call.SkillID, call.Action, call.Attempt)
		}
	}
	createdFromConversation := ""
	if updatedCall.Status == models.ActionCallStatusSucceeded && isAgentBuilderApprovalAction(bound) {
		if createdFromConversation, err = createdAgentBuilderConversation(ctx, w.store, call, sourceRun); err != nil {
			return models.ActionExecutionRecord{}, err
		}
	}
	terminalOutcome := updatedCall.Status == models.ActionCallStatusSucceeded || updatedCall.Status == models.ActionCallStatusFailed
	if terminalOutcome && w.hostSummary != "" && call.Execution == models.ActionExecutionHost {
		summary = w.hostSummary
	}
	// The Run is not changed by an outcome: its loop reads the call and
	// parks at step end. Only a human intervention request is made durable
	// on the Run with the call that raised it.
	if ownsDependency && !nested && updatedCall.Status == models.ActionCallStatusSucceeded {
		if request := humanInterventionFromAction(updatedCall, now, func() string { return humanInterventionID }); request != nil {
			updatedRun = models.CloneAgentRun(sourceRun)
			updatedRun.HumanInterventions = append(updatedRun.HumanInterventions, *request)
			eventType = "action.human_intervention_required"
			summary = request.Summary
		} else if request := skillbinding.PendingCredentialRequestFromAction(ctx, w.store, updatedCall); request != nil {
			eventType = "action.credential_requested"
			summary = "Waiting for the user to save a requested credential"
		} else if request := skillbinding.PendingConnectAppRequestFromAction(ctx, w.store, updatedCall); request != nil {
			eventType = "action.connection_requested"
			summary = "Waiting for the user to connect " + request.SkillName
		}
	}
	// Cancellation before dispatch releases unused capacity. A result obtained
	// after its Run ended still settles its own reservation.
	if !nested && sourceRun.Budget != nil && (terminalOutcome || updatedCall.Status == models.ActionCallStatusCanceled) {
		if _, reserved := sourceRun.BudgetReservations[agentrun.ActionBudgetReservationID(call.ID)]; reserved {
			if updatedRun == nil {
				updatedRun = models.CloneAgentRun(sourceRun)
			}
			var budgetErr error
			if updatedCall.Status == models.ActionCallStatusCanceled {
				budgetErr = models.ReleaseRunBudgetReservation(updatedRun, agentrun.ActionBudgetReservationID(call.ID))
			} else {
				budgetErr = models.SettleRunBudgetReservation(updatedRun, agentrun.ActionBudgetReservationID(call.ID), models.BudgetUsage{Actions: 1})
			}
			if budgetErr != nil {
				return models.ActionExecutionRecord{}, budgetErr
			}
		}
	}
	if updatedRun != nil {
		updatedRun.UpdatedAt = now
		updatedRun.Revision++
	}
	event := &models.ActivityEvent{
		ID: eventID, Scope: call.Scope, EventType: eventType, Severity: models.ActivitySeverityInfo,
		AgentID: call.DeploymentID, ObjectiveID: sourceRun.ObjectiveID, TeamID: models.TeamIDForRun(sourceRun), RunID: call.RunID, TurnID: call.TurnID, Actor: models.ActivityActor{Type: "worker", ID: workerID},
		Summary: summary, Visibility: models.ActivityVisibilityScope, CausationID: call.ID, CreatedAt: now,
		Payload: map[string]interface{}{"actionCallId": call.ID, "bindingId": call.BindingID, "bindingRevision": call.BindingRevision, "skillId": call.SkillID, "skillVersion": call.SkillVersion, "action": call.Action, "status": updatedCall.Status, "attempt": updatedCall.Attempt},
	}
	addActionOrigin(event.Payload, call)
	if eventType == "action.human_intervention_required" {
		event.Severity = models.ActivitySeverityWarning
		event.Payload["humanInterventionId"] = updatedRun.HumanInterventions[len(updatedRun.HumanInterventions)-1].ID
		event.Payload["challenge"] = append([]string(nil), updatedRun.HumanInterventions[len(updatedRun.HumanInterventions)-1].Challenge...)
	}
	if updatedCall.Status == models.ActionCallStatusSucceeded || updatedCall.Status == models.ActionCallStatusFailed {
		event.UsageDelta = &models.BudgetUsage{Actions: 1}
	}
	return models.ActionExecutionRecord{
		Call: updatedCall, ExpectedCallRevision: call.Revision, Run: updatedRun, ExpectedRunRevision: sourceRun.Revision,
		WorkerID: workerID, Now: now, Event: event, CreatedFromBuilderConversationID: createdFromConversation,
	}, nil
}

func humanInterventionFromAction(call *models.ActionCall, now time.Time, newID func() string) *models.HumanInterventionRequest {
	if call == nil || call.Output == nil {
		return nil
	}
	requiresHuman, _ := call.Output["requiresHuman"].(bool)
	if !requiresHuman {
		return nil
	}
	challenge := boundedChallengeKinds(call.Output["challenges"])
	return &models.HumanInterventionRequest{
		ID: newID(), Kind: "capability_challenge", Status: models.HumanInterventionStatusPending,
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

// SanitizeActionError is err's message with every credential value redacted,
// cut to 1 KiB.
func SanitizeActionError(err error, credentials map[string]string) string {
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

// An adapter's explicit top-level failure envelope is operational evidence,
// unlike the status of a fetched resource or a nested document. Some process
// Skills use success:false plus error instead of a transport error.
func explicitActionResultFailure(output map[string]interface{}) error {
	if succeeded, ok := output["success"].(bool); ok && !succeeded {
		if message, ok := output["error"].(string); ok && strings.TrimSpace(message) != "" {
			return errors.New(strings.TrimSpace(message))
		}
		return errors.New("The tool reported that its operation failed.")
	}
	if len(output) == 1 {
		if message, ok := output["error"].(string); ok && strings.TrimSpace(message) != "" {
			return errors.New(strings.TrimSpace(message))
		}
	}
	if failed, ok := output["isError"].(bool); ok && failed {
		return errors.New("The tool reported that its operation failed.")
	}
	return nil
}
