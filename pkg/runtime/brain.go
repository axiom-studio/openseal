package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/persona"
	"go.uber.org/zap"
)

// buildFileContextSection creates a system prompt section describing available files
// and includes content for text-based files
func (b *AgentBrain) buildFileContextSection(attachments []*FileAttachment) string {
	if len(attachments) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n## Available Files\n\n")
	sb.WriteString("You have access to these files:\n")

	for _, file := range attachments {
		sizeStr := formatFileSize(file.Size)
		sb.WriteString(fmt.Sprintf("- [%s] %s (%s, %s)\n", file.Id, file.Name, file.MimeType, sizeStr))

		// Try to read content for text-based files
		if b.fileStore != nil && isTextMimeType(file.MimeType) && file.Size < 100*1024 { // Max 100KB for inline content
			content, err := b.readFileContent(file.Id)
			if err != nil {
				b.logger.Debugw("failed to read file content", "fileId", file.Id, "error", err)
			} else if len(content) > 0 {
				sb.WriteString(fmt.Sprintf("\n**Content of %s:**\n```\n%s\n```\n", file.Name, truncateContent(content, 8000)))
			}
		}
	}

	sb.WriteString("\nWhen using workflow tools, you can pass file IDs to inputs that accept files.\n")

	return sb.String()
}

// isTextMimeType checks if a mime type represents text content
func isTextMimeType(mimeType string) bool {
	textTypes := []string{
		"text/",
		"application/json",
		"application/xml",
		"application/javascript",
		"application/x-yaml",
		"application/csv",
		"text/csv",
	}
	for _, t := range textTypes {
		if strings.HasPrefix(mimeType, t) || mimeType == t {
			return true
		}
	}
	// Also accept octet-stream for known text file extensions (handled by caller)
	return false
}

// isTextFileExtension checks if filename suggests a text file
func isTextFileExtension(filename string) bool {
	textExtensions := []string{
		".txt", ".csv", ".json", ".xml", ".yaml", ".yml", ".md", ".log",
		".py", ".js", ".ts", ".go", ".java", ".c", ".cpp", ".h",
		".sh", ".bash", ".zsh", ".env", ".ini", ".conf", ".cfg",
	}
	for _, e := range textExtensions {
		if strings.HasSuffix(strings.ToLower(filename), e) {
			return true
		}
	}
	return false
}

// isImageMimeType checks if a mime type represents an image
func isImageMimeType(mimeType string) bool {
	imageTypes := []string{
		"image/png",
		"image/jpeg",
		"image/jpg",
		"image/gif",
		"image/webp",
	}
	for _, t := range imageTypes {
		if mimeType == t || strings.HasPrefix(mimeType, "image/") {
			return true
		}
	}
	return false
}

// isPdfMimeType checks if a mime type represents a PDF
func isPdfMimeType(mimeType string) bool {
	return mimeType == "application/pdf" || strings.HasSuffix(mimeType, "pdf")
}

// readFileContent reads the content of a file from the file store
func (b *AgentBrain) readFileContent(fileId string) (string, error) {
	if b.fileStore == nil {
		return "", fmt.Errorf("file store not available")
	}

	reader, err := b.fileStore.GetReader(fileId)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}

	return string(content), nil
}

// readFileBytes reads the raw bytes of a file from the file store
func (b *AgentBrain) readFileBytes(fileId string) ([]byte, error) {
	if b.fileStore == nil {
		return nil, fmt.Errorf("file store not available")
	}

	reader, err := b.fileStore.GetReader(fileId)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

// truncateContent truncates content to a maximum length
func truncateContent(content string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}
	return content[:maxLen] + "\n... [truncated]"
}

// extractAttachmentsFromHistory parses [Attached: fileId.ext] patterns from history messages
// and resolves them to FileAttachment objects
func (b *AgentBrain) extractAttachmentsFromHistory(history []*ConversationMessage) []*FileAttachment {
	var attachments []*FileAttachment
	seenIds := make(map[string]bool)

	for _, msg := range history {
		// Parse [Attached: fileId.ext] patterns
		matches := extractAttachmentPattern(msg.Content)
		for _, match := range matches {
			fileId := match
			if seenIds[fileId] {
				continue // Skip duplicates
			}
			seenIds[fileId] = true

			// Try to get file metadata
			if b.fileStore != nil {
				storedFile, err := b.fileStore.Get(fileId)
				if err == nil {
					attachments = append(attachments, &FileAttachment{
						Id:       storedFile.Id,
						Name:     storedFile.Filename,
						MimeType: storedFile.MimeType,
						Size:     storedFile.Size,
					})
				}
			}
		}
	}

	return attachments
}

