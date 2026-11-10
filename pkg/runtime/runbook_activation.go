package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

var (
	ErrRunbookActivationNotFound    = errors.New("Runbook activation not found")
	ErrRunbookDefinitionNotFound    = errors.New("Runbook definition not found")
	ErrRunbookActivationInactive    = errors.New("Runbook activation is not active")
	ErrRunbookActivationRevision    = errors.New("Runbook activation revision conflict")
	ErrRunbookActivationIdempotency = errors.New("Runbook activation idempotency key was already used with different input")
	ErrRunbookActivationUnverified  = errors.New("Runbook activation verification failed")
)

// RunbookDetail combines an owner-scoped activation with the exact immutable
// definition it pins. This is the canonical inspection projection used by
// interactive clients; neither the activation nor its definition is copied
// into a second lifecycle.
type RunbookDetail struct {
	Activation   *RunbookActivation          `json:"activation"`
	Definition   *runbook.Definition         `json:"definition,omitempty"`
	Verification *runbook.VerificationReport `json:"verification,omitempty"`
}

type RunbookDefinitionCatalog interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	ListDefinitionVersions(context.Context, string) ([]*kernelagent.AgentDefinition, error)
}

type RunbookActivationReader interface {
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
}

// ResolveRunbookDetail follows the activation's tenant-scoped Agent deployment
// to find the exact historic Agent definition containing the pinned Runbook.
// It never substitutes the Agent's current version for the activation's pin.
func ResolveRunbookDetail(ctx context.Context, store RunbookActivationReader, catalog RunbookDefinitionCatalog, scope Scope, activationID string) (*RunbookDetail, error) {
	if store == nil || catalog == nil {
		return nil, errors.New("Runbook detail dependencies are not configured")
	}
	activation, err := store.GetRunbookActivation(ctx, scope, strings.TrimSpace(activationID))
	if err != nil {
		return nil, err
	}
	if activation == nil {
		return nil, ErrRunbookActivationNotFound
	}
	if activation.Task != nil {
		if err := validateScheduledTaskTarget(ctx, store, catalog, activation); err != nil {
			return nil, err
		}
		return &RunbookDetail{Activation: activation}, nil
	}
	deployment, err := catalog.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, activation.AssignedAgentID)
	if err != nil {
		return nil, err
	}
	versions, err := catalog.ListDefinitionVersions(ctx, deployment.DefinitionID)
	if err != nil {
		return nil, err
	}
	for _, definition := range versions {
		if definition == nil || definition.Runbook == nil {
			continue
		}
		if definition.Runbook.ID == activation.DefinitionID && definition.Runbook.Version == activation.DefinitionVersion {
			detail := &RunbookDetail{Activation: activation, Definition: definition.Runbook}
			if catalogStore, ok := store.(skill.CatalogStore); ok {
				detail.Verification = verifyActivatedRunbook(ctx, catalogStore, scope, deployment.ID, definition)
			}
			return detail, nil
		}
	}
	return nil, fmt.Errorf("%w: %s@%s", ErrRunbookDefinitionNotFound, activation.DefinitionID, activation.DefinitionVersion)
}

