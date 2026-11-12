package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// control_fork (section 04 §6): a settle-phase control tool that runs two to
// eight independent goals as concurrent child Runs. The dependency-group
// create moves the tool entry to awaiting_child, creates the same-Agent child
// sessions and parks the source on waiting_for_dependency in one transaction
// (CTL-5a), so the park is committed. When the group resolves, the source is
// queued again and the resume turns every branch's final answer (or failure)
// into this entry's result. A failed branch is an is_error result, never a
// Run failure (D6).

const (
	minimumForkBranches  = 2
	maximumForkBranches  = 8
	maximumForkGoalBytes = 16384

	forkNewMessageCode    = "new_message"
	forkNewMessageMessage = "A new message arrived; read it first."
	forkRejectedCode      = "fork_rejected"
	forkLostCode          = "fork_lost"
)

type forkBranchArgs struct {
	Goal    string                 `json:"goal"`
	AgentID string                 `json:"agentId,omitempty"`
	Context map[string]interface{} `json:"context,omitempty"`
}

type forkArgs struct {
	Branches []forkBranchArgs `json:"branches"`
	Mode     models.FanInMode `json:"mode,omitempty"`
}

// ForkSpec is one fan-out, after argument parsing. Model calls build it from
// control_fork arguments; deterministic producers (runbook engine, §12) and
// run_operation build it themselves and call Executor.Fork.
type ForkSpec struct {
	ForkID   string
	Policy   models.RunDependencyPolicy
	Branches []agentrun.RunForkBranch // Checkpoint may be non-empty only for OriginKernel
	// SourceCheckpoint is the source's checkpoint while it waits (kernel
	// origin only); model calls keep the Run's checkpoint unchanged.
	SourceCheckpoint map[string]interface{}
	ChildKind        models.RunKind // model origin: RunKindAgentWork
	// NoSessionForks keeps every child on a fresh session even when it runs
	// as the source Agent (deterministic children: run_operation).
	NoSessionForks bool
}

type forkHandler struct{}

func (forkHandler) Spec(a agentrun.ControlAuthority) ToolSpec {
	branch := map[string]interface{}{
		"goal": map[string]interface{}{
			"type": "string", "minLength": 1, "maxLength": maximumForkGoalBytes,
			"description": "Self-contained goal for this branch.",
		},
		"context": map[string]interface{}{
			"type": "object", "description": "Non-secret data the branch needs.",
		},
	}
	// No target enum: the Agents are in the capabilities note and the
	// handler checks each agentId, so the schema never changes with them.
	branch["agentId"] = map[string]interface{}{
		"type": "string", "description": "Omit to run the branch as yourself; otherwise an agentId from the capabilities note.",
	}
	return ToolSpec{
		Description: "Run two to eight independent goals concurrently; their final answers come back to you as this tool's result.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"branches"},
			"properties": map[string]interface{}{
				"branches": map[string]interface{}{
					"type": "array", "minItems": minimumForkBranches, "maxItems": maximumForkBranches,
					"items": map[string]interface{}{
						"type": "object", "additionalProperties": false, "required": []interface{}{"goal"},
						"properties": branch,
					},
				},
				"mode": map[string]interface{}{
					"type": "string", "enum": []interface{}{string(models.FanInModeAll), string(models.FanInModeAny)},
					"description": "all: wait for every branch (default). any: first success wins; the rest are canceled.",
				},
			},
		},
	}
}

func (forkHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[forkArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*forkArgs)
	if n := len(args.Branches); n < minimumForkBranches || n > maximumForkBranches {
		return nil, fmt.Errorf("branches must hold %d to %d goals, got %d", minimumForkBranches, maximumForkBranches, n)
	}
	for i := range args.Branches {
		branch := &args.Branches[i]
		branch.Goal = strings.TrimSpace(branch.Goal)
		branch.AgentID = strings.TrimSpace(branch.AgentID)
		if branch.Goal == "" {
			return nil, fmt.Errorf("branch %d needs a goal", i+1)
		}
		if len(branch.Goal) > maximumForkGoalBytes || !utf8.ValidString(branch.Goal) {
			return nil, fmt.Errorf("branch %d goal must be valid text of at most %d bytes", i+1, maximumForkGoalBytes)
		}
	}
	switch args.Mode {
	case "":
		args.Mode = models.FanInModeAll
	case models.FanInModeAll, models.FanInModeAny:
	default:
		return nil, fmt.Errorf("mode must be all or any, got %q", args.Mode)
	}
	return args, nil
}

