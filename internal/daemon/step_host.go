package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// ProviderStepHost runs single model steps against one OpenAI-compatible
// provider, in process. It has no host-native tools: the desktop has no
// workspace, VM or chat files. Governed actions run in the kernel (D1).
//
// The desktop does not stream: it emits no narration or answer-preview
// frames (DI-4).
type ProviderStepHost struct{ client *openAIClient }

var _ agentrun.StepHost = (*ProviderStepHost)(nil)

func NewProviderStepHost(cfg OpenAIProviderConfig) (*ProviderStepHost, error) {
	client, err := newOpenAIClient(cfg)
	if err != nil {
		return nil, err
	}
	return &ProviderStepHost{client: client}, nil
}

// Step host failure codes (all *modelstep.TurnHostFailure).
const (
	stepHostInvalidRequest     = "invalid_model_step_request"
	stepHostUnsupportedBinding = "unsupported_model_binding"
	stepHostUnavailable        = "provider_unavailable"
	stepHostHTTP               = "provider_http"
	stepHostInvalidResponse    = "invalid_response"
	stepHostOutputTruncated    = "provider_output_truncated"
	stepHostEmptyResponse      = "provider_empty_response"
)

const (
	// stepHostMalformedRetries is how many more times one step re-sends the
	// identical request when tool arguments are not a JSON object.
	stepHostMalformedRetries = 2
	// stepHostEmptyRetries is how many more times one step re-sends the
	// request when the provider returned neither text nor tool calls.
	stepHostEmptyRetries = 1
	// stepNarrationBytes matches the kernel's step narration bound.
	stepNarrationBytes = 2000
)

type openAIChatRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	Tools               []openAITool    `json:"tools,omitempty"`
	ToolChoice          interface{}     `json:"tool_choice,omitempty"`
	MaxCompletionTokens int64           `json:"max_completion_tokens,omitempty"`
	Stream              bool            `json:"stream"`
}

type openAIChatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content          json.RawMessage  `json:"content"` // string, null, or [{type:"text",text}]
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// parsedStep is one provider answer turned into the entry's parts.
type parsedStep struct {
	text       string
	reasoning  string
	calls      []models.ModelToolCall
	malformed  []string // per call: decoder error, "" when the arguments are an object
	stop       models.ModelStopReason
	truncated  bool // finish_reason "length"
	usage      models.ModelStepUsage
	providerMS int64
}

func (p parsedStep) hasMalformed() bool {
	for _, reason := range p.malformed {
		if reason != "" {
			return true
		}
	}
	return false
}

func stepFailure(code, message string, retryable bool) error {
	return modelstep.NewTurnHostFailure(code, message, retryable)
}

