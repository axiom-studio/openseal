package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

type ResolveRunDependencyRequest struct {
	Scope                      models.Scope
	GroupID                    string
	DependencyID               string
	ExpectedDependencyRevision int64
	State                      models.RunDependencyState
	Result                     map[string]interface{}
	Artifacts                  []models.ArtifactReference
	Error                      string
	Actor                      models.ActivityActor
	Visibility                 models.ActivityVisibility
}

type DependencyCoordinator struct {
	store models.RunDependencyStore
	runs  models.PortfolioStore
	now   func() time.Time
	newID func() string
}

func NewDependencyCoordinator(store models.DependencyKernelStore, opts ...models.ServiceOption) *DependencyCoordinator {
	return &DependencyCoordinator{store: store, runs: store, now: models.ClockFrom(opts), newID: uuid.NewString}
}

func (c *DependencyCoordinator) CreateRunDependencyGroup(ctx context.Context, req models.CreateRunDependencyGroupRequest) (*models.RunDependencyResult, error) {
	if c == nil || c.store == nil || c.runs == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !models.IsValidOpaqueIdentifier(req.SourceRunID, 128) || len(req.Dependencies) == 0 {
		return nil, fmt.Errorf("%w: source run and at least one dependency are required", models.ErrInvalidRunDependency)
	}
	if req.ExpectedSourceRevision <= 0 {
		return nil, fmt.Errorf("%w: expected source revision must be positive", models.ErrInvalidRunDependency)
	}
	if err := req.Policy.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" {
		return nil, fmt.Errorf("%w: activity actor is required", models.ErrInvalidRunDependency)
	}
	if req.Visibility == "" {
		req.Visibility = models.ActivityVisibilityScope
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key != "" {
		existing, err := c.store.FindRunDependencyGroupByIdempotencyKey(ctx, req.Scope, key)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			edges, err := c.store.ListRunDependencies(ctx, req.Scope, existing.ID)
			if err != nil {
				return nil, err
			}
			if !models.SameDependencyGroupRequest(existing, edges, req) {
				return nil, models.ErrDependencyConflict
			}
			source, err := c.runs.GetAgentRun(ctx, req.Scope, existing.SourceRunID)
			if err != nil {
				return nil, err
			}
			evaluation, err := models.EvaluateRunDependencies(existing, edges)
			return &models.RunDependencyResult{Group: existing, Dependencies: edges, Source: source, Evaluation: evaluation, Replayed: true}, err
		}
	}
	source, err := c.runs.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	if source.Revision != req.ExpectedSourceRevision {
		return nil, models.ErrRevisionConflict
	}
	if !models.CanEnterDependencyWait(source) {
		return nil, fmt.Errorf("%w: source run cannot enter dependency wait from %s", models.ErrInvalidRunTransition, source.Status)
	}
	now := c.now().UTC()
	groupID := strings.TrimSpace(req.ID)
	if groupID == "" {
		groupID = c.newID()
	}
	sealedAt := now
	group := &models.RunDependencyGroup{
		ID: groupID, Scope: req.Scope, SourceRunID: source.ID, Policy: req.Policy, ExpectedCount: len(req.Dependencies),
		Status: models.RunDependencyGroupWaiting, Revision: 1, CreatedAt: now, UpdatedAt: now, SealedAt: &sealedAt,
		IdempotencyKey: key,
	}
	edges := make([]*models.RunDependency, 0, len(req.Dependencies))
	for _, spec := range req.Dependencies {
		edgeID := strings.TrimSpace(spec.ID)
		if edgeID == "" {
			edgeID = c.newID()
		}
		required := true
		if spec.Required != nil {
			required = *spec.Required
		}
		edge := &models.RunDependency{
			ID: edgeID, Scope: req.Scope, GroupID: group.ID, SourceRunID: source.ID,
			TargetRunID: strings.TrimSpace(spec.TargetRunID), RequestID: strings.TrimSpace(spec.RequestID), Kind: spec.Kind,
			State: models.RunDependencyStatePending, Required: required, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := edge.Validate(); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	if _, err := models.EvaluateRunDependencies(group, edges); err != nil {
		return nil, err
	}
	updatedSource := models.CloneAgentRun(source)
	updatedSource.Status = models.AgentRunStatusWaitingForDependency
	updatedSource.WakeCondition = &models.WakeCondition{Type: "run_dependencies", Reference: group.ID}
	updatedSource.Revision++
	updatedSource.UpdatedAt = now
	updatedSource.LeaseOwner = ""
	updatedSource.LeaseExpiresAt = nil
	event := dependencyGroupEvent(updatedSource, group, edges, req.Actor, req.Visibility, now)
	return c.store.CreateRunDependencyGroup(ctx, models.RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, SourceRun: updatedSource, ExpectedSourceRevision: source.Revision, Event: event,
	})
}

