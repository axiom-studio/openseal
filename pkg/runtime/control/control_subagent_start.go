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
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// start_subagents (section 04 §6): a settle-phase control tool that hands one
// to eight self-contained goals to subagents, child Runs that work at the
// same time. Each gets a short id (s1, s2, ...).
//
// Foreground (the default): the dependency-group create moves the tool
// entry to awaiting_child, creates the same-Agent child sessions and parks
// the source on waiting_for_dependency in one transaction (CTL-5a). When the
// group resolves, the source is queued again and the resume turns every
// subagent's final answer (or failure) into this entry's result.
//
// Background: the group is detached and the entry completes in the same
// transaction with the ids; the source keeps stepping, and each subagent's
// end reaches it as a subagent note (SubagentNotes). Either way a failed
// subagent is part of the result, never a Run failure (D6).

const (
	minimumSubagents     = 1
	maximumSubagents     = 8
	maximumForkGoalBytes = 16384
)

type subagentBriefArgs struct {
	Goal    string                 `json:"goal"`
	AgentID string                 `json:"agentId,omitempty"`
	Context map[string]interface{} `json:"context,omitempty"`
	Fresh   bool                   `json:"fresh,omitempty"`
}

type startSubagentsArgs struct {
	Subagents  []subagentBriefArgs `json:"subagents"`
	Mode       models.FanInMode    `json:"mode,omitempty"`
	Background bool                `json:"background,omitempty"`
}

type startSubagentsHandler struct{}

func (startSubagentsHandler) Spec(a agentrun.ControlAuthority) ToolSpec {
	brief := map[string]interface{}{
		"goal": map[string]interface{}{
			"type": "string", "minLength": 1, "maxLength": maximumForkGoalBytes,
			"description": "Self-contained goal; say what to return.",
		},
		"context": map[string]interface{}{
			"type": "object", "description": "Non-secret data the subagent needs.",
		},
		// No target enum: the Agents are in the capabilities note and the
		// handler checks each agentId, so the schema never changes with them.
		"agentId": map[string]interface{}{
			"type": "string", "description": "Omit to run it as yourself; otherwise an agentId from the capabilities note.",
		},
		"fresh": map[string]interface{}{
			"type": "boolean", "description": "Start from the goal alone instead of this conversation (for a very long context).",
		},
	}
	return ToolSpec{
		Description: "Hand independent pieces of work to subagents that run at the same time. Each gets an id (s1, s2, ...); " +
			"only their final answers come back to you, so ask them to say what they checked and what is unsure.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"subagents"},
			"properties": map[string]interface{}{
				"subagents": map[string]interface{}{
					"type": "array", "minItems": minimumSubagents, "maxItems": maximumSubagents,
					"items": map[string]interface{}{
						"type": "object", "additionalProperties": false, "required": []interface{}{"goal"},
						"properties": brief,
					},
				},
				"mode": map[string]interface{}{
					"type": "string", "enum": []interface{}{string(models.FanInModeAll), string(models.FanInModeAny)},
					"description": "all: wait for every subagent (default). any: the first success wins; the rest are stopped.",
				},
				"background": map[string]interface{}{
					"type": "boolean",
					"description": "Return at once with the ids and keep working; each subagent's end arrives as a note. " +
						"You cannot answer while they run: wait for them or stop them.",
				},
			},
		},
	}
}

