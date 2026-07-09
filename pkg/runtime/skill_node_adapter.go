package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	skillgrpc "github.com/axiom-studio/openseal/pkg/skillgrpc"
	"github.com/axiom-studio/openseal/pkg/vault"
	"github.com/axiom-studio/skills.sdk/executor"
	"go.uber.org/zap"
)

// AgentSkillProvider looks up which skills are assigned to an agent instance.
// PersonaId == AgentInstanceId (1:1 shared PK), so personaId IS the agent_instance_id.
type AgentSkillProvider interface {
	ListEnabledSkillIDs(ctx context.Context, agentInstanceID int) ([]string, error)
	ListEnabledSkills(ctx context.Context, agentInstanceID int) ([]*skill.AgentInstanceSkill, error)
}

// SkillNodeToolAdapter allows agents to invoke individual skill nodeTypes directly
// without creating workflow records (ephemeral execution).
type SkillNodeToolAdapter struct {
	registry          *skillgrpc.Registry
	skillLoader       SkillManifestProvider
	agentSkillProvider AgentSkillProvider
	vaultSvc          vault.VaultService
	logger            *zap.SugaredLogger
}

// SkillManifestProvider provides access to skill manifests by skill ID
type SkillManifestProvider interface {
	GetSkillManifest(skillID string) *SkillManifestInfo
}

// SkillManifestInfo holds manifest and address info for a loaded skill.
type SkillManifestInfo struct {
	SkillID   string
	Name      string
	Address   string
	NodeTypes []string
}

// VaultClient resolves vault:// secrets for ephemeral resolver.
// If no vault is available (dev mode), nil is acceptable and bindings
// are returned as-is with a warning.
type VaultClient interface {
	GetSecret(path string) (string, error)
}

// vaultServiceAdapter wraps a vault.VaultService to implement VaultClient.
// It resolves vault://credential_name.field_name paths.
type vaultServiceAdapter struct {
	service     vault.VaultService
	projectId   *int
	ctx         context.Context
}

func (v *vaultServiceAdapter) GetSecret(path string) (string, error) {
	// path is expected to be "vault://credential_name.field_name"
	trimmed := strings.TrimPrefix(path, "vault://")
	parts := strings.SplitN(trimmed, ".", 2)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid vault path: %s", path)
	}
	credentialName := parts[0]
	fieldName := parts[1]

	fields, err := v.service.ResolveCredential(v.ctx, credentialName, v.projectId)
	if err != nil {
		return "", fmt.Errorf("failed to resolve vault credential '%s': %w", credentialName, err)
	}

	if value, ok := fields[fieldName]; ok {
		if s, ok := value.(string); ok {
			return s, nil
		}
		return fmt.Sprintf("%v", value), nil
	}

	return "", fmt.Errorf("field '%s' not found in credential '%s'", fieldName, credentialName)
}

func NewSkillNodeToolAdapter(
	registry *skillgrpc.Registry,
	skillLoader SkillManifestProvider,
	agentSkillProvider AgentSkillProvider,
	vaultSvc vault.VaultService,
	logger *zap.SugaredLogger,
) *SkillNodeToolAdapter {
	return &SkillNodeToolAdapter{
		registry:           registry,
		skillLoader:        skillLoader,
		agentSkillProvider: agentSkillProvider,
		vaultSvc:           vaultSvc,
		logger:             logger,
	}
}

// NewSkillNodeToolAdapterWithoutSkillProvider creates an adapter without an agent skill provider.
// In this mode, all globally registered skill nodeTypes are returned for every persona (legacy behavior).
func NewSkillNodeToolAdapterWithoutSkillProvider(
	registry *skillgrpc.Registry,
	skillLoader SkillManifestProvider,
	vaultSvc vault.VaultService,
	logger *zap.SugaredLogger,
) *SkillNodeToolAdapter {
	return &SkillNodeToolAdapter{
		registry:     registry,
		skillLoader:  skillLoader,
		vaultSvc:     vaultSvc,
		logger:       logger,
	}
}