func verifyActivatedRunbook(ctx context.Context, store skill.CatalogStore, scope Scope, deploymentID string, definition *kernelagent.AgentDefinition) *runbook.VerificationReport {
	bindings, err := store.ListSkillBindings(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if err != nil {
		report := runbook.Verify(definition.Runbook, runbook.VerificationEnvironment{})
		return &report
	}
	catalog := skill.NewCatalogWithStore(store)
	definitions := make(map[string]*capability.Definition, len(bindings))
	for _, binding := range bindings {
		if binding == nil {
			continue
		}
		var resolved *skill.Definition
		if strings.TrimSpace(binding.SourceIdentity) != "" {
			resolved, err = catalog.GetDefinitionVariant(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		} else {
			resolved, err = catalog.GetDefinition(ctx, binding.SkillID, binding.SkillVersion)
		}
		if err == nil && resolved != nil {
			definitions[binding.ID] = resolved
		}
	}
	report := runbook.Verify(definition.Runbook, runbookVerificationEnvironment(definition, bindings, definitions))
	return &report
}

type runbookActivationVerificationStore interface {
	RunbookActivationReader
	RunbookDefinitionCatalog
	skill.CatalogStore
}

func verifyRunbookActivationExecution(ctx context.Context, store interface{}, scope Scope, activationID string) error {
	resolved, ok := store.(runbookActivationVerificationStore)
	if !ok {
		// Small custom stores may separate immutable Agent and Skill catalogs.
		// Their activation boundary must call runbook.Verify explicitly; the
		// bundled stores implement this complete proof path.
		return nil
	}
	detail, err := ResolveRunbookDetail(ctx, resolved, resolved, scope, activationID)
	if err != nil {
		// Low-level embedders may use the scheduling kernel without installing
		// the Agent catalog. Resolved activation verification is enforced once
		// an activation is attached to an installed Agent definition; authoring
		// readiness remains the mandatory gate for normal workforce activation.
		if errors.Is(err, kernelagent.ErrDeploymentNotFound) || errors.Is(err, ErrRunbookDefinitionNotFound) {
			return nil
		}
		return err
	}
	if detail.Verification == nil || detail.Verification.Valid {
		return nil
	}
	if len(detail.Verification.Diagnostics) == 0 {
		return ErrRunbookActivationUnverified
	}
	first := detail.Verification.Diagnostics[0]
	return fmt.Errorf("%w: %s at %s", ErrRunbookActivationUnverified, first.Message, first.Path)
}

type RunbookActivationStatus string

const (
	RunbookActivationActive  RunbookActivationStatus = "active"
	RunbookActivationPaused  RunbookActivationStatus = "paused"
	RunbookActivationRetired RunbookActivationStatus = "retired"
)

// RunbookActivation is the owner-scoped executable Runbook shown beneath one
// Objective. The immutable definition describes the method; this activation
// binds its exact trigger, target, inputs, policy, and operational limits.
// Scheduling state therefore belongs to the Runbook and never to the outcome.
type RunbookActivation struct {
	LifecycleActionID    string                  `json:"lifecycleActionId,omitempty"`
	Task                 *ScheduledAgentTask     `json:"task,omitempty"`
	ID                   string                  `json:"id"`
	Scope                Scope                   `json:"scope"`
	Owner                ObjectiveOwner          `json:"owner"`
	ObjectiveID          string                  `json:"objectiveId"`
	AssignedAgentID      string                  `json:"assignedAgentId"`
	DefinitionID         string                  `json:"definitionId"`
	DefinitionVersion    string                  `json:"definitionVersion"`
	TriggerID            string                  `json:"triggerId"`
	Trigger              runbook.Trigger         `json:"trigger"`
	Input                map[string]interface{}  `json:"input,omitempty"`
	Policy               map[string]interface{}  `json:"policy,omitempty"`
	Budget               *BudgetPolicy           `json:"budget,omitempty"`
	MaximumConcurrent    int                     `json:"maximumConcurrent,omitempty"`
	Status               RunbookActivationStatus `json:"status"`
	NextOccurrenceBase   *time.Time              `json:"nextOccurrenceBase,omitempty"`
	NextRunAt            *time.Time              `json:"nextRunAt,omitempty"`
	OccurrencesProcessed int64                   `json:"occurrencesProcessed,omitempty"`
	// PausedReason says why the system paused the routine. It is set only
	// with Status paused and cleared on any other status change.
	PausedReason RoutinePausedReason `json:"pausedReason,omitempty"`
	// Exhausted is set when the event dispatcher retires an event routine
	// because it started its maximum of real batches. It is set only with
	// Status retired; a routine the person cancelled never has it.
	Exhausted           bool      `json:"exhausted,omitempty"`
	Revision            int64     `json:"revision"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
	IdempotencyKeyHash  string    `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string    `json:"creationFingerprint,omitempty"`
}

// Callable reports whether an activation can be started on demand. A schedule
// that reached its reviewed occurrence ceiling is no longer scheduler-active,
// but its immutable reviewed operation remains safe to invoke off-cycle. An
// event Task the dispatcher retired as exhausted is callable the same way.
// Paused activations and routines the person retired remain unavailable.
func (a *RunbookActivation) Callable() bool {
	if a == nil {
		return false
	}
	if a.Status == RunbookActivationActive {
		return true
	}
	if a.Status == RunbookActivationRetired && a.Exhausted && a.Task != nil && a.Task.Event != nil && a.Trigger.Kind == runbook.TriggerEvent {
		return true
	}
	return a.Status == RunbookActivationRetired && a.Trigger.Kind == runbook.TriggerSchedule &&
		a.Trigger.Schedule != nil && a.Trigger.Schedule.MaximumOccurrences > 0 &&
		a.OccurrencesProcessed >= a.Trigger.Schedule.MaximumOccurrences
}

func (a *RunbookActivation) Validate() error {
	if a == nil {
		return errors.New("Runbook activation is required")
	}
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if err := a.Owner.Validate(); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"id": a.ID, "objectiveId": a.ObjectiveID, "assignedAgentId": a.AssignedAgentID,
		"triggerId": a.TriggerID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Runbook activation %s is required", field)
		}
	}
	if a.Task != nil {
		if err := a.Task.Validate(); err != nil {
			return err
		}
		if a.Trigger.Entrypoint != "" || a.DefinitionID != "" || a.DefinitionVersion != "" {
			return errors.New("scheduled Agent task cannot specify a Runbook definition or entrypoint")
		}
		switch a.Trigger.Kind {
		case runbook.TriggerSchedule:
			if a.Task.Event != nil {
				return errors.New("scheduled Agent task cannot have an event trigger")
			}
		case runbook.TriggerEvent:
			if a.Task.Event == nil {
				return errors.New("event Agent task requires its event trigger")
			}
			if !onlyEventTriggerKind(a.Trigger) {
				return errors.New("event Agent task trigger can only name its kind")
			}
			if strings.TrimSpace(a.Task.CreatedBy) == "" {
				return errors.New("event Agent task requires the person who created it")
			}
		default:
			return errors.New("Runbook activation trigger kind is invalid")
		}
	} else {
		if strings.TrimSpace(a.DefinitionID) == "" || strings.TrimSpace(a.DefinitionVersion) == "" || strings.TrimSpace(a.Trigger.Entrypoint) == "" {
			return errors.New("Runbook activation trigger entrypoint is required")
		}
		if a.Trigger.Kind != runbook.TriggerSchedule {
			return errors.New("Runbook definition activations can only run on a schedule")
		}
	}
	if a.Trigger.Kind == runbook.TriggerSchedule {
		if a.Trigger.Schedule == nil {
			return errors.New("Runbook activation schedule trigger is required")
		}
		if err := a.Trigger.Schedule.Validate(); err != nil {
			return fmt.Errorf("Runbook activation trigger: %w", err)
		}
	}
	if err := validateCredentialFreeContext(a.Input); err != nil {
		return fmt.Errorf("Runbook activation input: %w", err)
	}
	if err := validateCredentialFreeContext(a.Policy); err != nil {
		return fmt.Errorf("Runbook activation policy: %w", err)
	}
	if a.Budget != nil {
		if err := a.Budget.Validate(); err != nil {
			return fmt.Errorf("Runbook activation budget: %w", err)
		}
	}
	if a.MaximumConcurrent < 0 {
		return errors.New("Runbook activation maximumConcurrent cannot be negative")
	}
	if a.OccurrencesProcessed < 0 || a.Trigger.Schedule != nil && a.Trigger.Schedule.MaximumOccurrences > 0 && a.OccurrencesProcessed > a.Trigger.Schedule.MaximumOccurrences {
		return errors.New("Runbook activation occurrence progress is invalid")
	}
	switch a.Status {
	case RunbookActivationActive, RunbookActivationPaused, RunbookActivationRetired:
	default:
		return errors.New("Runbook activation status is invalid")
	}
	if a.PausedReason != "" && (a.Status != RunbookActivationPaused || !validRoutinePausedReason(a.PausedReason)) {
		return errors.New("Runbook activation paused reason is invalid")
	}
	if a.Exhausted && (a.Status != RunbookActivationRetired || a.Trigger.Kind != runbook.TriggerEvent) {
		return errors.New("only a retired event Runbook activation can be exhausted")
	}
	if a.Revision < 1 || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) {
		return errors.New("Runbook activation requires valid revision and timestamps")
	}
	if a.Trigger.Kind == runbook.TriggerEvent && (a.NextOccurrenceBase != nil || a.NextRunAt != nil) {
		return errors.New("event Runbook activation cannot have a schedule cursor")
	}
	if a.Trigger.Kind == runbook.TriggerEvent && a.OccurrencesProcessed != 0 {
		return errors.New("event Runbook activation cannot have schedule occurrence progress")
	}
	if (a.NextOccurrenceBase == nil) != (a.NextRunAt == nil) {
		return errors.New("Runbook activation schedule cursor requires both base occurrence and due time")
	}
	if a.NextOccurrenceBase != nil {
		start, end, err := a.Trigger.Schedule.Window(*a.NextOccurrenceBase)
		if err != nil || a.NextRunAt.Before(start) || a.NextRunAt.After(end) {
			return errors.New("Runbook activation nextRunAt must be inside its cron jitter window")
		}
	}
	return nil
}

