package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/persona"
	"github.com/axiom-studio/openseal/pkg/types"
	"go.uber.org/zap"
)

// WorkflowToolAdapter converts workflows to LLM tools and executes them
type WorkflowToolAdapter struct {
	personaService   persona.PersonaService
	agentService     agent.AgentInstanceService
	orchestrator     agent.AgentOrchestrator
	fileStore        agent.FileStore
	skillNodeAdapter *SkillNodeToolAdapter
	logger           *zap.SugaredLogger
}

// NewWorkflowToolAdapter creates a new tool adapter
func NewWorkflowToolAdapter(
	personaService persona.PersonaService,
	agentService agent.AgentInstanceService,
	orchestrator agent.AgentOrchestrator,
	logger *zap.SugaredLogger,
) *WorkflowToolAdapter {
	fileStore := orchestrator.GetFileStore()

	return &WorkflowToolAdapter{
		personaService: personaService,
		agentService:   agentService,
		orchestrator:   orchestrator,
		fileStore:      fileStore,
		logger:         logger,
	}
}

func (a *WorkflowToolAdapter) SetSkillNodeAdapter(adapter *SkillNodeToolAdapter) {
	a.skillNodeAdapter = adapter
}

// GetToolsForPersona returns all tools available to a persona
// Tools are derived from triggers, each with its own input_schema
func (a *WorkflowToolAdapter) GetToolsForPersona(ctx context.Context, personaId int) ([]*ToolDefinition, error) {
	definitions := make([]*ToolDefinition, 0, 3)

	// Add built-in ask_user tool
	definitions = append(definitions, &ToolDefinition{
		Name:        ToolAskUser,
		Description: "Request input from the user ONLY when you cannot proceed without user-specific information (e.g. a file to upload, a choice between options, credentials, or personal preferences). NEVER use this for questions you can answer from your own knowledge.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"question": map[string]interface{}{
					"type":        "string",
					"description": "The question to ask the user",
				},
				"options": map[string]interface{}{
					"type":        "array",
					"items":       map[string]string{"type": "string"},
					"description": "Optional list of options for the user to choose from",
				},
			},
			"required": []string{"question"},
		},
	})

	// Get triggers for this instance - each trigger is a potential tool
	triggers, err := a.agentService.GetTriggersForInstance(personaId)
	if err != nil {
		a.logger.Warnw("failed to get triggers for persona", "personaId", personaId, "error", err)
		return definitions, nil
	}

	// Build a tool for each trigger
	for _, trigger := range triggers {
		tool := a.buildToolFromTrigger(trigger)
		if tool != nil {
			definitions = append(definitions, tool)
		}
	}

	if a.skillNodeAdapter != nil {
		skillNodeTools, err := a.skillNodeAdapter.GetToolsForPersona(ctx, personaId)
		if err == nil {
			definitions = append(definitions, skillNodeTools...)
		} else {
			a.logger.Warnw("failed to get skill node tools for persona", "personaId", personaId, "error", err)
		}
	}

	return definitions, nil
}

// buildToolFromTrigger creates a tool definition from a trigger's input_schema
func (a *WorkflowToolAdapter) buildToolFromTrigger(trigger *types.AgentTriggerBean) *ToolDefinition {
	// Generate tool name from trigger type and node
	toolName := fmt.Sprintf("%s_%s", trigger.TriggerType, trigger.NodeId)
	if trigger.TriggerType == "manual" {
		toolName = "run_workflow" // Keep backward compatibility for manual triggers
	}

	// Build description
	description := fmt.Sprintf("Execute the %s workflow.", trigger.TriggerType)
	if trigger.TriggerType == "manual" {
		description = "Execute the workflow to accomplish tasks."
	}

	// Build input schema from trigger's input_schema
	properties := make(map[string]interface{})
	required := make([]string, 0)

	for fieldName, field := range trigger.InputSchema {
		prop := a.buildPropertyFromInputField(field)
		properties[fieldName] = prop

		if field.Required {
			required = append(required, fieldName)
		}
	}

	// If no input_schema, use default
	if len(properties) == 0 {
		properties = map[string]interface{}{
			"input": map[string]interface{}{
				"type":        "object",
				"description": "Input data for the workflow",
			},
		}
	}

	inputSchema := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		inputSchema["required"] = required
	}

	return &ToolDefinition{
		Name:        toolName,
		Description: description,
		InputSchema: inputSchema,
		Config: map[string]interface{}{
			"triggerId":     trigger.Id,
			"triggerType":   trigger.TriggerType,
			"triggerNodeId": trigger.NodeId,
		},
	}
}

