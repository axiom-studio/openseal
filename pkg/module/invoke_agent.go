package module

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	a2a "github.com/axiom-studio/cortex/pkg/a2a/types"
	"github.com/google/uuid"
)

// InvokeAgentConfig holds the configuration for invoking another agent
type InvokeAgentConfig struct {
	// TargetAgentInstanceID is the ID of the agent instance to invoke
	TargetAgentInstanceID int `json:"targetAgentInstanceId"`
	// AgentEndpoint is the A2A endpoint URL (optional, uses default if not specified)
	AgentEndpoint string `json:"agentEndpoint,omitempty"`
	// MessageTemplate is a template for the message to send
	MessageTemplate string `json:"messageTemplate,omitempty"`
	// ParametersTemplate holds additional parameters as a template
	ParametersTemplate map[string]interface{} `json:"parametersTemplate,omitempty"`
	// TimeoutSeconds is the timeout for the agent invocation
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Synchronous specifies whether to wait for completion
	Synchronous bool `json:"synchronous,omitempty"`
}

// invokeAgentExecute executes an invoke_agent node to call another A2A-compatible agent
func invokeAgentExecute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config, err := parseInvokeConfig(step.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Resolve templates
	resolvedConfig := resolveInvokeTemplates(config, resolver)

	// Build the message
	messageText := buildInvokeMessage(resolvedConfig, resolver)

	// Build parameters
	parameters := buildInvokeParameters(resolvedConfig, resolver)

	// Set timeout
	timeout := time.Duration(resolvedConfig.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Invoke the agent via A2A
	response, err := invokeAgentA2A(execCtx, resolvedConfig, messageText, parameters)
	if err != nil {
		return nil, fmt.Errorf("failed to invoke agent: %w", err)
	}

	return &StepResult{Output: response}, nil
}

// parseInvokeConfig parses the invoke agent configuration
func parseInvokeConfig(config map[string]interface{}) (*InvokeAgentConfig, error) {
	result := &InvokeAgentConfig{
		Synchronous:    true,
		TimeoutSeconds: 300,
	}

	if config == nil {
		return result, nil
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return result, err
	}

	if err := json.Unmarshal(configJSON, result); err != nil {
		return result, err
	}

	return result, nil
}

// resolveInvokeTemplates resolves template variables in the configuration
func resolveInvokeTemplates(config *InvokeAgentConfig, resolver TemplateResolver) *InvokeAgentConfig {
	resolved := &InvokeAgentConfig{
		TargetAgentInstanceID: config.TargetAgentInstanceID,
		AgentEndpoint:         config.AgentEndpoint,
		TimeoutSeconds:        config.TimeoutSeconds,
		Synchronous:           config.Synchronous,
		MessageTemplate:       config.MessageTemplate,
	}

	// Resolve message template
	if config.MessageTemplate != "" {
		resolved.MessageTemplate = resolver.ResolveString(config.MessageTemplate)
	}

	// Resolve parameters templates
	if config.ParametersTemplate != nil {
		resolved.ParametersTemplate = resolver.ResolveMap(config.ParametersTemplate)
	}

	return resolved
}

// buildInvokeMessage builds the message to send to the target agent
func buildInvokeMessage(config *InvokeAgentConfig, resolver TemplateResolver) string {
	if config.MessageTemplate != "" {
		return config.MessageTemplate
	}

	// Get previous output as message
	prevOutput := resolver.GetStepOutput("previous")
	if prevOutput != nil {
		switch v := prevOutput.(type) {
		case string:
			return v
		default:
			data, _ := json.Marshal(v)
			return string(data)
		}
	}

	return "Process this request"
}

// buildInvokeParameters builds additional parameters for the agent invocation
func buildInvokeParameters(config *InvokeAgentConfig, resolver TemplateResolver) map[string]interface{} {
	parameters := make(map[string]interface{})

	if config.ParametersTemplate != nil {
		for k, v := range config.ParametersTemplate {
			parameters[k] = v
		}
	}

	return parameters
}

// invokeAgentA2A invokes the target agent using the A2A protocol
func invokeAgentA2A(ctx context.Context, config *InvokeAgentConfig, messageText string, parameters map[string]interface{}) (map[string]interface{}, error) {
	// Determine the endpoint
	endpoint := config.AgentEndpoint
	if endpoint == "" {
		// Use default Cortex A2A endpoint
		endpoint = "/api/v1/agent/a2a/rpc"
	}

	httpClient := &http.Client{
		Timeout: time.Duration(config.TimeoutSeconds) * time.Second,
	}
	if httpClient.Timeout == 0 {
		httpClient.Timeout = 5 * time.Minute
	}

	// Build A2A message
	message := a2a.AgentMessage{
		ID:    uuid.New().String(),
		Role:  "user",
		Parts: []a2a.Part{{Type: "text", Text: messageText}},
	}

	metadata := a2a.Metadata{
		"targetAgentInstanceId": config.TargetAgentInstanceID,
	}

	// Merge additional parameters into metadata
	for k, v := range parameters {
		metadata[k] = v
	}

	// Build JSON-RPC wrapper
	rpcReq := a2a.JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  a2a.MethodSendMessage,
		ID:      uuid.New().String(),
		Params: map[string]interface{}{
			"message":     message,
			"synchronous": config.Synchronous,
			"metadata":    metadata,
		},
	}

	rpcBody, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON-RPC request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rpcBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Execute request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent returned status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse JSON-RPC response
	var rpcResp a2a.JSONRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("failed to parse JSON-RPC response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("A2A error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	// Extract result
	return parseA2AResponse(&rpcResp)
}

// parseA2AResponse parses the A2A response and extracts the result
func parseA2AResponse(rpcResp *a2a.JSONRPCResponse) (map[string]interface{}, error) {
	resultMap := make(map[string]interface{})

	resultBytes, err := json.Marshal(rpcResp.Result)
	if err != nil {
		return nil, err
	}

	// Try to parse as SendMessageResponse
	var sendResp a2a.SendMessageResponse
	if err := json.Unmarshal(resultBytes, &sendResp); err == nil {
		if sendResp.Task != nil {
			resultMap["task_id"] = sendResp.Task.ID
			resultMap["status"] = sendResp.Task.Status.State
			resultMap["progress"] = sendResp.Task.Status.Progress

			// Extract messages
			if len(sendResp.Task.Messages) > 0 {
				var messages []string
				for _, msg := range sendResp.Task.Messages {
					for _, part := range msg.Parts {
						if part.Type == "text" && part.Text != "" {
							messages = append(messages, part.Text)
						}
					}
				}
				if len(messages) > 0 {
					resultMap["response"] = messages[len(messages)-1]
					resultMap["all_messages"] = messages
				}
			}

			// Include error if present
			if sendResp.Task.Error != nil {
				resultMap["error"] = sendResp.Task.Error.Message
			}
		}
		if sendResp.Message != nil {
			for _, part := range sendResp.Message.Parts {
				if part.Type == "text" && part.Text != "" {
					resultMap["response"] = part.Text
				}
			}
		}
		return resultMap, nil
	}

	// Generic fallback
	var genericResult map[string]interface{}
	if err := json.Unmarshal(resultBytes, &genericResult); err == nil {
		return genericResult, nil
	}

	resultMap["raw_result"] = string(resultBytes)
	return resultMap, nil
}