// onlyEventTriggerKind reports whether an event Task's trigger names nothing
// but its kind; everything else about the event lives in Task.Event.
func onlyEventTriggerKind(t runbook.Trigger) bool {
	return t.Kind == runbook.TriggerEvent && t.Schedule == nil && t.Entrypoint == "" && t.ObjectiveID == "" &&
		len(t.Input) == 0 && t.Budget == nil && t.Reporting == nil && t.MaximumConcurrent == 0
}

func validRoutinePausedReason(reason RoutinePausedReason) bool {
	switch reason {
	case RoutinePausedNeedsSignIn, RoutinePausedSourceRemoved, RoutinePausedUnverified:
		return true
	}
	return false
}

type RunbookActivationFilter struct {
	Scope        Scope
	Owner        *ObjectiveOwner
	ObjectiveID  string
	Statuses     []RunbookActivationStatus
	TriggerKinds []runbook.TriggerKind
	Limit        int
	Offset       int
}

type RunbookActivationStore interface {
	CreateRunbookActivation(context.Context, *RunbookActivation) error
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
	ListRunbookActivations(context.Context, RunbookActivationFilter) ([]*RunbookActivation, error)
	UpdateRunbookActivation(context.Context, *RunbookActivation, int64) error
	ListRunbookActivationScopes(context.Context) ([]Scope, error)
}