// buildPropertyFromInputField converts a TriggerInputField to JSON schema property
func (a *WorkflowToolAdapter) buildPropertyFromInputField(field types.TriggerInputField) map[string]interface{} {
	prop := map[string]interface{}{
		"type": field.Type,
	}

	if field.Description != "" {
		prop["description"] = field.Description
	}

	// For file types, indicate they take file IDs
	if field.Type == "file" {
		prop["type"] = "string"
		desc := field.Description
		if desc == "" {
			desc = "File attachment"
		}

		// Add file constraints to description
		if field.FileConstraints != nil {
			constraints := []string{}
			if len(field.FileConstraints.AllowedMimeTypes) > 0 {
				constraints = append(constraints, fmt.Sprintf("Accepts: %s", strings.Join(field.FileConstraints.AllowedMimeTypes, ", ")))
			}
			if field.FileConstraints.MaxFileSizeMB > 0 {
				constraints = append(constraints, fmt.Sprintf("Max size: %dMB", field.FileConstraints.MaxFileSizeMB))
			}
			if field.FileConstraints.MaxFiles > 1 {
				constraints = append(constraints, fmt.Sprintf("Max files: %d", field.FileConstraints.MaxFiles))
			}
			if len(constraints) > 0 {
				desc = fmt.Sprintf("%s. Pass file ID (e.g., 'f1'). %s", desc, strings.Join(constraints, ". "))
			}
		} else {
			desc = fmt.Sprintf("%s. Pass file ID (e.g., 'f1').", desc)
		}
		prop["description"] = desc
	}

	return prop
}

// ExecuteTool executes a tool by name with the given arguments
// attachments are file attachments from the chat message that need to be resolved for file-type inputs
func (a *WorkflowToolAdapter) ExecuteTool(ctx context.Context, personaId int, toolName string, args map[string]interface{}, attachments []*FileAttachment) (*ToolResult, error) {
	startTime := time.Now()

	// Handle built-in ask_user tool (no file resolution needed)
	if toolName == ToolAskUser {
		return a.executeAskUser(args)
	}

	// Handle skill node tools (invoke_skill_* prefix)
	if strings.HasPrefix(toolName, "invoke_skill_") {
		if a.skillNodeAdapter != nil {
			return a.skillNodeAdapter.ExecuteTool(ctx, personaId, toolName, args)
		}
		return nil, fmt.Errorf("skill node adapter not available for tool: %s", toolName)
	}

	// Handle auto-discovered run_workflow tool
	if toolName == ToolRunWorkflow {
		triggers, err := a.agentService.GetTriggersForInstance(personaId)
		if err != nil {
			a.logger.Warnw("failed to get triggers for file resolution", "personaId", personaId, "error", err)
		}

		var matchingTrigger *types.AgentTriggerBean
		for _, trigger := range triggers {
			if trigger.TriggerType != "manual" {
				continue
			}
			if trigger.InputSchema != nil && len(trigger.InputSchema) > 0 {
				matchCount := 0
				for argKey := range args {
					if _, exists := trigger.InputSchema[argKey]; exists {
						matchCount++
					}
				}
				if matchCount > 0 {
					matchingTrigger = trigger
					break
				}
			}
		}

		if matchingTrigger == nil {
			for _, trigger := range triggers {
				if trigger.TriggerType == "manual" {
					matchingTrigger = trigger
					break
				}
			}
		}

		if matchingTrigger == nil {
			return nil, fmt.Errorf("no manual trigger found for workflow")
		}

		resolvedArgs := a.resolveFileIdsInArgs(args, matchingTrigger.InputSchema, attachments)
		return a.executeWorkflow(ctx, personaId, resolvedArgs, matchingTrigger.NodeId, matchingTrigger.WorkflowId, startTime)
	}

	// Handle trigger-based tools (toolName format: triggerType_nodeId)
	// Get triggers for this instance to find matching tool
	triggers, err := a.agentService.GetTriggersForInstance(personaId)
	if err != nil {
		return nil, fmt.Errorf("failed to get triggers: %w", err)
	}

	// Find matching trigger based on tool name pattern
	var matchingTrigger *types.AgentTriggerBean
	for _, trigger := range triggers {
		expectedToolName := fmt.Sprintf("%s_%s", trigger.TriggerType, trigger.NodeId)
		if expectedToolName == toolName {
			matchingTrigger = trigger
			break
		}
	}

	if matchingTrigger == nil {
		return nil, fmt.Errorf("tool not found: %s", toolName)
	}

	// Resolve file IDs in args
	resolvedArgs := a.resolveFileIdsInArgs(args, matchingTrigger.InputSchema, attachments)
	return a.executeTriggerWorkflow(ctx, matchingTrigger, resolvedArgs, startTime)
}