func (a *SkillNodeToolAdapter) GetToolsForPersona(ctx context.Context, personaId int) ([]*ToolDefinition, error) {
	definitions := make([]*ToolDefinition, 0)

	if a.agentSkillProvider == nil {
		return a.getToolsForAllSkills(ctx, personaId), nil
	}

	instanceSkills, err := a.agentSkillProvider.ListEnabledSkills(ctx, personaId)
	if err != nil {
		a.logger.Warnw("failed to list skills for agent instance, falling back to all registered skills",
			"agentInstanceId", personaId, "error", err)
		return a.getToolsForAllSkills(ctx, personaId), nil
	}

	if len(instanceSkills) == 0 {
		return definitions, nil
	}

	for _, is := range instanceSkills {
		skillID := is.SkillID
		skillTypes := a.registry.GetSkillTypes(skillID)

		selectedTypes := filterNodeTypes(skillTypes, is)

		for _, nodeType := range selectedTypes {
			description := fmt.Sprintf("Invoke skill nodeType: %s", nodeType)

			if a.skillLoader != nil {
				if manifest := a.skillLoader.GetSkillManifest(skillID); manifest != nil {
					description = fmt.Sprintf("Invoke %s skill nodeType: %s", manifest.Name, nodeType)
				}
			}

			inputSchema := a.buildInputSchemaForNodeType(nodeType, skillID)
			sanitizedNodeType := sanitizeToolName(nodeType)

			config := map[string]interface{}{
				"nodeType": nodeType,
				"skillId":  skillID,
			}
			if overrides, ok := is.FieldOverrides[nodeType]; ok {
				if overrideMap, ok := overrides.(map[string]interface{}); ok {
					for k, v := range overrideMap {
						config[k] = v
					}
				}
			}

			tool := &ToolDefinition{
				Name:        fmt.Sprintf("invoke_skill_%s", sanitizedNodeType),
				Description: description,
				InputSchema: inputSchema,
				Config:      config,
			}

			definitions = append(definitions, tool)
		}
	}

	return definitions, nil
}

// getToolsForAllSkills returns tools for all globally registered skills (legacy fallback)
func (a *SkillNodeToolAdapter) getToolsForAllSkills(_ context.Context, _ int) []*ToolDefinition {
	definitions := make([]*ToolDefinition, 0)

	nodeTypes := a.registry.ListTypes()
	if len(nodeTypes) == 0 {
		return definitions
	}

	skillIDs := a.registry.ListSkills()

	nodeTypeToSkill := make(map[string]string)
	for _, skillID := range skillIDs {
		skillTypes := a.registry.GetSkillTypes(skillID)
		for _, nt := range skillTypes {
			nodeTypeToSkill[nt] = skillID
		}
	}

	for _, nodeType := range nodeTypes {
		skillID, ok := nodeTypeToSkill[nodeType]
		if !ok {
			continue
		}

		description := fmt.Sprintf("Invoke skill nodeType: %s", nodeType)

		if a.skillLoader != nil {
			if manifest := a.skillLoader.GetSkillManifest(skillID); manifest != nil {
				description = fmt.Sprintf("Invoke %s skill nodeType: %s", manifest.Name, nodeType)
			}
		}

		inputSchema := a.buildInputSchemaForNodeType(nodeType, skillID)
		sanitizedNodeType := sanitizeToolName(nodeType)

		tool := &ToolDefinition{
			Name:        fmt.Sprintf("invoke_skill_%s", sanitizedNodeType),
			Description: description,
			InputSchema: inputSchema,
			Config: map[string]interface{}{
				"nodeType": nodeType,
				"skillId":  skillID,
			},
		}

		definitions = append(definitions, tool)
	}

	return definitions
}

