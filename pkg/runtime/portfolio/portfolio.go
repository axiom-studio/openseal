package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

var (
	ErrInvalidObjectiveTransition = errors.New("invalid objective transition")
	ErrInvalidRunCommand          = errors.New("invalid run command")
)

type CreateObjectiveRequest struct {
	Scope           models.Scope
	Owner           models.ObjectiveOwner
	Title           string
	Goal            string
	Status          models.ObjectiveStatus
	Priority        int
	ExecutionPolicy *models.ObjectiveExecutionPolicy
	Budget          *models.BudgetPolicy
	Constraints     map[string]interface{}
	SuccessCriteria map[string]interface{}
	IdempotencyKey  string
	Actor           models.ActivityActor
	Visibility      models.ActivityVisibility
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64
	Title            *string
	Goal             *string
	Status           *models.ObjectiveStatus
	Priority         *int
	ExecutionPolicy  *models.ObjectiveExecutionPolicy
	Budget           *models.BudgetPolicy
	Constraints      map[string]interface{}
	SuccessCriteria  map[string]interface{}
	ProgressSummary  *string
	Actor            models.ActivityActor
	Visibility       models.ActivityVisibility
	Summary          string
}

type Service struct {
	store models.PortfolioStore
	now   func() time.Time
}

type CreateObjectiveResult struct {
	Objective *models.Objective `json:"objective"`
	Created   bool              `json:"created"`
}

func NewService(store models.PortfolioStore, opts ...models.ServiceOption) *Service {
	return &Service{store: store, now: models.ClockFrom(opts)}
}

func (s *Service) CreateObjective(ctx context.Context, req CreateObjectiveRequest) (*models.Objective, error) {
	result, err := s.CreateObjectiveIdempotent(ctx, req)
	if err != nil {
		return nil, err
	}
	return result.Objective, nil
}

func (s *Service) CreateObjectiveIdempotent(ctx context.Context, req CreateObjectiveRequest) (*CreateObjectiveResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	fingerprint, err := objectiveCreationFingerprint(req)
	if err != nil {
		return nil, err
	}
	now := s.now()
	status := req.Status
	if status == "" {
		status = models.ObjectiveStatusDraft
	}
	objectiveID := uuid.NewString()
	key := strings.TrimSpace(req.IdempotencyKey)
	if len(key) > 256 {
		return nil, fmt.Errorf("%w: idempotency key cannot exceed 256 characters", models.ErrObjectiveIdempotency)
	}
	if key != "" {
		objectiveID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(req.Scope.Kind+"\x00"+req.Scope.ID+"\x00"+models.SHA256DigestString(key))).String()
		current, getErr := s.store.GetObjective(ctx, req.Scope, objectiveID)
		if getErr != nil {
			return nil, getErr
		}
		if current != nil {
			if current.CreationFingerprint != fingerprint {
				return nil, models.ErrObjectiveIdempotency
			}
			return &CreateObjectiveResult{Objective: current, Created: false}, nil
		}
	}
	objective := &models.Objective{
		ID: objectiveID, Scope: req.Scope, Owner: req.Owner, Title: req.Title,
		Goal: req.Goal, Status: status, Priority: req.Priority,
		ExecutionPolicy: cloneObjectiveExecutionPolicy(req.ExecutionPolicy), Budget: models.CloneBudgetPolicy(req.Budget), Constraints: req.Constraints, SuccessCriteria: req.SuccessCriteria,
		Revision: 1, CreatedAt: now, UpdatedAt: now, CreationFingerprint: fingerprint,
	}
	if key != "" {
		objective.IdempotencyKeyHash = models.SHA256DigestString(key)
	}
	if err := objective.Validate(); err != nil {
		return nil, err
	}
	persistObjective := func() error {
		activityStore, ok := s.store.(models.ObjectiveActivityStore)
		if !ok {
			return s.store.CreateObjective(ctx, objective)
		}
		_, persistErr := activityStore.CreateObjectiveWithEvent(ctx, objective, models.ObjectiveActivityEvent(objective, req.Actor, req.Visibility, "objective.created", "Objective created", now, map[string]interface{}{"status": objective.Status}))
		return persistErr
	}
	if err := persistObjective(); err != nil {
		if key != "" {
			current, getErr := s.store.GetObjective(ctx, req.Scope, objectiveID)
			if getErr == nil && current != nil {
				if current.CreationFingerprint != fingerprint {
					return nil, models.ErrObjectiveIdempotency
				}
				return &CreateObjectiveResult{Objective: current, Created: false}, nil
			}
		}
		return nil, err
	}
	return &CreateObjectiveResult{Objective: objective, Created: true}, nil
}

