package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

var (
	ErrInvalidObjectiveTransition = errors.New("invalid objective transition")
	ErrInvalidRunCommand          = errors.New("invalid run command")
)

// RunCreditErrorWorkspaceCreditsExhausted is the only RunCreditError code.
const RunCreditErrorWorkspaceCreditsExhausted = "workspace_credits_exhausted"

// RunEngineLoop is AgentRun.Engine for Runs created by the run loop.
const RunEngineLoop = "loop"

func summarizeAgentRunOwners(runs []*models.AgentRun, owners []models.ObjectiveOwner) []models.AgentRunOwnerSummary {
	requested := make(map[string]models.ObjectiveOwner, len(owners))
	for _, owner := range owners {
		requested[string(owner.Type)+"\x1f"+owner.ID] = owner
	}
	summaries := make(map[string]models.AgentRunOwnerSummary, len(owners))
	for _, run := range runs {
		key := string(run.Owner.Type) + "\x1f" + run.Owner.ID
		owner, ok := requested[key]
		if !ok {
			continue
		}
		summary := summaries[key]
		summary.Owner = owner
		summary.RunCount++
		if summary.LastRunAt == nil || run.CreatedAt.After(*summary.LastRunAt) {
			at := run.CreatedAt
			summary.LastRunAt = &at
			summary.LastStatus = run.Status
		}
		summaries[key] = summary
	}
	result := make([]models.AgentRunOwnerSummary, 0, len(summaries))
	for _, owner := range owners {
		if summary, ok := summaries[string(owner.Type)+"\x1f"+owner.ID]; ok {
			result = append(result, summary)
		}
	}
	return result
}

type CreateObjectiveRequest struct {
	Scope           Scope
	Owner           ObjectiveOwner
	Title           string
	Goal            string
	Status          ObjectiveStatus
	Priority        int
	ExecutionPolicy *ObjectiveExecutionPolicy
	Budget          *BudgetPolicy
	Constraints     map[string]interface{}
	SuccessCriteria map[string]interface{}
	IdempotencyKey  string
	Actor           ActivityActor
	Visibility      ActivityVisibility
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64
	Title            *string
	Goal             *string
	Status           *ObjectiveStatus
	Priority         *int
	ExecutionPolicy  *ObjectiveExecutionPolicy
	Budget           *BudgetPolicy
	Constraints      map[string]interface{}
	SuccessCriteria  map[string]interface{}
	ProgressSummary  *string
	Actor            ActivityActor
	Visibility       ActivityVisibility
	Summary          string
}

type PortfolioService struct {
	store PortfolioStore
	now   func() time.Time
}

type CreateObjectiveResult struct {
	Objective *Objective `json:"objective"`
	Created   bool       `json:"created"`
}

func NewPortfolioService(store PortfolioStore) *PortfolioService {
	return &PortfolioService{store: store, now: time.Now}
}

func (s *PortfolioService) CreateObjective(ctx context.Context, req CreateObjectiveRequest) (*Objective, error) {
	result, err := s.CreateObjectiveIdempotent(ctx, req)
	if err != nil {
		return nil, err
	}
	return result.Objective, nil
}

func (s *PortfolioService) CreateObjectiveIdempotent(ctx context.Context, req CreateObjectiveRequest) (*CreateObjectiveResult, error) {
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
		status = ObjectiveStatusDraft
	}
	objectiveID := uuid.NewString()
	key := strings.TrimSpace(req.IdempotencyKey)
	if len(key) > 256 {
		return nil, fmt.Errorf("%w: idempotency key cannot exceed 256 characters", ErrObjectiveIdempotency)
	}
	if key != "" {
		objectiveID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(req.Scope.Kind+"\x00"+req.Scope.ID+"\x00"+hashString(key))).String()
		current, getErr := s.store.GetObjective(ctx, req.Scope, objectiveID)
		if getErr != nil {
			return nil, getErr
		}
		if current != nil {
			if current.CreationFingerprint != fingerprint {
				return nil, ErrObjectiveIdempotency
			}
			return &CreateObjectiveResult{Objective: current, Created: false}, nil
		}
	}
	objective := &Objective{
		ID: objectiveID, Scope: req.Scope, Owner: req.Owner, Title: req.Title,
		Goal: req.Goal, Status: status, Priority: req.Priority,
		ExecutionPolicy: cloneObjectiveExecutionPolicy(req.ExecutionPolicy), Budget: cloneBudgetPolicy(req.Budget), Constraints: req.Constraints, SuccessCriteria: req.SuccessCriteria,
		Revision: 1, CreatedAt: now, UpdatedAt: now, CreationFingerprint: fingerprint,
	}
	if key != "" {
		objective.IdempotencyKeyHash = hashString(key)
	}
	if err := objective.Validate(); err != nil {
		return nil, err
	}
	persistObjective := func() error {
		activityStore, ok := s.store.(ObjectiveActivityStore)
		if !ok {
			return s.store.CreateObjective(ctx, objective)
		}
		_, persistErr := activityStore.CreateObjectiveWithEvent(ctx, objective, objectiveActivityEvent(objective, req.Actor, req.Visibility, "objective.created", "Objective created", now, map[string]interface{}{"status": objective.Status}))
		return persistErr
	}
	if err := persistObjective(); err != nil {
		if key != "" {
			current, getErr := s.store.GetObjective(ctx, req.Scope, objectiveID)
			if getErr == nil && current != nil {
				if current.CreationFingerprint != fingerprint {
					return nil, ErrObjectiveIdempotency
				}
				return &CreateObjectiveResult{Objective: current, Created: false}, nil
			}
		}
		return nil, err
	}
	return &CreateObjectiveResult{Objective: objective, Created: true}, nil
}

