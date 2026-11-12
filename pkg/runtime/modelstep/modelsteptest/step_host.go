package modelsteptest

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

// RecordingStepHost is a model step host that records every step request
// and answers it with Reply. It has no host tools.
type RecordingStepHost struct {
	Requests []models.ModelStepRequest
	Reply    func(models.ModelStepRequest) models.ModelStepResult
}

func (h *RecordingStepHost) ExecuteModelStep(_ context.Context, req models.ModelStepRequest) (models.ModelStepResult, error) {
	h.Requests = append(h.Requests, req)
	return h.Reply(req), nil
}

func (h *RecordingStepHost) ExecuteHostTool(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	return models.HostToolResult{}, models.ErrHostToolUnsupported
}

// StepResult is a valid Anthropic step result for req that says text, ending
// the turn, or asking for calls when there are any.
func StepResult(req models.ModelStepRequest, text string, calls ...models.ModelToolCall) models.ModelStepResult {
	stop := models.ModelStopEnd
	if len(calls) > 0 {
		stop = models.ModelStopToolCalls
	}
	return models.ModelStepResult{
		APIVersion: models.ModelStepAPIVersion, StepID: req.StepID,
		Assistant: models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "test-model", SystemDigest: "sys", ToolsDigest: "tools",
			Text: text, ToolCalls: calls, StopReason: stop,
		},
		Model: models.ModelStepModelInfo{Provider: models.ModelStepProviderAnthropic, Model: "test-model"},
	}
}
