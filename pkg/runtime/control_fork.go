package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
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
	Mode     FanInMode        `json:"mode,omitempty"`
}

// ForkSpec is one fan-out, after argument parsing. Model calls build it from
// control_fork arguments; deterministic producers (runbook engine, §12) and
// run_operation build it themselves and call ControlExecutor.Fork.
type ForkSpec struct {
	ForkID   string
	Policy   RunDependencyPolicy
	Branches []RunForkBranch // Checkpoint may be non-empty only for ControlOriginKernel
	// SourceCheckpoint is the source's checkpoint while it waits (kernel
	// origin only); model calls keep the Run's checkpoint unchanged.
	SourceCheckpoint map[string]interface{}
	ChildKind        RunKind // model origin: RunKindAgentWork
	// NoSessionForks keeps every child on a fresh session even when it runs
	// as the source Agent (deterministic children: run_operation).
	NoSessionForks bool
}

type forkHandler struct{}

func init() { registerControlHandler(ControlToolFork, forkHandler{}) }

func (forkHandler) spec(a ControlAuthority) ControlToolSpec {
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
	return ControlToolSpec{
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
					"type": "string", "enum": []interface{}{string(FanInModeAll), string(FanInModeAny)},
					"description": "all: wait for every branch (default). any: first success wins; the rest are canceled.",
				},
			},
		},
	}
}

func (forkHandler) decode(raw json.RawMessage) (interface{}, error) {
	value, err := decodeControlInto[forkArgs](raw)
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
		args.Mode = FanInModeAll
	case FanInModeAll, FanInModeAny:
	default:
		return nil, fmt.Errorf("mode must be all or any, got %q", args.Mode)
	}
	return args, nil
}

func (forkHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*forkArgs)
	branches := make([]RunForkBranch, len(args.Branches))
	for i, arg := range args.Branches {
		id := fmt.Sprintf("branch-%d", i+1)
		if err := validateForkBranchContext(arg.Context); err != nil {
			return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, "context_invalid",
				fmt.Sprintf("Branch %d context is not allowed: %v.", i+1, err), nil))
		}
		agentID := ""
		if arg.AgentID != "" && arg.AgentID != cc.Run.AssignedAgentID {
			if !forkTargetEligible(arg.AgentID, cc.Authority.ForkTargets) {
				return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, "agent_not_eligible",
					fmt.Sprintf("Branch %d names an Agent you cannot run a branch as; omit agentId to run it as yourself.", i+1),
					map[string]interface{}{"agentId": arg.AgentID}))
			}
			agentID = arg.AgentID
		}
		branches[i] = RunForkBranch{
			ID: id, Goal: arg.Goal, AssignedAgentID: agentID, Context: cloneMap(arg.Context),
			Checkpoint: map[string]interface{}{},
		}
	}
	stepID := ""
	if cc.Step != nil {
		stepID = cc.Step.ID
	}
	budgets, err := controlBranchBudgets(cc.Run, stepID, len(branches))
	if err != nil {
		var insufficient *ControlBudgetInsufficientError
		if errors.As(err, &insufficient) {
			return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, insufficient.Code(), insufficient.Message(), nil))
		}
		return ControlResult{}, err
	}
	for i := range branches {
		branches[i].Budget = budgets[i]
	}
	return x.Fork(ctx, cc, ForkSpec{
		ForkID:    "fork-" + cc.Entry.ID,
		Policy:    RunDependencyPolicy{Mode: args.Mode, FailureMode: DependencyFailureFailFast},
		Branches:  branches,
		ChildKind: RunKindAgentWork,
	})
}