func (s *PortfolioService) UpdateObjective(ctx context.Context, scope Scope, objectiveID string, req UpdateObjectiveRequest) (*Objective, error) {
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
		return nil, ErrObjectiveNotFound
	}
	if req.ExpectedRevision != current.Revision {
		return nil, ErrRevisionConflict
	}
	if req.Status != nil && *req.Status != current.Status && !canTransitionObjective(current.Status, *req.Status) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrInvalidObjectiveTransition, current.Status, *req.Status)
	}
	previousStatus := current.Status
	applyObjectiveUpdate(current, req)
	current.UpdatedAt = s.now()
	current.Revision++
	if err := current.Validate(); err != nil {
		return nil, err
	}
	persistUpdate := func() error {
		activityStore, ok := s.store.(ObjectiveActivityStore)
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
		_, persistErr := activityStore.UpdateObjectiveWithEvent(ctx, current, req.ExpectedRevision, objectiveActivityEvent(current, req.Actor, req.Visibility, eventType, summary, current.UpdatedAt, map[string]interface{}{"previousStatus": previousStatus, "status": current.Status, "revision": current.Revision}))
		return persistErr
	}
	if err := persistUpdate(); err != nil {
		return nil, err
	}
	return current, nil
}

func objectiveActivityEvent(objective *Objective, actor ActivityActor, visibility ActivityVisibility, eventType, summary string, occurredAt time.Time, payload map[string]interface{}) *ActivityEvent {
	if strings.TrimSpace(actor.Type) == "" {
		actor = ActivityActor{Type: "system", ID: "openseal"}
	}
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: objective.Scope, ObjectiveID: objective.ID, EventType: eventType,
		Severity: ActivitySeverityInfo, Actor: actor, Summary: summary, Payload: payload,
		Visibility: visibility, CreatedAt: occurredAt,
	}
	event.AgentID = ownedItemEventAgentID(objective.Owner, actor)
	if objective.Owner.Type == OwnerTypeTeam {
		event.TeamID = objective.Owner.ID
	}
	return event
}

// ownedItemEventAgentID is the Agent an owned item's activity belongs to: the
// owning Agent, or the Team member that acted on a Team-owned item.
func ownedItemEventAgentID(owner ObjectiveOwner, actor ActivityActor) string {
	switch owner.Type {
	case OwnerTypeAgent:
		return owner.ID
	case OwnerTypeTeam:
		if actor.Type == "agent" {
			return actor.ID
		}
	}
	return ""
}

func (s *PortfolioService) GetObjective(ctx context.Context, scope Scope, objectiveID string) (*Objective, error) {
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
		return nil, ErrObjectiveNotFound
	}
	return objective, nil
}

func (s *PortfolioService) ListObjectives(ctx context.Context, filter ObjectiveFilter) ([]*Objective, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.ListObjectives(ctx, filter)
}