func (h *ProviderStepHost) ExecuteModelStep(ctx context.Context, req models.ModelStepRequest) (models.ModelStepResult, error) {
	if err := req.Validate(); err != nil {
		return models.ModelStepResult{}, stepFailure(stepHostInvalidRequest, "The kernel sent an invalid model step: "+err.Error(), false)
	}
	if req.ModelCredential != nil {
		return models.ModelStepResult{}, stepFailure(stepHostUnsupportedBinding, "This agent requires a deployment-specific model credential. The desktop task provider currently uses the workspace provider.", false)
	}
	render, err := renderOpenAIStep(req, h.client.model)
	if err != nil {
		return models.ModelStepResult{}, err
	}
	chat := openAIChatRequest{Model: h.client.model, Messages: render.Messages, MaxCompletionTokens: req.MaxOutputTokens}
	if len(render.Tools) > 0 {
		chat.Tools, chat.ToolChoice = render.Tools, render.ToolChoice
	}
	body, err := json.Marshal(chat)
	if err != nil {
		return models.ModelStepResult{}, stepFailure(stepHostInvalidRequest, "The model step could not be prepared.", false)
	}
	estimate := int64(len(body)+3) / 4
	if len(body) > maxProviderRequestBytes {
		return models.ModelStepResult{}, h.contextExceeded(estimate, maxProviderRequestBytes/4)
	}
	// A summary step offers tools only to keep the prompt prefix (tool_choice
	// none); tool-free purposes offer none. Calls returned anyway are dropped.
	acceptCalls := len(chat.Tools) > 0 && req.Purpose != models.ModelStepPurposeSummary

	started := time.Now()
	var total models.ModelStepUsage
	var step parsedStep
	malformedLeft, emptyLeft := stepHostMalformedRetries, stepHostEmptyRetries
	for {
		step, err = h.attempt(ctx, req, body, estimate, acceptCalls)
		if err != nil {
			return models.ModelStepResult{}, err
		}
		total = addStepUsage(total, step.usage)
		if step.hasMalformed() {
			if step.truncated {
				// Cut off mid-arguments: a re-send produces the same cut.
				return models.ModelStepResult{}, stepFailure(stepHostOutputTruncated, "The model's answer was cut off by its output limit before its tool call was complete.", false)
			}
			if malformedLeft > 0 {
				malformedLeft--
				continue
			}
			break
		}
		if step.text == "" && len(step.calls) == 0 {
			if emptyLeft > 0 {
				emptyLeft--
				continue
			}
			return models.ModelStepResult{}, stepFailure(stepHostEmptyResponse, "The task provider returned an empty answer.", false)
		}
		break
	}
	for i, reason := range step.malformed {
		if reason != "" {
			step.calls[i].Arguments = json.RawMessage(`{}`)
			step.calls[i].ArgumentsError = step.calls[i].Name + ": arguments are not a JSON object (" + reason + ")"
		}
	}
	total.ProviderDurationMS = step.providerMS
	total.DurationMS = time.Since(started).Milliseconds()

	tools, _ := json.Marshal(render.Tools)
	assistant := models.AssistantEntry{
		Provider:     models.ModelStepProviderChat,
		Model:        h.client.model,
		SystemDigest: digest([]byte(*render.Messages[0].Content)),
		ToolsDigest:  digest(tools),
		Text:         step.text,
		ToolCalls:    step.calls,
		StopReason:   step.stop,
		Usage:        total,
	}
	if step.reasoning != "" {
		assistant.Native = &models.AssistantNative{ChatReasoningField: defaultChatReasoningField, ChatReasoning: step.reasoning}
	}
	result := models.ModelStepResult{
		APIVersion: models.ModelStepAPIVersion,
		StepID:     req.StepID,
		Assistant:  assistant,
		Model: models.ModelStepModelInfo{
			Provider: models.ModelStepProviderChat, Model: h.client.model,
			ContextTokens: h.client.window, MaxOutputTokens: req.MaxOutputTokens,
		},
		Render: models.ModelStepRenderInfo{
			EstimatedInputTokens:    estimate,
			ReasoningCutoffSequence: render.Info.ReasoningCutoffSequence,
			ImagesOmitted:           render.Info.ImagesOmitted,
		},
	}
	if len(step.calls) > 0 {
		result.Narration = stepNarration(step.text)
	}
	if err := modelstep.ValidateModelStepResult(req, result); err != nil {
		return models.ModelStepResult{}, stepFailure(stepHostInvalidResponse, "The task provider returned an answer the kernel cannot record.", false)
	}
	return result, nil
}

// attempt makes one provider call and parses it.
func (h *ProviderStepHost) attempt(ctx context.Context, req models.ModelStepRequest, body []byte, estimate int64, acceptCalls bool) (parsedStep, error) {
	started := time.Now()
	raw, err := h.client.post(ctx, body)
	if err != nil {
		return parsedStep{}, h.classify(ctx, err, estimate)
	}
	elapsed := time.Since(started).Milliseconds()
	var response openAIChatResponse
	if json.Unmarshal(raw, &response) != nil || len(response.Choices) != 1 {
		return parsedStep{}, stepFailure(stepHostInvalidResponse, "The task provider did not return one answer.", false)
	}
	if response.Usage.PromptTokens < 0 || response.Usage.CompletionTokens < 0 {
		return parsedStep{}, stepFailure(stepHostInvalidResponse, "The task provider returned invalid usage data.", false)
	}
	choice := response.Choices[0]
	text, ok := chatContentText(choice.Message.Content)
	if !ok {
		return parsedStep{}, stepFailure(stepHostInvalidResponse, "The task provider returned content in an unsupported shape.", false)
	}
	step := parsedStep{text: text, reasoning: strings.TrimSpace(choice.Message.ReasoningContent), truncated: choice.FinishReason == "length", providerMS: elapsed}
	if acceptCalls {
		seen := make(map[string]bool, len(choice.Message.ToolCalls))
		for i, call := range choice.Message.ToolCalls {
			if (call.Type != "" && call.Type != "function") || strings.TrimSpace(call.Function.Name) == "" {
				return parsedStep{}, stepFailure(stepHostInvalidResponse, "The task provider returned a tool call without a function name.", false)
			}
			id := strings.TrimSpace(call.ID)
			if id == "" || seen[id] {
				// Deterministic, so a redo of the same step gets the same IDs.
				id = fmt.Sprintf("call_%s_%d", req.StepID, i)
			}
			seen[id] = true
			arguments, reason := toolArguments(call.Function.Arguments)
			step.calls = append(step.calls, models.ModelToolCall{ID: id, Name: strings.TrimSpace(call.Function.Name), Arguments: arguments})
			step.malformed = append(step.malformed, reason)
		}
	}
	switch {
	case len(step.calls) > 0:
		// Some servers finish with "stop" even when they called tools.
		step.stop = models.ModelStopToolCalls
	case choice.FinishReason == "length":
		step.stop = models.ModelStopMaxTokens
	case choice.FinishReason == "content_filter":
		step.stop = models.ModelStopRefusal
	default:
		step.stop = models.ModelStopEnd
	}
	step.usage = models.ModelStepUsage{InputTokens: response.Usage.PromptTokens, OutputTokens: response.Usage.CompletionTokens, Attempts: 1}
	// Compatible providers may omit usage. Charge a conservative estimate
	// rather than treating a call as free.
	if step.usage.InputTokens == 0 {
		step.usage.InputTokens = estimate
	}
	if step.usage.OutputTokens == 0 {
		output := len(step.text)
		for _, call := range choice.Message.ToolCalls {
			output += len(call.Function.Arguments)
		}
		step.usage.OutputTokens = int64(output)
	}
	return step, nil
}