// extractAttachmentPattern parses [Attached: fileId.ext] and returns file IDs
func extractAttachmentPattern(content string) []string {
	// Pattern: [Attached: fileId.ext] where fileId is hex
	// Example: [Attached: 0f719164-cedf-411f-b5f0-5fa9e5c9b516.pdf]
	pattern := regexp.MustCompile(`\[Attached:\s*([a-f0-9-]+)\.[a-zA-Z0-9]+\]`)
	matches := pattern.FindAllStringSubmatch(content, -1)

	var fileIds []string
	for _, match := range matches {
		if len(match) > 1 {
			fileIds = append(fileIds, match[1])
		}
	}
	return fileIds
}

// formatFileSize formats bytes into human-readable size
func formatFileSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	} else {
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}

// VaultService interface for fetching API keys from Vault
type VaultService interface {
	GetCredentialAsVirtualNodeOutputs(ctx context.Context, credId int) (map[string]interface{}, error)
}

// AgentBrain is the LLM brain of an autonomous agent
type AgentBrain struct {
	toolAdapter  *WorkflowToolAdapter
	httpClient   *http.Client
	logger       *zap.SugaredLogger
	vaultService VaultService
	fileStore    agent.FileStore
}

// NewAgentBrain creates a new agent brain
func NewAgentBrain(toolAdapter *WorkflowToolAdapter, logger *zap.SugaredLogger) *AgentBrain {
	return &AgentBrain{
		toolAdapter: toolAdapter,
		httpClient: &http.Client{
			Timeout: 10 * time.Minute, // Long timeout for complex reasoning
		},
		logger: logger,
	}
}

// SetFileStore sets the file store for reading file content
func (b *AgentBrain) SetFileStore(fs agent.FileStore) {
	b.fileStore = fs
}

// SetVaultService sets the vault service for fetching API keys
func (b *AgentBrain) SetVaultService(vs VaultService) {
	b.vaultService = vs
}

// getAPIKey retrieves the API key from persona config or Vault
func (b *AgentBrain) getAPIKey(ctx context.Context, p *persona.Persona, keyField string) (string, error) {
	// First check if direct API key is set
	if p.LLMApiKey != "" {
		return p.LLMApiKey, nil
	}

	// Then check if we have a Vault credential ID
	if p.LLMCredentialId > 0 && b.vaultService != nil {
		credOutputs, err := b.vaultService.GetCredentialAsVirtualNodeOutputs(ctx, p.LLMCredentialId)
		if err != nil {
			return "", fmt.Errorf("failed to get credential from vault: %w", err)
		}
		// Try common API key field names
		for _, field := range []string{keyField, "apiKey", "api_key", "token", "key"} {
			if key, ok := credOutputs[field].(string); ok && key != "" {
				return key, nil
			}
		}
	}

	if b.vaultService != nil {
		type defaultCredProvider interface {
			GetDefaultLLMCredentialForChat(ctx context.Context) (interface{}, error)
		}
		if provider, ok := b.vaultService.(defaultCredProvider); ok {
			result, err := provider.GetDefaultLLMCredentialForChat(ctx)
			if err != nil {
				b.logger.Warnw("failed to get default LLM credential", "error", err)
			} else if result != nil {
				if rv := reflect.ValueOf(result); rv.Kind() == reflect.Ptr && !rv.IsNil() {
					if idField := rv.Elem().FieldByName("Id"); idField.IsValid() && idField.Kind() == reflect.Int {
						credId := int(idField.Int())
						credOutputs, err := b.vaultService.GetCredentialAsVirtualNodeOutputs(ctx, credId)
						if err != nil {
							b.logger.Warnw("failed to get default credential fields", "error", err)
						} else {
							for _, field := range []string{keyField, "apiKey", "api_key", "token", "key"} {
								if key, ok := credOutputs[field].(string); ok && key != "" {
									return key, nil
								}
							}
						}
					}
				}
			}
		}
	}

	return "", nil
}