func (forkHandler) Run(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Result, error) {
	args := cc.Arguments.(*forkArgs)
	branches := make([]agentrun.RunForkBranch, len(args.Branches))
	for i, arg := range args.Branches {
		id := fmt.Sprintf("branch-%d", i+1)
		if err := validateForkBranchContext(arg.Context); err != nil {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, "context_invalid",
				fmt.Sprintf("Branch %d context is not allowed: %v.", i+1, err), nil))
		}
		agentID := ""
		if arg.AgentID != "" && arg.AgentID != cc.Run.AssignedAgentID {
			if !forkTargetEligible(arg.AgentID, cc.Authority.ForkTargets) {
				return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, "agent_not_eligible",
					fmt.Sprintf("Branch %d names an Agent you cannot run a branch as; omit agentId to run it as yourself.", i+1),
					map[string]interface{}{"agentId": arg.AgentID}))
			}
			agentID = arg.AgentID
		}
		branches[i] = agentrun.RunForkBranch{
			ID: id, Goal: arg.Goal, AssignedAgentID: agentID, Context: maps.Clone(arg.Context),
			Checkpoint: map[string]interface{}{},
		}
	}
	stepID := ""
	if cc.Step != nil {
		stepID = cc.Step.ID
	}
	budgets, err := modelstep.ControlBranchBudgets(cc.Run, stepID, len(branches))
	if err != nil {
		var insufficient *modelstep.ControlBudgetInsufficientError
		if errors.As(err, &insufficient) {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, insufficient.Code(), insufficient.Message(), nil))
		}
		return Result{}, err
	}
	for i := range branches {
		branches[i].Budget = budgets[i]
	}
	return x.Fork(ctx, cc, ForkSpec{
		ForkID:    "fork-" + cc.Entry.ID,
		Policy:    models.RunDependencyPolicy{Mode: args.Mode, FailureMode: models.DependencyFailureFailFast},
		Branches:  branches,
		ChildKind: models.RunKindAgentWork,
	})
}

func forkTargetEligible(agentID string, targets []agentrun.HostedAgentTarget) bool {
	for _, target := range targets {
		if target.ID == agentID {
			return true
		}
	}
	return false
}

// validateForkBranchContext: branch data is credential-free and never sets a
// kernel key.
func validateForkBranchContext(context map[string]interface{}) error {
	if err := models.ValidateCredentialFreeContext(context); err != nil {
		return err
	}
	for key := range context {
		if strings.HasPrefix(key, "_openseal") || strings.HasPrefix(key, "openseal.") || key == agentrun.RunContextSessionKey {
			return fmt.Errorf("the key %s is reserved", key)
		}
	}
	return nil
}

