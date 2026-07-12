package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/google/uuid"
)

const DelegationModeContextKey = "openseal.delegationMode"

type RunForkStore interface {
	PortfolioStore
	RunDependencyStore
	RunActivityStore
}

type RunForkBranch struct {
	ID              string                 `json:"id"`
	Goal            string                 `json:"goal"`
	AssignedAgentID string                 `json:"assignedAgentId,omitempty"`
	Context         map[string]interface{} `json:"context,omitempty"`
	Checkpoint      map[string]interface{} `json:"checkpoint"`
	Budget          *BudgetPolicy          `json:"budget,omitempty"`
	Timeout         time.Duration          `json:"timeout,omitempty"`
	Mode            runbook.DelegateMode   `json:"mode,omitempty"`
}

// TurnForkProposal is a proposal-only durable Turn output. The worker
// materializes it into a sealed dependency group and child Runs only after the
// Turn and continuation checkpoint have committed.
type TurnForkProposal struct {
	ForkID   string              `json:"forkId"`
	Policy   RunDependencyPolicy `json:"policy"`
	Branches []RunForkBranch     `json:"branches"`
}

type TurnDelegationProposal struct {
	StepID          string                 `json:"stepId"`
	AssignedAgentID string                 `json:"assignedAgentId"`
	Goal            string                 `json:"goal"`
	Context         map[string]interface{} `json:"context,omitempty"`
	Checkpoint      map[string]interface{} `json:"checkpoint"`
	Budget          *BudgetPolicy          `json:"budget,omitempty"`
	Timeout         time.Duration          `json:"timeout,omitempty"`
	Mode            runbook.DelegateMode   `json:"mode,omitempty"`
}

func (p *TurnDelegationProposal) Validate() error {
	if p == nil || strings.TrimSpace(p.StepID) == "" || strings.TrimSpace(p.AssignedAgentID) == "" || strings.TrimSpace(p.Goal) == "" || p.Timeout < 0 {
		return errors.New("delegation proposal requires step, assigned Agent, goal, and non-negative timeout")
	}
	if err := ValidateCredentialFreeContext(p.Context); err != nil {
		return err
	}
	if err := ValidateCredentialFreeContext(p.Checkpoint); err != nil {
		return err
	}
	if p.Budget != nil {
		return p.Budget.Validate()
	}
	if p.Mode != "" && p.Mode != runbook.DelegateBehavior && p.Mode != runbook.DelegateReason {
		return errors.New("delegation mode must be behavior or reason")
	}
	return nil
}

func (p *TurnForkProposal) Validate() error {
	if p == nil || strings.TrimSpace(p.ForkID) == "" || len(p.Branches) < 2 {
		return errors.New("fork proposal requires an ID and at least two branches")
	}
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	if p.Policy.Mode != FanInModeAll && p.Policy.Mode != FanInModeAny {
		return errors.New("fork proposal supports all or any fan-in")
	}
	seen := make(map[string]bool, len(p.Branches))
	for _, branch := range p.Branches {
		id := strings.TrimSpace(branch.ID)
		if id == "" || seen[id] || strings.TrimSpace(branch.Goal) == "" {
			return errors.New("fork proposal branches require unique IDs and goals")
		}
		seen[id] = true
		if err := ValidateCredentialFreeContext(branch.Checkpoint); err != nil {
			return fmt.Errorf("fork branch %s: %w", id, err)
		}
	}
	return nil
}

type CreateRunForkRequest struct {
	Scope                  Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	WorkerID               string
	ForkID                 string
	Policy                 RunDependencyPolicy
	Branches               []RunForkBranch
	ContinuationCheckpoint map[string]interface{}
	Actor                  ActivityActor
	Visibility             ActivityVisibility
}

type RunForkResult struct {
	DependencyGroup *RunDependencyResult `json:"dependencyGroup"`
	Children        []*AgentRun          `json:"children"`
}

type RunForkCoordinator struct {
	store RunForkStore
	now   func() time.Time
}

func NewRunForkCoordinator(store RunForkStore) *RunForkCoordinator {
	return &RunForkCoordinator{store: store, now: time.Now}
}

