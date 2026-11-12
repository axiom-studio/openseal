package agentrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/google/uuid"
)

const DelegationModeContextKey = "openseal.delegationMode"

// RunbookInvocationContextKey is runtime-owned context that preserves the
// caller's intent across a callable Runbook and any Agent work it delegates.
// Runbooks remain deterministic and keep their reviewed static goals, while
// delegated reasoning can also honor the exact invocation that started them.
const RunbookInvocationContextKey = "openseal.runbookInvocation"

type RunForkBranch struct {
	ID              string                 `json:"id"`
	Goal            string                 `json:"goal"`
	AssignedAgentID string                 `json:"assignedAgentId,omitempty"`
	Entrypoint      string                 `json:"entrypoint,omitempty"`
	Context         map[string]interface{} `json:"context,omitempty"`
	Checkpoint      map[string]interface{} `json:"checkpoint"`
	Budget          *models.BudgetPolicy   `json:"budget,omitempty"`
	Timeout         time.Duration          `json:"timeout,omitempty"`
	Mode            runbook.DelegateMode   `json:"mode,omitempty"`
}

// TurnRunbookProposal invokes one exact callable entrypoint on the current
// Agent as a durable child Run. The parent remains cognitive and resumes with
// the deterministic result through normal dependency fan-in.
type TurnRunbookProposal struct {
	Entrypoint string                 `json:"entrypoint"`
	Summary    string                 `json:"summary"`
	Arguments  map[string]interface{} `json:"arguments,omitempty"`
	Budget     *models.BudgetPolicy   `json:"budget,omitempty"`
}

func (p *TurnRunbookProposal) Validate() error {
	if p == nil || !models.IsValidOpaqueIdentifier(p.Entrypoint, 128) || strings.TrimSpace(p.Summary) == "" {
		return errors.New("runbook proposal requires an exact entrypoint and summary")
	}
	if err := models.ValidateCredentialFreeContext(p.Arguments); err != nil {
		return err
	}
	if p.Budget != nil {
		return p.Budget.Validate()
	}
	return nil
}

// TurnForkProposal is a proposal-only durable Turn output. The worker
// materializes it into a sealed dependency group and child Runs only after the
// Turn and continuation checkpoint have committed.
type TurnForkProposal struct {
	ForkID   string                     `json:"forkId"`
	Policy   models.RunDependencyPolicy `json:"policy"`
	Branches []RunForkBranch            `json:"branches"`
}

func (p *TurnForkProposal) Validate() error {
	if p == nil || !models.IsValidOpaqueIdentifier(p.ForkID, 128) || len(p.Branches) < 2 {
		return errors.New("fork proposal requires an ID and at least two branches")
	}
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	if p.Policy.Mode != models.FanInModeAll && p.Policy.Mode != models.FanInModeAny {
		return errors.New("fork proposal supports all or any fan-in")
	}
	seen := make(map[string]bool, len(p.Branches))
	for _, branch := range p.Branches {
		id := strings.TrimSpace(branch.ID)
		if id == "" || seen[id] || strings.TrimSpace(branch.Goal) == "" {
			return errors.New("fork proposal branches require unique IDs and goals")
		}
		if !models.IsValidOpaqueIdentifier(id, 128) || branch.AssignedAgentID != "" && !models.IsValidOpaqueIdentifier(branch.AssignedAgentID, 128) {
			return errors.New("fork proposal branch and assigned Agent IDs must be portable opaque identifiers")
		}
		if branch.Entrypoint != "" && !models.IsValidOpaqueIdentifier(branch.Entrypoint, 128) {
			return errors.New("fork proposal branch entrypoint must be a portable opaque identifier")
		}
		seen[id] = true
		if err := models.ValidateCredentialFreeContext(branch.Context); err != nil {
			return fmt.Errorf("fork branch %s context: %w", id, err)
		}
		if err := models.ValidateCredentialFreeContext(branch.Checkpoint); err != nil {
			return fmt.Errorf("fork branch %s: %w", id, err)
		}
		if branch.Budget != nil {
			if err := branch.Budget.Validate(); err != nil {
				return fmt.Errorf("fork branch %s budget: %w", id, err)
			}
		}
		if branch.Timeout < 0 {
			return fmt.Errorf("fork branch %s timeout cannot be negative", id)
		}
		if branch.Mode != "" && branch.Mode != runbook.DelegateBehavior && branch.Mode != runbook.DelegateReason {
			return fmt.Errorf("fork branch %s delegation mode is invalid", id)
		}
	}
	return nil
}