func (a *SkillNodeToolAdapter) ExecuteTool(ctx context.Context, personaId int, toolName string, args map[string]interface{}) (*ToolResult, error) {
	startTime := time.Now()

	if !strings.HasPrefix(toolName, "invoke_skill_") {
		return nil, fmt.Errorf("invalid skill tool name: %s", toolName)
	}

	defs, err := a.GetToolsForPersona(ctx, personaId)
	if err != nil {
		return nil, fmt.Errorf("failed to get tool definitions: %w", err)
	}

	var matchedDef *ToolDefinition
	for _, def := range defs {
		if def.Name == toolName {
			matchedDef = def
			break
		}
	}

	if matchedDef == nil {
		return nil, fmt.Errorf("tool not found: %s", toolName)
	}

	nodeType, _ := matchedDef.Config["nodeType"].(string)
	skillID, _ := matchedDef.Config["skillId"].(string)

	// Production approval guardrail for ArgoCD sync operations
	if strings.Contains(strings.ToLower(nodeType), "argocd") {
		targetEnv := getString(args, "environment", getString(args, "namespace", ""))
		if isProductionEnvironment(targetEnv) {
			a.logger.Warnw("blocked production ArgoCD sync attempt",
				"toolName", toolName,
				"nodeType", nodeType,
				"skillId", skillID,
				"targetEnv", targetEnv,
			)
			return &ToolResult{
				IsError: true,
				Content: map[string]interface{}{
					"error":            "Production deployment requires approval",
					"message":          "This operation targets a production environment. Please request approval from a Release Manager before proceeding.",
					"approval_required": true,
					"target":           targetEnv,
				},
			}, nil
		}
	}

	client := a.registry.GetClient(skillID)
	if client == nil {
		return nil, fmt.Errorf("skill not available: %s (nodeType: %s)", skillID, nodeType)
	}

	config := a.buildExecutionConfig(nodeType, args)

	for k, v := range matchedDef.Config {
		if k == "nodeType" || k == "skillId" {
			continue
		}
		if _, exists := config[k]; !exists {
			config[k] = v
		}
	}

	if a.skillLoader != nil {
		if manifest := a.skillLoader.GetSkillManifest(skillID); manifest != nil {
			config = a.enforceFieldSources(nodeType, config, skillID)
		}
	}

	step := &executor.StepDefinition{
		Id:     fmt.Sprintf("ephemeral_%s_%d", sanitizeToolName(nodeType), startTime.UnixMilli()),
		Name:   nodeType,
		Type:   nodeType,
		Config: config,
	}

	resolver := &ephemeralResolver{
		config:        config,
		outputs:       make(map[string]interface{}),
		vars:          make(map[string]interface{}),
		workflowInput:  make(map[string]interface{}),
	}

	if a.vaultSvc != nil {
		resolver.vaultClient = &vaultServiceAdapter{
			service:   a.vaultSvc,
			ctx:       ctx,
		}
	}

	result, err := client.Execute(ctx, step, resolver)
	if err != nil {
		a.logger.Errorw("skill nodeType execution failed",
			"toolName", toolName,
			"nodeType", nodeType,
			"skillId", skillID,
			"error", err,
			"duration", time.Since(startTime).Milliseconds(),
		)
		return nil, fmt.Errorf("skill execution failed for %s: %w", nodeType, err)
	}

	a.logger.Infow("executed skill nodeType tool",
		"toolName", toolName,
		"nodeType", nodeType,
		"skillId", skillID,
		"duration", time.Since(startTime).Milliseconds(),
	)

	output := result.Output
	if output == nil {
		output = make(map[string]interface{})
	}

	return &ToolResult{
		ToolCallID: fmt.Sprintf("skill_%s_%d", sanitizeToolName(nodeType), startTime.UnixMilli()),
		Content:    output,
	}, nil
}

