package modelstep

import (
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// validateStepOutput checks a producer's output against its kind.
func ValidateStepOutput(kind models.StepProducerKind, out agentrun.StepOutput) error {
	if err := out.Usage.Validate(); err != nil {
		return agentrun.InvalidStepOutputf("%v", err)
	}
	switch kind {
	case models.StepProducerModel:
		if out.Assistant == nil {
			return agentrun.InvalidStepOutputf("model producer returned no assistant entry")
		}
		if len(out.ToolCalls) > 0 || out.Final != nil || out.Park != nil || out.KernelState != nil {
			return agentrun.InvalidStepOutputf("model producer returned deterministic output")
		}
		if err := checkProducedAssistant(out.Assistant); err != nil {
			return agentrun.InvalidStepOutputf("%v", err)
		}
	case models.StepProducerDeterministic:
		if out.Assistant != nil {
			return agentrun.InvalidStepOutputf("deterministic producer returned an assistant entry")
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
			return agentrun.InvalidStepOutputf("deterministic producer must return exactly one of tool calls, final, park and failure (got %d)", set)
		}
		if err := validateLoopToolCalls(out.ToolCalls); err != nil {
			return err
		}
		if out.Final != nil {
			if err := out.Final.Validate(); err != nil {
				return err
			}
		}
		if out.Park != nil {
			if out.Park.Committed {
				return agentrun.InvalidStepOutputf("a producer park cannot be committed")
			}
			if err := out.Park.Validate(); err != nil {
				return err
			}
		}
	default:
		return agentrun.InvalidStepOutputf("unknown producer kind %q", kind)
	}
	if out.Failure != nil {
		if err := out.Failure.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateLoopToolCalls(calls []agentrun.LoopToolCall) error {
	seen := make(map[string]bool, len(calls))
	for i, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return agentrun.InvalidStepOutputf("tool call %d has no id", i)
		}
		if seen[call.ID] {
			return agentrun.InvalidStepOutputf("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = true
		if strings.TrimSpace(call.Name) == "" {
			return agentrun.InvalidStepOutputf("tool call %q has no name", call.ID)
		}
		if err := models.ValidateToolArguments("tool call "+call.ID+" arguments", call.Arguments); err != nil {
			return agentrun.InvalidStepOutputf("%v", err)
		}
		if call.Directive != nil && call.Directive.BindingRevision < 0 {
			return agentrun.InvalidStepOutputf("tool call %q directive has a negative binding revision", call.ID)
		}
	}
	return nil
}