func (s *Service) UpdateObjective(ctx context.Context, scope models.Scope, objectiveID string, req UpdateObjectiveRequest) (*models.Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	current, err := s.store.GetObjective(ctx, scope, objectiveID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, models.ErrObjectiveNotFound
	}
	if req.ExpectedRevision != current.Revision {
		return nil, models.ErrRevisionConflict
	}
	if req.Status != nil && *req.Status != current.Status && !models.CanTransitionObjective(current.Status, *req.Status) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidObjectiveTransition, current.Status, *req.Status)
	}
	previousStatus := current.Status
	ApplyObjectiveUpdate(current, req)
	current.UpdatedAt = s.now()
	current.Revision++
	if err := current.Validate(); err != nil {
		return nil, err
	}
	persistUpdate := func() error {
		activityStore, ok := s.store.(models.ObjectiveActivityStore)
		if !ok {
			return s.store.UpdateObjective(ctx, current, req.ExpectedRevision)
		}
		eventType, summary := "objective.updated", strings.TrimSpace(req.Summary)
		if req.Status != nil && current.Status != previousStatus {
			eventType = "objective.status_changed"
			if summary == "" {
				summary = fmt.Sprintf("Objective changed from %s to %s", previousStatus, current.Status)
			}
		}
		if summary == "" {
			summary = "Objective updated"
		}
		_, persistErr := activityStore.UpdateObjectiveWithEvent(ctx, current, req.ExpectedRevision, models.ObjectiveActivityEvent(current, req.Actor, req.Visibility, eventType, summary, current.UpdatedAt, map[string]interface{}{"previousStatus": previousStatus, "status": current.Status, "revision": current.Revision}))
		return persistErr
	}
	if err := persistUpdate(); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *Service) GetObjective(ctx context.Context, scope models.Scope, objectiveID string) (*models.Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	objective, err := s.store.GetObjective(ctx, scope, objectiveID)
	if err != nil {
		return nil, err
	}
	if objective == nil {
		return nil, models.ErrObjectiveNotFound
	}
	return objective, nil
}

func (s *Service) ListObjectives(ctx context.Context, filter models.ObjectiveFilter) ([]*models.Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListObjectives(ctx, filter)
}

func (s *Service) CreateAgentRun(ctx context.Context, req models.CreateAgentRunRequest) (*models.AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	run, err := BuildAgentRun(ctx, s.store, req, uuid.NewString(), s.now())
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateAgentRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

func BuildAgentRun(ctx context.Context, store models.PortfolioStore, req models.CreateAgentRunRequest, runID string, now time.Time) (*models.AgentRun, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", models.ErrInvalidAgentRun, err)
	}
	if err := req.Owner.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", models.ErrInvalidAgentRun, err)
	}
	for _, state := range []map[string]interface{}{req.Context, req.Plan, req.Checkpoint, req.Policy} {
		if err := models.ValidateCredentialFreeContext(state); err != nil {
			return nil, fmt.Errorf("%w: %w", models.ErrInvalidAgentRun, err)
		}
	}
	if req.ObjectiveID != "" {
		objective, err := store.GetObjective(ctx, req.Scope, req.ObjectiveID)
		if err != nil {
			return nil, err
		}
		if objective == nil {
			return nil, models.ErrObjectiveNotFound
		}
		if req.ParentRunID == "" {
			if err := models.ValidateObjectiveRunBudget(objective, req.Budget); err != nil {
				return nil, err
			}
		}
	}
	availableAt := now
	if req.AvailableAt != nil {
		availableAt = *req.AvailableAt
	}
	rootID := runID
	runContext := req.Context
	if req.ParentRunID != "" {
		parent, err := store.GetAgentRun(ctx, req.Scope, req.ParentRunID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, models.ErrRunNotFound
		}
		rootID = parent.RootRunID
		runContext = models.InheritRoutineEventBatch(parent, maps.Clone(req.Context))
	}
	source := req.Source
	if source == "" {
		source = models.RunSourceManual
	}
	switch source {
	case models.RunSourceManual, models.RunSourceChat, models.RunSourceSchedule, models.RunSourceEvent, models.RunSourceWebhook, models.RunSourceRequest, models.RunSourceRequestDecision, models.RunSourceCompletionReview, models.RunSourceHandoff, models.RunSourceEscalation, models.RunSourceObjective, models.RunSourceFork:
	default:
		return nil, fmt.Errorf("%w: unsupported run source %q", models.ErrInvalidAgentRun, source)
	}
	kind := models.NormalizeRunKind(req.Kind)
	if !models.IsValidRunKind(kind) {
		return nil, fmt.Errorf("%w: unsupported run kind %q", models.ErrInvalidAgentRun, req.Kind)
	}
	run := &models.AgentRun{
		ID: runID, Kind: kind, Scope: req.Scope, ObjectiveID: req.ObjectiveID,
		ParentRunID: req.ParentRunID, RootRunID: rootID, Owner: req.Owner,
		AssignedAgentID: req.AssignedAgentID, ActingUser: req.ActingUser, Entrypoint: strings.TrimSpace(req.Entrypoint), ConcurrencyKey: strings.TrimSpace(req.ConcurrencyKey),
		ResourceRequirements: CloneResourceQuantities(req.ResourceRequirements),
		Goal:                 req.Goal, Source: source,
		Status: models.AgentRunStatusQueued, Priority: req.Priority, Deadline: req.Deadline,
		AvailableAt: availableAt, QueueEnteredAt: now, Context: runContext,
		Plan: req.Plan, Checkpoint: req.Checkpoint, WakeCondition: req.WakeCondition,
		Budget: models.CloneBudgetPolicy(req.Budget), Policy: req.Policy, Revision: 1, CreatedAt: now, UpdatedAt: now,
		SessionID: req.SessionID, Engine: models.RunEngineLoop,
	}
	if run.Budget != nil {
		run.BudgetState = models.BudgetStateActive
	}
	if err := run.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", models.ErrInvalidAgentRun, err)
	}
	return run, nil
}