func (a *SkillNodeToolAdapter) buildInputSchemaForNodeType(nodeType, skillID string) map[string]interface{} {
	if client := a.registry.GetClient(skillID); client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		schemaBytes, err := client.GetNodeSchema(ctx, nodeType)
		if err == nil && len(schemaBytes) > 0 {
			var schema map[string]interface{}
			if json.Unmarshal(schemaBytes, &schema) == nil {
				if inputSchema := a.extractInputSchemaFromNodeSchema(schema); inputSchema != nil {
					return inputSchema
				}
			}
		}
	}

	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"config": map[string]interface{}{
				"type":        "object",
				"description": fmt.Sprintf("Configuration for %s nodeType", nodeType),
			},
		},
	}
}

func (a *SkillNodeToolAdapter) extractInputSchemaFromNodeSchema(schema map[string]interface{}) map[string]interface{} {
	properties := make(map[string]interface{})
	var required []string

	if sections, ok := schema["sections"].([]interface{}); ok {
		for _, sec := range sections {
			section, ok := sec.(map[string]interface{})
			if !ok {
				continue
			}
			if fields, ok := section["fields"].([]interface{}); ok {
				for _, f := range fields {
					field, ok := f.(map[string]interface{})
					if !ok {
						continue
					}
					key, _ := field["key"].(string)
					if key == "" {
						continue
					}
					if source, ok := field["source"].(string); ok && source == "connection" {
						continue
					}
					properties[key] = a.fieldSchemaToProperty(field)
					if req, ok := field["required"].(bool); ok && req {
						required = append(required, key)
					}
				}
			}
		}
	}

	if configSchema, ok := schema["configSchema"].(map[string]interface{}); ok {
		if props, ok := configSchema["properties"].(map[string]interface{}); ok {
			for k, v := range props {
				properties[k] = v
			}
		}
		if req, ok := configSchema["required"].([]interface{}); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
	}

	if len(properties) == 0 {
		return nil
	}

	inputSchema := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		inputSchema["required"] = required
	}
	return inputSchema
}

func (a *SkillNodeToolAdapter) fieldSchemaToProperty(field map[string]interface{}) map[string]interface{} {
	prop := map[string]interface{}{}

	fieldType, _ := field["type"].(string)
	switch fieldType {
	case "text", "textarea", "expression", "code", "cron":
		prop["type"] = "string"
	case "number", "slider":
		prop["type"] = "number"
	case "toggle":
		prop["type"] = "boolean"
	case "select":
		prop["type"] = "string"
		if options, ok := field["options"].([]interface{}); ok {
			enumValues := make([]string, 0, len(options))
			for _, opt := range options {
				if optMap, ok := opt.(map[string]interface{}); ok {
					if val, ok := optMap["value"].(string); ok {
						enumValues = append(enumValues, val)
					}
				}
				if s, ok := opt.(string); ok {
					enumValues = append(enumValues, s)
				}
			}
			if len(enumValues) > 0 {
				prop["enum"] = enumValues
			}
		}
	case "multiselect", "tags":
		prop["type"] = "array"
		prop["items"] = map[string]interface{}{"type": "string"}
	case "keyvalue", "json":
		prop["type"] = "object"
	default:
		prop["type"] = "string"
	}

	if label, ok := field["label"].(string); ok && label != "" {
		prop["description"] = label
	}
	if hint, ok := field["hint"].(string); ok && hint != "" {
		if desc, exists := prop["description"].(string); exists && desc != "" {
			prop["description"] = desc + ". " + hint
		} else {
			prop["description"] = hint
		}
	}
	if desc, ok := field["description"].(string); ok && desc != "" {
		prop["description"] = desc
	}
	if defaultVal, ok := field["default"]; ok && defaultVal != nil {
		prop["default"] = defaultVal
	}

	return prop
}

func (a *SkillNodeToolAdapter) buildExecutionConfig(_ string, args map[string]interface{}) map[string]interface{} {
	config := make(map[string]interface{})
	for k, v := range args {
		config[k] = v
	}
	return config
}

