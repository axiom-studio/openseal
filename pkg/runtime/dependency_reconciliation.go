package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const maximumWaitingDependencyGroupPage = 100

// RunDependencyGroupWork is metadata for one indexed waiting-group candidate.
// It contains no execution inputs or historical Run payloads.
type RunDependencyGroupWork struct {
	Scope          Scope
	GroupID        string
	GroupRevision  int64
	SourceRunID    string
	SourceRevision int64
	// Ready is a metadata hint. Reconciliation still verifies the source and
	// dependency outcomes under the store's transaction and revision fences.
	Ready bool
}

type ReconcileRunDependencyGroupRequest struct {
	Scope                  Scope
	GroupID                string
	ExpectedGroupRevision  int64
	ExpectedSourceRevision int64
	Actor                  ActivityActor
	Visibility             ActivityVisibility
}

type RunDependencyGroupReconciliationRecord struct {
	Scope                  Scope
	GroupID                string
	ExpectedGroupRevision  int64
	ExpectedSourceRevision int64
	Actor                  ActivityActor
	Visibility             ActivityVisibility
	OccurredAt             time.Time
}

// RunDependencyReconciliationStore is optional for custom stores. Canonical
// stores read the exact source and terminal target outcomes transactionally.
type RunDependencyReconciliationStore interface {
	ListWaitingRunDependencyGroups(context.Context, Scope, string, int) ([]RunDependencyGroupWork, error)
	ReconcileRunDependencyGroup(context.Context, RunDependencyGroupReconciliationRecord) (*RunDependencyResult, error)
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

func validateWaitingDependencyGroupPage(scope Scope, afterID string, limit int) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if (afterID != "" && !validOpaqueIdentifier(afterID, 128)) || limit < 1 || limit > maximumWaitingDependencyGroupPage {
		return ErrInvalidRunDependency
	}
	return nil
}

func validateDependencyGroupReconciliationRecord(record RunDependencyGroupReconciliationRecord) error {
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(record.GroupID, 128) || record.ExpectedGroupRevision < 1 || record.ExpectedSourceRevision < 1 || record.OccurredAt.IsZero() || strings.TrimSpace(record.Actor.Type) == "" || strings.TrimSpace(record.Actor.ID) == "" {
		return ErrInvalidRunDependency
	}
	return nil
}

func isTerminalDependencyGroup(group *RunDependencyGroup) bool {
	return group != nil && (group.Status == RunDependencyGroupSatisfied || group.Status == RunDependencyGroupFailed || group.Status == RunDependencyGroupCanceled)
}

func sourceWaitsForDependencyGroup(source *AgentRun, groupID string) (waiting, paused bool) {
	if source == nil {
		return false, false
	}
	condition := source.WakeCondition
	if source.Status == AgentRunStatusPaused && source.PausedFrom == AgentRunStatusWaitingForDependency {
		condition, paused = source.PausedWakeCondition, true
	} else if source.Status != AgentRunStatusWaitingForDependency {
		return false, false
	}
	return condition != nil && condition.Type == "run_dependencies" && condition.Reference == groupID, paused
}