// Think executes the agent's reasoning process
func (b *AgentBrain) Think(ctx context.Context, p *persona.Persona, req *RuntimeRequest, tools []*ToolDefinition) (*RuntimeResponse, error) {
	return b.ThinkWithCallbacks(ctx, p, req, tools, nil, nil, nil)
}

// ThinkWithCallbacks executes the agent's reasoning process with optional streaming callbacks
func (b *AgentBrain) ThinkWithCallbacks(ctx context.Context, p *persona.Persona, req *RuntimeRequest, tools []*ToolDefinition, onTextDelta func(string), onToolStart func(string, string, map[string]interface{}), onToolResult func(string, string, interface{}, int, error)) (*RuntimeResponse, error) {
	// Build messages with tool descriptions
	messages := b.buildMessages(p, req, tools)

	// Convert tools to provider format
	toolDefs := b.buildToolsForProvider(p.LLMProvider, tools)

	// Create callbacks struct for execute methods
	callbacks := &streamingCallbacks{
		onTextDelta:  onTextDelta,
		onToolStart:  onToolStart,
		onToolResult: onToolResult,
	}

	// Execute based on provider, passing attachments for file resolution
	switch p.LLMProvider {
	case "openai":
		return b.executeOpenAIWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks)
	case "anthropic":
		return b.executeAnthropicWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks)
	case "gemini":
		return b.executeGeminiWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks)
	default:
		return b.executeOpenAIWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks) // Default to OpenAI-compatible
	}
}

// ThinkWithCustomSystemPrompt executes reasoning with a custom system prompt and streaming callbacks
func (b *AgentBrain) ThinkWithCustomSystemPrompt(ctx context.Context, p *persona.Persona, req *RuntimeRequest, tools []*ToolDefinition, customSystemPrompt string, onTextDelta func(string), onToolStart func(string, string, map[string]interface{}), onToolResult func(string, string, interface{}, int, error)) (*RuntimeResponse, error) {
	messages := b.buildMessagesWithCustomSystemPrompt(p, req, tools, customSystemPrompt)

	toolDefs := b.buildToolsForProvider(p.LLMProvider, tools)

	callbacks := &streamingCallbacks{
		onTextDelta:  onTextDelta,
		onToolStart:  onToolStart,
		onToolResult: onToolResult,
	}

	switch p.LLMProvider {
	case "openai":
		return b.executeOpenAIWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks)
	case "anthropic":
		return b.executeAnthropicWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks)
	case "gemini":
		return b.executeGeminiWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks)
	default:
		return b.executeOpenAIWithCallbacks(ctx, p, messages, toolDefs, tools, req.Attachments, callbacks)
	}
}