func (a *SkillNodeToolAdapter) enforceFieldSources(nodeType string, config map[string]interface{}, skillID string) map[string]interface{} {
	schema := a.getNodeSchemaForEnforcement(nodeType, skillID)
	if schema == nil {
		return config
	}

	connectionFields := a.extractFieldsBySource(schema, "connection")

	filtered := make(map[string]interface{})
	for k, v := range config {
		if connectionFields[k] {
			a.logger.Warnw("stripped connection field from agent args",
				"field", k,
				"nodeType", nodeType,
				"skillId", skillID,
			)
			continue
		}
		filtered[k] = v
	}

	defaultFields := a.extractDefaultFields(schema)
	for key, defaultVal := range defaultFields {
		if _, exists := filtered[key]; !exists {
			filtered[key] = defaultVal
		}
	}

	return filtered
}

func (a *SkillNodeToolAdapter) getNodeSchemaForEnforcement(nodeType, skillID string) map[string]interface{} {
	if client := a.registry.GetClient(skillID); client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		schemaBytes, err := client.GetNodeSchema(ctx, nodeType)
		if err == nil && len(schemaBytes) > 0 {
			var schema map[string]interface{}
			if json.Unmarshal(schemaBytes, &schema) == nil {
				return schema
			}
		}
	}
	return nil
}

func (a *SkillNodeToolAdapter) extractFieldsBySource(schema map[string]interface{}, source string) map[string]bool {
	fields := make(map[string]bool)

	if sections, ok := schema["sections"].([]interface{}); ok {
		for _, sec := range sections {
			section, ok := sec.(map[string]interface{})
			if !ok {
				continue
			}
			if sectionFields, ok := section["fields"].([]interface{}); ok {
				for _, f := range sectionFields {
					field, ok := f.(map[string]interface{})
					if !ok {
						continue
					}
					key, _ := field["key"].(string)
					if key == "" {
						continue
					}
					if src, ok := field["source"].(string); ok && src == source {
						fields[key] = true
					}
				}
			}
		}
	}

	return fields
}

func (a *SkillNodeToolAdapter) extractDefaultFields(schema map[string]interface{}) map[string]interface{} {
	defaults := make(map[string]interface{})

	if sections, ok := schema["sections"].([]interface{}); ok {
		for _, sec := range sections {
			section, ok := sec.(map[string]interface{})
			if !ok {
				continue
			}
			if sectionFields, ok := section["fields"].([]interface{}); ok {
				for _, f := range sectionFields {
					field, ok := f.(map[string]interface{})
					if !ok {
						continue
					}
					key, _ := field["key"].(string)
					if key == "" {
						continue
					}
					src, _ := field["source"].(string)
					if src == "connection" {
						continue
					}
					if defaultVal, exists := field["default"]; exists && defaultVal != nil {
						defaults[key] = defaultVal
					}
				}
			}
		}
	}

	return defaults
}

func sanitizeToolName(name string) string {
	result := ""
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			result += string(c)
		} else if c == '.' || c == '/' || c == '-' || c == ' ' {
			result += "_"
		}
	}
	return strings.ToLower(result)
}

// isProductionEnvironment checks if the given environment name indicates a production environment.
func isProductionEnvironment(env string) bool {
	if env == "" {
		return false
	}
	prodKeywords := []string{"prod", "production", "live", "master"}
	envLower := strings.ToLower(env)
	for _, keyword := range prodKeywords {
		if strings.Contains(envLower, keyword) {
			return true
		}
	}
	return false
}

// getString retrieves a string value from a map, returning defaultValue if not found or not a string.
func getString(m map[string]interface{}, key, defaultValue string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultValue
}

type ephemeralResolver struct {
	config        map[string]interface{}
	outputs       map[string]interface{}
	vars          map[string]interface{}
	workflowInput map[string]interface{}
	vaultClient   VaultClient
}

