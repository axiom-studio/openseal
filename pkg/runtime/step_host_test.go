package runtime

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func validModelStepResultFixture() (ModelStepRequest, ModelStepResult) {
	req := ModelStepRequest{APIVersion: ModelStepAPIVersion, Purpose: ModelStepPurposeStep, StepID: "step-1"}
	res := ModelStepResult{
		APIVersion: ModelStepAPIVersion,
		StepID:     "step-1",
		Assistant: AssistantEntry{
			Provider:   ModelStepProviderAnthropic,
			Model:      "claude-x",
			Text:       "I'll check the drive first.",
			ToolCalls:  []ModelToolCall{{ID: "toolu_1", Name: "drive_search", Arguments: json.RawMessage(`{"query":"Q3"}`)}},
			StopReason: ModelStopToolCalls,
			Usage:      ModelStepUsage{InputTokens: 10, OutputTokens: 2, Attempts: 1},
		},
		Narration: "I'll check the drive first.",
		Model:     ModelStepModelInfo{Provider: ModelStepProviderAnthropic, Model: "claude-x", ContextTokens: 200000},
		Render:    ModelStepRenderInfo{EstimatedInputTokens: 10},
	}
	return req, res
}

func TestValidateModelStepResultAcceptsToolStep(t *testing.T) {
	req, res := validModelStepResultFixture()
	if err := ValidateModelStepResult(req, res); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	// Oversize arguments and unknown tool names are left to the executor.
	res.Assistant.ToolCalls[0].Name = "not_offered"
	res.Assistant.ToolCalls[0].Arguments = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxToolArgumentsBytes) + `"}`)
	if err := ValidateModelStepResult(req, res); err != nil {
		t.Fatalf("unknown tool / oversize arguments must not be a host error: %v", err)
	}
	// A refusal is a result, not an error.
	_, res = validModelStepResultFixture()
	res.Assistant.ToolCalls, res.Narration, res.Assistant.StopReason = nil, "", ModelStopRefusal
	if err := ValidateModelStepResult(req, res); err != nil {
		t.Fatalf("refusal rejected: %v", err)
	}
}

func TestValidateModelStepResultRejects(t *testing.T) {
	cases := map[string]func(*ModelStepRequest, *ModelStepResult){
		"api version":     func(_ *ModelStepRequest, r *ModelStepResult) { r.APIVersion = "v0" },
		"step id":         func(_ *ModelStepRequest, r *ModelStepResult) { r.StepID = "step-2" },
		"provider":        func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.Provider = "other" },
		"model":           func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.Model = " " },
		"model info":      func(_ *ModelStepRequest, r *ModelStepResult) { r.Model.Provider = ModelStepProviderChat },
		"model info name": func(_ *ModelStepRequest, r *ModelStepResult) { r.Model.Model = "" },
		"stop reason":     func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.StopReason = "tool_use" },
		"limits":          func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.Limits = &AssistantModelLimits{} },
		"usage":           func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.Usage.OutputTokens = -1 },
		"usage cost":      func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.Usage.Cost = math.NaN() },
		"negative render": func(_ *ModelStepRequest, r *ModelStepResult) { r.Render.EstimatedInputTokens = -1 },
		"text size": func(_ *ModelStepRequest, r *ModelStepResult) {
			r.Assistant.Text = strings.Repeat("a", maxModelStepResultTextBytes+1)
		},
		"empty call id": func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.ToolCalls[0].ID = "" },
		"duplicate call id": func(_ *ModelStepRequest, r *ModelStepResult) {
			r.Assistant.ToolCalls = append(r.Assistant.ToolCalls, r.Assistant.ToolCalls[0])
		},
		"empty call name": func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.ToolCalls[0].Name = "" },
		"array arguments": func(_ *ModelStepRequest, r *ModelStepResult) {
			r.Assistant.ToolCalls[0].Arguments = json.RawMessage(`[1]`)
		},
		"missing arguments": func(_ *ModelStepRequest, r *ModelStepResult) { r.Assistant.ToolCalls[0].Arguments = nil },
		"tool_calls without calls": func(_ *ModelStepRequest, r *ModelStepResult) {
			r.Assistant.ToolCalls, r.Narration = nil, ""
		},
		"narration without calls": func(_ *ModelStepRequest, r *ModelStepResult) {
			r.Assistant.ToolCalls, r.Assistant.StopReason = nil, ModelStopEnd
		},
		"native json": func(_ *ModelStepRequest, r *ModelStepResult) {
			r.Assistant.Native = &AssistantNative{Anthropic: []json.RawMessage{json.RawMessage(`{`)}}
		},
		"summary with calls": func(q *ModelStepRequest, _ *ModelStepResult) { q.Purpose = ModelStepPurposeSummary },
		"summary without text": func(q *ModelStepRequest, r *ModelStepResult) {
			q.Purpose = ModelStepPurposeSummary
			r.Assistant.ToolCalls, r.Narration, r.Assistant.StopReason, r.Assistant.Text = nil, "", ModelStopEnd, " "
		},
		"explanation with calls":     func(q *ModelStepRequest, _ *ModelStepResult) { q.Purpose = ModelStepPurposeExplanation },
		"acknowledgement with calls": func(q *ModelStepRequest, _ *ModelStepResult) { q.Purpose = ModelStepPurposeAcknowledgement },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req, res := validModelStepResultFixture()
			mutate(&req, &res)
			err := ValidateModelStepResult(req, res)
			if !errors.Is(err, ErrTurnHostConfiguration) {
				t.Fatalf("got %v, want ErrTurnHostConfiguration", err)
			}
		})
	}
}