// buildMessagesWithCustomSystemPrompt constructs messages using a custom system prompt
func (b *AgentBrain) buildMessagesWithCustomSystemPrompt(p *persona.Persona, req *RuntimeRequest, tools []*ToolDefinition, customSystemPrompt string) []map[string]interface{} {
	messages := make([]map[string]interface{}, 0)

	systemPrompt := customSystemPrompt
	if systemPrompt == "" {
		systemPrompt = p.SystemPrompt
	}
	if systemPrompt == "" {
		systemPrompt = fmt.Sprintf("You are %s, a helpful AI assistant.", p.DisplayName)
	}

	switch p.Tone {
	case persona.ToneProfessional:
		systemPrompt += "\n\nRespond in a professional, business-appropriate manner."
	case persona.ToneCasual:
		systemPrompt += "\n\nRespond in a friendly, casual manner."
	case persona.ToneTechnical:
		systemPrompt += "\n\nRespond with technical precision and detail."
	}

	if len(p.ExpertiseTags) > 0 {
		systemPrompt += fmt.Sprintf("\n\nYour areas of expertise include: %s.", strings.Join(p.ExpertiseTags, ", "))
	}

	if len(tools) > 0 {
		systemPrompt += "\n\n## Available Tools\n\nYou have access to the following tools:\n\n"
		for _, tool := range tools {
			systemPrompt += fmt.Sprintf("- **%s**: %s\n", tool.Name, tool.Description)
		}
		systemPrompt += "\nAnswer questions directly from your own knowledge when possible. Only call tools when the task genuinely requires executing a workflow, processing data, or obtaining user-specific input you don't have."
	} else {
		systemPrompt += "\n\nNo tools are currently available."
	}

	if len(req.Attachments) > 0 {
		systemPrompt += b.buildFileContextSection(req.Attachments)
	}

	if b.fileStore != nil && req.History != nil {
		historyAttachments := b.extractAttachmentsFromHistory(req.History)
		if len(historyAttachments) > 0 {
			allAttachments := append(req.Attachments, historyAttachments...)
			fileContext := b.buildFileContextSection(allAttachments)
			if len(historyAttachments) > 0 && len(req.Attachments) == 0 {
				systemPrompt += fileContext
			}
		}
	}

	messages = append(messages, map[string]interface{}{
		"role":    "system",
		"content": systemPrompt,
	})

	if req.History != nil {
		for _, msg := range req.History {
			messages = append(messages, map[string]interface{}{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}

	if len(req.Attachments) > 0 && b.fileStore != nil {
		contentArray := []map[string]interface{}{
			{"type": "text", "text": req.Message},
		}

		for _, file := range req.Attachments {
			isText := isTextMimeType(file.MimeType) ||
				(file.MimeType == "application/octet-stream" && isTextFileExtension(file.Name))

			if isText && file.Size < 100*1024 {
				content, err := b.readFileContent(file.Id)
				if err == nil && len(content) > 0 {
					contentArray = append(contentArray, map[string]interface{}{
						"type": "text",
						"text": fmt.Sprintf("\n---\n**File: %s**\n```\n%s\n```\n---", file.Name, truncateContent(content, 8000)),
					})
				}
			} else if isImageMimeType(file.MimeType) {
				imageData, err := b.readFileBytes(file.Id)
				if err == nil && len(imageData) > 0 {
					base64Data := base64.StdEncoding.EncodeToString(imageData)
					contentArray = append(contentArray, map[string]interface{}{
						"type": "image_url",
						"image_url": map[string]interface{}{
							"url": fmt.Sprintf("data:%s;base64,%s", file.MimeType, base64Data),
						},
					})
				}
			}
		}

		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": contentArray,
		})
	} else {
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": req.Message,
		})
	}

	return messages
}

// streamingCallbacks holds optional callbacks for streaming responses
type streamingCallbacks struct {
	onTextDelta  func(string)
	onToolStart  func(string, string, map[string]interface{})
	onToolResult func(string, string, interface{}, int, error)
}

