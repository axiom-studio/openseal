package runloop

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
)

// toolEntryPlanner is implemented by the claim executor.
type toolEntryPlanner interface {
	planToolEntries(binding *agentrun.TurnRunnerBinding, assistant *models.SessionEntry) ([]*models.SessionEntry, error)
	planToolCallEntries(binding *agentrun.TurnRunnerBinding, run *models.AgentRun, step *models.AgentTurn, sessionID string, calls []agentrun.LoopToolCall) ([]*models.SessionEntry, error)
}

// Plan-time refusal code and texts.
const (
	loopToolArgumentsTooLargeCode = "tool_arguments_too_large"
	loopToolNotAvailableCode      = "tool_not_available"
	loopToolNotLoadedCode         = "tool_not_loaded"
	loopToolRefusedCode           = "tool_refused"
	loopToolNotRunCode            = "not_run"
)

// PlanToolEntries builds the tool entries of a model step's assistant entry,
// one per tool call in call order, for the loop's single
// [assistant, tool×N] append. Each entry carries SideEffect, RepeatEffect and
// ArgumentsDigest. A call with ArgumentsError or oversize arguments is
// appended already error/refused. assistant must carry its session, Run and
// step identity; binding.Tools must be a claim executor from
// StepToolDispatcher.ForClaim.
func PlanToolEntries(binding *agentrun.TurnRunnerBinding, assistant *models.SessionEntry) ([]*models.SessionEntry, error) {
	planner, err := bindingToolPlanner(binding)
	if err != nil {
		return nil, err
	}
	return planner.planToolEntries(binding, assistant)
}

// PlanToolCallEntries builds the tool entries of a deterministic producer's
// tool calls (StepOutput.ToolCalls). Each entry stores its directive.
func PlanToolCallEntries(binding *agentrun.TurnRunnerBinding, run *models.AgentRun, step *models.AgentTurn, sessionID string, calls []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
	planner, err := bindingToolPlanner(binding)
	if err != nil {
		return nil, err
	}
	return planner.planToolCallEntries(binding, run, step, sessionID, calls)
}

func bindingToolPlanner(binding *agentrun.TurnRunnerBinding) (toolEntryPlanner, error) {
	if binding == nil || binding.Tools == nil {
		return nil, errors.New("tool entries need a binding with a tool executor")
	}
	planner, ok := binding.Tools.(toolEntryPlanner)
	if !ok {
		return nil, fmt.Errorf("tool executor %T cannot plan tool entries", binding.Tools)
	}
	return planner, nil
}

func (x *ClaimToolExecutor) planToolEntries(binding *agentrun.TurnRunnerBinding, assistant *models.SessionEntry) ([]*models.SessionEntry, error) {
	if assistant == nil || assistant.Type != models.SessionEntryAssistant || assistant.Assistant == nil {
		return nil, errors.New("plan tool entries needs an assistant entry")
	}
	if assistant.SessionID == "" || assistant.StepID == "" {
		return nil, errors.New("plan tool entries needs the assistant entry's session and step")
	}
	entries := make([]*models.SessionEntry, 0, len(assistant.Assistant.ToolCalls))
	for _, call := range assistant.Assistant.ToolCalls {
		entry := &models.SessionEntry{
			ID:    models.SessionEntryID(assistant.SessionID, models.ToolEntryKey(assistant.StepID, call.ID)),
			Scope: assistant.Scope, SessionID: assistant.SessionID, RunID: assistant.RunID,
			StepID: assistant.StepID, Step: assistant.Step,
			Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: call.ID,
			Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, ArgumentsError: call.ArgumentsError, SideEffect: models.ToolSideEffectNone},
		}
		if err := x.planEntry(binding, entry, nil); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (x *ClaimToolExecutor) planToolCallEntries(binding *agentrun.TurnRunnerBinding, run *models.AgentRun, step *models.AgentTurn, sessionID string, calls []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
	if run == nil || step == nil || sessionID == "" {
		return nil, errors.New("plan tool call entries needs the Run, its step and its session")
	}
	entries := make([]*models.SessionEntry, 0, len(calls))
	for _, call := range calls {
		entry := &models.SessionEntry{
			ID:    models.SessionEntryID(sessionID, models.ToolEntryKey(step.ID, call.ID)),
			Scope: run.Scope, SessionID: sessionID, RunID: run.ID, StepID: step.ID, Step: step.Sequence,
			Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: call.ID,
			Tool: &models.ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: models.ToolSideEffectNone},
		}
		if call.Directive != nil {
			encoded, err := json.Marshal(call.Directive)
			if err != nil {
				return nil, fmt.Errorf("encode directive of tool call %s: %w", call.ID, err)
			}
			entry.Tool.Directive = encoded
		}
		if err := x.planEntry(binding, entry, call.Directive); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// planEntry stamps one pending entry, or ends it error/refused when its
// arguments cannot be used.
func (x *ClaimToolExecutor) planEntry(binding *agentrun.TurnRunnerBinding, entry *models.SessionEntry, directive *agentrun.ToolCallDirective) error {
	tool := entry.Tool
	if len(tool.Arguments) > models.MaxToolArgumentsBytes {
		tool.ArgumentsError = fmt.Sprintf("The tool arguments are %d bytes; the limit is %d.", len(tool.Arguments), models.MaxToolArgumentsBytes)
		tool.Arguments = nil
		entry.State = models.ToolEntryError
		tool.Result = control.ErrorResult(models.ToolResultRefused, loopToolArgumentsTooLargeCode, tool.ArgumentsError, nil)
		return nil
	}
	if tool.ArgumentsError != "" {
		entry.State = models.ToolEntryError
		tool.Result = control.ErrorResult(models.ToolResultRefused, control.ArgumentsInvalidCode, tool.ArgumentsError, nil)
		return nil
	}
	resolved, err := x.resolveLoopTool(binding, tool.Name, tool.Arguments, directive)
	if err != nil {
		return err
	}
	tool.SideEffect = resolved.SideEffect
	return stampRepeatFields(tool, resolved.Subject)
}