// Create atomically seals the complete fan-out, persists every child Run, and
// suspends the source Run. Stable IDs make a crash after commit replay-safe.
func (c *RunForkCoordinator) Create(ctx context.Context, req CreateRunForkRequest) (*RunForkResult, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run fork store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.SourceRunID) == "" || req.ExpectedSourceRevision < 1 || strings.TrimSpace(req.WorkerID) == "" || strings.TrimSpace(req.ForkID) == "" || len(req.Branches) < 1 {
		return nil, errors.New("source Run, revision, worker, fork, and at least one branch are required")
	}
	if err := req.Policy.Validate(); err != nil {
		return nil, err
	}
	if req.Policy.Mode != FanInModeAll && req.Policy.Mode != FanInModeAny {
		return nil, errors.New("run forks support all or any fan-in")
	}
	if err := ValidateCredentialFreeContext(req.ContinuationCheckpoint); err != nil {
		return nil, err
	}
	source, err := c.store.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrRunNotFound
	}
	allocations := make([]*BudgetPolicy, len(req.Branches))
	seen := make(map[string]bool, len(req.Branches))
	for index, branch := range req.Branches {
		branch.ID = strings.TrimSpace(branch.ID)
		if branch.ID == "" || seen[branch.ID] || strings.TrimSpace(branch.Goal) == "" {
			return nil, errors.New("fork branches require unique IDs and goals")
		}
		seen[branch.ID] = true
		if err := ValidateCredentialFreeContext(branch.Checkpoint); err != nil {
			return nil, fmt.Errorf("branch %s checkpoint: %w", branch.ID, err)
		}
		if err := ValidateCredentialFreeContext(branch.Context); err != nil {
			return nil, fmt.Errorf("branch %s context: %w", branch.ID, err)
		}
		if branch.Timeout < 0 {
			return nil, fmt.Errorf("branch %s timeout cannot be negative", branch.ID)
		}
		if branch.Mode != "" && branch.Mode != runbook.DelegateBehavior && branch.Mode != runbook.DelegateReason {
			return nil, fmt.Errorf("branch %s delegation mode is invalid", branch.ID)
		}
		allocations[index] = branch.Budget
	}
	key := "fork:" + source.ID + ":" + req.ForkID
	existing, err := c.store.FindRunDependencyGroupByIdempotencyKey(ctx, req.Scope, key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return c.replay(ctx, source, existing, req)
	}
	if source.Revision != req.ExpectedSourceRevision {
		return nil, ErrRevisionConflict
	}
	if source.Status != AgentRunStatusRunning || source.LeaseOwner != req.WorkerID {
		return nil, ErrLeaseLost
	}
	if err := validateGroupedBudgetAllocations(source, allocations); err != nil {
		return nil, err
	}
	now := c.now().UTC()
	groupID := stableForkIdentifier(source.ID, req.ForkID, "group")
	children := make([]*AgentRun, 0, len(req.Branches))
	edges := make([]*RunDependency, 0, len(req.Branches))
	for _, branch := range req.Branches {
		childID := stableForkIdentifier(source.ID, req.ForkID, "child:"+branch.ID)
		assignedAgentID := strings.TrimSpace(branch.AssignedAgentID)
		childContext := cloneMap(branch.Context)
		var childPlan map[string]interface{}
		if assignedAgentID == "" {
			assignedAgentID = source.AssignedAgentID
			childContext = cloneMap(source.Context)
			childPlan = cloneMap(source.Plan)
		}
		if branch.Mode != "" {
			if childContext == nil {
				childContext = map[string]interface{}{}
			}
			childContext[DelegationModeContextKey] = string(branch.Mode)
		}
		childCheckpoint := cloneMap(branch.Checkpoint)
		if childCheckpoint == nil {
			childCheckpoint = map[string]interface{}{}
		}
		childCheckpoint["forkChild"] = map[string]interface{}{
			"sourceRunId": source.ID, "groupId": groupID, "dependencyId": branch.ID, "forkId": req.ForkID,
		}
		deadline := source.Deadline
		if branch.Timeout > 0 {
			candidate := now.Add(branch.Timeout)
			if deadline == nil || candidate.Before(*deadline) {
				deadline = &candidate
			}
		}
		child, err := buildAgentRun(ctx, c.store, CreateAgentRunRequest{
			Kind: source.Kind, Scope: source.Scope, ObjectiveID: source.ObjectiveID, ParentRunID: source.ID,
			Owner: source.Owner, AssignedAgentID: assignedAgentID,
			ConcurrencyKey: "fork:" + groupID + ":" + branch.ID,
			Goal:           strings.TrimSpace(branch.Goal), Source: RunSourceFork, Priority: source.Priority, Deadline: deadline,
			Context: childContext, Plan: childPlan, Checkpoint: childCheckpoint,
			Budget: cloneBudgetPolicy(branch.Budget), Policy: cloneMap(source.Policy),
		}, childID, now)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
		edges = append(edges, &RunDependency{
			ID: branch.ID, Scope: source.Scope, GroupID: groupID, SourceRunID: source.ID, TargetRunID: child.ID,
			Kind: RunDependencyKindRun, State: RunDependencyStateRunning, Required: true,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		})
	}
	sealedAt := now
	group := &RunDependencyGroup{
		ID: groupID, Scope: source.Scope, SourceRunID: source.ID, Policy: req.Policy,
		ExpectedCount: len(edges), Status: RunDependencyGroupWaiting, Revision: 1,
		CreatedAt: now, UpdatedAt: now, SealedAt: &sealedAt,
		IdempotencyKey: key,
	}
	updatedSource := cloneAgentRun(source)
	updatedSource.Status = AgentRunStatusWaitingForDependency
	updatedSource.WakeCondition = &WakeCondition{Type: "run_dependencies", Reference: group.ID}
	updatedSource.Checkpoint = cloneMap(req.ContinuationCheckpoint)
	updatedSource.Revision++
	updatedSource.UpdatedAt = now
	updatedSource.LeaseOwner = ""
	updatedSource.LeaseExpiresAt = nil
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	actor := req.Actor
	if actor.Type == "" || actor.ID == "" {
		actor = ActivityActor{Type: "worker", ID: req.WorkerID}
	}
	event := &ActivityEvent{
		ID: stableForkIdentifier(source.ID, req.ForkID, "event"), Scope: source.Scope, RunID: source.ID,
		AgentID: source.AssignedAgentID, ObjectiveID: source.ObjectiveID, TeamID: teamIDForRun(source),
		EventType: "run.forked", Summary: fmt.Sprintf("Forked %d concurrent child Runs", len(children)),
		Actor: actor, Visibility: visibility, CorrelationID: group.ID,
		Payload: map[string]interface{}{"forkId": req.ForkID, "groupId": group.ID, "mode": req.Policy.Mode, "childCount": len(children)}, CreatedAt: now,
	}
	result, err := c.store.CreateRunDependencyGroup(ctx, RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, TargetRuns: children, SourceRun: updatedSource,
		ExpectedSourceRevision: source.Revision, Event: event,
	})
	if err != nil {
		return nil, err
	}
	if result.Replayed {
		children = children[:0]
		for _, edge := range result.Dependencies {
			child, childErr := c.store.GetAgentRun(ctx, req.Scope, edge.TargetRunID)
			if childErr != nil {
				return nil, childErr
			}
			children = append(children, child)
		}
	}
	return &RunForkResult{DependencyGroup: result, Children: children}, nil
}