type CreateRunForkRequest struct {
	Scope                  models.Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	WorkerID               string
	ForkID                 string
	Policy                 models.RunDependencyPolicy
	Branches               []RunForkBranch
	ContinuationCheckpoint map[string]interface{}
	// ChildKind is the Kind of every child; empty keeps the source rule
	// (source.Kind, or agent work for callable entrypoints and task work).
	// Model forks pass RunKindAgentWork (DI-16).
	ChildKind models.RunKind
	// SessionForks maps a branch ID to the source session point its
	// same-Agent child forks from (A3). The child session, seeded with the
	// child's brief, is created in the same transaction as the child Run.
	// Branches without a ref (another Agent, deterministic children) get a
	// fresh session at their first claim.
	SessionForks map[string]*SessionForkRef
	// ToolTransition moves the source's control tool entry to awaiting_child
	// in the same transaction (control_fork, run_operation).
	ToolTransition *models.ToolEntryTransitionRequest
	// RequireInterventionCount refuses the create with ErrSteeringArrived
	// unless the source still has exactly that many PendingInterventions.
	RequireInterventionCount *int
	Actor                    models.ActivityActor
	Visibility               models.ActivityVisibility
}

type RunForkResult struct {
	DependencyGroup *models.RunDependencyResult `json:"dependencyGroup"`
	Children        []*models.AgentRun          `json:"children"`
}

type RunForkCoordinator struct {
	store             models.RunForkStore
	executionPreparer models.AcceptedRunExecutionPreparer
	now               func() time.Time
}