// Retirement closes relationships, not independently progressing target Runs.
// Already settled edge outcomes and the terminal source remain unchanged.
func retireDependencyGroup(group *RunDependencyGroup, edges []*RunDependency, source *AgentRun, actor ActivityActor, visibility ActivityVisibility, now time.Time) (*RunDependencyResult, error) {
	if group == nil || source == nil || !isTerminalAgentRunStatus(source.Status) || group.Status != RunDependencyGroupWaiting {
		return nil, ErrInvalidRunTransition
	}
	if source.Scope != group.Scope || source.ID != group.SourceRunID {
		return nil, ErrInvalidScope
	}
	if _, err := EvaluateRunDependencies(group, edges); err != nil {
		return nil, err
	}
	updated := cloneRunDependencyGroup(group)
	updated.Status, updated.Revision, updated.UpdatedAt = RunDependencyGroupCanceled, group.Revision+1, now
	updated.ResolvedAt, updated.WakeSignalID = &now, ""
	retired := cloneDependencySlice(edges)
	for _, edge := range retired {
		if edge.State != RunDependencyStatePending && edge.State != RunDependencyStateRunning {
			continue
		}
		edge.State, edge.Error, edge.Revision, edge.UpdatedAt, edge.ResolvedAt = RunDependencyStateCanceled, "dependency retired after its source Run became terminal", edge.Revision+1, now, &now
	}
	evaluation, err := EvaluateRunDependencies(updated, retired)
	if err != nil {
		return nil, err
	}
	event := dependencyGroupResolvedEvent(source, updated, evaluation, actor, visibility, now)
	return &RunDependencyResult{Group: updated, Dependencies: retired, Source: cloneAgentRun(source), Evaluation: evaluation, Events: []*ActivityEvent{event}}, nil
}

func applyDependencyGroupReconciliation(group *RunDependencyGroup, edges []*RunDependency, source *AgentRun, targets map[string]*AgentRun, record RunDependencyGroupReconciliationRecord) (*RunDependencyResult, error) {
	if group == nil {
		return nil, ErrDependencyGroupNotFound
	}
	if source == nil {
		return nil, ErrRunNotFound
	}
	if group.Scope != record.Scope || group.ID != record.GroupID || source.Scope != record.Scope || source.ID != group.SourceRunID {
		return nil, ErrInvalidScope
	}
	evaluation, err := EvaluateRunDependencies(group, edges)
	if err != nil {
		return nil, err
	}
	evaluation.Wake = false
	result := &RunDependencyResult{Group: cloneRunDependencyGroup(group), Dependencies: cloneDependencySlice(edges), Source: cloneAgentRun(source), Evaluation: evaluation, Replayed: true}
	if isTerminalDependencyGroup(group) {
		return result, nil
	}
	if group.Revision != record.ExpectedGroupRevision || source.Revision != record.ExpectedSourceRevision {
		return nil, ErrRevisionConflict
	}
	if group.Status != RunDependencyGroupWaiting {
		return nil, ErrInvalidRunTransition
	}
	if isTerminalAgentRunStatus(source.Status) {
		return retireDependencyGroup(group, edges, source, record.Actor, record.Visibility, record.OccurredAt)
	}
	if waiting, _ := sourceWaitsForDependencyGroup(source, group.ID); !waiting {
		return nil, ErrInvalidRunTransition
	}
	var events []*ActivityEvent
	wake := false
	for _, edge := range result.Dependencies {
		if edge.Kind != RunDependencyKindRun || (edge.State != RunDependencyStatePending && edge.State != RunDependencyStateRunning) {
			continue
		}
		target := targets[edge.TargetRunID]
		if target == nil {
			continue
		}
		if target.Scope != group.Scope || target.ID != edge.TargetRunID {
			return nil, ErrInvalidScope
		}
		if !isTerminalAgentRunStatus(target.Status) {
			continue
		}
		state, message := RunDependencyStateSatisfied, ""
		if target.Status == AgentRunStatusFailed {
			state, message = RunDependencyStateFailed, target.Error
		} else if target.Status == AgentRunStatusCanceled {
			state, message = RunDependencyStateCanceled, target.Error
		}
		resolved, err := applyRunDependencyResolution(result.Group, result.Dependencies, result.Source, RunDependencyResolutionRecord{
			Scope: record.Scope, GroupID: group.ID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision,
			State: state, Result: map[string]interface{}{"branchId": edge.ID, "checkpoint": cloneMap(target.Checkpoint), "output": cloneMap(target.Output)},
			Error: message, Actor: record.Actor, Visibility: record.Visibility, OccurredAt: record.OccurredAt,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, resolved.Events...)
		wake = wake || resolved.Evaluation.Wake
		result = resolved
	}
	result.Events, result.Evaluation.Wake = events, wake
	return result, nil
}
