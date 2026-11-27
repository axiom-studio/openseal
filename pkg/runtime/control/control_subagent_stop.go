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

// stop_subagents (inline phase) cancels running subagents through the Run
// cancel command, then resolves their dependency edges at once (a stopped
// child counts as canceled), so a wait never hangs on them.

type stopSubagentsArgs struct {
	IDs []string `json:"ids"`
}

type stopSubagentsHandler struct{}

func (stopSubagentsHandler) Spec(agentrun.ControlAuthority) ToolSpec {
	return ToolSpec{
		Description: "Stop subagents that are still running.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"ids"},
			"properties": map[string]interface{}{
				"ids": map[string]interface{}{
					"type": "array", "items": map[string]interface{}{"type": "string"}, "minItems": 1, "maxItems": 64,
					"description": "Subagent ids (s1, ...).",
				},
			},
		},
	}
}

func (stopSubagentsHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[stopSubagentsArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*stopSubagentsArgs)
	if len(args.IDs) == 0 {
		return nil, errors.New("ids must name at least one subagent")
	}
	return args, nil
}

func (stopSubagentsHandler) Run(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Result, error) {
	args := cc.Arguments.(*stopSubagentsArgs)
	subagents, err := LoadSubagents(ctx, x.Sessions, x.Portfolio, cc.Run, cc.Entry.ID)
	if err != nil {
		return Result{}, err
	}
	var refs []SubagentRef
	seen := map[string]bool{}
	for _, id := range args.IDs {
		ref, _, ok := subagents.lookup(id)
		if !ok {
			return x.refuse(ctx, cc, subagents.unknown(strings.TrimSpace(id)))
		}
		if !seen[ref.ID] {
			seen[ref.ID] = true
			refs = append(refs, ref)
		}
	}
	values := make([]interface{}, 0, len(refs))
	meta := &EntryMeta{}
	for _, ref := range refs {
		child, stopped, err := x.stopSubagent(ctx, cc, subagents.runs[ref.RunID])
		if err != nil {
			return Result{}, err
		}
		value := map[string]interface{}{"id": ref.ID, "status": subagentStatus(child)}
		if stopped {
			meta.Reported = append(meta.Reported, child.ID)
		} else {
			value["message"] = "It had already ended."
		}
		values = append(values, value)
	}
	return x.complete(ctx, cc, models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{"subagents": values}), meta)
}

// stopSubagent cancels child unless it already ended; stopped is true when
// this call stopped it.
func (x *Executor) stopSubagent(ctx context.Context, cc agentrun.ControlContext, child *models.AgentRun) (*models.AgentRun, bool, error) {
	commands, err := x.runCommands()
	if err != nil {
		return nil, false, err
	}
	actor := models.ActivityActor{Type: "agent", ID: cc.Run.AssignedAgentID}
	for attempt := 0; attempt < 4; attempt++ {
		if child == nil {
			return nil, false, models.ErrRunNotFound
		}
		if models.IsTerminalAgentRunStatus(child.Status) {
			return child, false, nil
		}
		result, err := commands.CommandAgentRun(ctx, agentrun.CommandRequest{
			Scope: child.Scope, RunID: child.ID, ExpectedRevision: child.Revision, Kind: agentrun.CommandCancel,
			Actor: actor, Summary: "Stopped by the Run that started it",
		})
		switch {
		case err == nil:
			if x.Forks.Configured() {
				if _, err := x.Forks.CompleteChild(ctx, result.Run, actor); err != nil && !errors.Is(err, models.ErrDependencyConflict) {
					return nil, false, fmt.Errorf("resolve stopped subagent %s: %w", child.ID, err)
				}
			}
			return result.Run, true, nil
		case !errors.Is(err, models.ErrRevisionConflict) && !errors.Is(err, models.ErrInvalidRunTransition):
			return nil, false, err
		}
		if child, err = x.Portfolio.GetAgentRun(ctx, child.Scope, child.ID); err != nil {
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("stop subagent run %s: %w", child.ID, models.ErrRevisionConflict)
}