func NewRunForkCoordinator(store models.RunForkStore, opts ...models.ServiceOption) *RunForkCoordinator {
	return &RunForkCoordinator{store: store, now: models.ClockFrom(opts)}
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
	if req.Policy.Mode != models.FanInModeAll && req.Policy.Mode != models.FanInModeAny {
		return nil, errors.New("run forks support all or any fan-in")
	}
	if err := models.ValidateCredentialFreeContext(req.ContinuationCheckpoint); err != nil {
		return nil, err
	}
	for branchID, ref := range req.SessionForks {
		if ref == nil || strings.TrimSpace(ref.ParentSessionID) == "" || ref.ForkSequence < 1 {
			return nil, fmt.Errorf("branch %s session fork needs a parent session and a fork sequence", branchID)
		}
		known := false
		for _, branch := range req.Branches {
			known = known || strings.TrimSpace(branch.ID) == branchID
		}
		if !known {
			return nil, fmt.Errorf("session fork names unknown branch %s", branchID)
		}
	}
	source, err := c.store.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	allocations := make([]*models.BudgetPolicy, len(req.Branches))
	seen := make(map[string]bool, len(req.Branches))
	for index, branch := range req.Branches {
		branch.ID = strings.TrimSpace(branch.ID)
		if branch.ID == "" || seen[branch.ID] || strings.TrimSpace(branch.Goal) == "" {
			return nil, errors.New("fork branches require unique IDs and goals")
		}
		seen[branch.ID] = true
		if err := models.ValidateCredentialFreeContext(branch.Checkpoint); err != nil {
			return nil, fmt.Errorf("branch %s checkpoint: %w", branch.ID, err)
		}
		if err := models.ValidateCredentialFreeContext(branch.Context); err != nil {
			return nil, fmt.Errorf("branch %s context: %w", branch.ID, err)
		}
		if branch.Timeout < 0 {
			return nil, fmt.Errorf("branch %s timeout cannot be negative", branch.ID)
		}
		if branch.Entrypoint != "" && !models.IsValidOpaqueIdentifier(branch.Entrypoint, 128) {
			return nil, fmt.Errorf("branch %s entrypoint is invalid", branch.ID)
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
		return nil, models.ErrRevisionConflict
	}
	if source.Status != models.AgentRunStatusRunning || source.LeaseOwner != req.WorkerID {
		return nil, models.ErrLeaseLost
	}
	if err := models.ValidateGroupedBudgetAllocations(source, allocations); err != nil {
		return nil, err
	}
	sourceTask, err := c.persistedForkTaskOrigin(ctx, source)
	if err != nil {
		return nil, err
	}
	if sourceTask != nil {
		for _, branch := range req.Branches {
			assignedAgentID := strings.TrimSpace(branch.AssignedAgentID)
			if assignedAgentID != "" && assignedAgentID != source.AssignedAgentID {
				return nil, models.ErrInvalidConversationTask
			}
		}
	}
	now := c.now().UTC()
	groupID := StableForkIdentifier(source.ID, req.ForkID, "group")
	children := make([]*models.AgentRun, 0, len(req.Branches))
	edges := make([]*models.RunDependency, 0, len(req.Branches))
	var childSessions []models.CreateRunAgentSessionTxRequest
	for _, branch := range req.Branches {
		branch.ID = strings.TrimSpace(branch.ID)
		childID := StableForkIdentifier(source.ID, req.ForkID, "child:"+branch.ID)
		assignedAgentID := strings.TrimSpace(branch.AssignedAgentID)
		implicitAgent := assignedAgentID == ""
		childContext := maps.Clone(branch.Context)
		var childPlan map[string]interface{}
		if assignedAgentID == "" {
			assignedAgentID = source.AssignedAgentID
			// The branch runs as the source Agent with the source's context;
			// its own data is added, but never in place of the source's
			// lineage keys.
			var overlayErr error
			if childContext, overlayErr = overlayForkBranchContext(source.Context, branch.Context); overlayErr != nil {
				return nil, fmt.Errorf("branch %s: %w", branch.ID, overlayErr)
			}
			childPlan = maps.Clone(source.Plan)
		}
		if sourceTask != nil && assignedAgentID == source.AssignedAgentID {
			childContext = maps.Clone(source.Context)
			childPlan = maps.Clone(source.Plan)
			if childContext == nil {
				childContext = map[string]interface{}{}
			}
			for key, value := range branch.Context {
				if immutableForkTaskContextKey(key) {
					if !reflect.DeepEqual(value, childContext[key]) {
						return nil, models.ErrInvalidConversationTask
					}
					continue
				}
				childContext[key] = cloneForkContextValue(value)
			}
			childContext[models.ConversationTaskContextKey] = sourceTask.ID
		} else if !implicitAgent {
			// Task lineage remains an authority checked through the persisted
			// root. A branch cannot manufacture it in caller-owned context or
			// transfer the source Agent's private context to another Agent.
			delete(childContext, models.ConversationTaskContextKey)
		}
		if branch.Mode != "" {
			if childContext == nil {
				childContext = map[string]interface{}{}
			}
			childContext[DelegationModeContextKey] = string(branch.Mode)
		}
		childContext = models.InheritRoutineEventBatch(source, childContext)
		childCheckpoint := maps.Clone(branch.Checkpoint)
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
		entrypoint := strings.TrimSpace(branch.Entrypoint)
		if branch.Mode == runbook.DelegateReason && entrypoint != "" {
			return nil, models.ErrAcceptedRunExecution
		}
		if implicitAgent && entrypoint == "" && branch.Mode != runbook.DelegateReason {
			entrypoint = source.Entrypoint
		}
		childKind := source.Kind
		if entrypoint != "" || sourceTask != nil {
			childKind = models.RunKindAgentWork
		}
		if req.ChildKind != "" {
			childKind = req.ChildKind
		}
		sessionFork := req.SessionForks[branch.ID]
		if sessionFork != nil && assignedAgentID != source.AssignedAgentID {
			// Another Agent never inherits the source Agent's session (DI-5).
			return nil, fmt.Errorf("branch %s: only a same-Agent child can fork the source session", branch.ID)
		}
		childRequest := models.CreateAgentRunRequest{
			Kind: childKind, Scope: source.Scope, ObjectiveID: source.ObjectiveID, ParentRunID: source.ID,
			Owner: source.Owner, AssignedAgentID: assignedAgentID, ActingUser: source.ActingUser, Entrypoint: entrypoint,
			ConcurrencyKey: "fork:" + groupID + ":" + branch.ID,
			Goal:           strings.TrimSpace(branch.Goal), Source: models.RunSourceFork, Priority: source.Priority, Deadline: deadline,
			Context: childContext, Plan: childPlan, Checkpoint: childCheckpoint,
			Budget: models.CloneBudgetPolicy(branch.Budget), Policy: maps.Clone(source.Policy),
		}
		if sessionFork != nil {
			childRequest.SessionID = models.RunAgentSessionID(source.Scope, childID)
		}
		inherit := entrypoint != "" && assignedAgentID == source.AssignedAgentID && entrypoint == source.Entrypoint
		if err := PrepareAcceptedChildRun(ctx, source, &childRequest, c.executionPreparer, inherit); err != nil {
			return nil, err
		}
		child, err := portfolio.BuildAgentRun(ctx, c.store, childRequest, childID, now)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
		if sessionFork != nil {
			seed, err := ControlChildSeed(child)
			if err != nil {
				return nil, fmt.Errorf("branch %s seed: %w", branch.ID, err)
			}
			session, err := models.NewChildSession(sessionFork.childSessionRequest(source.Scope, child.ID, assignedAgentID, seed))
			if err != nil {
				return nil, fmt.Errorf("branch %s session: %w", branch.ID, err)
			}
			session.Now = now
			childSessions = append(childSessions, session)
		}
		edges = append(edges, &models.RunDependency{
			ID: branch.ID, Scope: source.Scope, GroupID: groupID, SourceRunID: source.ID, TargetRunID: child.ID,
			Kind: models.RunDependencyKindRun, State: models.RunDependencyStateRunning, Required: true,
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		})
	}
	sealedAt := now
	group := &models.RunDependencyGroup{
		ID: groupID, Scope: source.Scope, SourceRunID: source.ID, Policy: req.Policy,
		ExpectedCount: len(edges), Status: models.RunDependencyGroupWaiting, Revision: 1,
		CreatedAt: now, UpdatedAt: now, SealedAt: &sealedAt,
		IdempotencyKey: key,
	}
	updatedSource := models.CloneAgentRun(source)
	for index, child := range children {
		if err := models.AddRunBudgetAllocation(updatedSource, child.ID, req.Branches[index].Budget); err != nil {
			return nil, err
		}
	}
	updatedSource.Status = models.AgentRunStatusWaitingForDependency
	updatedSource.WakeCondition = &models.WakeCondition{Type: "run_dependencies", Reference: group.ID}
	updatedSource.Checkpoint = maps.Clone(req.ContinuationCheckpoint)
	updatedSource.Revision++
	updatedSource.UpdatedAt = now
	updatedSource.LeaseOwner = ""
	updatedSource.LeaseExpiresAt = nil
	visibility := req.Visibility
	if visibility == "" {
		visibility = models.ActivityVisibilityScope
	}
	actor := req.Actor
	if actor.Type == "" || actor.ID == "" {
		actor = models.ActivityActor{Type: "worker", ID: req.WorkerID}
	}
	event := &models.ActivityEvent{
		ID: StableForkIdentifier(source.ID, req.ForkID, "event"), Scope: source.Scope, RunID: source.ID,
		AgentID: source.AssignedAgentID, ObjectiveID: source.ObjectiveID, TeamID: models.TeamIDForRun(source),
		EventType: "run.forked", Summary: fmt.Sprintf("Forked %d concurrent child Runs", len(children)),
		Actor: actor, Visibility: visibility, CorrelationID: group.ID,
		Payload: map[string]interface{}{"forkId": req.ForkID, "groupId": group.ID, "mode": req.Policy.Mode, "childCount": len(children)}, CreatedAt: now,
	}
	result, err := c.store.CreateRunDependencyGroup(ctx, models.RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, TargetRuns: children, SourceRun: updatedSource,
		ExpectedSourceRevision: source.Revision, Event: event,
		ToolTransition: req.ToolTransition, ChildSessions: childSessions, RequireInterventionCount: req.RequireInterventionCount,
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

// overlayForkBranchContext is the context of a branch that runs as the
// source Agent: the source's context plus the branch's own data. A branch
// cannot replace a lineage key of its source.
func overlayForkBranchContext(source, branch map[string]interface{}) (map[string]interface{}, error) {
	out := maps.Clone(source)
	if len(branch) == 0 {
		return out, nil
	}
	if out == nil {
		out = map[string]interface{}{}
	}
	for key, value := range branch {
		if strings.HasPrefix(key, "_openseal") || strings.HasPrefix(key, "openseal.") {
			return nil, fmt.Errorf("branch context cannot set the kernel key %s", key)
		}
		if immutableForkTaskContextKey(key) {
			if existing, ok := out[key]; !ok || !reflect.DeepEqual(value, existing) {
				return nil, fmt.Errorf("branch context cannot set its source's %s", key)
			}
			continue
		}
		out[key] = cloneForkContextValue(value)
	}
	return out, nil
}

// cloneForkContextValue deep-copies the JSON-shaped values a branch adds to
// its child's context, so the child never aliases its source's maps or slices.
func cloneForkContextValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		if typed == nil {
			return typed
		}
		result := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			result[key] = cloneForkContextValue(item)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, item := range typed {
			result[index] = cloneForkContextValue(item)
		}
		return result
	case []map[string]interface{}:
		result := make([]interface{}, len(typed))
		for index, item := range typed {
			result[index] = cloneForkContextValue(item)
		}
		return result
	default:
		return typed
	}
}

func immutableForkTaskContextKey(key string) bool {
	switch key {
	case models.ConversationTaskContextKey, models.ConversationRunConversationIDContextKey, models.ConversationRunTriggerIDContextKey,
		"threadRootMessageId", models.RunReportingRootRunIDContextKey, models.RunReportingMilestonesContextKey:
		return true
	default:
		return false
	}
}

func (c *RunForkCoordinator) persistedForkTaskOrigin(ctx context.Context, source *models.AgentRun) (*models.ConversationTask, error) {
	return PersistedTaskWorkOrigin(ctx, c.store, source)
}

func PersistedTaskWorkOrigin(ctx context.Context, store models.PortfolioStore, source *models.AgentRun) (*models.ConversationTask, error) {
	_, hinted := source.Context[models.ConversationTaskContextKey]
	taskLike := hinted || strings.HasPrefix(source.ConcurrencyKey, "task:")
	tasks, ok := store.(models.ConversationTaskStore)
	if !ok {
		if taskLike {
			return nil, models.ErrInvalidConversationTask
		}
		return nil, nil
	}
	rootID := source.RootRunID
	if rootID == "" {
		rootID = source.ID
	}
	task, err := PersistedTaskOrigin(ctx, tasks, source.Scope, rootID)
	if errors.Is(err, ErrConversationTaskNotFound) && !taskLike {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	root, err := store.GetAgentRun(ctx, source.Scope, task.WorkRunID)
	if err != nil {
		return nil, err
	}
	if !models.IsValidConversationTaskAdmissionRoot(root, task) {
		return nil, models.ErrInvalidConversationTask
	}
	if task.Mode == models.ConversationTaskModeContinuation {
		proof, ok := store.(models.ConversationTaskProofStore)
		if !ok {
			return nil, models.ErrInvalidConversationTask
		}
		valid, err := VerifyConversationTaskWorkRun(ctx, proof, task, root)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, models.ErrInvalidConversationTask
		}
	}
	current := source
	seen := make(map[string]bool)
	for depth := 0; depth < 64 && current != nil; depth++ {
		if seen[current.ID] || current.Scope != task.Scope || current.Owner != task.Owner || current.AssignedAgentID != root.AssignedAgentID ||
			current.RootRunID != task.WorkRunID {
			return nil, models.ErrInvalidConversationTask
		}
		if value, exists := current.Context[models.ConversationTaskContextKey]; exists && value != task.ID {
			return nil, models.ErrInvalidConversationTask
		}
		if current.ID == task.WorkRunID {
			if !models.ConversationTaskMatchesWorkRun(task, current) {
				return nil, models.ErrInvalidConversationTask
			}
			return task, nil
		}
		if current.Kind != models.RunKindAgentWork || current.ParentRunID == "" || current.Source != models.RunSourceFork {
			return nil, models.ErrInvalidConversationTask
		}
		seen[current.ID] = true
		current, err = store.GetAgentRun(ctx, task.Scope, current.ParentRunID)
		if err != nil {
			return nil, err
		}
	}
	return nil, models.ErrInvalidConversationTask
}

func (c *RunForkCoordinator) replay(ctx context.Context, source *models.AgentRun, group *models.RunDependencyGroup, req CreateRunForkRequest) (*RunForkResult, error) {
	if group.SourceRunID != source.ID || group.Policy != req.Policy || group.ExpectedCount != len(req.Branches) || group.ID != StableForkIdentifier(source.ID, req.ForkID, "group") {
		return nil, models.ErrDependencyConflict
	}
	edges, err := c.store.ListRunDependencies(ctx, req.Scope, group.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*models.RunDependency, len(edges))
	for _, edge := range edges {
		byID[edge.ID] = edge
	}
	children := make([]*models.AgentRun, 0, len(req.Branches))
	for _, branch := range req.Branches {
		edge := byID[branch.ID]
		expectedChild := StableForkIdentifier(source.ID, req.ForkID, "child:"+branch.ID)
		if edge == nil || edge.Kind != models.RunDependencyKindRun || edge.TargetRunID != expectedChild {
			return nil, models.ErrDependencyConflict
		}
		child, childErr := c.store.GetAgentRun(ctx, req.Scope, edge.TargetRunID)
		if childErr != nil || child == nil {
			if childErr == nil {
				childErr = models.ErrRunNotFound
			}
			return nil, childErr
		}
		children = append(children, child)
	}
	evaluation, err := models.EvaluateRunDependencies(group, edges)
	if err != nil {
		return nil, err
	}
	return &RunForkResult{DependencyGroup: &models.RunDependencyResult{Group: group, Dependencies: edges, Source: source, Evaluation: evaluation, Replayed: true}, Children: children}, nil
}

func StableForkIdentifier(runID, forkID, suffix string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal:fork:"+runID+":"+forkID+":"+suffix)).String()
}

// CompleteChild resolves a branch edge from the child Run's durable terminal
// state. For join_any it also cancels and resolves every losing child so no
// second wake or unnecessary background work survives the winner.
func (c *RunForkCoordinator) CompleteChild(ctx context.Context, child *models.AgentRun, actor models.ActivityActor) (*models.RunDependencyResult, error) {
	return c.completeChild(ctx, child, actor, true)
}

func (c *RunForkCoordinator) completeChild(ctx context.Context, child *models.AgentRun, actor models.ActivityActor, cancelLosers bool) (*models.RunDependencyResult, error) {
	if c == nil || c.store == nil || child == nil || !models.IsTerminalAgentRunStatus(child.Status) {
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
		return nil, models.ErrInvalidRunDependency
	}
	group, err := c.store.GetRunDependencyGroup(ctx, child.Scope, groupID)
	if err != nil || group == nil {
		if err == nil {
			err = models.ErrDependencyGroupNotFound
		}
		return nil, err
	}
	edges, err := c.store.ListRunDependencies(ctx, child.Scope, group.ID)
	if err != nil {
		return nil, err
	}
	var edge *models.RunDependency
	for _, candidate := range edges {
		if candidate.ID == dependencyID && candidate.TargetRunID == child.ID {
			edge = candidate
			break
		}
	}
	if edge == nil {
		return nil, models.ErrRunDependencyNotFound
	}
	if !ownedForkChild(group, edge, child) {
		return nil, models.ErrInvalidRunDependency
	}
	state := models.RunDependencyStateSatisfied
	errorMessage := ""
	if child.Status == models.AgentRunStatusFailed {
		state, errorMessage = models.RunDependencyStateFailed, child.Error
	} else if child.Status == models.AgentRunStatusCanceled {
		state, errorMessage = models.RunDependencyStateCanceled, child.Error
	}
	if actor.Type == "" || actor.ID == "" {
		actor = models.ActivityActor{Type: "system", ID: "run-fork-coordinator"}
	}
	result, err := portfolio.NewDependencyCoordinator(c.store, models.WithClock(c.now)).ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
		Scope: child.Scope, GroupID: group.ID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision,
		State: state, Result: map[string]interface{}{
			"branchId": dependencyID, "checkpoint": maps.Clone(child.Checkpoint), "output": maps.Clone(child.Output),
		}, Error: errorMessage, Actor: actor, Visibility: models.ActivityVisibilityScope,
	})
	if err != nil {
		return nil, err
	}
	if cancelLosers && state == models.RunDependencyStateSatisfied && result.Source != nil && !models.IsTerminalAgentRunStatus(result.Source.Status) &&
		result.Group.Policy.Mode == models.FanInModeAny && result.Group.Status == models.RunDependencyGroupSatisfied {
		if err := c.cancelLosingChildren(ctx, result, child.ID, actor); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// ReconcileRunDependencyGroup preserves typed fork cancellation before generic
// fan-in repair. Terminal-source retirement never mutates target Runs. Only a
// terminal child with exact kernel-created fork lineage can use CompleteChild.
func (c *RunForkCoordinator) ReconcileRunDependencyGroup(ctx context.Context, req portfolio.ReconcileRunDependencyGroupRequest) (*models.RunDependencyResult, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run fork store is not configured")
	}
	coordinator := portfolio.NewDependencyCoordinator(c.store, models.WithClock(c.now))
	group, err := c.store.GetRunDependencyGroup(ctx, req.Scope, req.GroupID)
	if err != nil || group == nil {
		if err == nil {
			err = models.ErrDependencyGroupNotFound
		}
		return nil, err
	}
	if group.Policy.Mode != models.FanInModeAny {
		return coordinator.ReconcileRunDependencyGroup(ctx, req)
	}
	source, err := c.store.GetAgentRun(ctx, req.Scope, group.SourceRunID)
	if err != nil || source == nil {
		if err == nil {
			err = models.ErrRunNotFound
		}
		return nil, err
	}
	if group.Revision != req.ExpectedGroupRevision || source.Revision != req.ExpectedSourceRevision {
		return nil, models.ErrRevisionConflict
	}
	if waiting, _ := models.SourceWaitsForDependencyGroup(source, group.ID); waiting && group.Status == models.RunDependencyGroupWaiting {
		edges, err := c.store.ListRunDependencies(ctx, req.Scope, group.ID)
		if err != nil {
			return nil, err
		}
		for _, edge := range edges {
			if edge.Kind != models.RunDependencyKindRun || edge.State != models.RunDependencyStatePending && edge.State != models.RunDependencyStateRunning {
				continue
			}
			child, err := c.store.GetAgentRun(ctx, req.Scope, edge.TargetRunID)
			if err != nil {
				return nil, err
			}
			if child != nil && models.IsTerminalAgentRunStatus(child.Status) && ownedForkChild(group, edge, child) {
				return c.CompleteChild(ctx, child, req.Actor)
			}
		}
	}
	return coordinator.ReconcileRunDependencyGroup(ctx, req)
}

