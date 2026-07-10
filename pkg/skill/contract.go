package skill

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type RiskLevel string

const (
	RiskLevelRead        RiskLevel = "read"
	RiskLevelWrite       RiskLevel = "write"
	RiskLevelExternal    RiskLevel = "external"
	RiskLevelProduction  RiskLevel = "production"
	RiskLevelDestructive RiskLevel = "destructive"
)

type SideEffect string

const (
	SideEffectNone        SideEffect = "none"
	SideEffectRead        SideEffect = "read"
	SideEffectWrite       SideEffect = "write"
	SideEffectExternal    SideEffect = "external"
	SideEffectDestructive SideEffect = "destructive"
)

type IdempotencyMode string

const (
	IdempotencyNone      IdempotencyMode = "none"
	IdempotencySupported IdempotencyMode = "supported"
	IdempotencyRequired  IdempotencyMode = "required"
)

type CredentialRequirement struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Optional bool   `json:"optional,omitempty"`
}

// CredentialReference is an opaque binding identifier. Resolved values must
// never be placed in definitions, model catalogs, prompts, or activity events.
type CredentialReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type ActionRetryPolicy struct {
	MaxAttempts    int      `json:"maxAttempts"`
	InitialBackoff Duration `json:"initialBackoff,omitempty"`
	MaxBackoff     Duration `json:"maxBackoff,omitempty"`
}

type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

type Action struct {
	Name                 string                  `json:"name"`
	Description          string                  `json:"description"`
	InputSchema          map[string]interface{}  `json:"inputSchema"`
	OutputSchema         map[string]interface{}  `json:"outputSchema,omitempty"`
	SideEffect           SideEffect              `json:"sideEffect"`
	Risk                 RiskLevel               `json:"risk"`
	Permissions          []string                `json:"permissions,omitempty"`
	Credentials          []CredentialRequirement `json:"credentials,omitempty"`
	Timeout              Duration                `json:"timeout,omitempty"`
	Retry                ActionRetryPolicy       `json:"retry"`
	Idempotency          IdempotencyMode         `json:"idempotency"`
	DryRunAction         string                  `json:"dryRunAction,omitempty"`
	CompensationAction   string                  `json:"compensationAction,omitempty"`
	EmittedArtifactTypes []string                `json:"emittedArtifactTypes,omitempty"`
	EmittedEventTypes    []string                `json:"emittedEventTypes,omitempty"`
	Transport            *TransportReference     `json:"transport,omitempty"`
}

type TransportReference struct {
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint,omitempty"`
}

type Definition struct {
	ID           string             `json:"id"`
	Version      string             `json:"version"`
	Name         string             `json:"name"`
	Description  string             `json:"description,omitempty"`
	Actions      map[string]Action  `json:"actions"`
	Transport    TransportReference `json:"transport"`
	Prompt       *PromptModule      `json:"prompt,omitempty"`
	Requirements Requirements       `json:"requirements,omitempty"`
	Installers   []Installer        `json:"installers,omitempty"`
	Resources    []Resource         `json:"resources,omitempty"`
	Source       *SourceProvenance  `json:"source,omitempty"`
}

type ArgumentRule struct {
	Const interface{}   `json:"const,omitempty"`
	Enum  []interface{} `json:"enum,omitempty"`
}

type Binding struct {
	ID                   string                             `json:"id"`
	Scope                ScopeReference                     `json:"scope"`
	DeploymentID         string                             `json:"deploymentId"`
	SkillID              string                             `json:"skillId"`
	SkillVersion         string                             `json:"skillVersion"`
	AllowedActions       []string                           `json:"allowedActions"`
	EnablePrompt         bool                               `json:"enablePrompt,omitempty"`
	MaximumRisk          RiskLevel                          `json:"maximumRisk"`
	ArgumentRestrictions map[string]map[string]ArgumentRule `json:"argumentRestrictions,omitempty"`
	Credentials          map[string]CredentialReference     `json:"credentials,omitempty"`
	Config               map[string]interface{}             `json:"config,omitempty"`
	Revision             int64                              `json:"revision"`
}