type CreateRunbookActivationRequest struct {
	Task              *ScheduledAgentTask
	ID                string
	Scope             Scope
	Owner             ObjectiveOwner
	ObjectiveID       string
	AssignedAgentID   string
	DefinitionID      string
	DefinitionVersion string
	TriggerID         string
	Trigger           runbook.Trigger
	Input             map[string]interface{}
	Policy            map[string]interface{}
	Budget            *BudgetPolicy
	MaximumConcurrent int
	Status            RunbookActivationStatus
	IdempotencyKey    string
}

// UpdateRunbookActivationRequest changes the operational lifecycle of one
// Objective-owned Runbook. Definition, trigger, authority, and input changes
// require a newly reviewed activation; this command intentionally changes only
// whether new Runs may be created.
type UpdateRunbookActivationRequest struct {
	ActionID string `json:"-"`
	// CreatedBy records the person resuming a routine that has no creator
	// yet; the kernel sets it, never a client.
	CreatedBy        string                  `json:"-"`
	ExpectedRevision int64                   `json:"expectedRevision"`
	Status           RunbookActivationStatus `json:"status"`
	// PausedReason is set only by system pauses, with Status paused; the
	// kernel sets it, never a client. A person's pause or resume leaves it
	// empty, which clears any earlier reason.
	PausedReason RoutinePausedReason `json:"-"`
	// Exhausted is set only by the event dispatcher, with Status retired,
	// when the routine started its maximum of real batches.
	Exhausted bool `json:"-"`
}

type StartRunbookActivationRequest struct {
	IdempotencyKey string             `json:"idempotencyKey,omitempty"`
	Actor          ActivityActor      `json:"actor,omitempty"`
	Visibility     ActivityVisibility `json:"visibility,omitempty"`
}

// StartRunbookActivation creates one manual Run from the exact reviewed
// activation. Clients never manufacture the definition pin or execution
// policy themselves.
func StartRunbookActivation(ctx context.Context, store KernelStore, scope Scope, activationID string, request StartRunbookActivationRequest) (*AgentRunCommandResult, error) {
	return StartRunbookActivationWithExecutionPreparer(ctx, store, scope, activationID, request, nil)
}