func TestValidateModelStepResultAcceptsSummaryText(t *testing.T) {
	req, res := validModelStepResultFixture()
	req.Purpose = ModelStepPurposeSummary
	res.Assistant.ToolCalls, res.Narration, res.Assistant.StopReason = nil, "", ModelStopEnd
	res.Assistant.Text = "Summary of the work so far."
	if err := ValidateModelStepResult(req, res); err != nil {
		t.Fatalf("summary rejected: %v", err)
	}
}

func modelStepOutputFixture() agentrun.StepOutput {
	_, res := validModelStepResultFixture()
	assistant := res.Assistant
	assistant.Limits = &AssistantModelLimits{ContextTokens: 200000, PromptTokens: 10}
	return agentrun.StepOutput{ModelProvider: "anthropic", Model: "claude-x", Usage: TurnUsage{InputTokens: 10}, Assistant: &assistant}
}

func TestValidateStepOutputModel(t *testing.T) {
	out := modelStepOutputFixture()
	if err := validateStepOutput(StepProducerModel, out); err != nil {
		t.Fatalf("valid model output rejected: %v", err)
	}
	out.Failure = &agentrun.LoopFailure{Kind: "execution", Code: "refusal", Message: "The model refused."}
	if err := validateStepOutput(StepProducerModel, out); err != nil {
		t.Fatalf("model failure rejected: %v", err)
	}
	bad := map[string]func(*agentrun.StepOutput){
		"no assistant": func(o *agentrun.StepOutput) { o.Assistant = nil },
		"tool calls":   func(o *agentrun.StepOutput) { o.ToolCalls = []agentrun.LoopToolCall{{ID: "a", Name: "x"}} },
		"final":        func(o *agentrun.StepOutput) { o.Final = &agentrun.LoopFinal{} },
		"park": func(o *agentrun.StepOutput) {
			o.Park = &agentrun.LoopPark{Status: AgentRunStatusSleeping, Wake: &WakeCondition{Type: "timer"}}
		},
		"kernel state":    func(o *agentrun.StepOutput) { o.KernelState = map[string]interface{}{} },
		"assistant shape": func(o *agentrun.StepOutput) { o.Assistant.StopReason = "" },
		"usage":           func(o *agentrun.StepOutput) { o.Usage.OutputTokens = -1 },
		"failure no code": func(o *agentrun.StepOutput) { o.Failure = &agentrun.LoopFailure{Kind: "execution", Message: "m"} },
		"duplicate call": func(o *agentrun.StepOutput) {
			o.Assistant.ToolCalls = append(o.Assistant.ToolCalls, o.Assistant.ToolCalls[0])
		},
		"unknown producer": nil,
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			o := modelStepOutputFixture()
			kind := StepProducerModel
			if mutate == nil {
				kind = "other"
			} else {
				mutate(&o)
			}
			if err := validateStepOutput(kind, o); !errors.Is(err, agentrun.ErrInvalidStepOutput) {
				t.Fatalf("got %v, want ErrInvalidStepOutput", err)
			}
		})
	}
}