// buildMessages constructs the message array for the LLM
func (b *AgentBrain) buildMessages(p *persona.Persona, req *RuntimeRequest, tools []*ToolDefinition) []map[string]interface{} {
	messages := make([]map[string]interface{}, 0)

	// Add system prompt
	systemPrompt := p.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = fmt.Sprintf("You are %s, a helpful AI assistant.", p.DisplayName)
	}

	// Add tone guidance
	switch p.Tone {
	case persona.ToneProfessional:
		systemPrompt += "\n\nRespond in a professional, business-appropriate manner."
	case persona.ToneCasual:
		systemPrompt += "\n\nRespond in a friendly, casual manner."
	case persona.ToneTechnical:
		systemPrompt += "\n\nRespond with technical precision and detail."
	}

	// Add expertise context
	if len(p.ExpertiseTags) > 0 {
		systemPrompt += fmt.Sprintf("\n\nYour areas of expertise include: %s.", strings.Join(p.ExpertiseTags, ", "))
	}

	// Add tool descriptions to system prompt
	if len(tools) > 0 {
		systemPrompt += "\n\n## Available Tools\n\nYou have access to the following tools:\n\n"
		for _, tool := range tools {
			systemPrompt += fmt.Sprintf("- **%s**: %s\n", tool.Name, tool.Description)
		}
		systemPrompt += "\nAnswer questions directly from your own knowledge when possible. Only call tools when the task genuinely requires executing a workflow, processing data, or obtaining user-specific input you don't have."
	} else {
		systemPrompt += "\n\nNo tools are currently available."
	}

	// Add file context if attachments present
	if len(req.Attachments) > 0 {
		systemPrompt += b.buildFileContextSection(req.Attachments)
	}

	// Also extract file IDs from history messages (format: [Attached: fileId.ext])
	if b.fileStore != nil && req.History != nil {
		historyAttachments := b.extractAttachmentsFromHistory(req.History)
		if len(historyAttachments) > 0 {
			// Merge with any current attachments (avoid duplicates)
			allAttachments := append(req.Attachments, historyAttachments...)
			// Rebuild file context with all attachments
			fileContext := b.buildFileContextSection(allAttachments)
			// Only add the history-extracted files if not already in systemPrompt
			if len(historyAttachments) > 0 && len(req.Attachments) == 0 {
				systemPrompt += fileContext
			}
		}
	}

	messages = append(messages, map[string]interface{}{
		"role":    "system",
		"content": systemPrompt,
	})

	// Add conversation history
	if req.History != nil {
		for _, msg := range req.History {
			messages = append(messages, map[string]interface{}{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}

	// Add current message with multimodal content if attachments present
	if len(req.Attachments) > 0 && b.fileStore != nil {
		// Build multimodal content array for OpenAI
		contentArray := []map[string]interface{}{
			{"type": "text", "text": req.Message},
		}

		// Add file content for text files
		for _, file := range req.Attachments {
			// Check if it's a text file by mime type or extension
			isText := isTextMimeType(file.MimeType) ||
				(file.MimeType == "application/octet-stream" && isTextFileExtension(file.Name))

			if isText && file.Size < 100*1024 {
				content, err := b.readFileContent(file.Id)
				if err == nil && len(content) > 0 {
					contentArray = append(contentArray, map[string]interface{}{
						"type": "text",
						"text": fmt.Sprintf("\n---\n**File: %s**\n```\n%s\n```\n---", file.Name, truncateContent(content, 8000)),
					})
				}
			} else if isImageMimeType(file.MimeType) {
				// For images, include base64 data for OpenAI Vision
				imageData, err := b.readFileBytes(file.Id)
				if err == nil && len(imageData) > 0 {
					base64Data := base64.StdEncoding.EncodeToString(imageData)
					contentArray = append(contentArray, map[string]interface{}{
						"type": "image_url",
						"image_url": map[string]interface{}{
							"url": fmt.Sprintf("data:%s;base64,%s", file.MimeType, base64Data),
						},
					})
				}
			}
		}

		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": contentArray,
		})
	} else {
		// Simple text message
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": req.Message,
		})
	}

	return messages
}

// buildToolsForProvider converts tools to the appropriate format
func (b *AgentBrain) buildToolsForProvider(provider string, tools []*ToolDefinition) []map[string]interface{} {
	switch provider {
	case "anthropic":
		return BuildToolSchemasForAnthropic(tools)
	default:
		return BuildToolSchemasForOpenAI(tools)
	}
}

