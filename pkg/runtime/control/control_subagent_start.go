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

// start_subagents (section 04 §6): a settle-phase control tool that runs two to
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

type startSubagentsHandler struct{}

func (startSubagentsHandler) Spec(a agentrun.ControlAuthority) ToolSpec {
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

func (startSubagentsHandler) Decode(raw json.RawMessage) (interface{}, error) {
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

func (startSubagentsHandler) Run(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Result, error) {
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

// Resume turns the resolved dependency group into the entry's result: every
// branch's final answer, or its failure. A group still waiting re-parks.
func (startSubagentsHandler) Resume(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Resume, error) {
	return x.ResumeFork(ctx, cc)
}
