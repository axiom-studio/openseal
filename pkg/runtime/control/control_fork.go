package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Executor.Fork is the fan-out primitive of start_subagents, run_operation
// and the runbook engine's fork node: it creates the child Runs and parks the
// source on their dependency group in one transaction, and ResumeFork turns
// the resolved group into the entry's result.

const (
	forkNewMessageCode    = "new_message"
	forkNewMessageMessage = "A new message arrived; read it first."
	forkRejectedCode      = "fork_rejected"
	forkLostCode          = "fork_lost"
)

// ForkSpec is one fan-out, after argument parsing. Model calls build it from
// start_subagents arguments; deterministic producers (runbook engine, §12) and
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
	// Fresh names the branches that start from their brief alone although
	// they run as the source Agent (start_subagents fresh).
	Fresh map[string]bool
	// Background keeps the source running (model origin only): the group is
	// detached and the entry completes at once with BackgroundResult.
	Background       bool
	BackgroundResult *models.ToolResult
	// Subagents names the children for the model (EntryMeta.Subagents), and
	// Waited the ones the entry's result reports when it resumes.
	Subagents []SubagentRef
	// Surfaced are the tools a background entry makes callable as it
	// completes; a parked entry surfaces them when it resumes.
	Surfaced []models.SurfacedTool
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
		if !model || spec.NoSessionForks || spec.Fresh[strings.TrimSpace(branch.ID)] {
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
	if spec.Background && (!model || cc.Entry == nil || spec.BackgroundResult == nil) {
		return Result{}, errors.New("a background fork needs a model call's entry and its result")
	}
	meta := EntryMeta{Kind: cc.ToolName(), ForkID: spec.ForkID, GroupID: groupID, ChildRunIDs: childIDs, Subagents: spec.Subagents}
	if !spec.Background {
		meta.Waited = spec.Subagents
	}
	if meta.Kind == "" {
		meta.Kind = models.ControlToolStartSubagents
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
			if spec.Background {
				transition.To, transition.Wait, transition.Result = models.ToolEntryCompleted, nil, spec.BackgroundResult
				transition.Kernel.SurfacedTools = models.CloneSurfacedTools(spec.Surfaced)
				transition.Tokens = modelstep.EstimateEntryTokens(WithToolResult(entry, models.ToolEntryCompleted, spec.BackgroundResult))
			}
		}
		created, err := x.Forks.Create(ctx, agentrun.CreateRunForkRequest{
			Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: current.Revision, WorkerID: cc.WorkerID,
			ForkID: spec.ForkID, Policy: spec.Policy, Branches: spec.Branches, ContinuationCheckpoint: checkpoint,
			ChildKind: spec.ChildKind, SessionForks: sessionForks,
			ToolTransition: transition, RequireInterventionCount: required, Detached: spec.Background,
			Actor: models.ActivityActor{Type: "worker", ID: cc.WorkerID},
		})
		switch {
		case err == nil && spec.Background:
			return x.forkStarted(ctx, cc, created)
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

// forkStarted is the outcome of a background fork: the entry completed in
// the create's transaction and the source keeps running.
func (x *Executor) forkStarted(ctx context.Context, cc agentrun.ControlContext, created *agentrun.RunForkResult) (Result, error) {
	var entry *models.SessionEntry
	var source *models.AgentRun
	if created != nil && created.DependencyGroup != nil {
		entry, source = created.DependencyGroup.ToolEntry, created.DependencyGroup.Source
	}
	if entry == nil {
		reread, err := x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, cc.Entry.ID)
		if err != nil {
			return Result{}, err
		}
		entry = reread
	}
	if entry.State != models.ToolEntryCompleted {
		return Result{}, fmt.Errorf("%w: background fork entry %s is %s after the group was created", models.ErrToolEntryStateConflict, entry.ID, entry.State)
	}
	return Result{Kind: Continue, Run: source, Entry: entry}, nil
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