// executeOpenAIWithCallbacks executes the reasoning with OpenAI Responses API
func (b *AgentBrain) executeOpenAIWithCallbacks(ctx context.Context, p *persona.Persona, messages []map[string]interface{}, toolDefs []map[string]interface{}, tools []*ToolDefinition, attachments []*FileAttachment, callbacks *streamingCallbacks) (*RuntimeResponse, error) {
	apiKey, err := b.getAPIKey(ctx, p, "apiKey")
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("OpenAI API key not configured")
	}

	model := p.LLMModel
	if model == "" {
		model = "gpt-5.4"
	}

	baseURL := "https://api.openai.com/v1/responses"

	// Build initial input from messages
	var inputText string
	var userMessage string
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "system" {
			if content, ok := msg["content"].(string); ok {
				inputText = content + "\n\n"
			}
		} else if role == "user" {
			if content, ok := msg["content"].(string); ok {
				userMessage = content
			} else if contentArray, ok := msg["content"].([]interface{}); ok {
				for _, item := range contentArray {
					if itemMap, ok := item.(map[string]interface{}); ok {
						if itemMap["type"] == "text" {
							if text, ok := itemMap["text"].(string); ok {
								userMessage = text
							}
						}
					}
				}
			}
		}
	}
	inputText += userMessage

	// Build tools in Responses API format
	responsesTools := b.buildToolsForResponsesAPI(tools)

	var toolCallHistory []*ToolCallResult
	var totalUsage TokenUsage
	var previousResponseID string

	for iteration := 0; iteration < p.MaxToolIterations; iteration++ {
		var reqBody map[string]interface{}

		if iteration == 0 {
			reqBody = map[string]interface{}{
				"model":             model,
				"input":             inputText,
				"temperature":       p.LLMTemperature,
				"max_output_tokens": p.LLMMaxTokens,
			}
		} else {
			// Subsequent requests use previous_response_id
			reqBody = map[string]interface{}{
				"model":                model,
				"previous_response_id": previousResponseID,
				"input":                b.buildToolResultsInput(toolCallHistory),
				"temperature":          p.LLMTemperature,
				"max_output_tokens":    p.LLMMaxTokens,
			}
		}

		if len(responsesTools) > 0 {
			reqBody["tools"] = responsesTools
		}

		bodyBytes, _ := json.Marshal(reqBody)

		req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := b.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request failed: %w", err)
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("API error: %s", string(respBody))
		}

		var respMap map[string]interface{}
		json.Unmarshal(respBody, &respMap)

		// Capture response ID for next iteration
		if respID, ok := respMap["id"].(string); ok {
			previousResponseID = respID
		}

		// Extract usage
		if usage, ok := respMap["usage"].(map[string]interface{}); ok {
			if pt, ok := usage["input_tokens"].(float64); ok {
				totalUsage.InputTokens += int(pt)
			} else if pt, ok := usage["prompt_tokens"].(float64); ok {
				totalUsage.InputTokens += int(pt)
			}
			if ct, ok := usage["output_tokens"].(float64); ok {
				totalUsage.OutputTokens += int(ct)
			} else if ct, ok := usage["completion_tokens"].(float64); ok {
				totalUsage.OutputTokens += int(ct)
			}
			totalUsage.TotalTokens = totalUsage.InputTokens + totalUsage.OutputTokens
		}

		// Parse Responses API output
		outputArray, ok := respMap["output"].([]interface{})
		if !ok || len(outputArray) == 0 {
			return nil, fmt.Errorf("no output from API")
		}

		var content string
		var rawToolCalls []interface{}

		for _, outputItem := range outputArray {
			item, ok := outputItem.(map[string]interface{})
			if !ok {
				continue
			}

			itemType, _ := item["type"].(string)

			if itemType == "function_call" || itemType == "tool_call" {
				rawToolCalls = append(rawToolCalls, item)
				continue
			}

			if itemType == "message" {
				contentArray, ok := item["content"].([]interface{})
				if !ok {
					continue
				}

				for _, contentItem := range contentArray {
					contentObj, ok := contentItem.(map[string]interface{})
					if !ok {
						continue
					}

					if contentObj["type"] == "output_text" || contentObj["type"] == "text" {
						if text, ok := contentObj["text"].(string); ok {
							content = text
						}
					}

					if contentObj["type"] == "tool_call" || contentObj["type"] == "function_call" {
						rawToolCalls = append(rawToolCalls, contentObj)
					}
				}
			}
		}

		if callbacks != nil && callbacks.onTextDelta != nil && content != "" {
			callbacks.onTextDelta(content)
		}

		if len(rawToolCalls) == 0 {
			return &RuntimeResponse{
				Response:   content,
				State:      TaskStateCompleted,
				ToolCalls:  toolCallHistory,
				TokenUsage: &totalUsage,
			}, nil
		}

		// Execute tool calls
		for _, tc := range rawToolCalls {
			tcMap, _ := tc.(map[string]interface{})

			var toolName string
			var args map[string]interface{}
			var toolCallId string

			// Get tool call ID
			if id, ok := tcMap["call_id"].(string); ok {
				toolCallId = id
			} else if id, ok := tcMap["id"].(string); ok {
				toolCallId = id
			}

			// Get tool name
			if name, ok := tcMap["name"].(string); ok {
				toolName = name
			}

			// Get arguments
			if argsObj, ok := tcMap["arguments"].(map[string]interface{}); ok {
				args = argsObj
			} else if argsStr, ok := tcMap["arguments"].(string); ok {
				json.Unmarshal([]byte(argsStr), &args)
			}

			if callbacks != nil && callbacks.onToolStart != nil {
				callbacks.onToolStart(toolCallId, toolName, args)
			}

			startTime := time.Now()

			result, execErr := b.toolAdapter.ExecuteTool(ctx, p.Id, toolName, args, attachments)

			durationMs := int(time.Since(startTime).Milliseconds())

			toolCallResult := &ToolCallResult{
				ToolCallID: toolCallId,
				ToolName:   toolName,
				Arguments:  args,
				Duration:   int64(durationMs),
			}

			if execErr != nil {
				toolCallResult.Error = execErr.Error()
				toolCallHistory = append(toolCallHistory, toolCallResult)

				if callbacks != nil && callbacks.onToolResult != nil {
					callbacks.onToolResult(toolCallId, toolName, nil, durationMs, execErr)
				}
				continue
			}

			toolCallResult.Result = result.Content
			toolCallHistory = append(toolCallHistory, toolCallResult)

			if callbacks != nil && callbacks.onToolResult != nil {
				callbacks.onToolResult(toolCallId, toolName, result.Content, durationMs, nil)
			}

			if resultMap, ok := result.Content.(map[string]interface{}); ok {
				if action, ok := resultMap["action"].(string); ok && action == "input_required" {
					return &RuntimeResponse{
						Response: content,
						State:    TaskStateInputRequired,
						InputRequest: &InputRequest{
							Prompt:  resultMap["question"].(string),
							Options: toStringSlice(resultMap["options"]),
						},
						ToolCalls:  toolCallHistory,
						TokenUsage: &totalUsage,
					}, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("exceeded maximum tool iterations (%d)", p.MaxToolIterations)
}

func (b *AgentBrain) buildToolsForResponsesAPI(tools []*ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		result[i] = map[string]interface{}{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.InputSchema,
		}
	}
	return result
}

func (b *AgentBrain) buildToolResultsInput(toolCallHistory []*ToolCallResult) []map[string]interface{} {
	items := make([]map[string]interface{}, 0)
	for _, tc := range toolCallHistory {
		output := ""
		if tc.Error != "" {
			output = fmt.Sprintf("Error: %s", tc.Error)
		} else {
			switch v := tc.Result.(type) {
			case string:
				output = v
			default:
				if jsonBytes, err := json.Marshal(v); err == nil {
					output = string(jsonBytes)
				} else {
					output = fmt.Sprintf("%v", v)
				}
			}
		}

		items = append(items, map[string]interface{}{
			"type":    "function_call_output",
			"call_id": tc.ToolCallID,
			"output":  output,
		})
	}
	return items
}

// executeAnthropicWithCallbacks wraps executeAnthropic with callback support
func (b *AgentBrain) executeAnthropicWithCallbacks(ctx context.Context, p *persona.Persona, messages []map[string]interface{}, toolDefs []map[string]interface{}, tools []*ToolDefinition, attachments []*FileAttachment, callbacks *streamingCallbacks) (*RuntimeResponse, error) {
	// For now, just call the non-callback version
	// TODO: Add proper streaming support for Anthropic
	return b.executeAnthropic(ctx, p, messages, toolDefs, tools, attachments)
}

// executeGeminiWithCallbacks wraps executeGemini with callback support
func (b *AgentBrain) executeGeminiWithCallbacks(ctx context.Context, p *persona.Persona, messages []map[string]interface{}, toolDefs []map[string]interface{}, tools []*ToolDefinition, attachments []*FileAttachment, callbacks *streamingCallbacks) (*RuntimeResponse, error) {
	// For now, just call the non-callback version
	// TODO: Add proper streaming support for Gemini
	return b.executeGemini(ctx, p, messages, toolDefs, tools, attachments)
}

// executeAnthropic executes the reasoning with Anthropic
func (b *AgentBrain) executeAnthropic(ctx context.Context, p *persona.Persona, messages []map[string]interface{}, toolDefs []map[string]interface{}, tools []*ToolDefinition, attachments []*FileAttachment) (*RuntimeResponse, error) {
	apiKey, err := b.getAPIKey(ctx, p, "apiKey")
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("Anthropic API key not configured")
	}

	model := p.LLMModel
	if model == "" {
		model = "claude-3-5-sonnet-20241022"
	}

	// Extract system message
	var systemPrompt string
	var chatMessages []map[string]interface{}
	for _, msg := range messages {
		if role, _ := msg["role"].(string); role == "system" {
			systemPrompt, _ = msg["content"].(string)
		} else {
			chatMessages = append(chatMessages, msg)
		}
	}

	// Build request
	reqBody := map[string]interface{}{
		"model":      model,
		"max_tokens": p.LLMMaxTokens,
		"system":     systemPrompt,
		"messages":   chatMessages,
	}

	if len(toolDefs) > 0 {
		reqBody["tools"] = toolDefs
	}

	bodyBytes, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: %s", string(respBody))
	}

	var respMap map[string]interface{}
	json.Unmarshal(respBody, &respMap)

	// Parse response
	content, _ := respMap["content"].([]interface{})
	if len(content) == 0 {
		return nil, fmt.Errorf("no response from API")
	}

	var textContent string
	for _, c := range content {
		if cMap, ok := c.(map[string]interface{}); ok {
			if t, ok := cMap["type"].(string); ok && t == "text" {
				textContent, _ = cMap["text"].(string)
				break
			}
		}
	}

	// Extract usage
	var totalUsage TokenUsage
	if usage, ok := respMap["usage"].(map[string]interface{}); ok {
		if pt, ok := usage["input_tokens"].(float64); ok {
			totalUsage.InputTokens = int(pt)
		}
		if ct, ok := usage["output_tokens"].(float64); ok {
			totalUsage.OutputTokens = int(ct)
		}
		totalUsage.TotalTokens = totalUsage.InputTokens + totalUsage.OutputTokens
	}

	return &RuntimeResponse{
		Response:   textContent,
		State:      TaskStateCompleted,
		TokenUsage: &totalUsage,
	}, nil
}