// executeTriggerWorkflow triggers a workflow via a specific trigger
func (a *WorkflowToolAdapter) executeTriggerWorkflow(ctx context.Context, trigger *types.AgentTriggerBean, args map[string]interface{}, startTime time.Time) (*ToolResult, error) {
	// Get the workflow instance
	instance, err := a.agentService.GetInstance(trigger.AgentInstanceId)
	if err != nil {
		return nil, fmt.Errorf("workflow instance not found: %w", err)
	}

	if !instance.Enabled {
		return nil, fmt.Errorf("workflow is disabled")
	}

	// Prepare trigger data from arguments
	triggerData := make(map[string]interface{})
	for k, v := range args {
		triggerData[k] = v
	}

	// Add trigger node ID for targeted execution
	triggerData["__triggerNodeId"] = trigger.NodeId

	// Trigger the workflow
	triggerReq := &agent.TriggerAgentRequest{
		AgentInstanceId: trigger.AgentInstanceId,
		TriggerNodeId:   trigger.NodeId,
		TriggeredBy:     "autonomous-agent",
		TriggerData:     triggerData,
	}

	run, err := a.orchestrator.TriggerAgent(ctx, triggerReq)
	if err != nil {
		return nil, fmt.Errorf("failed to trigger workflow: %w", err)
	}

	// Wait for completion
	timeout := time.Duration(instance.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	completedRun, err := a.orchestrator.WaitForRunCompletion(ctx, run.Id, timeout)
	if err != nil {
		return nil, fmt.Errorf("workflow execution failed: %w", err)
	}

	// Extract result
	result := &ToolResult{
		ToolCallID: fmt.Sprintf("tool_%d_%d", trigger.AgentInstanceId, run.Id),
	}

	if completedRun.Status == agent.RunStatusFailed {
		result.IsError = true
		result.Content = map[string]interface{}{
			"error":  completedRun.Error,
			"status": "failed",
			"runId":  run.Id,
		}
	} else {
		// Extract output from step runs
		output := a.extractOutput(completedRun)
		result.Content = output
	}

	a.logger.Infow("executed trigger workflow tool",
		"triggerId", trigger.Id,
		"triggerType", trigger.TriggerType,
		"runId", run.Id,
		"duration", time.Since(startTime).Milliseconds(),
	)

	return result, nil
}

// executeWorkflow triggers a workflow and waits for completion
func (a *WorkflowToolAdapter) executeWorkflow(ctx context.Context, workflowInstanceId int, args map[string]interface{}, triggerNodeId string, workflowId *int, startTime time.Time) (*ToolResult, error) {
	// Get the workflow instance
	workflowInstance, err := a.agentService.GetInstance(workflowInstanceId)
	if err != nil {
		return nil, fmt.Errorf("workflow instance not found: %w", err)
	}

	if !workflowInstance.Enabled {
		return nil, fmt.Errorf("workflow is disabled")
	}

	// Prepare trigger data from arguments
	triggerData := make(map[string]interface{})
	for k, v := range args {
		triggerData[k] = v
	}

	// Trigger the workflow
	triggerReq := &agent.TriggerAgentRequest{
		AgentInstanceId: workflowInstanceId,
		TriggerNodeId:   triggerNodeId,
		TriggeredBy:     "autonomous-agent",
		TriggerData:     triggerData,
		WorkflowId:      workflowId,
	}

	run, err := a.orchestrator.TriggerAgent(ctx, triggerReq)
	if err != nil {
		return nil, fmt.Errorf("failed to trigger workflow: %w", err)
	}

	// Wait for completion
	timeout := time.Duration(workflowInstance.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	completedRun, err := a.orchestrator.WaitForRunCompletion(ctx, run.Id, timeout)
	if err != nil {
		return nil, fmt.Errorf("workflow execution failed: %w", err)
	}

	// Extract result
	result := &ToolResult{
		ToolCallID: fmt.Sprintf("tool_%d_%d", workflowInstanceId, run.Id),
	}

	if completedRun.Status == agent.RunStatusFailed {
		result.IsError = true
		result.Content = map[string]interface{}{
			"error":  completedRun.Error,
			"status": "failed",
			"runId":  run.Id,
		}
	} else {
		// Extract output from step runs
		output := a.extractOutput(completedRun)
		result.Content = output
	}

	a.logger.Infow("executed workflow tool",
		"workflowId", workflowInstanceId,
		"runId", run.Id,
		"duration", time.Since(startTime).Milliseconds(),
	)

	return result, nil
}

// executeAskUser handles the built-in ask_user tool
func (a *WorkflowToolAdapter) executeAskUser(args map[string]interface{}) (*ToolResult, error) {
	question, ok := args["question"].(string)
	if !ok {
		return nil, fmt.Errorf("ask_user requires 'question' argument")
	}

	// Return a special result that signals input is required
	return &ToolResult{
		ToolCallID: "ask_user",
		Content: map[string]interface{}{
			"action":   "input_required",
			"question": question,
			"options":  args["options"],
		},
	}, nil
}

// extractOutput extracts the output from a completed run
func (a *WorkflowToolAdapter) extractOutput(run *agent.AgentRunBean) map[string]interface{} {
	output := make(map[string]interface{})

	if run == nil {
		return output
	}

	// Add run metadata
	output["runId"] = run.Id
	output["status"] = run.Status

	// Extract outputs from step runs
	if len(run.StepRuns) > 0 {
		// Find the last successful step with output
		for i := len(run.StepRuns) - 1; i >= 0; i-- {
			step := run.StepRuns[i]
			if step.Status == agent.RunStatusCompleted && step.Output != nil {
				// Merge step output into result
				for k, v := range step.Output {
					output[k] = v
				}
			}
		}
	}

	return output
}

// resolveFileIdsInArgs resolves file IDs to FileObjects for file-type inputs
func (a *WorkflowToolAdapter) resolveFileIdsInArgs(args map[string]interface{}, inputSchema map[string]agent.TriggerInputField, attachments []*FileAttachment) map[string]interface{} {
	if len(attachments) == 0 {
		return args
	}

	// Build attachment lookup map
	attachmentMap := make(map[string]*FileAttachment)
	for _, att := range attachments {
		attachmentMap[att.Id] = att
	}

	// Resolve file IDs for file-type inputs
	resolved := make(map[string]interface{})
	for fieldName, value := range args {
		fieldSchema, hasSchema := inputSchema[fieldName]
		if hasSchema && fieldSchema.Type == "file" {
			resolved[fieldName] = a.resolveFileValue(value, attachmentMap)
		} else {
			resolved[fieldName] = value
		}
	}

	return resolved
}

// resolveFileValue converts file ID(s) to FileObject(s)
func (a *WorkflowToolAdapter) resolveFileValue(value interface{}, attachmentMap map[string]*FileAttachment) interface{} {
	// Handle single file ID (string)
	if fileId, ok := value.(string); ok {
		attachment := attachmentMap[fileId]
		if attachment == nil {
			return value // Return original if not found
		}
		return a.buildFileObject(attachment)
	}

	// Handle array of file IDs
	if fileIds, ok := value.([]interface{}); ok {
		files := make([]interface{}, 0, len(fileIds))
		for _, id := range fileIds {
			if fileId, ok := id.(string); ok {
				attachment := attachmentMap[fileId]
				if attachment != nil {
					files = append(files, a.buildFileObject(attachment))
				}
			}
		}
		return files
	}

	return value
}

// buildFileObject creates a FileObject from FileAttachment
func (a *WorkflowToolAdapter) buildFileObject(attachment *FileAttachment) map[string]interface{} {
	return map[string]interface{}{
		"_type":    "file",
		"id":       attachment.Id,
		"url":      fmt.Sprintf("/orchestrator/agent/internal/files/%s", attachment.Id),
		"filename": attachment.Name,
		"mimeType": attachment.MimeType,
		"size":     attachment.Size,
	}
}

// BuildToolSchemasForOpenAI converts tool definitions to OpenAI format
func BuildToolSchemasForOpenAI(tools []*ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(tools))

	for _, tool := range tools {
		t := map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        tool.Name,
				"description": tool.Description,
			},
		}

		if tool.InputSchema != nil {
			t["function"].(map[string]interface{})["parameters"] = tool.InputSchema
		}

		result = append(result, t)
	}

	return result
}

// BuildToolSchemasForAnthropic converts tool definitions to Anthropic format
func BuildToolSchemasForAnthropic(tools []*ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(tools))

	for _, tool := range tools {
		t := map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
		}

		if tool.InputSchema != nil {
			t["input_schema"] = tool.InputSchema
		}

		result = append(result, t)
	}

	return result
}