func ownedForkChild(group *models.RunDependencyGroup, edge *models.RunDependency, child *models.AgentRun) bool {
	if group == nil || edge == nil || child == nil || edge.Kind != models.RunDependencyKindRun || child.Source != models.RunSourceFork ||
		child.Scope != group.Scope || edge.Scope != group.Scope || child.ParentRunID != group.SourceRunID ||
		edge.GroupID != group.ID || edge.SourceRunID != group.SourceRunID || edge.TargetRunID != child.ID {
		return false
	}
	metadata, ok := child.Checkpoint["forkChild"].(map[string]interface{})
	if !ok {
		return false
	}
	forkID, _ := metadata["forkId"].(string)
	return forkID != "" && metadata["sourceRunId"] == group.SourceRunID && metadata["groupId"] == group.ID && metadata["dependencyId"] == edge.ID &&
		group.ID == StableForkIdentifier(group.SourceRunID, forkID, "group") && child.ID == StableForkIdentifier(group.SourceRunID, forkID, "child:"+edge.ID)
}

func (c *RunForkCoordinator) cancelLosingChildren(ctx context.Context, winner *models.RunDependencyResult, winnerID string, actor models.ActivityActor) error {
	runActivity := activity.NewRunActivityService(c.store, c.store, models.WithClock(c.now))
	for _, edge := range winner.Dependencies {
		if edge.TargetRunID == winnerID || edge.State == models.RunDependencyStateSatisfied || edge.State == models.RunDependencyStateFailed || edge.State == models.RunDependencyStateCanceled {
			continue
		}
		child, err := c.cancelLosingChild(ctx, runActivity, edge, winner.Group, actor)
		if err != nil {
			return err
		}
		if _, err := c.completeChild(ctx, child, actor, false); err != nil && !errors.Is(err, models.ErrDependencyConflict) {
			return err
		}
	}
	return nil
}