func (s *PortfolioService) CreateAgentRun(ctx context.Context, req CreateAgentRunRequest) (*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	run, err := buildAgentRun(ctx, s.store, req, uuid.NewString(), s.now())
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateAgentRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

func buildAgentRun(ctx context.Context, store PortfolioStore, req CreateAgentRunRequest, runID string, now time.Time) (*AgentRun, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	if err := req.Owner.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	for _, state := range []map[string]interface{}{req.Context, req.Plan, req.Checkpoint, req.Policy} {
		if err := ValidateCredentialFreeContext(state); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
		}
	}
	if req.ObjectiveID != "" {
		objective, err := store.GetObjective(ctx, req.Scope, req.ObjectiveID)
		if err != nil {
			return nil, err
		}
		if objective == nil {
			return nil, ErrObjectiveNotFound
		}
		if req.ParentRunID == "" {
			if err := validateObjectiveRunBudget(objective, req.Budget); err != nil {
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
			return nil, ErrRunNotFound
		}
		rootID = parent.RootRunID
		runContext = inheritRoutineEventBatch(parent, cloneMap(req.Context))
	}
	source := req.Source
	if source == "" {
		source = RunSourceManual
	}
	switch source {
	case RunSourceManual, RunSourceChat, RunSourceSchedule, RunSourceEvent, RunSourceWebhook, RunSourceRequest, RunSourceRequestDecision, RunSourceCompletionReview, RunSourceHandoff, RunSourceEscalation, RunSourceObjective, RunSourceFork:
	default:
		return nil, fmt.Errorf("%w: unsupported run source %q", ErrInvalidAgentRun, source)
	}
	kind := normalizeRunKind(req.Kind)
	if !validRunKind(kind) {
		return nil, fmt.Errorf("%w: unsupported run kind %q", ErrInvalidAgentRun, req.Kind)
	}
	run := &AgentRun{
		ID: runID, Kind: kind, Scope: req.Scope, ObjectiveID: req.ObjectiveID,
		ParentRunID: req.ParentRunID, RootRunID: rootID, Owner: req.Owner,
		AssignedAgentID: req.AssignedAgentID, ActingUser: req.ActingUser, Entrypoint: strings.TrimSpace(req.Entrypoint), ConcurrencyKey: strings.TrimSpace(req.ConcurrencyKey),
		ResourceRequirements: cloneResourceQuantities(req.ResourceRequirements),
		Goal:                 req.Goal, Source: source,
		Status: AgentRunStatusQueued, Priority: req.Priority, Deadline: req.Deadline,
		AvailableAt: availableAt, QueueEnteredAt: now, Context: runContext,
		Plan: req.Plan, Checkpoint: req.Checkpoint, WakeCondition: req.WakeCondition,
		Budget: cloneBudgetPolicy(req.Budget), Policy: req.Policy, Revision: 1, CreatedAt: now, UpdatedAt: now,
		SessionID: req.SessionID, Engine: RunEngineLoop,
	}
	if run.Budget != nil {
		run.BudgetState = BudgetStateActive
	}
	if err := run.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	return run, nil
}

func cloneResourceQuantities(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	cloned := make(map[string]int, len(values))
	for name, quantity := range values {
		cloned[name] = quantity
	}
	return cloned
}

func (s *PortfolioService) GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error) {
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
		return nil, ErrRunNotFound
	}
	return run, nil
}

func (s *PortfolioService) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("portfolio store is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.Kind != "" && !validRunKind(filter.Kind) {
		return nil, fmt.Errorf("%w: unsupported run kind %q", ErrInvalidAgentRun, filter.Kind)
	}
	if filter.Order != "" && filter.Order != AgentRunOrderScheduler && filter.Order != AgentRunOrderCreatedDesc {
		return nil, fmt.Errorf("%w: unsupported run order %q", ErrInvalidAgentRun, filter.Order)
	}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
	}
	return s.store.ListAgentRuns(ctx, filter)
}

func (s *PortfolioService) SummarizeAgentRuns(ctx context.Context, scope Scope, owners []ObjectiveOwner) ([]AgentRunOwnerSummary, error) {
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
	store, ok := s.store.(AgentRunSummaryStore)
	if !ok {
		return nil, errors.New("agent run summaries are unavailable")
	}
	return store.SummarizeAgentRuns(ctx, scope, owners)
}

func applyObjectiveUpdate(objective *Objective, req UpdateObjectiveRequest) {
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
		objective.Budget = cloneBudgetPolicy(req.Budget)
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

func canTransitionObjective(from, to ObjectiveStatus) bool {
	allowed := map[ObjectiveStatus]map[ObjectiveStatus]bool{
		ObjectiveStatusDraft:  {ObjectiveStatusActive: true, ObjectiveStatusRetired: true},
		ObjectiveStatusActive: {ObjectiveStatusPaused: true, ObjectiveStatusSatisfied: true, ObjectiveStatusFailed: true, ObjectiveStatusRetired: true},
		ObjectiveStatusPaused: {ObjectiveStatusActive: true, ObjectiveStatusFailed: true, ObjectiveStatusRetired: true},
	}
	return allowed[from][to]
}

func objectiveCreationFingerprint(req CreateObjectiveRequest) (string, error) {
	payload := struct {
		Scope           Scope                     `json:"scope"`
		Owner           ObjectiveOwner            `json:"owner"`
		Title           string                    `json:"title"`
		Goal            string                    `json:"goal"`
		Status          ObjectiveStatus           `json:"status"`
		Priority        int                       `json:"priority"`
		ExecutionPolicy *ObjectiveExecutionPolicy `json:"executionPolicy,omitempty"`
		Budget          *BudgetPolicy             `json:"budget,omitempty"`
		Constraints     map[string]interface{}    `json:"constraints,omitempty"`
		SuccessCriteria map[string]interface{}    `json:"successCriteria,omitempty"`
	}{req.Scope, req.Owner, req.Title, req.Goal, req.Status, req.Priority, req.ExecutionPolicy,
		req.Budget, req.Constraints, req.SuccessCriteria}
	if payload.Status == "" {
		payload.Status = ObjectiveStatusDraft
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode objective creation fingerprint: %w", err)
	}
	return hashBytes(encoded), nil
}