// executeGemini executes the reasoning with Google Gemini
func (b *AgentBrain) executeGemini(ctx context.Context, p *persona.Persona, messages []map[string]interface{}, toolDefs []map[string]interface{}, tools []*ToolDefinition, attachments []*FileAttachment) (*RuntimeResponse, error) {
	apiKey, err := b.getAPIKey(ctx, p, "apiKey")
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("Google API key not configured")
	}

	model := p.LLMModel
	if model == "" {
		model = "gemini-1.5-pro"
	}

	// Build Gemini format
	contents := make([]map[string]interface{}, 0)
	var systemInstruction string

	for _, msg := range messages {
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)

		if role == "system" {
			systemInstruction = content
			continue
		}

		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}

		contents = append(contents, map[string]interface{}{
			"role": geminiRole,
			"parts": []map[string]string{
				{"text": content},
			},
		})
	}

	reqBody := map[string]interface{}{
		"contents": contents,
		"generationConfig": map[string]interface{}{
			"temperature":     p.LLMTemperature,
			"maxOutputTokens": p.LLMMaxTokens,
		},
	}

	if systemInstruction != "" {
		reqBody["systemInstruction"] = map[string]string{
			"parts": systemInstruction,
		}
	}

	bodyBytes, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: %s", string(respBody))
	}

	var respMap map[string]interface{}
	json.Unmarshal(respBody, &respMap)

	// Parse response
	candidates, _ := respMap["candidates"].([]interface{})
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no response from API")
	}

	candidate, _ := candidates[0].(map[string]interface{})
	content2, _ := candidate["content"].(map[string]interface{})
	parts, _ := content2["parts"].([]interface{})

	var textContent string
	for _, part := range parts {
		if pMap, ok := part.(map[string]interface{}); ok {
			if t, ok := pMap["text"].(string); ok {
				textContent = t
				break
			}
		}
	}

	// Extract usage
	var totalUsage TokenUsage
	if usage, ok := respMap["usageMetadata"].(map[string]interface{}); ok {
		if pt, ok := usage["promptTokenCount"].(float64); ok {
			totalUsage.InputTokens = int(pt)
		}
		if ct, ok := usage["candidatesTokenCount"].(float64); ok {
			totalUsage.OutputTokens = int(ct)
		}
		totalUsage.TotalTokens = totalUsage.InputTokens + totalUsage.OutputTokens
	}

	return &RuntimeResponse{
		Response:   textContent,
		State:      TaskStateCompleted,
		TokenUsage: &totalUsage,
	}, nil
}

// toStringSlice converts an interface{} to []string
func toStringSlice(v interface{}) []string {
	if v == nil {
		return nil
	}
	if arr, ok := v.([]interface{}); ok {
		result := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	if arr, ok := v.([]string); ok {
		return arr
	}
	return nil
}
