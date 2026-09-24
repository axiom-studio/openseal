package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// ProviderTurnHost performs one bounded, proposal-only model call. Durable
// actions, delegation, approval, and completion remain kernel-owned.
type ProviderTurnHost struct {
	endpoint, key, model string
	client               *http.Client
}

func NewProviderTurnHost(endpoint, key, model string) (*ProviderTurnHost, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(key) == "" || strings.TrimSpace(model) == "" {
		return nil, errors.New("task provider requires a valid HTTP API URL, credential, and model")
	}
	endpoint = strings.TrimRight(u.String(), "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	return &ProviderTurnHost{endpoint: endpoint, key: key, model: strings.TrimSpace(model), client: &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (h *ProviderTurnHost) ExecuteHostedTurn(ctx context.Context, request runtime.HostedTurnRequest) (*runtime.HostedTurnResponse, error) {
	fail := func(code, message string, retry bool) (*runtime.HostedTurnResponse, error) {
		return nil, runtime.NewTurnHostFailure(code, message, retry)
	}
	// This host has no native filesystem executor or media adapter. Do not offer
	// operations it cannot perform, or silently discard actual media input.
	if len(request.ModelMedia) > 0 {
		return fail("unsupported_media", "The desktop task provider does not support media input yet.", false)
	}
	if request.ModelCredential != nil {
		return fail("unsupported_model_binding", "This agent requires a deployment-specific model credential. The desktop task provider currently uses the workspace provider.", false)
	}
	request.WorkspaceOperations = nil
	request.Workspace = nil
	request.WorkspaceCredentials = nil
	input, err := runtime.MarshalHostedTurnModelInput(request)
	if err != nil {
		return fail("invalid_input", "The task context could not be prepared.", false)
	}
	refs := make([]string, 0, len(request.SkillPrompts))
	for _, p := range request.SkillPrompts {
		refs = append(refs, p.Reference)
	}
	schema, err := runtime.HostedTurnFormJSONSchema(request.Actions, runtime.HostedTurnFormAuthority{CanDelegate: len(request.EligibleAgents) > 0, CanInvokeRunbook: len(request.RunbookOperations) > 0, SkillPromptReferences: refs})
	if err != nil {
		return fail("invalid_schema", "The task response contract could not be prepared.", false)
	}
	schema["properties"].(map[string]interface{})["runOutput"] = generatedOutputSchema()
	limit := 4096
	if request.Budget != nil && request.Budget.TurnReservation.OutputTokens > 0 && request.Budget.TurnReservation.OutputTokens < int64(limit) {
		limit = int(request.Budget.TurnReservation.OutputTokens)
	}
	payload := map[string]interface{}{"model": h.model, "max_completion_tokens": limit, "messages": []map[string]string{{"role": "system", "content": "Perform one governed agent turn. Follow the supplied system instructions and goal. Return submit_agent_turn using the provided schema. Propose only offered capabilities; never invent tools, access, action receipts, or successful external effects. Do not claim filesystem or media access. Disposition every offered skill. For a completed text task put the user-facing answer in runOutput.reply. When the user requests a file, provide its complete text in runOutput.generatedFiles using name, mediaType and text. Supported formats are plain text, Markdown, CSV and JSON; use at most eight files with 256 KiB total UTF-8 text, within your output token budget. Generated files are output drafts stored by the kernel after accepting this turn, not writes to the user filesystem. Never supply artifactRefs or claim publication before it occurs. Generated files are allowed only with nextRunStatus completed. Preserve continuation context when more work remains. The kernel owns execution, approval, and state transitions."}, {"role": "user", "content": string(input)}}, "tools": []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "submit_agent_turn", "description": "Propose the next governed agent turn", "parameters": schema}}}, "tool_choice": map[string]interface{}{"type": "function", "function": map[string]string{"name": "submit_agent_turn"}}}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > 4<<20 {
		return fail("input_too_large", "The task context exceeds the desktop provider limit.", false)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		return fail("invalid_provider", "The task provider request could not be prepared.", false)
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	res, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return fail("provider_unavailable", "The task provider could not be reached. Check its connection and configuration.", true)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fail("provider_http", fmt.Sprintf("The task provider returned HTTP %d. Check provider settings and limits.", res.StatusCode), res.StatusCode == 429 || res.StatusCode >= 500)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return fail("invalid_response", "The task provider response was unreadable or exceeded the size limit.", false)
	}
	var envelope struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Input  int `json:"prompt_tokens"`
			Output int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Choices) != 1 {
		return fail("invalid_response", "The task provider did not return one structured turn.", false)
	}
	choice := envelope.Choices[0]
	if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) != 1 || choice.Message.ToolCalls[0].Type != "function" || choice.Message.ToolCalls[0].Function.Name != "submit_agent_turn" {
		return fail("incomplete_response", "The task provider returned an incomplete or unsupported turn.", false)
	}
	var form runtime.HostedTurnForm
	decoder := json.NewDecoder(strings.NewReader(choice.Message.ToolCalls[0].Function.Arguments))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&form) != nil || decoder.Decode(new(interface{})) != io.EOF {
		return fail("invalid_turn", "The task provider returned an invalid turn form.", false)
	}
	if _, forged := form.RunOutput["artifactRefs"]; forged {
		return fail("invalid_files", "Artifact references are assigned by the kernel, not the model.", false)
	}
	if _, err := generatedFiles(form.RunOutput, form.NextRunStatus); err != nil {
		return fail("invalid_files", "The task provider returned invalid generated files: "+err.Error(), false)
	}
	result, err := runtime.CompileHostedTurnForm(form, request.Actions)
	if err != nil {
		return fail("invalid_turn", "The task provider proposed an invalid or unauthorized operation.", false)
	}
	if runtime.ValidateHostedSkillSelections(request.SkillPrompts, result.SkillSelections) != nil || runtime.ValidateHostedTurnCompletion(request, result) != nil {
		return fail("invalid_evidence", "The task provider returned unsupported skill choices or completion claims.", false)
	}
	result.APIVersion = runtime.HostedTurnAPIVersion
	result.InvocationID = request.InvocationID
	result.ModelProvider = "openai-compatible"
	result.Model = h.model
	result.Usage = runtime.TurnUsage{InputTokens: envelope.Usage.Input, OutputTokens: envelope.Usage.Output, ProviderDurationMS: time.Since(start).Milliseconds()}
	if result.Usage.Validate() != nil {
		return fail("invalid_usage", "The task provider returned invalid usage data.", false)
	}
	// Compatible providers may omit usage. Charge a conservative estimate
	// rather than treating a successful invocation as zero-cost execution.
	if result.Usage.InputTokens == 0 {
		estimate, _ := runtime.EstimateHostedTurnInputTokens(request)
		result.Usage.InputTokens = int(estimate)
	}
	if result.Usage.OutputTokens == 0 {
		result.Usage.OutputTokens = len(choice.Message.ToolCalls[0].Function.Arguments)
	}
	return result, nil
}