func (startSubagentsHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[startSubagentsArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*startSubagentsArgs)
	if n := len(args.Subagents); n < minimumSubagents || n > maximumSubagents {
		return nil, fmt.Errorf("subagents must hold %d to %d goals, got %d", minimumSubagents, maximumSubagents, n)
	}
	for i := range args.Subagents {
		brief := &args.Subagents[i]
		brief.Goal = strings.TrimSpace(brief.Goal)
		brief.AgentID = strings.TrimSpace(brief.AgentID)
		if brief.Goal == "" {
			return nil, fmt.Errorf("subagent %d needs a goal", i+1)
		}
		if len(brief.Goal) > maximumForkGoalBytes || !utf8.ValidString(brief.Goal) {
			return nil, fmt.Errorf("subagent %d goal must be valid text of at most %d bytes", i+1, maximumForkGoalBytes)
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

func (startSubagentsHandler) Run(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Result, error) {
	args := cc.Arguments.(*startSubagentsArgs)
	subagents, err := LoadSubagents(ctx, x.Sessions, x.Portfolio, cc.Run, cc.Entry.ID)
	if err != nil {
		return Result{}, err
	}
	if refusal := subagentCapacityRefusal(subagents, len(args.Subagents)); refusal != nil {
		return x.refuse(ctx, cc, refusal)
	}
	forkID := "fork-" + cc.Entry.ID
	ids := subagents.nextIDs(len(args.Subagents))
	branches := make([]agentrun.RunForkBranch, len(args.Subagents))
	refs := make([]SubagentRef, len(args.Subagents))
	fresh := map[string]bool{}
	for i, arg := range args.Subagents {
		if err := validateForkBranchContext(arg.Context); err != nil {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, "context_invalid",
				fmt.Sprintf("Subagent %d context is not allowed: %v.", i+1, err), nil))
		}
		agentID := ""
		if arg.AgentID != "" && arg.AgentID != cc.Run.AssignedAgentID {
			if !forkTargetEligible(arg.AgentID, cc.Authority.ForkTargets) {
				return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, "agent_not_eligible",
					fmt.Sprintf("Subagent %d names an Agent you cannot hand work to; omit agentId to run it as yourself.", i+1),
					map[string]interface{}{"agentId": arg.AgentID}))
			}
			agentID = arg.AgentID
		}
		branches[i] = agentrun.RunForkBranch{
			ID: ids[i], Goal: arg.Goal, AssignedAgentID: agentID, Context: maps.Clone(arg.Context),
			Checkpoint: map[string]interface{}{},
		}
		refs[i] = SubagentRef{ID: ids[i], RunID: agentrun.StableForkIdentifier(cc.Run.ID, forkID, "child:"+ids[i]), Background: args.Background}
		fresh[ids[i]] = arg.Fresh
	}
	if err := x.fundBranches(cc, branches); err != nil {
		var insufficient *modelstep.ControlBudgetInsufficientError
		if errors.As(err, &insufficient) {
			return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, insufficient.Code(), insufficient.Message(), nil))
		}
		return Result{}, err
	}
	spec := ForkSpec{
		ForkID: forkID, Branches: branches, ChildKind: models.RunKindAgentWork, Fresh: fresh,
		// Every subagent ends before the group does, so none is left running
		// unreported when the parent resumes.
		Policy:    models.RunDependencyPolicy{Mode: args.Mode, FailureMode: models.DependencyFailureWait},
		Subagents: refs, Background: args.Background,
	}
	if args.Background {
		spec.BackgroundResult = startedSubagentsResult(refs, branches)
		spec.Surfaced = x.subagentFollowUpSurface(cc.Authority)
	}
	return x.Fork(ctx, cc, spec)
}

// subagentCapacityRefusal refuses more than maximumRunningSubagents at once.
func subagentCapacityRefusal(subagents *Subagents, adding int) *models.ToolResult {
	running := subagents.running(false)
	if len(running)+adding <= maximumRunningSubagents {
		return nil
	}
	return ErrorResult(models.ToolResultRefused, "subagents_limit", fmt.Sprintf(
		"At most %d subagents can work at once and %d are running (%s); wait for or stop some first.",
		maximumRunningSubagents, len(running), strings.Join(running, ", ")), nil)
}

// fundBranches gives each branch its share of a bounded Run's budget; the
// children of an unbounded Run stay unbounded.
func (x *Executor) fundBranches(cc agentrun.ControlContext, branches []agentrun.RunForkBranch) error {
	stepID := ""
	if cc.Step != nil {
		stepID = cc.Step.ID
	}
	budgets, err := modelstep.ControlBranchBudgets(cc.Run, stepID, len(branches))
	if err != nil {
		return err
	}
	for i := range branches {
		branches[i].Budget = budgets[i]
	}
	return nil
}

// startedSubagentsResult is a background start's result: the ids, at once.
func startedSubagentsResult(refs []SubagentRef, branches []agentrun.RunForkBranch) *models.ToolResult {
	started := make([]interface{}, len(refs))
	ids := make([]string, len(refs))
	for i, ref := range refs {
		started[i] = map[string]interface{}{"id": ref.ID, "goal": truncateControlText(branches[i].Goal, ref.ID), "status": "running"}
		ids[i] = ref.ID
	}
	return models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{
		"subagents": started,
		"message":   fmt.Sprintf("Started %s in the background. A note tells you as each one ends; wait_subagents waits for them.", strings.Join(ids, ", ")),
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

// Resume turns the resolved group into the entry's result: every subagent's
// final answer, or its failure. A group still waiting re-parks. The result
// surfaces the follow-up tools unless no subagent succeeded.
func (startSubagentsHandler) Resume(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Resume, error) {
	return x.resumeSubagentWait(ctx, cc, true)
}
