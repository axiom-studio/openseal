package runtime

import (
	"strings"
)

// validateStepOutput checks a producer's output against its kind.
func validateStepOutput(kind StepProducerKind, out StepOutput) error {
	if err := out.Usage.Validate(); err != nil {
		return invalidStepOutput("%v", err)
	}
	switch kind {
	case StepProducerModel:
		if out.Assistant == nil {
			return invalidStepOutput("model producer returned no assistant entry")
		}
		if len(out.ToolCalls) > 0 || out.Final != nil || out.Park != nil || out.KernelState != nil {
			return invalidStepOutput("model producer returned deterministic output")
		}
		if err := checkProducedAssistant(out.Assistant); err != nil {
			return invalidStepOutput("%v", err)
		}
	case StepProducerDeterministic:
		if out.Assistant != nil {
			return invalidStepOutput("deterministic producer returned an assistant entry")
		}
		set := 0
		if len(out.ToolCalls) > 0 {
			set++
		}
		if out.Final != nil {
			set++
		}
		if out.Park != nil {
			set++
		}
		if out.Failure != nil {
			set++
		}
		if set != 1 {
			return invalidStepOutput("deterministic producer must return exactly one of tool calls, final, park and failure (got %d)", set)
		}
		if err := validateLoopToolCalls(out.ToolCalls); err != nil {
			return err
		}
		if out.Final != nil {
			if err := out.Final.validate(); err != nil {
				return err
			}
		}
		if out.Park != nil {
			if out.Park.Committed {
				return invalidStepOutput("a producer park cannot be committed")
			}
			if err := out.Park.validate(); err != nil {
				return err
			}
		}
	default:
		return invalidStepOutput("unknown producer kind %q", kind)
	}
	if out.Failure != nil {
		if err := out.Failure.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateLoopToolCalls(calls []LoopToolCall) error {
	seen := make(map[string]bool, len(calls))
	for i, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return invalidStepOutput("tool call %d has no id", i)
		}
		if seen[call.ID] {
			return invalidStepOutput("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = true
		if strings.TrimSpace(call.Name) == "" {
			return invalidStepOutput("tool call %q has no name", call.ID)
		}
		if err := validateToolArguments("tool call "+call.ID+" arguments", call.Arguments); err != nil {
			return invalidStepOutput("%v", err)
		}
		if call.Directive != nil && call.Directive.BindingRevision < 0 {
			return invalidStepOutput("tool call %q directive has a negative binding revision", call.ID)
		}
	}
	return nil
}