// StartRunbookActivationWithExecutionPreparer adds the host's authoritative
// immutable execution snapshot to newly accepted manual work. Delivery replay
// uses the accepted request before resolving a potentially changed deployment.
func StartRunbookActivationWithExecutionPreparer(ctx context.Context, store KernelStore, scope Scope, activationID string, request StartRunbookActivationRequest, preparer AcceptedRunExecutionPreparer) (*AgentRunCommandResult, error) {
	if store == nil {
		return nil, errors.New("Runbook activation store is not configured")
	}
	activation, err := store.GetRunbookActivation(ctx, scope, strings.TrimSpace(activationID))
	if err != nil {
		return nil, err
	}
	if activation == nil {
		return nil, ErrRunbookActivationNotFound
	}
	if !activation.Callable() {
		return nil, ErrRunbookActivationInactive
	}
	if err := verifyRunbookActivationExecution(ctx, store, scope, activation.ID); err != nil {
		return nil, err
	}
	objective, err := store.GetObjective(ctx, scope, activation.ObjectiveID)
	if err != nil {
		return nil, err
	}
	if objective == nil {
		return nil, ErrObjectiveNotFound
	}
	if objective.Status != ObjectiveStatusActive || objective.Owner != activation.Owner {
		return nil, errors.New("Runbook activation requires its active owning Objective")
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if (activation.Trigger.Reporting != nil || activation.Task != nil) && idempotencyKey == "" {
		idempotencyKey = "runbook-manual:" + activation.ID + ":" + uuid.NewString()
	}
	var reportingStore ConversationStore
	if value, ok := store.(ConversationStore); ok {
		reportingStore = value
	}
	result, err := startActivationOccurrence(ctx, store, reportingStore, activation, objective, RunSourceManual, idempotencyKey, nil, request.Actor, request.Visibility, preparer)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// startActivationOccurrence is the one place a routine occurrence Run is
// created: by the scheduler, by a manual start and by the event dispatcher.
// Only a definition activation (Task == nil) carries the entrypoint, plan and
// definition keys; Task occurrences never do.
func startActivationOccurrence(
	ctx context.Context,
	store RunCommandStore,
	reportingStore ConversationStore,
	activation *RunbookActivation,
	objective *Objective,
	source RunSource,
	idempotencyKey string,
	extraContext map[string]interface{},
	actor ActivityActor,
	visibility ActivityVisibility,
	preparer AcceptedRunExecutionPreparer,
) (*AgentRunCommandResult, error) {
	contextValues := cloneMap(activation.Input)
	if contextValues == nil {
		contextValues = make(map[string]interface{})
	}
	for key, value := range extraContext {
		contextValues[key] = value
	}
	contextValues["runbookActivationId"] = activation.ID
	contextValues["runbookTriggerId"] = activation.TriggerID
	request := CreateAgentRunRequest{
		Scope: activation.Scope, ObjectiveID: objective.ID, Owner: activation.Owner, AssignedAgentID: activation.AssignedAgentID,
		ActingUser: activationActingUser(activation), ConcurrencyKey: "runbook:" + activation.ID,
		Goal: activationGoal(activation, objective), Source: source, Priority: objective.Priority, Context: contextValues,
		Policy: cloneMap(activation.Policy), Budget: cloneBudgetPolicy(activation.Budget),
		IdempotencyKey: idempotencyKey, Actor: actor, Visibility: visibility,
	}
	if activation.Task == nil {
		request.Entrypoint = activation.Trigger.Entrypoint
		request.Plan = runbookActivationPlan(activation)
		contextValues["runbookDefinitionId"] = activation.DefinitionID
		contextValues["runbookDefinitionVersion"] = activation.DefinitionVersion
	}
	var channel *Conversation
	messageKey := ""
	if activation.Trigger.Reporting != nil || activation.Task != nil {
		var err error
		channel, messageKey, err = prepareActivationReporting(ctx, reportingStore, activation, runIDForIdempotencyKey(activation.Scope, idempotencyKey), contextValues)
		if err != nil {
			return nil, fmt.Errorf("prepare Runbook activation %s reporting: %w", activation.ID, err)
		}
	}
	created, err := createAgentRunWithAcceptedExecution(ctx, NewRunCommandService(store), request, preparer)
	if err != nil {
		return nil, err
	}
	if err := projectRunReportingStart(ctx, reportingStore, channel, messageKey, created.Run); err != nil {
		return nil, fmt.Errorf("project Runbook activation %s start: %w", activation.ID, err)
	}
	return created, nil
}

// activationBackpressured reports whether the activation already has its
// maximum of non-terminal Runs (default 1). The scheduler and the event
// dispatcher share this rule.
func activationBackpressured(ctx context.Context, store interface {
	ListAgentRuns(context.Context, AgentRunFilter) ([]*AgentRun, error)
}, activation *RunbookActivation) (bool, error) {
	maximum := activation.MaximumConcurrent
	if maximum == 0 {
		maximum = 1
	}
	runs, err := store.ListAgentRuns(ctx, AgentRunFilter{
		Scope: activation.Scope, ObjectiveID: activation.ObjectiveID,
		// Every non-terminal status, paused included: a paused occurrence
		// may resume, and the tenant cap counts it too.
		Statuses: []AgentRunStatus{
			AgentRunStatusQueued, AgentRunStatusPlanning, AgentRunStatusRunning, AgentRunStatusPaused, AgentRunStatusSleeping,
			AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent, AgentRunStatusWaitingForApproval,
			AgentRunStatusWaitingForEvent,
		},
		Limit: 500,
	})
	if err != nil {
		return false, err
	}
	count := 0
	for _, run := range runs {
		if id, _ := run.Context["runbookActivationId"].(string); strings.TrimSpace(id) == activation.ID {
			count++
		}
	}
	return count >= maximum, nil
}

// activationActingUser is the person a routine's occurrences act for: the
// person who created it in a chat. Activations without a task act for no one.
func activationActingUser(activation *RunbookActivation) string {
	if activation == nil || activation.Task == nil {
		return ""
	}
	return activation.Task.CreatedBy
}

func runbookActivationPlan(activation *RunbookActivation) map[string]interface{} {
	if activation.Task != nil {
		return nil
	}
	return map[string]interface{}{"runbook": map[string]interface{}{
		"id": activation.DefinitionID, "version": activation.DefinitionVersion, "trigger": activation.TriggerID,
	}}
}

type RunbookActivationService struct {
	store interface {
		RunbookActivationStore
		PortfolioStore
	}
	now func() time.Time
}

func NewRunbookActivationService(store interface {
	RunbookActivationStore
	PortfolioStore
}) *RunbookActivationService {
	return &RunbookActivationService{store: store, now: time.Now}
}

func (s *RunbookActivationService) Create(ctx context.Context, request CreateRunbookActivationRequest) (*RunbookActivation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("Runbook activation store is not configured")
	}
	objective, err := s.store.GetObjective(ctx, request.Scope, strings.TrimSpace(request.ObjectiveID))
	if err != nil {
		return nil, err
	}
	if objective == nil {
		return nil, ErrObjectiveNotFound
	}
	if objective.Owner != request.Owner {
		return nil, errors.New("Runbook activation owner must match its Objective owner")
	}
	now := s.now().UTC()
	id := strings.TrimSpace(request.ID)
	if id == "" {
		id = uuid.NewString()
	}
	key := strings.TrimSpace(request.IdempotencyKey)
	if len(key) > 256 {
		return nil, fmt.Errorf("%w: idempotency key cannot exceed 256 characters", ErrRunbookActivationIdempotency)
	}
	if key != "" && strings.TrimSpace(request.ID) == "" {
		id = RunbookActivationIDForKey(request.Scope, key)
	}
	status := request.Status
	if status == "" {
		status = RunbookActivationActive
	}
	activation := &RunbookActivation{
		Task: cloneScheduledAgentTask(request.Task), ID: id, Scope: request.Scope, Owner: request.Owner, ObjectiveID: strings.TrimSpace(request.ObjectiveID),
		AssignedAgentID: strings.TrimSpace(request.AssignedAgentID), DefinitionID: strings.TrimSpace(request.DefinitionID),
		DefinitionVersion: strings.TrimSpace(request.DefinitionVersion), TriggerID: strings.TrimSpace(request.TriggerID), Trigger: request.Trigger,
		Input: cloneMap(request.Input), Policy: cloneMap(request.Policy), Budget: cloneBudgetPolicy(request.Budget),
		MaximumConcurrent: request.MaximumConcurrent, Status: status, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if key != "" {
		activation.IdempotencyKeyHash = hashString(key)
		activation.CreationFingerprint = fingerprintRunbookActivation(activation)
		current, getErr := s.store.GetRunbookActivation(ctx, request.Scope, id)
		if getErr != nil {
			return nil, getErr
		}
		if current != nil {
			if current.CreationFingerprint != activation.CreationFingerprint {
				return nil, ErrRunbookActivationIdempotency
			}
			if err := s.ensureReportingChannel(ctx, current); err != nil {
				return nil, err
			}
			return current, nil
		}
	}
	if activation.Task != nil && activation.Status == RunbookActivationActive && activation.Trigger.Kind == runbook.TriggerSchedule {
		base, err := activation.Trigger.Schedule.NextBase(now)
		if err != nil {
			return nil, err
		}
		due, err := activation.Trigger.Schedule.DueAt(runbookTriggerKey(activation), base)
		if err != nil {
			return nil, err
		}
		activation.NextOccurrenceBase, activation.NextRunAt = &base, &due
	}
	if err := activation.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateRunbookActivation(ctx, activation); err != nil {
		// Idempotent creates can race after the pre-insert lookup. Re-read the
		// deterministic resource and converge when the winning request persisted
		// the same reviewed contract. A different contract remains a conflict.
		if key != "" {
			current, getErr := s.store.GetRunbookActivation(ctx, request.Scope, id)
			if getErr != nil {
				return nil, getErr
			}
			if current != nil {
				if current.CreationFingerprint != activation.CreationFingerprint {
					return nil, ErrRunbookActivationIdempotency
				}
				if err := s.ensureReportingChannel(ctx, current); err != nil {
					return nil, err
				}
				return current, nil
			}
		}
		return nil, err
	}
	notifyRoutineActivationWrite(ctx, s.store, nil, activation)
	if err := s.ensureReportingChannel(ctx, activation); err != nil {
		return nil, err
	}
	return cloneRunbookActivation(activation), nil
}

// RunbookActivationIDForKey is the activation ID Create assigns to a request
// with this idempotency key and no explicit ID, so callers can know it first.
func RunbookActivationIDForKey(scope Scope, idempotencyKey string) string {
	key := strings.TrimSpace(idempotencyKey)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(scope.Kind+"\x00"+scope.ID+"\x00runbook-activation\x00"+hashString(key))).String()
}

func (s *RunbookActivationService) Update(ctx context.Context, scope Scope, id string, request UpdateRunbookActivationRequest) (*RunbookActivation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("Runbook activation store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if request.ExpectedRevision < 1 {
		return nil, ErrRunbookActivationRevision
	}
	current, err := s.store.GetRunbookActivation(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrRunbookActivationNotFound
	}
	if current.Revision != request.ExpectedRevision {
		return nil, ErrRunbookActivationRevision
	}
	switch request.Status {
	case RunbookActivationActive, RunbookActivationPaused, RunbookActivationRetired:
	default:
		return nil, errors.New("Runbook activation status is invalid")
	}
	if request.PausedReason != "" && (request.Status != RunbookActivationPaused || !validRoutinePausedReason(request.PausedReason)) {
		return nil, errors.New("Runbook activation paused reason is invalid")
	}
	if request.Exhausted && (request.Status != RunbookActivationRetired || current.Trigger.Kind != runbook.TriggerEvent) {
		return nil, errors.New("only a retired event Runbook activation can be exhausted")
	}
	if current.Status == RunbookActivationRetired && request.Status != RunbookActivationRetired {
		return nil, errors.New("retired Runbook activation cannot be resumed")
	}
	// A system pause of a routine that is already paused still records why.
	reasonChanged := current.Status == RunbookActivationPaused && request.PausedReason != "" && request.PausedReason != current.PausedReason
	if current.Status == request.Status && !reasonChanged {
		if err := s.ensureReportingChannel(ctx, current); err != nil {
			return nil, err
		}
		return cloneRunbookActivation(current), nil
	}
	next := cloneRunbookActivation(current)
	next.Status = request.Status
	next.PausedReason = request.PausedReason
	next.Exhausted = request.Exhausted
	next.LifecycleActionID = request.ActionID
	if next.Task != nil && next.Task.CreatedBy == "" && request.CreatedBy != "" {
		next.Task.CreatedBy = request.CreatedBy
	}
	next.NextOccurrenceBase = nil
	next.NextRunAt = nil
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := s.store.UpdateRunbookActivation(ctx, next, current.Revision); err != nil {
		return nil, err
	}
	notifyRoutineActivationWrite(ctx, s.store, current, next)
	if err := s.ensureReportingChannel(ctx, next); err != nil {
		return nil, err
	}
	return cloneRunbookActivation(next), nil
}

func (s *RunbookActivationService) ensureReportingChannel(ctx context.Context, activation *RunbookActivation) error {
	if activation != nil && activation.Task != nil && activation.Status == RunbookActivationActive {
		store, ok := s.store.(ConversationStore)
		if !ok {
			return errors.New("scheduled task reporting requires a conversation store")
		}
		_, err := scheduledTaskConversation(ctx, store, activation.Scope, activation.Owner, activation.Task)
		return err
	}
	if activation == nil || activation.Status != RunbookActivationActive || activation.Trigger.Reporting == nil {
		return nil
	}
	store, ok := s.store.(ConversationStore)
	if !ok {
		return errors.New("active Runbook reporting requires a conversation store")
	}
	_, err := ensureRunReportingChannel(ctx, store, activation.Scope, activation.Owner, activation.Trigger.Reporting)
	return err
}

func matchesRunbookActivationFilter(value *RunbookActivation, filter RunbookActivationFilter) bool {
	if value == nil || value.Scope != filter.Scope {
		return false
	}
	if filter.Owner != nil && value.Owner != *filter.Owner {
		return false
	}
	if strings.TrimSpace(filter.ObjectiveID) != "" && value.ObjectiveID != strings.TrimSpace(filter.ObjectiveID) {
		return false
	}
	if len(filter.Statuses) > 0 {
		matched := false
		for _, status := range filter.Statuses {
			matched = matched || value.Status == status
		}
		if !matched {
			return false
		}
	}
	if len(filter.TriggerKinds) > 0 {
		matched := false
		for _, kind := range filter.TriggerKinds {
			matched = matched || value.Trigger.Kind == kind
		}
		if !matched {
			return false
		}
	}
	return true
}

func sortAndLimitRunbookActivations(values []*RunbookActivation, limit, offset int) []*RunbookActivation {
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*RunbookActivation{}
	}
	values = values[offset:]
	if len(values) > limit {
		values = values[:limit]
	}
	return values
}