func (c *RunForkCoordinator) replay(ctx context.Context, source *AgentRun, group *RunDependencyGroup, req CreateRunForkRequest) (*RunForkResult, error) {
	if group.SourceRunID != source.ID || group.Policy != req.Policy || group.ExpectedCount != len(req.Branches) || group.ID != stableForkIdentifier(source.ID, req.ForkID, "group") {
		return nil, ErrDependencyConflict
	}
	edges, err := c.store.ListRunDependencies(ctx, req.Scope, group.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*RunDependency, len(edges))
	for _, edge := range edges {
		byID[edge.ID] = edge
	}
	children := make([]*AgentRun, 0, len(req.Branches))
	for _, branch := range req.Branches {
		edge := byID[branch.ID]
		expectedChild := stableForkIdentifier(source.ID, req.ForkID, "child:"+branch.ID)
		if edge == nil || edge.Kind != RunDependencyKindRun || edge.TargetRunID != expectedChild {
			return nil, ErrDependencyConflict
		}
		child, childErr := c.store.GetAgentRun(ctx, req.Scope, edge.TargetRunID)
		if childErr != nil || child == nil {
			if childErr == nil {
				childErr = ErrRunNotFound
			}
			return nil, childErr
		}
		children = append(children, child)
	}
	evaluation, err := EvaluateRunDependencies(group, edges)
	if err != nil {
		return nil, err
	}
	return &RunForkResult{DependencyGroup: &RunDependencyResult{Group: group, Dependencies: edges, Source: source, Evaluation: evaluation, Replayed: true}, Children: children}, nil
}

