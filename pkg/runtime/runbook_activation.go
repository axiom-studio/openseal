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
)

// RunbookDetail combines an owner-scoped activation with the exact immutable
// definition it pins. This is the canonical inspection projection used by
// interactive clients; neither the activation nor its definition is copied
// into a second lifecycle.
type RunbookDetail struct {
	Activation   *RunbookActivation          `json:"activation"`
	Definition   *runbook.Definition         `json:"definition"`
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
	report := runbook.Verify(definition.Runbook, workforceRunbookVerificationEnvironment(definition, bindings, definitions))
	return &report
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
	Revision             int64                   `json:"revision"`
	CreatedAt            time.Time               `json:"createdAt"`
	UpdatedAt            time.Time               `json:"updatedAt"`
	IdempotencyKeyHash   string                  `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint  string                  `json:"creationFingerprint,omitempty"`
}

// Callable reports whether an activation can be started on demand. A schedule
// that reached its reviewed occurrence ceiling is no longer scheduler-active,
// but its immutable reviewed operation remains safe to invoke off-cycle.
// Paused activations and explicitly retired activations without an exhausted
// schedule remain unavailable.
func (a *RunbookActivation) Callable() bool {
	if a == nil {
		return false
	}
	if a.Status == RunbookActivationActive {
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
		"definitionId": a.DefinitionID, "definitionVersion": a.DefinitionVersion, "triggerId": a.TriggerID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Runbook activation %s is required", field)
		}
	}
	if strings.TrimSpace(a.Trigger.Entrypoint) == "" {
		return errors.New("Runbook activation trigger entrypoint is required")
	}
	switch a.Trigger.Kind {
	case runbook.TriggerSchedule:
		if a.Trigger.Schedule == nil {
			return errors.New("Runbook activation schedule trigger is required")
		}
		if err := a.Trigger.Schedule.Validate(); err != nil {
			return fmt.Errorf("Runbook activation trigger: %w", err)
		}
		if strings.TrimSpace(a.Trigger.EventType) != "" {
			return errors.New("scheduled Runbook activation cannot declare an event type")
		}
	case runbook.TriggerEvent:
		if a.Trigger.Schedule != nil || !validateEventSelector(a.Trigger.EventType) || a.Trigger.EventType == "*" {
			return errors.New("event Runbook activation requires one concrete event type and no schedule")
		}
	default:
		return errors.New("Runbook activation trigger kind is invalid")
	}
	if err := validateCredentialFreeContext(a.Input); err != nil {
		return fmt.Errorf("Runbook activation input: %w", err)
	}
	if err := validateCredentialFreeContext(a.Policy); err != nil {
		return fmt.Errorf("Runbook activation policy: %w", err)
	}
	if err := a.Trigger.Evidence.Validate(); err != nil {
		return fmt.Errorf("Runbook activation evidence projection: %w", err)
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
	ExpectedRevision int64                   `json:"expectedRevision"`
	Status           RunbookActivationStatus `json:"status"`
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
	contextValues := cloneMap(activation.Input)
	if contextValues == nil {
		contextValues = make(map[string]interface{})
	}
	contextValues["runbookActivationId"] = activation.ID
	contextValues["runbookDefinitionId"] = activation.DefinitionID
	contextValues["runbookDefinitionVersion"] = activation.DefinitionVersion
	contextValues["runbookTriggerId"] = activation.TriggerID
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if activation.Trigger.Reporting != nil && idempotencyKey == "" {
		idempotencyKey = "runbook-manual:" + activation.ID + ":" + uuid.NewString()
	}
	var reportingStore ConversationStore
	if value, ok := store.(ConversationStore); ok {
		reportingStore = value
	}
	var channel *Conversation
	messageKey := ""
	if activation.Trigger.Reporting != nil {
		var err error
		channel, messageKey, err = prepareRunReporting(ctx, reportingStore, scope, activation.Owner, activation.Trigger.Reporting, runIDForIdempotencyKey(scope, idempotencyKey), contextValues)
		if err != nil {
			return nil, err
		}
	}
	result, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, ObjectiveID: objective.ID, Owner: activation.Owner, AssignedAgentID: activation.AssignedAgentID,
		Entrypoint: activation.Trigger.Entrypoint, ConcurrencyKey: "runbook:" + activation.ID,
		Goal: objective.Goal, Source: RunSourceManual, Priority: objective.Priority, Context: contextValues,
		Plan: runbookActivationPlan(activation), Policy: cloneMap(activation.Policy), Budget: cloneBudgetPolicy(activation.Budget),
		IdempotencyKey: idempotencyKey, Actor: request.Actor, Visibility: request.Visibility,
	})
	if err != nil {
		return nil, err
	}
	if err := projectRunReportingStart(ctx, reportingStore, channel, messageKey, result.Run); err != nil {
		return nil, err
	}
	return result, nil
}

func runbookActivationPlan(activation *RunbookActivation) map[string]interface{} {
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
		id = uuid.NewSHA1(uuid.NameSpaceOID, []byte(request.Scope.Kind+"\x00"+request.Scope.ID+"\x00runbook-activation\x00"+hashString(key))).String()
	}
	status := request.Status
	if status == "" {
		status = RunbookActivationActive
	}
	activation := &RunbookActivation{
		ID: id, Scope: request.Scope, Owner: request.Owner, ObjectiveID: strings.TrimSpace(request.ObjectiveID),
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
	if err := s.ensureReportingChannel(ctx, activation); err != nil {
		return nil, err
	}
	return cloneRunbookActivation(activation), nil
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
	if current.Status == RunbookActivationRetired && request.Status != RunbookActivationRetired {
		return nil, errors.New("retired Runbook activation cannot be resumed")
	}
	if current.Status == request.Status {
		if err := s.ensureReportingChannel(ctx, current); err != nil {
			return nil, err
		}
		return cloneRunbookActivation(current), nil
	}
	next := cloneRunbookActivation(current)
	next.Status = request.Status
	next.NextOccurrenceBase = nil
	next.NextRunAt = nil
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := s.store.UpdateRunbookActivation(ctx, next, current.Revision); err != nil {
		return nil, err
	}
	if err := s.ensureReportingChannel(ctx, next); err != nil {
		return nil, err
	}
	return cloneRunbookActivation(next), nil
}

func (s *RunbookActivationService) ensureReportingChannel(ctx context.Context, activation *RunbookActivation) error {
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