func CloneResourceQuantities(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	cloned := make(map[string]int, len(values))
	for name, quantity := range values {
		cloned[name] = quantity
	}
	return cloned
}

func (s *Service) GetAgentRun(ctx context.Context, scope models.Scope, runID string) (*models.AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	run, err := s.store.GetAgentRun(ctx, scope, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, models.ErrRunNotFound
	}
	return run, nil
}

func (s *Service) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.Kind != "" && !models.IsValidRunKind(filter.Kind) {
		return nil, fmt.Errorf("%w: unsupported run kind %q", models.ErrInvalidAgentRun, filter.Kind)
	}
	if filter.Order != "" && filter.Order != models.AgentRunOrderScheduler && filter.Order != models.AgentRunOrderCreatedDesc {
		return nil, fmt.Errorf("%w: unsupported run order %q", models.ErrInvalidAgentRun, filter.Order)
	}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
	}
	return s.store.ListAgentRuns(ctx, filter)
}

func (s *Service) SummarizeAgentRuns(ctx context.Context, scope models.Scope, owners []models.ObjectiveOwner) ([]models.AgentRunOwnerSummary, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio service is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	for _, owner := range owners {
		if err := owner.Validate(); err != nil {
			return nil, err
		}
	}
	store, ok := s.store.(models.AgentRunSummaryStore)
	if !ok {
		return nil, errors.New("agent run summaries are unavailable")
	}
	return store.SummarizeAgentRuns(ctx, scope, owners)
}

func ApplyObjectiveUpdate(objective *models.Objective, req UpdateObjectiveRequest) {
	if req.Title != nil {
		objective.Title = *req.Title
	}
	if req.Goal != nil {
		objective.Goal = *req.Goal
	}
	if req.Status != nil {
		objective.Status = *req.Status
	}
	if req.Priority != nil {
		objective.Priority = *req.Priority
	}
	if req.ExecutionPolicy != nil {
		objective.ExecutionPolicy = cloneObjectiveExecutionPolicy(req.ExecutionPolicy)
	}
	if req.Budget != nil {
		objective.Budget = models.CloneBudgetPolicy(req.Budget)
	}
	if req.Constraints != nil {
		objective.Constraints = req.Constraints
	}
	if req.SuccessCriteria != nil {
		objective.SuccessCriteria = req.SuccessCriteria
	}
	if req.ProgressSummary != nil {
		objective.ProgressSummary = *req.ProgressSummary
	}
}

func objectiveCreationFingerprint(req CreateObjectiveRequest) (string, error) {
	payload := struct {
		Scope           models.Scope                     `json:"scope"`
		Owner           models.ObjectiveOwner            `json:"owner"`
		Title           string                           `json:"title"`
		Goal            string                           `json:"goal"`
		Status          models.ObjectiveStatus           `json:"status"`
		Priority        int                              `json:"priority"`
		ExecutionPolicy *models.ObjectiveExecutionPolicy `json:"executionPolicy,omitempty"`
		Budget          *models.BudgetPolicy             `json:"budget,omitempty"`
		Constraints     map[string]interface{}           `json:"constraints,omitempty"`
		SuccessCriteria map[string]interface{}           `json:"successCriteria,omitempty"`
	}{req.Scope, req.Owner, req.Title, req.Goal, req.Status, req.Priority, req.ExecutionPolicy,
		req.Budget, req.Constraints, req.SuccessCriteria}
	if payload.Status == "" {
		payload.Status = models.ObjectiveStatusDraft
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode objective creation fingerprint: %w", err)
	}
	return models.SHA256Digest(encoded), nil
}