func (c *DependencyCoordinator) ResolveRunDependency(ctx context.Context, req ResolveRunDependencyRequest) (*models.RunDependencyResult, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !models.IsValidOpaqueIdentifier(req.GroupID, 128) || !models.IsValidOpaqueIdentifier(req.DependencyID, 128) || req.ExpectedDependencyRevision <= 0 {
		return nil, fmt.Errorf("%w: group, dependency, and expected revision are required", models.ErrInvalidRunDependency)
	}
	if req.State != models.RunDependencyStateSatisfied && req.State != models.RunDependencyStateFailed && req.State != models.RunDependencyStateCanceled {
		return nil, fmt.Errorf("%w: dependency resolution must be satisfied, failed, or canceled", models.ErrInvalidRunDependency)
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" {
		return nil, fmt.Errorf("%w: activity actor is required", models.ErrInvalidRunDependency)
	}
	if req.Visibility == "" {
		req.Visibility = models.ActivityVisibilityScope
	}
	record := models.RunDependencyResolutionRecord{
		Scope: req.Scope, GroupID: strings.TrimSpace(req.GroupID), DependencyID: strings.TrimSpace(req.DependencyID),
		ExpectedDependencyRevision: req.ExpectedDependencyRevision, State: req.State, Result: maps.Clone(req.Result),
		Artifacts: models.CloneArtifactReferences(req.Artifacts), Error: strings.TrimSpace(req.Error), Actor: req.Actor,
		Visibility: req.Visibility, OccurredAt: c.now().UTC(),
	}
	return c.store.ResolveRunDependency(ctx, record)
}

func (c *DependencyCoordinator) GetRunDependencyGroup(ctx context.Context, scope models.Scope, groupID string) (*models.RunDependencyGroup, error) {
	group, err := c.store.GetRunDependencyGroup(ctx, scope, strings.TrimSpace(groupID))
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, models.ErrDependencyGroupNotFound
	}
	return group, nil
}

func (c *DependencyCoordinator) FindRunDependencyGroupByIdempotencyKey(ctx context.Context, scope models.Scope, key string) (*models.RunDependencyGroup, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return c.store.FindRunDependencyGroupByIdempotencyKey(ctx, scope, strings.TrimSpace(key))
}

func (c *DependencyCoordinator) ListRunDependencies(ctx context.Context, scope models.Scope, groupID string) ([]*models.RunDependency, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	return c.store.ListRunDependencies(ctx, scope, strings.TrimSpace(groupID))
}

func dependencyGroupEvent(run *models.AgentRun, group *models.RunDependencyGroup, edges []*models.RunDependency, actor models.ActivityActor, visibility models.ActivityVisibility, now time.Time) *models.ActivityEvent {
	ids := make([]string, 0, len(edges))
	for _, edge := range edges {
		ids = append(ids, edge.ID)
	}
	sort.Strings(ids)
	return &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "dependency.group_waiting", Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: models.TeamIDForRun(run), Actor: actor, Summary: fmt.Sprintf("Waiting on %d coordinated dependencies", len(edges)),
		Payload:    map[string]interface{}{"groupId": group.ID, "policy": group.Policy, "dependencyIds": ids},
		Visibility: visibility, CorrelationID: group.ID, CausationID: run.ID, CreatedAt: now,
	}
}

type ReconcileRunDependencyGroupRequest struct {
	Scope                  models.Scope
	GroupID                string
	ExpectedGroupRevision  int64
	ExpectedSourceRevision int64
	Actor                  models.ActivityActor
	Visibility             models.ActivityVisibility
}

func (c *DependencyCoordinator) ListWaitingRunDependencyGroups(ctx context.Context, scope models.Scope, afterID string, limit int) ([]models.RunDependencyGroupWork, error) {
	if c == nil || c.store == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	if err := models.ValidateWaitingDependencyGroupPage(scope, afterID, limit); err != nil {
		return nil, err
	}
	store, ok := c.store.(models.RunDependencyReconciliationStore)
	if !ok {
		return nil, fmt.Errorf("run dependency reconciliation store is not configured")
	}
	return store.ListWaitingRunDependencyGroups(ctx, scope, afterID, limit)
}

func (c *DependencyCoordinator) ReconcileRunDependencyGroup(ctx context.Context, request ReconcileRunDependencyGroupRequest) (*models.RunDependencyResult, error) {
	if c == nil || c.store == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	store, ok := c.store.(models.RunDependencyReconciliationStore)
	if !ok {
		return nil, fmt.Errorf("run dependency reconciliation store is not configured")
	}
	visibility := request.Visibility
	if visibility == "" {
		visibility = models.ActivityVisibilityScope
	}
	record := models.RunDependencyGroupReconciliationRecord{
		Scope: request.Scope, GroupID: request.GroupID, ExpectedGroupRevision: request.ExpectedGroupRevision,
		ExpectedSourceRevision: request.ExpectedSourceRevision, Actor: request.Actor, Visibility: visibility, OccurredAt: c.now().UTC(),
	}
	if err := models.ValidateDependencyGroupReconciliationRecord(record); err != nil {
		return nil, err
	}
	return store.ReconcileRunDependencyGroup(ctx, record)
}
