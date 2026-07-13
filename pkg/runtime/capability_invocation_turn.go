package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const capabilityInvocationContextKey = "capabilityInvocation"

// CapabilityInvocationTurnRunner deterministically projects a typed scheduled
// or event capability request into the normal governed action lifecycle. It
// never executes a tool itself and never lets a model rename or widen the
// capability selected by the durable Objective/Event template.
type CapabilityInvocationTurnRunner struct {
	actions []capability.ModelAction
}

func NewCapabilityInvocationTurnRunner(actions []capability.ModelAction) (*CapabilityInvocationTurnRunner, error) {
	if len(actions) == 0 {
		return nil, errors.New("deterministic capability invocation requires authorized actions")
	}
	return &CapabilityInvocationTurnRunner{actions: cloneHostedModelActions(actions)}, nil
}

func (r *CapabilityInvocationTurnRunner) RunTurn(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || input.Run == nil || input.Turn == nil {
		return nil, errors.New("deterministic capability invocation requires a durable Run and Turn")
	}
	requested, ok := input.Run.Context[capabilityInvocationContextKey].(map[string]interface{})
	if !ok {
		return nil, errors.New("Run has no typed capability invocation")
	}
	skillID, _ := requested["skillId"].(string)
	version, _ := requested["skillVersion"].(string)
	action, _ := requested["action"].(string)
	arguments, _ := requested["inputs"].(map[string]interface{})
	skillID, version, action = strings.TrimSpace(skillID), strings.TrimSpace(version), strings.TrimSpace(action)
	var selected *capability.ModelAction
	for index := range r.actions {
		candidate := &r.actions[index]
		if candidate.SkillID == skillID && candidate.Version == version && candidate.Action == action {
			if selected != nil {
				return nil, errors.New("typed capability invocation matches multiple authorized actions")
			}
			selected = candidate
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("typed capability %s@%s/%s is not authorized", skillID, version, action)
	}
	checkpoint := cloneMap(input.Run.Checkpoint)
	if checkpoint == nil {
		checkpoint = map[string]interface{}{}
	}
	if last, resumed := checkpoint["lastAction"].(map[string]interface{}); resumed {
		if (selected.BindingID != "" && (last["bindingId"] != selected.BindingID || fmt.Sprint(last["bindingRevision"]) != fmt.Sprint(selected.BindingRevision))) ||
			last["skillId"] != selected.SkillID || last["skillVersion"] != selected.Version || last["action"] != selected.Action {
			return nil, errors.New("durable action result does not match the typed capability invocation")
		}
		status, _ := last["status"].(string)
		if status != string(ActionCallStatusSucceeded) {
			return &TurnOutcome{
				OutputSummary: "Governed capability invocation failed", RunError: "governed capability invocation failed",
				ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusFailed,
			}, nil
		}
		result := last["result"]
		delete(checkpoint, "lastAction")
		delete(checkpoint, "capabilityActionInputs")
		return &TurnOutcome{
			Decisions:     []TurnDecision{{Summary: "Consumed the authoritative governed capability result"}},
			OutputSummary: "Governed capability invocation completed", ContinuationCheckpoint: checkpoint,
			RunOutput: map[string]interface{}{"capabilityResult": result}, NextRunStatus: AgentRunStatusCompleted,
		}, nil
	}
	checkpoint["capabilityActionInputs"] = map[string]interface{}{"requested": cloneMap(arguments)}
	return &TurnOutcome{
		Decisions: []TurnDecision{{Summary: "Selected the exact capability declared by the durable Run template"}},
		ProposedActions: []TurnAction{{
			Type: "skill_action", Capability: selected.Name, Summary: "Execute governed " + selected.Name,
			BindingID: selected.BindingID, BindingRevision: selected.BindingRevision,
			IdempotencyKey: "capability-invocation:" + input.Run.ID, InputRef: "/capabilityActionInputs/requested",
		}},
		OutputSummary:          "Requested governed capability " + selected.Name,
		ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusRunning,
	}, nil
}
