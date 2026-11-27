package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// wait_subagents (settle phase) parks the Run until its subagents end: all
// of them, or (mode any) the first that succeeds. It waits on a wait group
// over the subagents' Runs (RunForkCoordinator.CreateWait), so a child's end
// wakes the parent through the usual dependency resolution. The result
// carries the answers of the ones that ended; an end already given to the
// model in a note is "already reported", not repeated.

type waitSubagentsArgs struct {
	IDs  []string         `json:"ids,omitempty"`
	Mode models.FanInMode `json:"mode,omitempty"`
}

type waitSubagentsHandler struct{}

func (waitSubagentsHandler) Spec(agentrun.ControlAuthority) ToolSpec {
	return ToolSpec{
		Description: "Wait for subagents to end and get their answers. Answers you already got in a note are not repeated.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"ids": map[string]interface{}{
					"type": "array", "items": map[string]interface{}{"type": "string"}, "maxItems": 64,
					"description": "Subagent ids (s1, ...). Default: every one still running or not yet reported.",
				},
				"mode": map[string]interface{}{
					"type": "string", "enum": []interface{}{string(models.FanInModeAll), string(models.FanInModeAny)},
					"description": "all: wait for every one (default). any: return when the first one succeeds.",
				},
			},
		},
	}
}

func (waitSubagentsHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[waitSubagentsArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*waitSubagentsArgs)
	switch args.Mode {
	case "":
		args.Mode = models.FanInModeAll
	case models.FanInModeAll, models.FanInModeAny:
	default:
		return nil, fmt.Errorf("mode must be all or any, got %q", args.Mode)
	}
	return args, nil
}

func (waitSubagentsHandler) Run(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Result, error) {
	args := cc.Arguments.(*waitSubagentsArgs)
	subagents, err := LoadSubagents(ctx, x.Sessions, x.Portfolio, cc.Run, cc.Entry.ID)
	if err != nil {
		return Result{}, err
	}
	ids := args.IDs
	if len(ids) == 0 {
		ids = subagents.open()
	}
	refs := make([]SubagentRef, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		ref, _, ok := subagents.lookup(id)
		if !ok {
			return x.refuse(ctx, cc, subagents.unknown(id))
		}
		if !seen[ref.ID] {
			seen[ref.ID] = true
			refs = append(refs, ref)
		}
	}
	meta := &EntryMeta{Waited: refs}
	if len(refs) == 0 || subagentWaitSatisfied(args.Mode, subagents, refs) {
		return x.completeSubagentWait(ctx, cc, meta, false)
	}
	return x.parkOnSubagents(ctx, cc, args.Mode, refs, subagents, meta)
}

func (waitSubagentsHandler) Resume(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Resume, error) {
	return x.resumeSubagentWait(ctx, cc, false)
}

// open is the ids still running or ended without being reported.
func (s *Subagents) open() []string {
	var ids []string
	for _, id := range s.order {
		ref, child, _ := s.lookup(id)
		if child != nil && (!models.IsTerminalAgentRunStatus(child.Status) || !s.reported[ref.RunID]) {
			ids = append(ids, id)
		}
	}
	return ids
}

// subagentWaitSatisfied: all = every one ended; any = one succeeded or
// every one ended (the wait group's fan-in).
func subagentWaitSatisfied(mode models.FanInMode, subagents *Subagents, refs []SubagentRef) bool {
	ended := 0
	for _, ref := range refs {
		child := subagents.runs[ref.RunID]
		if child == nil || !models.IsTerminalAgentRunStatus(child.Status) {
			continue
		}
		if mode == models.FanInModeAny && child.Status == models.AgentRunStatusCompleted {
			return true
		}
		ended++
	}
	return ended == len(refs)
}

// parkOnSubagents parks the Run on a wait group over the refs still running,
// with the entry moved to awaiting_child in the same transaction.
func (x *Executor) parkOnSubagents(ctx context.Context, cc agentrun.ControlContext, mode models.FanInMode, refs []SubagentRef, subagents *Subagents, meta *EntryMeta) (Result, error) {
	if !x.Forks.Configured() {
		return Result{}, errors.New("control executor has no fork coordinator")
	}
	run := cc.Run
	var targets []agentrun.WaitTarget
	var childIDs []string
	for _, ref := range refs {
		if child := subagents.runs[ref.RunID]; child == nil || !models.IsTerminalAgentRunStatus(child.Status) {
			targets = append(targets, agentrun.WaitTarget{ID: ref.ID, RunID: ref.RunID})
			childIDs = append(childIDs, ref.RunID)
		}
	}
	groupID := agentrun.WaitGroupID(run.ID, cc.Entry.ID)
	stored := *meta
	stored.Kind, stored.GroupID = cc.ToolName(), groupID
	control, err := json.Marshal(&stored)
	if err != nil {
		return Result{}, err
	}
	entry := cc.Entry
	for attempt := 0; ; attempt++ {
		current, err := x.Portfolio.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			return Result{}, err
		}
		if current == nil || current.Status != models.AgentRunStatusRunning || current.LeaseOwner != cc.WorkerID {
			return Result{}, models.ErrLeaseLost
		}
		count := len(run.PendingInterventions)
		if len(current.PendingInterventions) != count {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, forkNewMessageCode, forkNewMessageMessage, nil))
		}
		now := x.now()
		created, err := x.Forks.CreateWait(ctx, agentrun.CreateRunWaitRequest{
			Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: current.Revision, WorkerID: cc.WorkerID,
			WaitID: cc.Entry.ID, Mode: mode, Targets: targets, RequireInterventionCount: &count,
			ToolTransition: &models.ToolEntryTransitionRequest{
				Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, Lease: cc.LeaseGuard(now),
				From: entry.State, ExpectedRevision: entry.Revision, To: models.ToolEntryAwaitingChild,
				Wait:   &models.ToolEntryWait{GroupID: groupID, ChildRunIDs: childIDs},
				Kernel: &models.ToolEntryKernelPatch{Control: control}, Now: now,
			},
			Actor: models.ActivityActor{Type: "worker", ID: cc.WorkerID},
		})
		switch {
		case err == nil:
			return Result{
				Kind: Parked, Run: created.Source, Entry: created.ToolEntry,
				Park: &agentrun.LoopPark{
					Status: models.AgentRunStatusWaitingForDependency, Wake: &models.WakeCondition{Type: "run_dependencies", Reference: groupID},
					Summary: "Waiting for " + strings.Join(subagentIDs(refs), ", "), Committed: true,
				},
			}, nil
		case errors.Is(err, models.ErrSteeringArrived):
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, forkNewMessageCode, forkNewMessageMessage, nil))
		case attempt == 0 && errors.Is(err, models.ErrRevisionConflict):
			continue
		case forkAbortsClaim(err):
			return Result{}, err
		default:
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, forkRejectedCode, err.Error(), nil))
		}
	}
}