func stableForkIdentifier(runID, forkID, suffix string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal:fork:"+runID+":"+forkID+":"+suffix)).String()
}

// CompleteChild resolves a branch edge from the child Run's durable terminal
// state. For join_any it also cancels and resolves every losing child so no
// second wake or unnecessary background work survives the winner.
func (c *RunForkCoordinator) CompleteChild(ctx context.Context, child *AgentRun, actor ActivityActor) (*RunDependencyResult, error) {
	if c == nil || c.store == nil || child == nil || !isTerminalAgentRunStatus(child.Status) {
		return nil, errors.New("terminal fork child is required")
	}
	metadata, ok := child.Checkpoint["forkChild"].(map[string]interface{})
	if !ok {
		return nil, errors.New("Run is not a fork child")
	}
	groupID, _ := metadata["groupId"].(string)
	dependencyID, _ := metadata["dependencyId"].(string)
	sourceRunID, _ := metadata["sourceRunId"].(string)
	if groupID == "" || dependencyID == "" || sourceRunID != child.ParentRunID {
		return nil, ErrInvalidRunDependency
	}
	group, err := c.store.GetRunDependencyGroup(ctx, child.Scope, groupID)
	if err != nil || group == nil {
		if err == nil {
			err = ErrDependencyGroupNotFound
		}
		return nil, err
	}
	edges, err := c.store.ListRunDependencies(ctx, child.Scope, group.ID)
	if err != nil {
		return nil, err
	}
	var edge *RunDependency
	for _, candidate := range edges {
		if candidate.ID == dependencyID && candidate.TargetRunID == child.ID {
			edge = candidate
			break
		}
	}
	if edge == nil {
		return nil, ErrRunDependencyNotFound
	}
	state := RunDependencyStateSatisfied
	errorMessage := ""
	if child.Status == AgentRunStatusFailed {
		state, errorMessage = RunDependencyStateFailed, child.Error
	} else if child.Status == AgentRunStatusCanceled {
		state, errorMessage = RunDependencyStateCanceled, child.Error
	}
	if actor.Type == "" || actor.ID == "" {
		actor = ActivityActor{Type: "system", ID: "run-fork-coordinator"}
	}
	result, err := NewDependencyCoordinator(c.store).ResolveRunDependency(ctx, ResolveRunDependencyRequest{
		Scope: child.Scope, GroupID: group.ID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision,
		State: state, Result: map[string]interface{}{
			"branchId": dependencyID, "checkpoint": cloneMap(child.Checkpoint), "output": cloneMap(child.Output),
		}, Error: errorMessage, Actor: actor, Visibility: ActivityVisibilityScope,
	})
	if err != nil {
		return nil, err
	}
	if result.Evaluation.Wake && result.Group.Policy.Mode == FanInModeAny && result.Group.Status == RunDependencyGroupSatisfied {
		if err := c.cancelLosingChildren(ctx, result, child.ID, actor); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (c *RunForkCoordinator) cancelLosingChildren(ctx context.Context, winner *RunDependencyResult, winnerID string, actor ActivityActor) error {
	activity := NewRunActivityService(c.store, c.store)
	for _, edge := range winner.Dependencies {
		if edge.TargetRunID == winnerID || edge.State == RunDependencyStateSatisfied || edge.State == RunDependencyStateFailed || edge.State == RunDependencyStateCanceled {
			continue
		}
		child, err := c.store.GetAgentRun(ctx, edge.Scope, edge.TargetRunID)
		if err != nil || child == nil {
			if err == nil {
				err = ErrRunNotFound
			}
			return err
		}
		if !isTerminalAgentRunStatus(child.Status) {
			child, _, err = activity.TransitionRun(ctx, child.Scope, child.ID, RunTransitionRequest{
				ExpectedRevision: child.Revision, Status: AgentRunStatusCanceled,
				Error: "canceled after another fork branch satisfied join_any", Actor: actor,
				EventType: "run.fork_loser_canceled", Summary: "Canceled losing join_any branch", CorrelationID: winner.Group.ID,
			})
			if err != nil {
				return err
			}
		}
		if _, err := c.CompleteChild(ctx, child, actor); err != nil {
			return err
		}
	}
	return nil
}