// Fork creates spec's child Runs and parks the source on their dependency
// group, with the call's tool entry moved to awaiting_child in the same
// transaction. The ids are derived from spec.ForkID, so a settle repeated
// after a crash creates the group once.
//
// Model-origin calls are refused with new_message when an intervention
// arrived after the step was planned. Same-Agent branches of model calls
// fork the Run's session just before the forking assistant entry (A3).
func (x *Executor) Fork(ctx context.Context, cc agentrun.ControlContext, spec ForkSpec) (Result, error) {
	if x == nil || x.Forks == nil || x.Portfolio == nil {
		return Result{}, errors.New("control executor has no fork coordinator")
	}
	if cc.Run == nil || strings.TrimSpace(spec.ForkID) == "" || len(spec.Branches) == 0 {
		return Result{}, errors.New("fork needs the Run, a fork id and branches")
	}
	run := cc.Run
	model := cc.Origin != OriginKernel
	groupID := agentrun.StableForkIdentifier(run.ID, spec.ForkID, "group")
	childIDs := make([]string, len(spec.Branches))
	var sessionForks map[string]*agentrun.SessionForkRef
	for i, branch := range spec.Branches {
		if model && len(branch.Checkpoint) > 0 {
			return Result{}, errors.New("model fork branches cannot carry a checkpoint")
		}
		childIDs[i] = agentrun.StableForkIdentifier(run.ID, spec.ForkID, "child:"+strings.TrimSpace(branch.ID))
		if !model || spec.NoSessionForks {
			continue
		}
		agentID := strings.TrimSpace(branch.AssignedAgentID)
		if agentID == "" {
			agentID = run.AssignedAgentID
		}
		if ref := agentrun.ControlChildSessionFork(cc, agentID); ref != nil {
			if sessionForks == nil {
				sessionForks = map[string]*agentrun.SessionForkRef{}
			}
			sessionForks[strings.TrimSpace(branch.ID)] = ref
		}
	}
	checkpoint := maps.Clone(spec.SourceCheckpoint)
	if model {
		checkpoint = maps.Clone(run.Checkpoint)
	}
	meta := EntryMeta{Kind: cc.ToolName(), ForkID: spec.ForkID, GroupID: groupID, ChildRunIDs: childIDs}
	if meta.Kind == "" {
		meta.Kind = models.ControlToolFork
	}
	control, err := json.Marshal(&meta)
	if err != nil {
		return Result{}, fmt.Errorf("encode fork control metadata: %w", err)
	}
	entry := cc.Entry
	for attempt := 0; ; attempt++ {
		current, err := x.Portfolio.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			return Result{}, err
		}
		if current == nil {
			return Result{}, models.ErrRunNotFound
		}
		if current.Status != models.AgentRunStatusRunning || current.LeaseOwner != cc.WorkerID {
			return Result{}, models.ErrLeaseLost
		}
		var required *int
		if model {
			count := len(run.PendingInterventions)
			if len(current.PendingInterventions) != count {
				return x.forkRefuse(ctx, cc, ErrorResult(models.ToolResultRefused, forkNewMessageCode, forkNewMessageMessage, nil))
			}
			required = &count
		}
		now := x.now()
		var transition *models.ToolEntryTransitionRequest
		if entry != nil {
			transition = &models.ToolEntryTransitionRequest{
				Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, Lease: cc.LeaseGuard(now),
				From: entry.State, ExpectedRevision: entry.Revision, To: models.ToolEntryAwaitingChild,
				Wait:   &models.ToolEntryWait{GroupID: groupID, ChildRunIDs: append([]string(nil), childIDs...)},
				Kernel: &models.ToolEntryKernelPatch{Control: control}, Now: now,
			}
		}
		created, err := x.Forks.Create(ctx, agentrun.CreateRunForkRequest{
			Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: current.Revision, WorkerID: cc.WorkerID,
			ForkID: spec.ForkID, Policy: spec.Policy, Branches: spec.Branches, ContinuationCheckpoint: checkpoint,
			ChildKind: spec.ChildKind, SessionForks: sessionForks,
			ToolTransition: transition, RequireInterventionCount: required,
			Actor: models.ActivityActor{Type: "worker", ID: cc.WorkerID},
		})
		switch {
		case err == nil:
			return x.forkParked(ctx, cc, created, groupID, len(childIDs))
		case errors.Is(err, models.ErrSteeringArrived):
			return x.forkRefuse(ctx, cc, ErrorResult(models.ToolResultRefused, forkNewMessageCode, forkNewMessageMessage, nil))
		case attempt == 0 && errors.Is(err, models.ErrRevisionConflict):
			continue
		case attempt == 0 && errors.Is(err, models.ErrToolEntryStateConflict) && entry != nil:
			reread, getErr := x.Sessions.GetSessionEntry(ctx, run.Scope, entry.ID)
			if getErr != nil {
				return Result{}, getErr
			}
			if reread.State != entry.State {
				return Result{}, err
			}
			entry = reread
			continue
		case forkAbortsClaim(err):
			return Result{}, err
		default:
			return x.forkRefuse(ctx, cc, ErrorResult(models.ToolResultRefused, forkRejectedCode, err.Error(), nil))
		}
	}
}

