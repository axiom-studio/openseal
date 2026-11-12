package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// capabilityInvocationToolCallID is the id of the one governed tool call a
// capability invocation Run makes.
const capabilityInvocationToolCallID = "capability-invocation"

// CapabilityInvocationStepProducer deterministically projects a typed
// scheduled or event capability request into one governed tool call, then
// completes the Run with that call's output. It never executes a tool itself
// and never lets a model rename or widen the capability selected by the
// durable Objective/Event template.
type CapabilityInvocationStepProducer struct {
	actions []capability.ModelAction
	calls   ActionCallReader
}

func NewCapabilityInvocationStepProducer(actions []capability.ModelAction, calls ActionCallReader) (*CapabilityInvocationStepProducer, error) {
	if len(actions) == 0 {
		return nil, errors.New("deterministic capability invocation requires authorized actions")
	}
	if calls == nil {
		return nil, errors.New("deterministic capability invocation requires the action-call store")
	}
	return &CapabilityInvocationStepProducer{actions: cloneHostedModelActions(actions), calls: calls}, nil
}

func (p *CapabilityInvocationStepProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (p *CapabilityInvocationStepProducer) Produce(ctx context.Context, in StepInput) (StepOutput, error) {
	if p == nil || in.Run == nil || in.Step == nil {
		return StepOutput{}, errors.New("deterministic capability invocation requires a durable Run and step")
	}
	requested, ok := in.Run.Context[capabilityInvocationContextKey].(map[string]interface{})
	if !ok {
		return StepOutput{}, errors.New("Run has no typed capability invocation")
	}
	skillID, _ := requested["skillId"].(string)
	version, _ := requested["skillVersion"].(string)
	action, _ := requested["action"].(string)
	arguments, _ := requested["inputs"].(map[string]interface{})
	skillID, version, action = strings.TrimSpace(skillID), strings.TrimSpace(version), strings.TrimSpace(action)
	var selected *capability.ModelAction
	for index := range p.actions {
		candidate := &p.actions[index]
		if candidate.SkillID == skillID && candidate.Version == version && candidate.Action == action {
			if selected != nil {
				return StepOutput{}, errors.New("typed capability invocation matches multiple authorized actions")
			}
			selected = candidate
		}
	}
	if selected == nil {
		return StepOutput{}, fmt.Errorf("typed capability %s@%s/%s is not authorized", skillID, version, action)
	}
	result, call, err := producerToolResult(ctx, p.calls, in.Run, in.Path, capabilityInvocationToolCallID)
	if err != nil {
		return StepOutput{}, err
	}
	if result == nil {
		if arguments == nil {
			arguments = map[string]interface{}{}
		}
		toolCall, err := producerGovernedToolCall(capabilityInvocationToolCallID, selected, arguments, &ToolCallDirective{
			IdempotencyKey: "capability-invocation:" + in.Run.ID,
			Summary:        "Execute governed " + selected.Name,
		})
		if err != nil {
			return StepOutput{}, err
		}
		return StepOutput{ToolCalls: []LoopToolCall{toolCall}}, nil
	}
	if call == nil {
		return StepOutput{Failure: &LoopFailure{
			Kind: "execution", Code: "capability_invocation_refused",
			Message: "The scheduled capability " + selected.Name + " was refused before it ran.",
		}}, nil
	}
	if !producerCallMatches(call, selected) {
		return StepOutput{}, errors.New("durable action result does not match the typed capability invocation")
	}
	// The ActionCall is authoritative: a succeeded call completes the Run
	// whatever state its tool entry ended in.
	if call.Status != ActionCallStatusSucceeded {
		return StepOutput{Failure: &LoopFailure{
			Kind: "execution", Code: "capability_invocation_failed",
			Message: fmt.Sprintf("The scheduled capability %s did not complete (%s).", selected.Name, call.Status),
		}}, nil
	}
	return StepOutput{Final: &LoopFinal{
		Answer: "Completed " + selected.Name + ".",
		Output: map[string]interface{}{"capabilityResult": result.Output},
		Status: AgentRunStatusCompleted,
	}}, nil
}

// producerGovernedToolCall builds a deterministic producer's governed tool
// call pinned to the selected binding and Skill version.
func producerGovernedToolCall(id string, selected *capability.ModelAction, arguments map[string]interface{}, directive *ToolCallDirective) (LoopToolCall, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return LoopToolCall{}, fmt.Errorf("encode %s arguments: %w", selected.Name, err)
	}
	directive.BindingID = selected.BindingID
	directive.BindingRevision = selected.BindingRevision
	directive.ExpectedSkillVersion = selected.Version
	return LoopToolCall{ID: id, Name: selected.Name, Arguments: raw, Directive: directive}, nil
}

func producerCallMatches(call *ActionCall, selected *capability.ModelAction) bool {
	if selected.BindingID != "" && (call.BindingID != selected.BindingID || call.BindingRevision != selected.BindingRevision) {
		return false
	}
	return call.SkillID == selected.SkillID && call.SkillVersion == selected.Version && call.Action == selected.Action
}

// producerToolResult returns the latest terminal tool entry the Run recorded
// for toolCallID on its session path, with the ActionCall behind it (nil when
// the call was refused before any row existed). It returns nil, nil, nil when
// the producer has not asked for that call yet. An entry that is still open
// is an error: the loop resumes open tool entries before it asks a
// deterministic producer for the next step.
func producerToolResult(ctx context.Context, calls ActionCallReader, run *AgentRun, path *AgentSessionPath, toolCallID string) (*LoopToolResult, *ActionCall, error) {
	if path == nil {
		return nil, nil, nil
	}
	var entry *SessionEntry
	for index := len(path.Entries) - 1; index >= 0; index-- {
		candidate := path.Entries[index]
		if candidate != nil && candidate.Type == SessionEntryTool && candidate.Tool != nil &&
			candidate.RunID == run.ID && candidate.ToolCallID == toolCallID {
			entry = candidate
			break
		}
	}
	if entry == nil {
		return nil, nil, nil
	}
	switch entry.State {
	case ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted:
	default:
		return nil, nil, fmt.Errorf("tool call %s is still %s", toolCallID, entry.State)
	}
	result := &LoopToolResult{ToolCallID: entry.ToolCallID, Name: entry.Tool.Name, State: entry.State, ActionCallID: entry.ActionCallID}
	if entry.Tool.Result != nil {
		result.Content = append([]ContentPart(nil), entry.Tool.Result.Content...)
	}
	if strings.TrimSpace(entry.ActionCallID) == "" {
		return result, nil, nil
	}
	call, err := calls.GetActionCall(ctx, run.Scope, entry.ActionCallID)
	if err != nil {
		return nil, nil, fmt.Errorf("load action call %s of tool call %s: %w", entry.ActionCallID, toolCallID, err)
	}
	if call == nil || call.RunID != run.ID {
		return nil, nil, fmt.Errorf("tool call %s points at an action call of another Run", toolCallID)
	}
	if !terminalApprovalActionStatus(call.Status) {
		return nil, nil, fmt.Errorf("action call %s of tool call %s is still %s", call.ID, toolCallID, call.Status)
	}
	if call.Status == ActionCallStatusSucceeded {
		result.Output = cloneMap(call.Output)
	}
	return result, call, nil
}
