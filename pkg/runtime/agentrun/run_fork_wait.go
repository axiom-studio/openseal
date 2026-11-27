package agentrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

// A wait group parks a source on child Runs it already has (wait_subagents,
// a follow-up to a subagent that is still running). Its edges target those
// Runs; it creates none. A child's own group is the one in its forkChild
// checkpoint, so CompleteChild also resolves the edge of the wait group its
// parent is parked on.

// WaitTarget is one child Run a wait group waits for; ID names its edge.
type WaitTarget struct {
	ID    string
	RunID string
}

type CreateRunWaitRequest struct {
	Scope                  models.Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	WorkerID               string
	// WaitID is stable per call (the tool entry), so a repeated settle
	// creates the group once.
	WaitID  string
	Mode    models.FanInMode
	Targets []WaitTarget
	// ToolTransition moves the source's tool entry to awaiting_child in the
	// same transaction.
	ToolTransition           *models.ToolEntryTransitionRequest
	RequireInterventionCount *int
	Actor                    models.ActivityActor
}

// WaitGroupID is the id of the wait group of waitID.
func WaitGroupID(sourceRunID, waitID string) string {
	return StableForkIdentifier(sourceRunID, "wait:"+waitID, "group")
}

// CreateWait parks the source on a new group over existing child Runs. A
// target that ended before the group existed is resolved right after the
// create, so the wait never outlives its last child.
func (c *RunForkCoordinator) CreateWait(ctx context.Context, req CreateRunWaitRequest) (*models.RunDependencyResult, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run fork store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.SourceRunID) == "" || strings.TrimSpace(req.WaitID) == "" || strings.TrimSpace(req.WorkerID) == "" || len(req.Targets) == 0 {
		return nil, errors.New("a wait needs its source Run, worker, wait id and targets")
	}
	if req.Mode != models.FanInModeAll && req.Mode != models.FanInModeAny {
		return nil, errors.New("waits support all or any fan-in")
	}
	source, err := c.store.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	groupID := WaitGroupID(source.ID, req.WaitID)
	key := "wait:" + source.ID + ":" + req.WaitID
	now := c.now().UTC()
	edges := make([]*models.RunDependency, 0, len(req.Targets))
	for _, target := range req.Targets {
		edges = append(edges, &models.RunDependency{
			ID: target.ID, Scope: source.Scope, GroupID: groupID, SourceRunID: source.ID, TargetRunID: target.RunID,
			Kind: models.RunDependencyKindRun, State: models.RunDependencyStateRunning, Required: true,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		})
	}
	group := &models.RunDependencyGroup{
		ID: groupID, Scope: source.Scope, SourceRunID: source.ID,
		Policy:        models.RunDependencyPolicy{Mode: req.Mode, FailureMode: models.DependencyFailureWait},
		ExpectedCount: len(edges), Status: models.RunDependencyGroupWaiting, Revision: 1,
		CreatedAt: now, UpdatedAt: now, SealedAt: &now, IdempotencyKey: key,
	}
	parked := models.CloneAgentRun(source)
	parked.Status = models.AgentRunStatusWaitingForDependency
	parked.WakeCondition = &models.WakeCondition{Type: "run_dependencies", Reference: groupID}
	parked.Revision++
	parked.UpdatedAt = now
	parked.LeaseOwner = ""
	parked.LeaseExpiresAt = nil
	actor := req.Actor
	if actor.Type == "" || actor.ID == "" {
		actor = models.ActivityActor{Type: "worker", ID: req.WorkerID}
	}
	event := &models.ActivityEvent{
		ID: StableForkIdentifier(source.ID, "wait:"+req.WaitID, "event"), Scope: source.Scope, RunID: source.ID,
		AgentID: source.AssignedAgentID, ObjectiveID: source.ObjectiveID, TeamID: models.TeamIDForRun(source),
		EventType: "run.waiting_for_children", Summary: fmt.Sprintf("Waiting for %d child Runs", len(edges)),
		Actor: actor, Visibility: models.ActivityVisibilityScope, CorrelationID: groupID,
		Payload: map[string]interface{}{"groupId": groupID, "mode": req.Mode, "childCount": len(edges)}, CreatedAt: now,
	}
	result, err := c.store.CreateRunDependencyGroup(ctx, models.RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, SourceRun: parked, ExpectedSourceRevision: req.ExpectedSourceRevision, Event: event,
		ToolTransition: req.ToolTransition, RequireInterventionCount: req.RequireInterventionCount,
	})
	if err != nil {
		return nil, err
	}
	for _, target := range req.Targets {
		child, err := c.store.GetAgentRun(ctx, req.Scope, target.RunID)
		if err != nil {
			return nil, err
		}
		if child == nil || !models.IsTerminalAgentRunStatus(child.Status) {
			continue
		}
		if _, err := c.resolveSourceWait(ctx, child, actor); err != nil && !errors.Is(err, models.ErrDependencyConflict) {
			return nil, err
		}
	}
	return result, nil
}