// forkAbortsClaim: lease, revision, session binding, entry state and context
// errors abort the claim (rule 5); the next claim's resume settles again.
func forkAbortsClaim(err error) bool {
	for _, target := range []error{
		models.ErrLeaseLost, models.ErrRevisionConflict, models.ErrSessionMoved, models.ErrToolEntryStateConflict, models.ErrRunNotFound,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// forkRefuse ends the call with an is_error result. A kernel call without a
// tool entry gets the refusal as a Go error.
func (x *Executor) forkRefuse(ctx context.Context, cc agentrun.ControlContext, result *models.ToolResult) (Result, error) {
	if cc.Entry == nil {
		return Result{}, fmt.Errorf("fork refused: %s", ResultText(result))
	}
	return x.refuse(ctx, cc, result)
}

func (x *Executor) forkParked(ctx context.Context, cc agentrun.ControlContext, created *agentrun.RunForkResult, groupID string, children int) (Result, error) {
	var entry *models.SessionEntry
	var source *models.AgentRun
	if created != nil && created.DependencyGroup != nil {
		entry, source = created.DependencyGroup.ToolEntry, created.DependencyGroup.Source
	}
	if entry == nil && cc.Entry != nil {
		reread, err := x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, cc.Entry.ID)
		if err != nil {
			return Result{}, err
		}
		entry = reread
	}
	if entry != nil && entry.State != models.ToolEntryAwaitingChild {
		return Result{}, fmt.Errorf("%w: fork entry %s is %s after the group was created", models.ErrToolEntryStateConflict, entry.ID, entry.State)
	}
	return Result{
		Kind: Parked, Run: source, Entry: entry,
		Park: &agentrun.LoopPark{
			Status: models.AgentRunStatusWaitingForDependency, Wake: &models.WakeCondition{Type: "run_dependencies", Reference: groupID},
			Summary: fmt.Sprintf("Waiting for %d forked branches", children), Committed: true,
		},
	}, nil
}

// Resume turns the resolved dependency group into the entry's result: every
// branch's final answer, or its failure. A group still waiting re-parks.
func (forkHandler) Resume(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Resume, error) {
	return x.ResumeFork(ctx, cc)
}

func (x *Executor) ResumeFork(ctx context.Context, cc agentrun.ControlContext) (Resume, error) {
	if !x.Forks.Configured() {
		return Resume{}, errors.New("control executor has no fork coordinator")
	}
	meta, err := DecodeControlEntryMeta(cc.Entry)
	if err != nil {
		return Resume{}, err
	}
	groupID := ""
	if meta != nil {
		groupID = meta.GroupID
	}
	if groupID == "" && cc.Entry.Tool.Wait != nil {
		groupID = cc.Entry.Tool.Wait.GroupID
	}
	var group *models.RunDependencyGroup
	if groupID != "" {
		group, err = x.Forks.DependencyGroup(ctx, cc.Run.Scope, groupID)
		if err != nil && !errors.Is(err, models.ErrDependencyGroupNotFound) {
			return Resume{}, err
		}
	}
	if group == nil {
		// The group is created with the entry's transition, so this is a
		// store the kernel can no longer read; tell the model plainly.
		result, err := x.complete(ctx, cc, ErrorResult(models.ToolResultFailed, forkLostCode,
			"The forked work can no longer be found; start it again if it is still needed.", nil), nil)
		return Resume{Resolved: true, Result: result}, err
	}
	if group.Status == models.RunDependencyGroupWaiting || group.Status == models.RunDependencyGroupOpen {
		return Resume{Park: &agentrun.LoopPark{
			Status: models.AgentRunStatusWaitingForDependency, Wake: &models.WakeCondition{Type: "run_dependencies", Reference: group.ID},
			Summary: "Waiting for forked branches",
		}}, nil
	}
	edges, err := x.Forks.Dependencies(ctx, cc.Run.Scope, group.ID)
	if err != nil {
		return Resume{}, err
	}
	value, err := x.forkResultValue(ctx, cc.Run, group, edges, meta)
	if err != nil {
		return Resume{}, err
	}
	status := models.ToolResultSucceeded
	switch group.Status {
	case models.RunDependencyGroupFailed:
		status = models.ToolResultFailed
	case models.RunDependencyGroupCanceled:
		status = models.ToolResultCanceled
	}
	result, err := x.complete(ctx, cc, models.ControlJSONResult(status, value), nil)
	return Resume{Resolved: true, Result: result}, err
}

// forkResultValue is the model-visible result of a resolved group. It holds
// each child's answer, never its checkpoint.
func (x *Executor) forkResultValue(ctx context.Context, source *models.AgentRun, group *models.RunDependencyGroup, edges []*models.RunDependency, meta *EntryMeta) (map[string]interface{}, error) {
	byTarget := make(map[string]*models.RunDependency, len(edges))
	order := make([]string, 0, len(edges))
	for _, edge := range edges {
		byTarget[edge.TargetRunID] = edge
	}
	if meta != nil && len(meta.ChildRunIDs) == len(edges) {
		order = append(order, meta.ChildRunIDs...)
	} else {
		for _, edge := range edges {
			order = append(order, edge.TargetRunID)
		}
	}
	branches := make([]interface{}, 0, len(order))
	for _, childID := range order {
		edge := byTarget[childID]
		if edge == nil {
			return nil, fmt.Errorf("%w: fork child %s has no edge in group %s", models.ErrInvalidRunDependency, childID, group.ID)
		}
		child, err := x.Portfolio.GetAgentRun(ctx, source.Scope, childID)
		if err != nil {
			return nil, err
		}
		if child == nil {
			return nil, models.ErrRunNotFound
		}
		branch := map[string]interface{}{
			"branch": edge.ID, "goal": truncateControlText(child.Goal, child.ID), "runId": child.ID,
			"status": string(child.Status),
		}
		if child.AssignedAgentID != source.AssignedAgentID {
			branch["agentId"] = child.AssignedAgentID
		}
		if answer := agentbuilder.ChildRunAnswer(child); answer != "" {
			branch["answer"] = truncateControlText(answer, child.ID)
		}
		if len(edge.Artifacts) > 0 {
			artifacts := make([]interface{}, 0, len(edge.Artifacts))
			for _, artifact := range edge.Artifacts {
				artifacts = append(artifacts, map[string]interface{}{"id": artifact.ID, "version": artifact.Version, "name": artifact.Name})
			}
			branch["artifacts"] = artifacts
		}
		if message := strings.TrimSpace(child.Error); message != "" {
			branch["error"] = truncateControlText(message, child.ID)
		} else if message := strings.TrimSpace(edge.Error); message != "" {
			branch["error"] = truncateControlText(message, child.ID)
		}
		branches = append(branches, branch)
	}
	return map[string]interface{}{
		"status": string(group.Status), "mode": string(group.Policy.Mode), "branches": branches,
	}, nil
}

// ResultText is the message of an is_error control result.
func ResultText(result *models.ToolResult) string {
	if result == nil {
		return ""
	}
	for _, part := range result.Content {
		var value struct{ Error, Message string }
		if part.Type == models.ContentPartJSON && json.Unmarshal(part.Value, &value) == nil {
			return value.Error + ": " + value.Message
		}
		if part.Type == models.ContentPartText {
			return part.Text
		}
	}
	return string(result.Status)
}