// ScopeReference mirrors the portable kernel scope without coupling skill
// contracts to runtime persistence implementation details.
type ScopeReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type ModelAction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	SkillID     string                 `json:"skillId"`
	Version     string                 `json:"version"`
	Action      string                 `json:"action"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	Risk        RiskLevel              `json:"risk"`
	SideEffect  SideEffect             `json:"sideEffect"`
}

type ModelPrompt struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	SkillID        string `json:"skillId"`
	Version        string `json:"version"`
	AlwaysActive   bool   `json:"alwaysActive,omitempty"`
	UserInvocable  bool   `json:"userInvocable"`
	ModelInvocable bool   `json:"modelInvocable"`
}

type BoundAction struct {
	Definition *Definition
	Action     Action
	Binding    *Binding
}

type Catalog struct {
	mu       sync.RWMutex
	skills   map[string]*Definition
	bindings map[string]*Binding
	schemas  map[string]*compiledActionSchemas
}

func NewCatalog() *Catalog {
	return &Catalog{
		skills: make(map[string]*Definition), bindings: make(map[string]*Binding),
		schemas: make(map[string]*compiledActionSchemas),
	}
}

func (c *Catalog) Register(_ context.Context, definition *Definition) error {
	if c == nil {
		return errors.New("skill catalog is not configured")
	}
	if err := validateDefinition(definition); err != nil {
		return err
	}
	compiled := make(map[string]*compiledActionSchemas, len(definition.Actions))
	for name, action := range definition.Actions {
		schemas, err := compileActionSchemas(definition.ID, definition.Version, action)
		if err != nil {
			return fmt.Errorf("compile %s.%s schemas: %w", definition.ID, name, err)
		}
		compiled[name] = schemas
	}
	copy := cloneDefinition(definition)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := definitionKey(definition.ID, definition.Version)
	if c.skills[key] != nil {
		return errors.New("skill definition versions are immutable")
	}
	c.skills[key] = copy
	for name, schemas := range compiled {
		c.schemas[actionKey(definition.ID, definition.Version, name)] = schemas
	}
	return nil
}

func (c *Catalog) GetDefinition(_ context.Context, id, version string) (*Definition, error) {
	if c == nil {
		return nil, errors.New("skill catalog is not configured")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(version) == "" {
		return nil, errors.New("skill id and version are required")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	definition := c.skills[definitionKey(id, version)]
	if definition == nil {
		return nil, nil
	}
	return cloneDefinition(definition), nil
}

func (c *Catalog) Bind(_ context.Context, binding *Binding) error {
	if c == nil {
		return errors.New("skill catalog is not configured")
	}
	if err := validateBindingShape(binding); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	definition := c.skills[definitionKey(binding.SkillID, binding.SkillVersion)]
	if definition == nil {
		return errors.New("skill definition is not registered")
	}
	if err := validateBindingAgainstDefinition(binding, definition); err != nil {
		return err
	}
	key := bindingKey(binding.Scope, binding.DeploymentID, binding.ID)
	if current := c.bindings[key]; current != nil && binding.Revision != current.Revision+1 {
		return errors.New("skill binding revision conflict")
	}
	c.bindings[key] = cloneBinding(binding)
	return nil
}

func (c *Catalog) ListModelActions(_ context.Context, scope ScopeReference, deploymentID string) ([]ModelAction, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]ModelAction, 0)
	for _, binding := range c.bindings {
		if binding.Scope != scope || binding.DeploymentID != deploymentID {
			continue
		}
		definition := c.skills[definitionKey(binding.SkillID, binding.SkillVersion)]
		if definition == nil {
			continue
		}
		for _, name := range binding.AllowedActions {
			action := definition.Actions[name]
			result = append(result, ModelAction{
				Name: definition.ID + "." + name, Description: action.Description,
				SkillID: definition.ID, Version: definition.Version, Action: name,
				InputSchema: cloneMap(action.InputSchema), Risk: action.Risk, SideEffect: action.SideEffect,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (c *Catalog) ListModelPrompts(_ context.Context, scope ScopeReference, deploymentID string) ([]ModelPrompt, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]ModelPrompt, 0)
	for _, binding := range c.bindings {
		if binding.Scope != scope || binding.DeploymentID != deploymentID || !binding.EnablePrompt {
			continue
		}
		definition := c.skills[definitionKey(binding.SkillID, binding.SkillVersion)]
		if definition == nil || definition.Prompt == nil {
			continue
		}
		result = append(result, ModelPrompt{
			Name: definition.Name, Description: definition.Description, SkillID: definition.ID, Version: definition.Version,
			AlwaysActive: definition.Prompt.AlwaysActive, UserInvocable: definition.Prompt.UserInvocable,
			ModelInvocable: !definition.Prompt.DisableModelInvocation,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (c *Catalog) ResolvePrompt(_ context.Context, scope ScopeReference, deploymentID, skillID, version string) (*PromptModule, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, binding := range c.bindings {
		if binding.Scope != scope || binding.DeploymentID != deploymentID || binding.SkillID != skillID || binding.SkillVersion != version || !binding.EnablePrompt {
			continue
		}
		definition := c.skills[definitionKey(skillID, version)]
		if definition != nil && definition.Prompt != nil {
			copy := cloneDefinition(definition)
			return copy.Prompt, nil
		}
	}
	return nil, errors.New("bound skill prompt not found")
}

func (c *Catalog) Resolve(_ context.Context, scope ScopeReference, deploymentID, skillID, version, actionName string) (*BoundAction, error) {
	if err := validateScopeAndDeployment(scope, deploymentID); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, binding := range c.bindings {
		if binding.Scope != scope || binding.DeploymentID != deploymentID || binding.SkillID != skillID || binding.SkillVersion != version ||
			!containsString(binding.AllowedActions, actionName) {
			continue
		}
		definition := c.skills[definitionKey(skillID, version)]
		if definition == nil {
			break
		}
		copy := cloneDefinition(definition)
		return &BoundAction{Definition: copy, Action: copy.Actions[actionName], Binding: cloneBinding(binding)}, nil
	}
	return nil, errors.New("bound skill action not found")
}

func (c *Catalog) ValidateInput(_ context.Context, bound *BoundAction, input map[string]interface{}) error {
	if bound == nil || bound.Definition == nil || bound.Binding == nil {
		return errors.New("bound skill action is required")
	}
	c.mu.RLock()
	schemas := c.schemas[actionKey(bound.Definition.ID, bound.Definition.Version, bound.Action.Name)]
	c.mu.RUnlock()
	if schemas == nil || schemas.input == nil {
		return errors.New("input schema is not compiled")
	}
	if err := schemas.input.validate(input); err != nil {
		return fmt.Errorf("skill input is invalid: %w", err)
	}
	for field, rule := range bound.Binding.ArgumentRestrictions[bound.Action.Name] {
		value, ok := input[field]
		if rule.Const != nil && (!ok || !valuesEqual(value, rule.Const)) {
			return fmt.Errorf("skill input field %s must equal its binding constant", field)
		}
		if len(rule.Enum) > 0 && (!ok || !containsValue(rule.Enum, value)) {
			return fmt.Errorf("skill input field %s is outside its binding allowlist", field)
		}
	}
	return nil
}

func (c *Catalog) ValidateOutput(_ context.Context, bound *BoundAction, output map[string]interface{}) error {
	if bound == nil || bound.Definition == nil || bound.Binding == nil {
		return errors.New("bound skill action is required")
	}
	c.mu.RLock()
	schemas := c.schemas[actionKey(bound.Definition.ID, bound.Definition.Version, bound.Action.Name)]
	c.mu.RUnlock()
	if schemas == nil {
		return errors.New("output schema is not compiled")
	}
	if schemas.output == nil {
		return nil
	}
	if err := schemas.output.validate(output); err != nil {
		return fmt.Errorf("skill output is invalid: %w", err)
	}
	return nil
}

func definitionKey(id, version string) string     { return id + "@" + version }
func actionKey(id, version, action string) string { return definitionKey(id, version) + ":" + action }
func bindingKey(scope ScopeReference, deploymentID, bindingID string) string {
	return scope.Kind + ":" + scope.ID + ":" + deploymentID + ":" + bindingID
}

func validateDefinition(definition *Definition) error {
	if definition == nil || strings.TrimSpace(definition.ID) == "" || strings.TrimSpace(definition.Version) == "" || strings.TrimSpace(definition.Name) == "" {
		return errors.New("skill id, version, and name are required")
	}
	if len(definition.Actions) == 0 && definition.Prompt == nil {
		return errors.New("skill must declare at least one action or prompt module")
	}
	if len(definition.Actions) > 0 && strings.TrimSpace(definition.Transport.Kind) == "" {
		return errors.New("skill transport kind is required")
	}
	if definition.Prompt != nil && strings.TrimSpace(definition.Prompt.Instructions) == "" {
		return errors.New("skill prompt instructions are required")
	}
	for name, action := range definition.Actions {
		if name == "" || action.Name != name || strings.TrimSpace(action.Description) == "" || action.InputSchema == nil {
			return fmt.Errorf("skill action %s is incomplete", name)
		}
		if !validRisk(action.Risk) || !validSideEffect(action.SideEffect) || !validIdempotency(action.Idempotency) {
			return fmt.Errorf("skill action %s has invalid policy metadata", name)
		}
		if action.SideEffect != SideEffectNone && action.SideEffect != SideEffectRead && action.Idempotency == IdempotencyNone {
			return fmt.Errorf("skill action %s with side effects must support idempotency", name)
		}
		seenCredentials := make(map[string]bool)
		for _, requirement := range action.Credentials {
			if strings.TrimSpace(requirement.Name) == "" || strings.TrimSpace(requirement.Kind) == "" || seenCredentials[requirement.Name] {
				return fmt.Errorf("skill action %s has an invalid credential requirement", name)
			}
			seenCredentials[requirement.Name] = true
		}
		if action.DryRunAction != "" {
			if _, ok := definition.Actions[action.DryRunAction]; !ok {
				return fmt.Errorf("skill action %s references missing dry-run action", name)
			}
		}
		if action.CompensationAction != "" {
			if _, ok := definition.Actions[action.CompensationAction]; !ok {
				return fmt.Errorf("skill action %s references missing compensation action", name)
			}
		}
	}
	return nil
}

func validateBindingShape(binding *Binding) error {
	if binding == nil || strings.TrimSpace(binding.ID) == "" || strings.TrimSpace(binding.SkillID) == "" ||
		strings.TrimSpace(binding.SkillVersion) == "" || binding.Revision < 1 {
		return errors.New("binding id, skill id, version, and revision are required")
	}
	if err := validateScopeAndDeployment(binding.Scope, binding.DeploymentID); err != nil {
		return err
	}
	if (len(binding.AllowedActions) == 0 && !binding.EnablePrompt) || !validRisk(binding.MaximumRisk) {
		return errors.New("binding must enable a prompt or explicitly allow actions and set maximum risk")
	}
	return nil
}

func validateBindingAgainstDefinition(binding *Binding, definition *Definition) error {
	if binding.EnablePrompt && definition.Prompt == nil {
		return errors.New("binding enables a prompt that the skill does not define")
	}
	for _, name := range binding.AllowedActions {
		action, ok := definition.Actions[name]
		if !ok {
			return fmt.Errorf("binding action %s does not exist", name)
		}
		if riskRank(action.Risk) > riskRank(binding.MaximumRisk) {
			return fmt.Errorf("binding action %s exceeds maximum risk", name)
		}
		for _, requirement := range action.Credentials {
			ref, ok := binding.Credentials[requirement.Name]
			if requirement.Optional && !ok {
				continue
			}
			if !ok || ref.ID == "" || ref.Kind != requirement.Kind {
				return fmt.Errorf("binding action %s is missing credential %s of kind %s", name, requirement.Name, requirement.Kind)
			}
		}
		for field := range binding.ArgumentRestrictions[name] {
			properties, _ := action.InputSchema["properties"].(map[string]interface{})
			if _, ok := properties[field]; !ok {
				return fmt.Errorf("binding restriction references unknown field %s", field)
			}
		}
	}
	return nil
}

func validateScopeAndDeployment(scope ScopeReference, deploymentID string) error {
	if strings.TrimSpace(scope.Kind) == "" || strings.TrimSpace(scope.ID) == "" || strings.TrimSpace(deploymentID) == "" {
		return errors.New("scope and deployment id are required")
	}
	return nil
}

func validRisk(value RiskLevel) bool { return riskRank(value) >= 0 }
func riskRank(value RiskLevel) int {
	switch value {
	case RiskLevelRead:
		return 0
	case RiskLevelWrite:
		return 1
	case RiskLevelExternal:
		return 2
	case RiskLevelProduction:
		return 3
	case RiskLevelDestructive:
		return 4
	default:
		return -1
	}
}

func validSideEffect(value SideEffect) bool {
	return value == SideEffectNone || value == SideEffectRead || value == SideEffectWrite || value == SideEffectExternal || value == SideEffectDestructive
}

func validIdempotency(value IdempotencyMode) bool {
	return value == IdempotencyNone || value == IdempotencySupported || value == IdempotencyRequired
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