func (r *ephemeralResolver) ResolveString(template string) string {
	if template == "" {
		return ""
	}

	if strings.HasPrefix(template, "vault://") {
		if r.vaultClient != nil {
			secret, err := r.vaultClient.GetSecret(template)
			if err == nil {
				return secret
			}
		}
		return template
	}

	if strings.HasPrefix(template, "workflow.input.") {
		key := strings.TrimPrefix(template, "workflow.input.")
		if val, ok := r.workflowInput[key]; ok {
			if s, ok := val.(string); ok {
				return s
			}
			return fmt.Sprintf("%v", val)
		}
		return template
	}

	if strings.HasPrefix(template, "previous.output.") {
		key := strings.TrimPrefix(template, "previous.output.")
		if val, ok := r.outputs[key]; ok {
			if s, ok := val.(string); ok {
				return s
			}
			return fmt.Sprintf("%v", val)
		}
		return template
	}

	return template
}

func (r *ephemeralResolver) ResolveMap(input map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range input {
		if s, ok := v.(string); ok {
			result[k] = r.ResolveString(s)
		} else {
			result[k] = v
		}
	}
	return result
}

func (r *ephemeralResolver) EvaluateCondition(condition string) bool {
	return condition != "" && condition != "false" && condition != "0"
}

func (r *ephemeralResolver) SetVariable(name string, value interface{}) {
	if r.vars == nil {
		r.vars = make(map[string]interface{})
	}
	r.vars[name] = value
}

func (r *ephemeralResolver) GetStepOutput(stepName string) interface{} {
	return r.outputs[stepName]
}

func (r *ephemeralResolver) SetStepOutput(stepName string, output interface{}) {
	if r.outputs == nil {
		r.outputs = make(map[string]interface{})
	}
	r.outputs[stepName] = output
}

func (r *ephemeralResolver) GetContextData() map[string]interface{} {
	return map[string]interface{}{
		"bindings": map[string]interface{}{},
		"trigger":  map[string]interface{}{},
		"prev":     map[string]interface{}{},
		"nodes":    map[string]interface{}{},
		"vars":     r.vars,
		"self":     r.config,
		"run":      map[string]interface{}{},
	}
}

// AgentInstanceSkillProvider adapts AgentInstanceSkillRepository to the AgentSkillProvider interface.
type AgentInstanceSkillProvider struct {
	repo skill.AgentInstanceSkillRepository
}

func NewAgentInstanceSkillProvider(repo skill.AgentInstanceSkillRepository) *AgentInstanceSkillProvider {
	return &AgentInstanceSkillProvider{repo: repo}
}

func (p *AgentInstanceSkillProvider) ListEnabledSkillIDs(ctx context.Context, agentInstanceID int) ([]string, error) {
	instanceSkills, err := p.repo.ListSkillsByAgent(ctx, agentInstanceID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(instanceSkills))
	for _, is := range instanceSkills {
		if is.IsEnabled {
			ids = append(ids, is.SkillID)
		}
	}
	return ids, nil
}

func (p *AgentInstanceSkillProvider) ListEnabledSkills(ctx context.Context, agentInstanceID int) ([]*skill.AgentInstanceSkill, error) {
	instanceSkills, err := p.repo.ListSkillsByAgent(ctx, agentInstanceID)
	if err != nil {
		return nil, err
	}
	enabled := make([]*skill.AgentInstanceSkill, 0, len(instanceSkills))
	for _, is := range instanceSkills {
		if is.IsEnabled {
			enabled = append(enabled, is)
		}
	}
	return enabled, nil
}

func filterNodeTypes(allTypes []string, instanceSkill *skill.AgentInstanceSkill) []string {
	if instanceSkill == nil || len(instanceSkill.SelectedNodeTypes) == 0 {
		return allTypes
	}
	selected := make(map[string]bool, len(instanceSkill.SelectedNodeTypes))
	for _, nt := range instanceSkill.SelectedNodeTypes {
		selected[nt] = true
	}
	filtered := make([]string, 0, len(instanceSkill.SelectedNodeTypes))
	for _, nt := range allTypes {
		if selected[nt] {
			filtered = append(filtered, nt)
		}
	}
	return filtered
}