// classify maps a post error to the step's failure. A canceled context is
// returned as it is.
func (h *ProviderStepHost) classify(ctx context.Context, err error, estimate int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var status *providerHTTPError
	switch {
	case errors.As(err, &status):
		if (status.Status == 400 || status.Status == 413) && contextLengthRejection(status.Body) {
			return h.contextExceeded(estimate, h.client.window)
		}
		return stepFailure(stepHostHTTP, fmt.Sprintf("The task provider returned HTTP %d. Check provider settings and limits.", status.Status), status.Status == 429 || status.Status >= 500)
	case errors.Is(err, errProviderResponseTooLarge):
		return stepFailure(stepHostInvalidResponse, "The task provider response was unreadable or exceeded the size limit.", false)
	case errors.Is(err, errProviderRequestTooLarge):
		return h.contextExceeded(estimate, maxProviderRequestBytes/4)
	}
	return stepFailure(stepHostUnavailable, "The task provider could not be reached. Check its connection and configuration.", true)
}

// contextExceeded is the model_context_exceeded failure. ContextPressure
// tells the kernel the limit, so it compacts and retries once (D10).
func (h *ProviderStepHost) contextExceeded(estimate, limit int64) error {
	if limit < 1 {
		limit = 1
	}
	return &modelstep.TurnHostFailure{
		Code:            modelstep.TurnHostContextExceeded,
		Message:         "This conversation is too long for " + h.client.model + " to read at once.",
		ContextPressure: &modelstep.HostedContextPressure{EstimatedTokens: estimate, LimitTokens: limit},
	}
}

func contextLengthRejection(body []byte) bool {
	var envelope struct {
		Error struct {
			Code interface{} `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code == "context_length_exceeded" {
		return true
	}
	lower := strings.ToLower(string(body))
	for _, phrase := range []string{"context length", "context window", "maximum context", "prompt is too long", "too many tokens"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// chatContentText reads message.content: a string, null, or an array of
// parts whose text parts are concatenated. The result is trimmed.
func chatContentText(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(b.String()), true
}

// toolArguments compacts a call's arguments. "" means {}. When the text is
// not a JSON object it returns {} and the decoder error.
func toolArguments(text string) (json.RawMessage, string) {
	if strings.TrimSpace(text) == "" {
		return json.RawMessage(`{}`), ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		return json.RawMessage(`{}`), err.Error()
	}
	if object == nil {
		return json.RawMessage(`{}`), "arguments are null"
	}
	compact, err := json.Marshal(object)
	if err != nil {
		return json.RawMessage(`{}`), err.Error()
	}
	return compact, ""
}

func addStepUsage(total, u models.ModelStepUsage) models.ModelStepUsage {
	total.InputTokens += u.InputTokens
	total.OutputTokens += u.OutputTokens
	total.Attempts += u.Attempts
	return total
}

// stepNarration is the text of a tool-call step with whitespace runs
// collapsed, cut to stepNarrationBytes on a rune boundary.
func stepNarration(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= stepNarrationBytes {
		return text
	}
	cut := stepNarrationBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut])
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ExecuteHostTool always refuses: the desktop runtime has no host tools, so
// the kernel never offers any.
func (h *ProviderStepHost) ExecuteHostTool(context.Context, models.HostToolRequest) (models.HostToolResult, error) {
	return models.HostToolResult{}, models.ErrHostToolUnsupported
}
