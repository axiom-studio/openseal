package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

type RunDependencySpec struct {
	ID          string
	TargetRunID string
	RequestID   string
	Kind        models.RunDependencyKind
	Required    *bool
}

type CreateRunDependencyGroupRequest struct {
	ID                     string
	Scope                  models.Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	Policy                 models.RunDependencyPolicy
	Dependencies           []RunDependencySpec
	IdempotencyKey         string
	Actor                  models.ActivityActor
	Visibility             models.ActivityVisibility
}

type ResolveRunDependencyRequest struct {
	Scope                      Scope
	GroupID                    string
	DependencyID               string
	ExpectedDependencyRevision int64
	State                      RunDependencyState
	Result                     map[string]interface{}
	Artifacts                  []ArtifactReference
	Error                      string
	Actor                      ActivityActor
	Visibility                 ActivityVisibility
}

type DependencyCoordinator struct {
	store RunDependencyStore
	runs  PortfolioStore
	now   func() time.Time
	newID func() string
}

func NewDependencyCoordinator(store DependencyKernelStore) *DependencyCoordinator {
	return &DependencyCoordinator{store: store, runs: store, now: time.Now, newID: uuid.NewString}
}

func (c *DependencyCoordinator) CreateRunDependencyGroup(ctx context.Context, req CreateRunDependencyGroupRequest) (*RunDependencyResult, error) {
	if c == nil || c.store == nil || c.runs == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(req.SourceRunID, 128) || len(req.Dependencies) == 0 {
		return nil, fmt.Errorf("%w: source run and at least one dependency are required", ErrInvalidRunDependency)
	}
	if req.ExpectedSourceRevision <= 0 {
		return nil, fmt.Errorf("%w: expected source revision must be positive", ErrInvalidRunDependency)
	}
	if err := req.Policy.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" {
		return nil, fmt.Errorf("%w: activity actor is required", ErrInvalidRunDependency)
	}
	if req.Visibility == "" {
		req.Visibility = ActivityVisibilityScope
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
			if !sameDependencyGroupRequest(existing, edges, req) {
				return nil, ErrDependencyConflict
			}
			source, err := c.runs.GetAgentRun(ctx, req.Scope, existing.SourceRunID)
			if err != nil {
				return nil, err
			}
			evaluation, err := EvaluateRunDependencies(existing, edges)
			return &RunDependencyResult{Group: existing, Dependencies: edges, Source: source, Evaluation: evaluation, Replayed: true}, err
		}
	}
	source, err := c.runs.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrRunNotFound
	}
	if source.Revision != req.ExpectedSourceRevision {
		return nil, ErrRevisionConflict
	}
	if !canEnterDependencyWait(source) {
		return nil, fmt.Errorf("%w: source run cannot enter dependency wait from %s", ErrInvalidRunTransition, source.Status)
	}
	now := c.now().UTC()
	groupID := strings.TrimSpace(req.ID)
	if groupID == "" {
		groupID = c.newID()
	}
	sealedAt := now
	group := &RunDependencyGroup{
		ID: groupID, Scope: req.Scope, SourceRunID: source.ID, Policy: req.Policy, ExpectedCount: len(req.Dependencies),
		Status: RunDependencyGroupWaiting, Revision: 1, CreatedAt: now, UpdatedAt: now, SealedAt: &sealedAt,
		IdempotencyKey: key,
	}
	edges := make([]*RunDependency, 0, len(req.Dependencies))
	for _, spec := range req.Dependencies {
		edgeID := strings.TrimSpace(spec.ID)
		if edgeID == "" {
			edgeID = c.newID()
		}
		required := true
		if spec.Required != nil {
			required = *spec.Required
		}
		edge := &RunDependency{
			ID: edgeID, Scope: req.Scope, GroupID: group.ID, SourceRunID: source.ID,
			TargetRunID: strings.TrimSpace(spec.TargetRunID), RequestID: strings.TrimSpace(spec.RequestID), Kind: spec.Kind,
			State: RunDependencyStatePending, Required: required, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := edge.Validate(); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	if _, err := EvaluateRunDependencies(group, edges); err != nil {
		return nil, err
	}
	updatedSource := cloneAgentRun(source)
	updatedSource.Status = AgentRunStatusWaitingForDependency
	updatedSource.WakeCondition = &WakeCondition{Type: "run_dependencies", Reference: group.ID}
	updatedSource.Revision++
	updatedSource.UpdatedAt = now
	updatedSource.LeaseOwner = ""
	updatedSource.LeaseExpiresAt = nil
	event := dependencyGroupEvent(updatedSource, group, edges, req.Actor, req.Visibility, now)
	return c.store.CreateRunDependencyGroup(ctx, RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, SourceRun: updatedSource, ExpectedSourceRevision: source.Revision, Event: event,
	})
}

func (c *DependencyCoordinator) ResolveRunDependency(ctx context.Context, req ResolveRunDependencyRequest) (*RunDependencyResult, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(req.GroupID, 128) || !validOpaqueIdentifier(req.DependencyID, 128) || req.ExpectedDependencyRevision <= 0 {
		return nil, fmt.Errorf("%w: group, dependency, and expected revision are required", ErrInvalidRunDependency)
	}
	if req.State != RunDependencyStateSatisfied && req.State != RunDependencyStateFailed && req.State != RunDependencyStateCanceled {
		return nil, fmt.Errorf("%w: dependency resolution must be satisfied, failed, or canceled", ErrInvalidRunDependency)
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" {
		return nil, fmt.Errorf("%w: activity actor is required", ErrInvalidRunDependency)
	}
	if req.Visibility == "" {
		req.Visibility = ActivityVisibilityScope
	}
	record := RunDependencyResolutionRecord{
		Scope: req.Scope, GroupID: strings.TrimSpace(req.GroupID), DependencyID: strings.TrimSpace(req.DependencyID),
		ExpectedDependencyRevision: req.ExpectedDependencyRevision, State: req.State, Result: maps.Clone(req.Result),
		Artifacts: cloneArtifactReferences(req.Artifacts), Error: strings.TrimSpace(req.Error), Actor: req.Actor,
		Visibility: req.Visibility, OccurredAt: c.now().UTC(),
	}
	return c.store.ResolveRunDependency(ctx, record)
}