func subagentIDs(refs []SubagentRef) []string {
	ids := make([]string, len(refs))
	for i, ref := range refs {
		ids[i] = ref.ID
	}
	return ids
}

// resumeSubagentWait completes a parked subagent entry once its group
// resolved; a group still waiting re-parks. start marks a start_subagents
// entry: it fails when no subagent succeeded and otherwise surfaces the
// follow-up tools.
func (x *Executor) resumeSubagentWait(ctx context.Context, cc agentrun.ControlContext, start bool) (Resume, error) {
	if !x.Forks.Configured() {
		return Resume{}, errors.New("control executor has no fork coordinator")
	}
	meta, err := DecodeControlEntryMeta(cc.Entry)
	if err != nil {
		return Resume{}, err
	}
	if meta == nil || meta.GroupID == "" {
		return Resume{}, fmt.Errorf("subagent entry %s has no group", cc.Entry.ID)
	}
	group, err := x.Forks.DependencyGroup(ctx, cc.Run.Scope, meta.GroupID)
	if err != nil && !errors.Is(err, models.ErrDependencyGroupNotFound) {
		return Resume{}, err
	}
	if group == nil {
		result, err := x.complete(ctx, cc, ErrorResult(models.ToolResultFailed, forkLostCode,
			"The subagents can no longer be found; start them again if they are still needed.", nil), nil)
		return Resume{Resolved: true, Result: result}, err
	}
	if group.Status == models.RunDependencyGroupWaiting || group.Status == models.RunDependencyGroupOpen {
		return Resume{Park: &agentrun.LoopPark{
			Status: models.AgentRunStatusWaitingForDependency, Wake: &models.WakeCondition{Type: "run_dependencies", Reference: group.ID},
			Summary: "Waiting for " + strings.Join(subagentIDs(meta.Waited), ", "),
		}}, nil
	}
	result, err := x.completeSubagentWait(ctx, cc, meta, start)
	return Resume{Resolved: true, Result: result}, err
}

// completeSubagentWait ends the entry with the answers of meta.Waited.
func (x *Executor) completeSubagentWait(ctx context.Context, cc agentrun.ControlContext, meta *EntryMeta, start bool) (Result, error) {
	subagents, err := LoadSubagents(ctx, x.Sessions, x.Portfolio, cc.Run, cc.Entry.ID)
	if err != nil {
		return Result{}, err
	}
	values := make([]interface{}, 0, len(meta.Waited))
	next := *meta
	next.Reported = nil
	succeeded := false
	for _, ref := range meta.Waited {
		child, err := x.Portfolio.GetAgentRun(ctx, cc.Run.Scope, ref.RunID)
		if err != nil {
			return Result{}, err
		}
		if child == nil {
			return Result{}, models.ErrRunNotFound
		}
		reported := subagents.reported[child.ID]
		values = append(values, subagentValue(ref.ID, child, cc.Run, reported))
		succeeded = succeeded || child.Status == models.AgentRunStatusCompleted
		if models.IsTerminalAgentRunStatus(child.Status) && !reported {
			next.Reported = append(next.Reported, child.ID)
		}
	}
	value := map[string]interface{}{"subagents": values}
	if len(values) == 0 {
		value["message"] = "No subagent is running, and every result was already reported."
	}
	status := models.ToolResultSucceeded
	if start && !succeeded {
		status = models.ToolResultFailed
	}
	result := models.ControlJSONResult(status, value)
	var surfaced []models.SurfacedTool
	if start && !result.IsError {
		surfaced = x.subagentFollowUpSurface(cc.Authority)
	}
	to := models.ToolEntryCompleted
	if result.IsError {
		to = models.ToolEntryError
	}
	entry, err := x.transitionSurfacing(ctx, cc, to, nil, result, &next, surfaced)
	if err != nil {
		return Result{}, err
	}
	return Result{Kind: Continue, Entry: entry}, nil
}