func forkTargetEligible(agentID string, targets []HostedAgentTarget) bool {
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
	if err := ValidateCredentialFreeContext(context); err != nil {
		return err
	}
	for key := range context {
		if strings.HasPrefix(key, "_openseal") || strings.HasPrefix(key, "openseal.") || key == RunContextSessionKey {
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
func (x *ControlExecutor) Fork(ctx context.Context, cc ControlContext, spec ForkSpec) (ControlResult, error) {
	if x == nil || x.Forks == nil || x.Portfolio == nil {
		return ControlResult{}, errors.New("control executor has no fork coordinator")
	}
	if cc.Run == nil || strings.TrimSpace(spec.ForkID) == "" || len(spec.Branches) == 0 {
		return ControlResult{}, errors.New("fork needs the Run, a fork id and branches")
	}
	run := cc.Run
	model := cc.Origin != ControlOriginKernel
	groupID := stableForkIdentifier(run.ID, spec.ForkID, "group")
	childIDs := make([]string, len(spec.Branches))
	var sessionForks map[string]*SessionForkRef
	for i, branch := range spec.Branches {
		if model && len(branch.Checkpoint) > 0 {
			return ControlResult{}, errors.New("model fork branches cannot carry a checkpoint")
		}
		childIDs[i] = stableForkIdentifier(run.ID, spec.ForkID, "child:"+strings.TrimSpace(branch.ID))
		if !model || spec.NoSessionForks {
			continue
		}
		agentID := strings.TrimSpace(branch.AssignedAgentID)
		if agentID == "" {
			agentID = run.AssignedAgentID
		}
		if ref := controlChildSessionFork(cc, agentID); ref != nil {
			if sessionForks == nil {
				sessionForks = map[string]*SessionForkRef{}
			}
			sessionForks[strings.TrimSpace(branch.ID)] = ref
		}
	}
	checkpoint := cloneMap(spec.SourceCheckpoint)
	if model {
		checkpoint = cloneMap(run.Checkpoint)
	}
	meta := ControlEntryMeta{Kind: cc.toolName(), ForkID: spec.ForkID, GroupID: groupID, ChildRunIDs: childIDs}
	if meta.Kind == "" {
		meta.Kind = ControlToolFork
	}
	control, err := json.Marshal(&meta)
	if err != nil {
		return ControlResult{}, fmt.Errorf("encode fork control metadata: %w", err)
	}
	entry := cc.Entry
	for attempt := 0; ; attempt++ {
		current, err := x.Portfolio.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			return ControlResult{}, err
		}
		if current == nil {
			return ControlResult{}, ErrRunNotFound
		}
		if current.Status != AgentRunStatusRunning || current.LeaseOwner != cc.WorkerID {
			return ControlResult{}, ErrLeaseLost
		}
		var required *int
		if model {
			count := len(run.PendingInterventions)
			if len(current.PendingInterventions) != count {
				return x.forkRefuse(ctx, cc, controlErrorResult(ToolResultRefused, forkNewMessageCode, forkNewMessageMessage, nil))
			}
			required = &count
		}
		now := x.now()
		var transition *ToolEntryTransitionRequest
		if entry != nil {
			transition = &ToolEntryTransitionRequest{
				Scope: run.Scope, EntryID: entry.ID, RunID: run.ID, Lease: cc.lease(now),
				From: entry.State, ExpectedRevision: entry.Revision, To: ToolEntryAwaitingChild,
				Wait:   &ToolEntryWait{GroupID: groupID, ChildRunIDs: append([]string(nil), childIDs...)},
				Kernel: &ToolEntryKernelPatch{Control: control}, Now: now,
			}
		}
		created, err := x.Forks.Create(ctx, CreateRunForkRequest{
			Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: current.Revision, WorkerID: cc.WorkerID,
			ForkID: spec.ForkID, Policy: spec.Policy, Branches: spec.Branches, ContinuationCheckpoint: checkpoint,
			ChildKind: spec.ChildKind, SessionForks: sessionForks,
			ToolTransition: transition, RequireInterventionCount: required,
			Actor: ActivityActor{Type: "worker", ID: cc.WorkerID},
		})
		switch {
		case err == nil:
			return x.forkParked(ctx, cc, created, groupID, len(childIDs))
		case errors.Is(err, ErrSteeringArrived):
			return x.forkRefuse(ctx, cc, controlErrorResult(ToolResultRefused, forkNewMessageCode, forkNewMessageMessage, nil))
		case attempt == 0 && errors.Is(err, ErrRevisionConflict):
			continue
		case attempt == 0 && errors.Is(err, ErrToolEntryStateConflict) && entry != nil:
			reread, getErr := x.Sessions.GetSessionEntry(ctx, run.Scope, entry.ID)
			if getErr != nil {
				return ControlResult{}, getErr
			}
			if reread.State != entry.State {
				return ControlResult{}, err
			}
			entry = reread
			continue
		case forkAbortsClaim(err):
			return ControlResult{}, err
		default:
			return x.forkRefuse(ctx, cc, controlErrorResult(ToolResultRefused, forkRejectedCode, err.Error(), nil))
		}
	}
}

// forkAbortsClaim: lease, revision, session binding, entry state and context
// errors abort the claim (rule 5); the next claim's resume settles again.
func forkAbortsClaim(err error) bool {
	for _, target := range []error{
		ErrLeaseLost, ErrRevisionConflict, ErrSessionMoved, ErrToolEntryStateConflict, ErrRunNotFound,
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
func (x *ControlExecutor) forkRefuse(ctx context.Context, cc ControlContext, result *ToolResult) (ControlResult, error) {
	if cc.Entry == nil {
		return ControlResult{}, fmt.Errorf("fork refused: %s", controlResultText(result))
	}
	return x.refuse(ctx, cc, result)
}

func (x *ControlExecutor) forkParked(ctx context.Context, cc ControlContext, created *RunForkResult, groupID string, children int) (ControlResult, error) {
	var entry *SessionEntry
	var source *AgentRun
	if created != nil && created.DependencyGroup != nil {
		entry, source = created.DependencyGroup.ToolEntry, created.DependencyGroup.Source
	}
	if entry == nil && cc.Entry != nil {
		reread, err := x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, cc.Entry.ID)
		if err != nil {
			return ControlResult{}, err
		}
		entry = reread
	}
	if entry != nil && entry.State != ToolEntryAwaitingChild {
		return ControlResult{}, fmt.Errorf("%w: fork entry %s is %s after the group was created", ErrToolEntryStateConflict, entry.ID, entry.State)
	}
	return ControlResult{
		Kind: ControlParked, Run: source, Entry: entry,
		Park: &LoopPark{
			Status: AgentRunStatusWaitingForDependency, Wake: &WakeCondition{Type: "run_dependencies", Reference: groupID},
			Summary: fmt.Sprintf("Waiting for %d forked branches", children), Committed: true,
		},
	}, nil
}

// resume turns the resolved dependency group into the entry's result: every
// branch's final answer, or its failure. A group still waiting re-parks.
func (forkHandler) resume(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResume, error) {
	return x.resumeFork(ctx, cc)
}

func (x *ControlExecutor) resumeFork(ctx context.Context, cc ControlContext) (ControlResume, error) {
	if x.Forks == nil || x.Forks.store == nil {
		return ControlResume{}, errors.New("control executor has no fork coordinator")
	}
	meta, err := DecodeControlEntryMeta(cc.Entry)
	if err != nil {
		return ControlResume{}, err
	}
	groupID := ""
	if meta != nil {
		groupID = meta.GroupID
	}
	if groupID == "" && cc.Entry.Tool.Wait != nil {
		groupID = cc.Entry.Tool.Wait.GroupID
	}
	store := x.Forks.store
	var group *RunDependencyGroup
	if groupID != "" {
		group, err = store.GetRunDependencyGroup(ctx, cc.Run.Scope, groupID)
		if err != nil && !errors.Is(err, ErrDependencyGroupNotFound) {
			return ControlResume{}, err
		}
	}
	if group == nil {
		// The group is created with the entry's transition, so this is a
		// store the kernel can no longer read; tell the model plainly.
		result, err := x.complete(ctx, cc, controlErrorResult(ToolResultFailed, forkLostCode,
			"The forked work can no longer be found; start it again if it is still needed.", nil), nil)
		return ControlResume{Resolved: true, Result: result}, err
	}
	if group.Status == RunDependencyGroupWaiting || group.Status == RunDependencyGroupOpen {
		return ControlResume{Park: &LoopPark{
			Status: AgentRunStatusWaitingForDependency, Wake: &WakeCondition{Type: "run_dependencies", Reference: group.ID},
			Summary: "Waiting for forked branches",
		}}, nil
	}
	edges, err := store.ListRunDependencies(ctx, cc.Run.Scope, group.ID)
	if err != nil {
		return ControlResume{}, err
	}
	value, err := x.forkResultValue(ctx, cc.Run, group, edges, meta)
	if err != nil {
		return ControlResume{}, err
	}
	status := ToolResultSucceeded
	switch group.Status {
	case RunDependencyGroupFailed:
		status = ToolResultFailed
	case RunDependencyGroupCanceled:
		status = ToolResultCanceled
	}
	result, err := x.complete(ctx, cc, controlJSONResult(status, value), nil)
	return ControlResume{Resolved: true, Result: result}, err
}

// forkResultValue is the model-visible result of a resolved group. It holds
// each child's answer, never its checkpoint.
func (x *ControlExecutor) forkResultValue(ctx context.Context, source *AgentRun, group *RunDependencyGroup, edges []*RunDependency, meta *ControlEntryMeta) (map[string]interface{}, error) {
	byTarget := make(map[string]*RunDependency, len(edges))
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
			return nil, fmt.Errorf("%w: fork child %s has no edge in group %s", ErrInvalidRunDependency, childID, group.ID)
		}
		child, err := x.Portfolio.GetAgentRun(ctx, source.Scope, childID)
		if err != nil {
			return nil, err
		}
		if child == nil {
			return nil, ErrRunNotFound
		}
		branch := map[string]interface{}{
			"branch": edge.ID, "goal": truncateControlText(child.Goal, child.ID), "runId": child.ID,
			"status": string(child.Status),
		}
		if child.AssignedAgentID != source.AssignedAgentID {
			branch["agentId"] = child.AssignedAgentID
		}
		if answer := forkChildAnswer(child); answer != "" {
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

// forkChildAnswer is a child's final answer: its report, else its summary.
func forkChildAnswer(child *AgentRun) string {
	for _, key := range []string{finalOutputReportKey, finalOutputSummaryKey} {
		if text, ok := child.Output[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// controlResultText is the message of an is_error control result.
func controlResultText(result *ToolResult) string {
	if result == nil {
		return ""
	}
	for _, part := range result.Content {
		var value struct{ Error, Message string }
		if part.Type == ContentPartJSON && json.Unmarshal(part.Value, &value) == nil {
			return value.Error + ": " + value.Message
		}
		if part.Type == ContentPartText {
			return part.Text
		}
	}
	return string(result.Status)
}