func (c *RunForkCoordinator) cancelLosingChild(ctx context.Context, runActivity *activity.RunActivityService, edge *models.RunDependency, group *models.RunDependencyGroup, actor models.ActivityActor) (*models.AgentRun, error) {
	const revisionRetries = 8
	for attempt := 0; attempt < revisionRetries; attempt++ {
		child, err := c.store.GetAgentRun(ctx, edge.Scope, edge.TargetRunID)
		if err != nil || child == nil {
			if err == nil {
				err = models.ErrRunNotFound
			}
			return nil, err
		}
		if models.IsTerminalAgentRunStatus(child.Status) {
			return child, nil
		}
		if !ownedForkChild(group, edge, child) {
			return nil, models.ErrInvalidRunDependency
		}
		child, _, err = runActivity.TransitionRun(ctx, child.Scope, child.ID, activity.RunTransitionRequest{
			ExpectedRevision: child.Revision, Status: models.AgentRunStatusCanceled,
			Error: "canceled after another fork branch satisfied join_any", Actor: actor,
			EventType: "run.fork_loser_canceled", Summary: "Canceled losing join_any branch", CorrelationID: group.ID,
		})
		if errors.Is(err, models.ErrRevisionConflict) {
			continue
		}
		return child, err
	}
	return nil, fmt.Errorf("cancel fork child %s after concurrent revisions: %w", edge.TargetRunID, models.ErrRevisionConflict)
}

// configured reports whether c has a store (false for a nil coordinator).
func (c *RunForkCoordinator) Configured() bool {
	return c != nil && c.store != nil
}

// dependencyGroup reads a fork's dependency group.
func (c *RunForkCoordinator) DependencyGroup(ctx context.Context, scope models.Scope, groupID string) (*models.RunDependencyGroup, error) {
	return c.store.GetRunDependencyGroup(ctx, scope, groupID)
}

// dependencies lists the edges of a fork's dependency group.
func (c *RunForkCoordinator) Dependencies(ctx context.Context, scope models.Scope, groupID string) ([]*models.RunDependency, error) {
	return c.store.ListRunDependencies(ctx, scope, groupID)
}