// resolveSourceWait resolves child's edge in the wait group its parent is
// parked on, when that is not the child's own group. It returns nil when the
// parent waits on nothing else for this child.
func (c *RunForkCoordinator) resolveSourceWait(ctx context.Context, child *models.AgentRun, actor models.ActivityActor) (*models.RunDependencyResult, error) {
	if child == nil || child.ParentRunID == "" || !models.IsTerminalAgentRunStatus(child.Status) {
		return nil, nil
	}
	parent, err := c.store.GetAgentRun(ctx, child.Scope, child.ParentRunID)
	if err != nil || parent == nil {
		return nil, err
	}
	condition := parent.WakeCondition
	if parent.Status == models.AgentRunStatusPaused {
		condition = parent.PausedWakeCondition
	}
	if condition == nil || condition.Type != "run_dependencies" || condition.Reference == "" {
		return nil, nil
	}
	if own, _ := child.Checkpoint["forkChild"].(map[string]interface{}); own != nil && own["groupId"] == condition.Reference {
		return nil, nil
	}
	if waiting, _ := models.SourceWaitsForDependencyGroup(parent, condition.Reference); !waiting {
		return nil, nil
	}
	group, err := c.store.GetRunDependencyGroup(ctx, child.Scope, condition.Reference)
	if err != nil || group == nil || group.SourceRunID != parent.ID || group.Status != models.RunDependencyGroupWaiting {
		return nil, err
	}
	edges, err := c.store.ListRunDependencies(ctx, child.Scope, group.ID)
	if err != nil {
		return nil, err
	}
	for _, edge := range edges {
		if edge.Kind != models.RunDependencyKindRun || edge.TargetRunID != child.ID ||
			edge.State != models.RunDependencyStatePending && edge.State != models.RunDependencyStateRunning {
			continue
		}
		state, message := childDependencyOutcome(child)
		if actor.Type == "" || actor.ID == "" {
			actor = models.ActivityActor{Type: "system", ID: "run-fork-coordinator"}
		}
		return portfolio.NewDependencyCoordinator(c.store, models.WithClock(c.now)).ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
			Scope: child.Scope, GroupID: group.ID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision,
			State: state, Result: map[string]interface{}{
				"branchId": edge.ID, "checkpoint": maps.Clone(child.Checkpoint), "output": maps.Clone(child.Output),
			}, Error: message, Actor: actor, Visibility: models.ActivityVisibilityScope,
		})
	}
	return nil, nil
}

// childDependencyOutcome is the edge state a terminal child resolves to.
func childDependencyOutcome(child *models.AgentRun) (models.RunDependencyState, string) {
	switch child.Status {
	case models.AgentRunStatusFailed:
		return models.RunDependencyStateFailed, child.Error
	case models.AgentRunStatusCanceled:
		return models.RunDependencyStateCanceled, child.Error
	}
	return models.RunDependencyStateSatisfied, ""
}
