package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// StepHost runs one model call or one host-native tool for the kernel loop.
// Atlas implements it over HTTP (sentinel → atlas), the OSS daemon in
// process. Neither call executes governed actions: those run inline in the
// kernel.
type StepHost interface {
	ExecuteModelStep(ctx context.Context, req ModelStepRequest) (ModelStepResult, error)
	ExecuteHostTool(ctx context.Context, req HostToolRequest) (HostToolResult, error)
}

// maxModelStepResultTextBytes caps the assistant text of one model step.
const maxModelStepResultTextBytes = 256 << 10

func invalidModelStepResult(format string, args ...interface{}) error {
	return fmt.Errorf("%w: model step result: %s", ErrTurnHostConfiguration, fmt.Sprintf(format, args...))
}

// ValidateModelStepResult checks what a StepHost returned for req before the
// kernel appends it. Every violation wraps ErrTurnHostConfiguration.
//
// It does not check tool names against req.Tools, nor argument size: an
// unknown tool, arguments the host flagged in ArgumentsError and oversize
// arguments become error tool entries so the model can correct itself.
func ValidateModelStepResult(req ModelStepRequest, res ModelStepResult) error {
	if res.APIVersion != ModelStepAPIVersion {
		return invalidModelStepResult("apiVersion %q, want %q", res.APIVersion, ModelStepAPIVersion)
	}
	if res.StepID != req.StepID {
		return invalidModelStepResult("stepId %q does not match the request's %q", res.StepID, req.StepID)
	}
	a := res.Assistant
	if a.Limits != nil {
		return invalidModelStepResult("assistant limits are kernel-filled")
	}
	if err := checkProducedAssistant(&a); err != nil {
		return invalidModelStepResult("%v", err)
	}
	if res.Model.Provider != a.Provider {
		return invalidModelStepResult("model provider %q does not match the assistant's %q", res.Model.Provider, a.Provider)
	}
	if strings.TrimSpace(res.Model.Model) == "" {
		return invalidModelStepResult("model info has no model")
	}
	if res.Model.ContextTokens < 0 || res.Model.InputLimitTokens < 0 || res.Model.MaxOutputTokens < 0 ||
		res.Render.EstimatedInputTokens < 0 || res.Render.ReasoningCutoffSequence < 0 {
		return invalidModelStepResult("model limits and render info cannot be negative")
	}
	if len(a.ToolCalls) == 0 && res.Narration != "" {
		return invalidModelStepResult("narration without tool calls")
	}
	switch {
	case req.Purpose == ModelStepPurposeSummary:
		if len(a.ToolCalls) > 0 {
			return invalidModelStepResult("summary step returned tool calls")
		}
		if strings.TrimSpace(a.Text) == "" {
			return invalidModelStepResult("summary step returned no summary text")
		}
	case req.Purpose.toolFree():
		if len(a.ToolCalls) > 0 {
			return invalidModelStepResult("%s step returned tool calls", req.Purpose)
		}
	}
	return nil
}

// checkProducedAssistant is the shape check shared by ValidateModelStepResult
// and validateStepOutput. Unlike AssistantEntry.validate it does not cap
// argument size, because oversize arguments become an error tool entry.
func checkProducedAssistant(a *AssistantEntry) error {
	if !a.Provider.Valid() {
		return fmt.Errorf("unknown assistant provider %q", a.Provider)
	}
	if strings.TrimSpace(a.Model) == "" {
		return errors.New("assistant entry has no model")
	}
	if !a.StopReason.Valid() {
		return fmt.Errorf("unknown stop reason %q", a.StopReason)
	}
	if len(a.Text) > maxModelStepResultTextBytes {
		return fmt.Errorf("assistant text is %d bytes (max %d)", len(a.Text), maxModelStepResultTextBytes)
	}
	u := a.Usage
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.ReasoningTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 ||
		u.DurationMS < 0 || u.ProviderDurationMS < 0 || u.FirstOutputMS < 0 || u.Attempts < 0 ||
		u.Cost < 0 || math.IsNaN(u.Cost) || math.IsInf(u.Cost, 0) {
		return errors.New("usage cannot be negative, NaN, or infinite")
	}
	seen := make(map[string]bool, len(a.ToolCalls))
	for i, call := range a.ToolCalls {
		if strings.TrimSpace(call.ID) == "" {
			return fmt.Errorf("tool call %d has no id", i)
		}
		if seen[call.ID] {
			return fmt.Errorf("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = true
		if strings.TrimSpace(call.Name) == "" {
			return fmt.Errorf("tool call %q has no name", call.ID)
		}
		if !isModelToolArgumentsObject(call.Arguments) {
			return fmt.Errorf("tool call %q arguments are not a JSON object", call.ID)
		}
	}
	if a.StopReason == ModelStopToolCalls && len(a.ToolCalls) == 0 {
		return fmt.Errorf("stop reason %q with no tool calls", a.StopReason)
	}
	if a.Native != nil {
		for i, raw := range a.Native.Anthropic {
			if !json.Valid(raw) {
				return fmt.Errorf("native anthropic block %d is not JSON", i)
			}
		}
		for i, raw := range a.Native.Responses {
			if !json.Valid(raw) {
				return fmt.Errorf("native responses item %d is not JSON", i)
			}
		}
	}
	return nil
}

func isModelToolArgumentsObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