func fingerprintRunbookActivation(value *RunbookActivation) string {
	probe := *value
	probe.CreatedAt, probe.UpdatedAt, probe.IdempotencyKeyHash, probe.CreationFingerprint = time.Time{}, time.Time{}, "", ""
	probe.Revision = 0
	encoded, _ := json.Marshal(probe)
	return hashString(string(encoded))
}

func cloneRunbookActivation(value *RunbookActivation) *RunbookActivation {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Task = cloneScheduledAgentTask(value.Task)
	clone.Input = cloneMap(value.Input)
	clone.Policy = cloneMap(value.Policy)
	clone.Budget = cloneBudgetPolicy(value.Budget)
	if value.NextOccurrenceBase != nil {
		base := *value.NextOccurrenceBase
		clone.NextOccurrenceBase = &base
	}
	if value.NextRunAt != nil {
		due := *value.NextRunAt
		clone.NextRunAt = &due
	}
	return &clone
}

func runbookVerificationEnvironment(definition *kernelagent.AgentDefinition, bindings []*capability.Binding, definitions map[string]*capability.Definition) runbook.VerificationEnvironment {
	environment := runbook.VerificationEnvironment{}
	if definition == nil {
		return environment
	}
	approvalRoutes := len(definition.Authority.ApprovalDestinations) > 0
	standing := make(map[string]bool, len(definition.Authority.StandingGrants))
	for _, grant := range definition.Authority.StandingGrants {
		// A destination-scoped external grant depends on runtime arguments and
		// therefore cannot statically bypass approval. An unconstrained exact
		// action grant can.
		if strings.TrimSpace(grant.ExternalOperation) == "" && strings.TrimSpace(grant.ResourcePrefix) == "" {
			standing[grant.SkillID+"\x00"+grant.Action] = true
		}
	}
	for _, binding := range bindings {
		if binding == nil {
			continue
		}
		skillDefinition := definitions[binding.ID]
		if skillDefinition == nil {
			continue
		}
		allowed := make(map[string]bool, len(binding.AllowedActions))
		for _, action := range binding.AllowedActions {
			allowed[action] = true
		}
		actionNames := make([]string, 0, len(skillDefinition.Actions))
		for actionName := range skillDefinition.Actions {
			actionNames = append(actionNames, actionName)
		}
		sort.Strings(actionNames)
		for _, actionName := range actionNames {
			action := canonicalActionReview(skillDefinition, skillDefinition.Actions[actionName])
			requiresApproval := runbookActionRequiresApproval(definition, binding.SkillID, actionName, action)
			environment.Actions = append(environment.Actions, runbook.ResolvedAction{
				SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity,
				Action: actionName, BindingID: binding.ID, BindingRevision: binding.Revision,
				Enabled: !binding.Disabled, Allowed: allowed[actionName], MaximumRisk: binding.MaximumRisk,
				Risk: action.Risk, SideEffect: action.SideEffect, Idempotency: action.Idempotency,
				RequiredCredentials: append([]capability.CredentialRequirement(nil), action.Credentials...),
				BoundCredentials:    cloneCredentialReferences(binding.Credentials), CompensationAction: action.CompensationAction,
				FinalizerAction: action.FinalizerAction, RequiresApproval: requiresApproval,
				ApprovalRoutePresent: !requiresApproval || approvalRoutes || standing[binding.SkillID+"\x00"+actionName] && !action.AlwaysReview(),
			})
		}
	}
	return environment
}

func runbookActionRequiresApproval(definition *kernelagent.AgentDefinition, skillID, actionName string, action capability.Action) bool {
	if definition == nil || action.SideEffect == capability.SideEffectNone || action.SideEffect == capability.SideEffectRead {
		return false
	}
	if action.AlwaysReview() {
		return true
	}
	for _, grant := range definition.Authority.StandingGrants {
		if grant.SkillID == skillID && grant.Action == actionName && strings.TrimSpace(grant.ExternalOperation) == "" && strings.TrimSpace(grant.ResourcePrefix) == "" {
			return false
		}
	}
	threshold := definition.Authority.RequireApprovalAt
	if threshold == "" {
		return true
	}
	return runbookRiskRank(action.Risk) >= runbookRiskRank(threshold)
}

func runbookRiskRank(risk capability.RiskLevel) int {
	switch risk {
	case capability.RiskLevelRead:
		return 1
	case capability.RiskLevelWrite:
		return 2
	case capability.RiskLevelExternal:
		return 3
	case capability.RiskLevelProduction:
		return 4
	case capability.RiskLevelDestructive:
		return 5
	default:
		return 0
	}
}