func TestValidateStepOutputDeterministic(t *testing.T) {
	wake := &WakeCondition{Type: "event"}
	good := map[string]agentrun.StepOutput{
		"tool calls": {
			ToolCalls: []agentrun.LoopToolCall{
				{ID: "op-1", Name: "skill_action", Arguments: json.RawMessage(`{"a":1}`), Directive: &agentrun.ToolCallDirective{BindingID: "b", BindingRevision: 2, IdempotencyKey: "k"}},
				{ID: "op-2", Name: "skill_action"},
			},
			KernelState: map[string]interface{}{"cursor": 1},
		},
		"final":        {Final: &agentrun.LoopFinal{Answer: "done", Status: AgentRunStatusCompleted}},
		"failed final": {Final: &agentrun.LoopFinal{Status: AgentRunStatusFailed}},
		"park":         {Park: &agentrun.LoopPark{Status: AgentRunStatusWaitingForEvent, Wake: wake}},
		"failure":      {Failure: &agentrun.LoopFailure{Kind: "execution", Code: "routine_script", Message: "The script failed."}},
	}
	for name, out := range good {
		if err := validateStepOutput(StepProducerDeterministic, out); err != nil {
			t.Fatalf("%s rejected: %v", name, err)
		}
	}
	bad := map[string]agentrun.StepOutput{
		"nothing":        {},
		"two results":    {Final: &agentrun.LoopFinal{}, Failure: &agentrun.LoopFailure{Kind: "k", Code: "c", Message: "m"}},
		"assistant":      {Assistant: &AssistantEntry{}, Final: &agentrun.LoopFinal{}},
		"empty call id":  {ToolCalls: []agentrun.LoopToolCall{{Name: "x"}}},
		"duplicate call": {ToolCalls: []agentrun.LoopToolCall{{ID: "a", Name: "x"}, {ID: "a", Name: "y"}}},
		"empty name":     {ToolCalls: []agentrun.LoopToolCall{{ID: "a"}}},
		"bad arguments":  {ToolCalls: []agentrun.LoopToolCall{{ID: "a", Name: "x", Arguments: json.RawMessage(`"s"`)}}},
		"bad directive":  {ToolCalls: []agentrun.LoopToolCall{{ID: "a", Name: "x", Directive: &agentrun.ToolCallDirective{BindingRevision: -1}}}},
		"committed park": {Park: &agentrun.LoopPark{Status: AgentRunStatusWaitingForEvent, Wake: wake, Committed: true}},
		"park no wake":   {Park: &agentrun.LoopPark{Status: AgentRunStatusWaitingForEvent}},
		"park status":    {Park: &agentrun.LoopPark{Status: AgentRunStatusPaused, Wake: wake}},
		"final status":   {Final: &agentrun.LoopFinal{Status: AgentRunStatusCanceled}},
		"usage":          {Final: &agentrun.LoopFinal{}, Usage: TurnUsage{Cost: -1}},
	}
	for name, out := range bad {
		t.Run(name, func(t *testing.T) {
			if err := validateStepOutput(StepProducerDeterministic, out); !errors.Is(err, agentrun.ErrInvalidStepOutput) {
				t.Fatalf("got %v, want ErrInvalidStepOutput", err)
			}
		})
	}
}
