package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/google/uuid"
)

var (
	ErrRunbookActivationNotFound    = errors.New("Runbook activation not found")
	ErrRunbookActivationRevision    = errors.New("Runbook activation revision conflict")
	ErrRunbookActivationIdempotency = errors.New("Runbook activation idempotency key was already used with different input")
)

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
	ID                  string                  `json:"id"`
	Scope               Scope                   `json:"scope"`
	Owner               ObjectiveOwner          `json:"owner"`
	ObjectiveID         string                  `json:"objectiveId"`
	AssignedAgentID     string                  `json:"assignedAgentId"`
	DefinitionID        string                  `json:"definitionId"`
	DefinitionVersion   string                  `json:"definitionVersion"`
	TriggerID           string                  `json:"triggerId"`
	Trigger             runbook.Trigger         `json:"trigger"`
	Input               map[string]interface{}  `json:"input,omitempty"`
	Policy              map[string]interface{}  `json:"policy,omitempty"`
	Budget              *BudgetPolicy           `json:"budget,omitempty"`
	MaximumConcurrent   int                     `json:"maximumConcurrent,omitempty"`
	Status              RunbookActivationStatus `json:"status"`
	NextOccurrenceBase  *time.Time              `json:"nextOccurrenceBase,omitempty"`
	NextRunAt           *time.Time              `json:"nextRunAt,omitempty"`
	Revision            int64                   `json:"revision"`
	CreatedAt           time.Time               `json:"createdAt"`
	UpdatedAt           time.Time               `json:"updatedAt"`
	IdempotencyKeyHash  string                  `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string                  `json:"creationFingerprint,omitempty"`
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
	if a.Trigger.Kind != runbook.TriggerSchedule {
		return errors.New("Runbook activation currently requires a schedule trigger")
	}
	if a.Trigger.Schedule == nil {
		return errors.New("Runbook activation schedule trigger is required")
	}
	if err := a.Trigger.Schedule.Validate(); err != nil {
		return fmt.Errorf("Runbook activation trigger: %w", err)
	}
	if strings.TrimSpace(a.Trigger.Entrypoint) == "" {
		return errors.New("Runbook activation trigger entrypoint is required")
	}
	if strings.TrimSpace(a.Trigger.EventType) != "" {
		return errors.New("scheduled Runbook activation cannot declare an event type")
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
	switch a.Status {
	case RunbookActivationActive, RunbookActivationPaused, RunbookActivationRetired:
	default:
		return errors.New("Runbook activation status is invalid")
	}
	if a.Revision < 1 || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) {
		return errors.New("Runbook activation requires valid revision and timestamps")
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
	Scope       Scope
	Owner       *ObjectiveOwner
	ObjectiveID string
	Statuses    []RunbookActivationStatus
	Limit       int
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
			return current, nil
		}
	}
	if err := activation.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateRunbookActivation(ctx, activation); err != nil {
		return nil, err
	}
	return cloneRunbookActivation(activation), nil
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
	return true
}

func sortAndLimitRunbookActivations(values []*RunbookActivation, limit int) []*RunbookActivation {
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	if limit <= 0 || limit > 500 {
		limit = 100
	}
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