func (c *DependencyCoordinator) GetRunDependencyGroup(ctx context.Context, scope Scope, groupID string) (*RunDependencyGroup, error) {
	group, err := c.store.GetRunDependencyGroup(ctx, scope, strings.TrimSpace(groupID))
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, ErrDependencyGroupNotFound
	}
	return group, nil
}

func (c *DependencyCoordinator) FindRunDependencyGroupByIdempotencyKey(ctx context.Context, scope Scope, key string) (*RunDependencyGroup, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return c.store.FindRunDependencyGroupByIdempotencyKey(ctx, scope, strings.TrimSpace(key))
}

func (c *DependencyCoordinator) ListRunDependencies(ctx context.Context, scope Scope, groupID string) ([]*RunDependency, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	return c.store.ListRunDependencies(ctx, scope, strings.TrimSpace(groupID))
}

func canEnterDependencyWait(run *models.AgentRun) bool {
	if run == nil || run.WakeCondition != nil {
		return false
	}
	switch run.Status {
	case models.AgentRunStatusQueued, models.AgentRunStatusPlanning, models.AgentRunStatusRunning:
		return true
	default:
		return false
	}
}

func sameDependencyGroupRequest(group *models.RunDependencyGroup, edges []*models.RunDependency, req CreateRunDependencyGroupRequest) bool {
	if group == nil || group.Scope != req.Scope || group.SourceRunID != strings.TrimSpace(req.SourceRunID) ||
		group.Policy != req.Policy || group.IdempotencyKey != strings.TrimSpace(req.IdempotencyKey) || len(edges) != len(req.Dependencies) {
		return false
	}
	type intent struct {
		target, request string
		kind            models.RunDependencyKind
		required        bool
	}
	existing := make([]intent, 0, len(edges))
	for _, edge := range edges {
		existing = append(existing, intent{target: edge.TargetRunID, request: edge.RequestID, kind: edge.Kind, required: edge.Required})
	}
	wanted := make([]intent, 0, len(req.Dependencies))
	for _, spec := range req.Dependencies {
		required := true
		if spec.Required != nil {
			required = *spec.Required
		}
		wanted = append(wanted, intent{target: strings.TrimSpace(spec.TargetRunID), request: strings.TrimSpace(spec.RequestID), kind: spec.Kind, required: required})
	}
	sort.Slice(existing, func(i, j int) bool { return fmt.Sprint(existing[i]) < fmt.Sprint(existing[j]) })
	sort.Slice(wanted, func(i, j int) bool { return fmt.Sprint(wanted[i]) < fmt.Sprint(wanted[j]) })
	return reflect.DeepEqual(existing, wanted)
}

func dependencyGroupEvent(run *AgentRun, group *RunDependencyGroup, edges []*RunDependency, actor ActivityActor, visibility ActivityVisibility, now time.Time) *ActivityEvent {
	ids := make([]string, 0, len(edges))
	for _, edge := range edges {
		ids = append(ids, edge.ID)
	}
	sort.Strings(ids)
	return &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "dependency.group_waiting", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: teamIDForRun(run), Actor: actor, Summary: fmt.Sprintf("Waiting on %d coordinated dependencies", len(edges)),
		Payload:    map[string]interface{}{"groupId": group.ID, "policy": group.Policy, "dependencyIds": ids},
		Visibility: visibility, CorrelationID: group.ID, CausationID: run.ID, CreatedAt: now,
	}
}

type ReconcileRunDependencyGroupRequest struct {
	Scope                  Scope
	GroupID                string
	ExpectedGroupRevision  int64
	ExpectedSourceRevision int64
	Actor                  ActivityActor
	Visibility             ActivityVisibility
}

func (c *DependencyCoordinator) ListWaitingRunDependencyGroups(ctx context.Context, scope Scope, afterID string, limit int) ([]RunDependencyGroupWork, error) {
	if c == nil || c.store == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	if err := validateWaitingDependencyGroupPage(scope, afterID, limit); err != nil {
		return nil, err
	}
	store, ok := c.store.(RunDependencyReconciliationStore)
	if !ok {
		return nil, fmt.Errorf("run dependency reconciliation store is not configured")
	}
	return store.ListWaitingRunDependencyGroups(ctx, scope, afterID, limit)
}

func (c *DependencyCoordinator) ReconcileRunDependencyGroup(ctx context.Context, request ReconcileRunDependencyGroupRequest) (*RunDependencyResult, error) {
	if c == nil || c.store == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	store, ok := c.store.(RunDependencyReconciliationStore)
	if !ok {
		return nil, fmt.Errorf("run dependency reconciliation store is not configured")
	}
	visibility := request.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	record := RunDependencyGroupReconciliationRecord{
		Scope: request.Scope, GroupID: request.GroupID, ExpectedGroupRevision: request.ExpectedGroupRevision,
		ExpectedSourceRevision: request.ExpectedSourceRevision, Actor: request.Actor, Visibility: visibility, OccurredAt: c.now().UTC(),
	}
	if err := validateDependencyGroupReconciliationRecord(record); err != nil {
		return nil, err
	}
	return store.ReconcileRunDependencyGroup(ctx, record)
}
