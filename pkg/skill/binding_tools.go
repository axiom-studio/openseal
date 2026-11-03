package skill

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type BindingToolsSpec = capability.BindingToolsSpec
type BindingTool = capability.BindingTool
type BindingToolEffect = capability.BindingToolEffect
type ModelActionBindingTool = capability.ModelActionBindingTool

const (
	BindingToolEffectRead  = capability.BindingToolEffectRead
	BindingToolEffectWrite = capability.BindingToolEffectWrite
)

// Bounds for binding-declared tools. A binding is refused when it exceeds
// them, so one connected service cannot make activation or the model-facing
// catalog unbounded.
const (
	MaxBindingTools                = 200
	MaxBindingToolNameBytes        = 128
	MaxBindingToolDescriptionBytes = MaxActionDescriptionBytes
	MaxBindingToolSchemaBytes      = 64 << 10
	MaxBindingToolsBytes           = 1 << 20
	MaxBindingSlugBytes            = 32
	MaxBindingToolModelPrefixBytes = 64
)

var (
	bindingSlugPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,30}[a-z0-9])?$`)
	bindingToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	bindingToolPrefix      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	bindingSlugReplacer    = regexp.MustCompile(`[^a-z0-9]+`)
)

// BindingSlug returns the binding's stable model-name segment: its declared
// Slug, or one derived from its ID.
func BindingSlug(binding *Binding) string {
	if binding == nil {
		return ""
	}
	if slug := strings.TrimSpace(binding.Slug); slug != "" {
		return slug
	}
	slug := strings.Trim(bindingSlugReplacer.ReplaceAllString(strings.ToLower(binding.ID), "-"), "-")
	if len(slug) > MaxBindingSlugBytes {
		slug = strings.TrimRight(slug[:MaxBindingSlugBytes], "-_")
	}
	if slug == "" {
		return "binding"
	}
	return slug
}

// bindingClaimsSlug reports whether a binding's slug appears in model names
// and must therefore be unique within its deployment.
func bindingClaimsSlug(binding *Binding) bool {
	return binding != nil && !binding.Disabled && (strings.TrimSpace(binding.Slug) != "" || len(binding.Tools) > 0)
}

func bindingToolModelPrefix(definition *Definition) string {
	if definition == nil || definition.BindingTools == nil {
		return ""
	}
	if prefix := strings.TrimSpace(definition.BindingTools.ModelPrefix); prefix != "" {
		return prefix
	}
	return definition.ID
}

// BindingToolModelName is the model action name of one binding tool.
func BindingToolModelName(definition *Definition, binding *Binding, tool string) string {
	return bindingToolModelPrefix(definition) + "." + BindingSlug(binding) + "." + tool
}

func validateBindingToolsSpec(definition *Definition) error {
	spec := definition.BindingTools
	if spec == nil {
		return nil
	}
	if spec.ModelPrefix != strings.TrimSpace(spec.ModelPrefix) || (spec.ModelPrefix != "" && !bindingToolPrefix.MatchString(spec.ModelPrefix)) {
		return errors.New("binding tools model prefix is invalid")
	}
	if spec.ReadAction == "" && spec.WriteAction == "" {
		return errors.New("binding tools must route to a read or write action")
	}
	if spec.ReadAction != "" && spec.ReadAction == spec.WriteAction {
		return errors.New("binding tools read and write actions must differ")
	}
	inputArgument := strings.TrimSpace(spec.InputArgument)
	if inputArgument == "" || inputArgument != spec.InputArgument {
		return errors.New("binding tools input argument is required")
	}
	seen := map[string]bool{inputArgument: true}
	for _, name := range spec.FixedArguments {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || seen[name] {
			return errors.New("binding tools fixed arguments must be unique, non-empty, and distinct from the input argument")
		}
		seen[name] = true
	}
	if len(spec.FixedArguments) == 0 {
		return errors.New("binding tools must fix at least one argument that identifies the tool")
	}
	for _, effect := range []BindingToolEffect{BindingToolEffectRead, BindingToolEffectWrite} {
		actionName := spec.Action(effect)
		if actionName == "" {
			continue
		}
		action, ok := definition.Actions[actionName]
		if !ok {
			return fmt.Errorf("binding tools %s action %s does not exist", effect, actionName)
		}
		if effect == BindingToolEffectRead && (action.Risk != RiskLevelRead || (action.SideEffect != SideEffectRead && action.SideEffect != SideEffectNone)) {
			return fmt.Errorf("binding tools read action %s must be read-only", actionName)
		}
		properties, _ := action.InputSchema["properties"].(map[string]interface{})
		input, ok := properties[inputArgument].(map[string]interface{})
		if !ok {
			return fmt.Errorf("binding tools action %s does not declare input argument %s", actionName, inputArgument)
		}
		if kind, declared := input["type"]; declared && kind != "object" {
			return fmt.Errorf("binding tools action %s input argument %s must be an object", actionName, inputArgument)
		}
		for _, name := range spec.FixedArguments {
			if _, ok := properties[name].(map[string]interface{}); !ok {
				return fmt.Errorf("binding tools action %s does not declare fixed argument %s", actionName, name)
			}
		}
		if !schemaRequires(action.InputSchema, append([]string{inputArgument}, spec.FixedArguments...)) {
			return fmt.Errorf("binding tools action %s must require its input and fixed arguments", actionName)
		}
	}
	return nil
}

func schemaRequires(schema map[string]interface{}, names []string) bool {
	required := make(map[string]bool)
	switch values := schema["required"].(type) {
	case []interface{}:
		for _, value := range values {
			if name, ok := value.(string); ok {
				required[name] = true
			}
		}
	case []string:
		for _, name := range values {
			required[name] = true
		}
	}
	for _, name := range names {
		if !required[name] {
			return false
		}
	}
	return true
}

// validateBindingTools checks a binding's declared tools against its
// definition's BindingTools contract.
func validateBindingTools(binding *Binding, definition *Definition) error {
	if binding.Slug != "" && !bindingSlugPattern.MatchString(binding.Slug) {
		return fmt.Errorf("binding slug must match %s", bindingSlugPattern.String())
	}
	spec := definition.BindingTools
	if spec == nil {
		if len(binding.Tools) > 0 {
			return errors.New("binding tools are not declared by the skill")
		}
		return nil
	}
	if len(binding.Tools) > MaxBindingTools {
		return fmt.Errorf("binding declares %d tools; at most %d are allowed", len(binding.Tools), MaxBindingTools)
	}
	for _, name := range binding.AllowedActions {
		if spec.RoutesAction(name) && len(binding.ArgumentRestrictions[name]) > 0 {
			return fmt.Errorf("binding action %s routes binding tools and cannot also carry argument restrictions", name)
		}
		if _, bound := binding.ArgumentBindings[name][spec.InputArgument]; spec.RoutesAction(name) && bound {
			return fmt.Errorf("binding action %s cannot bind the binding tool input argument %s", name, spec.InputArgument)
		}
	}
	fixedSchemas := make(map[string]*compiledSchema)
	total := 0
	names := make(map[string]bool, len(binding.Tools))
	identities := make(map[string]string, len(binding.Tools))
	for index, tool := range binding.Tools {
		if !bindingToolNamePattern.MatchString(tool.Name) {
			return fmt.Errorf("binding tool %d name is invalid", index)
		}
		if names[tool.Name] {
			return fmt.Errorf("binding tool %s is declared more than once", tool.Name)
		}
		names[tool.Name] = true
		description := strings.TrimSpace(tool.Description)
		if description == "" || len(description) > MaxBindingToolDescriptionBytes {
			return fmt.Errorf("binding tool %s description must be 1 to %d bytes", tool.Name, MaxBindingToolDescriptionBytes)
		}
		actionName := spec.Action(tool.Effect)
		if actionName == "" {
			return fmt.Errorf("binding tool %s effect %q is not routed by the skill", tool.Name, tool.Effect)
		}
		if !containsString(binding.AllowedActions, actionName) {
			return fmt.Errorf("binding tool %s requires allowed action %s", tool.Name, actionName)
		}
		size, err := validateBindingToolSchema(tool)
		if err != nil {
			return fmt.Errorf("binding tool %s input schema is invalid: %w", tool.Name, err)
		}
		if total += size + len(tool.Name) + len(description); total > MaxBindingToolsBytes {
			return fmt.Errorf("binding tools must encode to at most %d bytes", MaxBindingToolsBytes)
		}
		if len(tool.Arguments) != len(spec.FixedArguments) {
			return fmt.Errorf("binding tool %s must fix exactly the arguments %s", tool.Name, strings.Join(spec.FixedArguments, ", "))
		}
		if err := validateNonSecretConfiguration(tool.Arguments, ""); err != nil {
			return fmt.Errorf("binding tool %s arguments are invalid: %w", tool.Name, err)
		}
		action := definition.Actions[actionName]
		properties, _ := action.InputSchema["properties"].(map[string]interface{})
		for _, field := range spec.FixedArguments {
			value, ok := tool.Arguments[field]
			if !ok {
				return fmt.Errorf("binding tool %s must fix argument %s", tool.Name, field)
			}
			if _, bound := binding.ArgumentBindings[actionName][field]; bound {
				return fmt.Errorf("binding tool %s fixed argument %s is also bound by the binding", tool.Name, field)
			}
			key := actionName + "\x00" + field
			compiled := fixedSchemas[key]
			if compiled == nil {
				property, _ := properties[field].(map[string]interface{})
				var err error
				if compiled, err = compileSchema("binding-tool-argument.json", property); err != nil {
					return fmt.Errorf("compile binding tool argument %s: %w", field, err)
				}
				fixedSchemas[key] = compiled
			}
			if err := compiled.validate(value); err != nil {
				return fmt.Errorf("binding tool %s argument %s does not satisfy action %s: %w", tool.Name, field, actionName, err)
			}
		}
		identity := bindingToolIdentity(actionName, spec.FixedArguments, tool.Arguments)
		if previous, duplicate := identities[identity]; duplicate {
			return fmt.Errorf("binding tools %s and %s fix identical arguments", previous, tool.Name)
		}
		identities[identity] = tool.Name
	}
	return nil
}

// validateBindingToolSchema compiles a tool's input schema and returns its
// encoded size.
func validateBindingToolSchema(tool BindingTool) (int, error) {
	if tool.InputSchema == nil {
		return 0, errors.New("an object schema is required")
	}
	if kind, _ := tool.InputSchema["type"].(string); kind != "object" {
		return 0, errors.New("the schema must have type object")
	}
	if err := validateSchemaReferences(tool.InputSchema); err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil || len(encoded) > MaxBindingToolSchemaBytes {
		return 0, fmt.Errorf("the schema must encode to at most %d bytes", MaxBindingToolSchemaBytes)
	}
	if strings.Contains(string(encoded), `"x-openseal-`) {
		return 0, errors.New("OpenSeal schema extensions are reserved for Skill definitions")
	}
	if _, err := compileSchemaJSON("binding-tool-input.json", encoded); err != nil {
		return 0, err
	}
	return len(encoded), nil
}

func bindingToolIdentity(action string, fields []string, arguments map[string]interface{}) string {
	values := make([]interface{}, 0, len(fields)+1)
	values = append(values, action)
	for _, field := range fields {
		values = append(values, arguments[field])
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// SelectedBindingTool returns the tool of binding that an input to the base
// action actionName selects by its fixed arguments, or nil when actionName
// routes no binding tools or the input selects none.
func SelectedBindingTool(definition *Definition, binding *Binding, actionName string, input map[string]interface{}) *BindingTool {
	if definition == nil || binding == nil {
		return nil
	}
	spec := definition.BindingTools
	if !spec.RoutesAction(actionName) {
		return nil
	}
	for index := range binding.Tools {
		tool := &binding.Tools[index]
		if spec.Action(tool.Effect) != actionName {
			continue
		}
		matches := true
		for _, field := range spec.FixedArguments {
			value, ok := input[field]
			if !ok || !valuesEqual(value, tool.Arguments[field]) {
				matches = false
				break
			}
		}
		if matches {
			return tool
		}
	}
	return nil
}

// validateBindingToolInput enforces, at call time, that an input to a base
// action of BindingTools selects one tool the binding declares for that
// action and that the tool input satisfies the tool's schema. Direct
// proposals therefore cannot reach an undeclared or differently pinned tool.
func validateBindingToolInput(definition *Definition, binding *Binding, actionName string, input map[string]interface{}) error {
	spec := definition.BindingTools
	if !spec.RoutesAction(actionName) {
		return nil
	}
	tool := SelectedBindingTool(definition, binding, actionName, input)
	if tool == nil {
		return errors.New("skill input does not select a tool declared by its binding")
	}
	arguments, ok := input[spec.InputArgument].(map[string]interface{})
	if !ok {
		return fmt.Errorf("skill input field %s must be the binding tool input object", spec.InputArgument)
	}
	compiled, err := compileSchema("binding-tool-input.json", tool.InputSchema)
	if err != nil {
		return fmt.Errorf("binding tool %s input schema is invalid: %w", tool.Name, err)
	}
	if err := compiled.validate(arguments); err != nil {
		return fmt.Errorf("binding tool %s input is invalid: %w", tool.Name, err)
	}
	return nil
}

// bindingModelActions projects the model actions one binding exposes:
// each allowed definition action except BindingTools base actions, which are
// reachable only through the binding's declared tools. include filters
// definition actions that the host cannot run.
func bindingModelActions(definition *Definition, binding *Binding, include func(Action) bool) []ModelAction {
	spec := definition.BindingTools
	actions := make([]ModelAction, 0, len(binding.AllowedActions)+len(binding.Tools))
	for _, name := range binding.AllowedActions {
		action := definition.Actions[name]
		if spec.RoutesAction(name) || (include != nil && !include(action)) {
			continue
		}
		actions = append(actions, ModelAction{
			Name: definition.ID + "." + name, Description: action.Description,
			BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: definition.ID,
			Version: definition.Version, Action: name, InputSchema: modelVisibleInputSchemaForBinding(action.InputSchema, binding.ArgumentBindings[name]),
			SemanticArguments: cloneStringMap(action.SemanticArguments), Risk: action.Risk, SideEffect: action.SideEffect,
			ExternalOperationPolicy: action.ExternalOperationPolicy,
		})
	}
	if spec == nil {
		return actions
	}
	for _, tool := range binding.Tools {
		name := spec.Action(tool.Effect)
		action, ok := definition.Actions[name]
		if !ok || !containsString(binding.AllowedActions, name) || (include != nil && !include(action)) {
			continue
		}
		fixed := make(map[string]interface{}, len(tool.Arguments))
		for field, value := range tool.Arguments {
			fixed[field] = cloneValue(value)
		}
		actions = append(actions, ModelAction{
			Name: BindingToolModelName(definition, binding, tool.Name), Description: strings.TrimSpace(tool.Description),
			BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: definition.ID,
			Version: definition.Version, Action: name, InputSchema: cloneMap(tool.InputSchema),
			Risk: action.Risk, SideEffect: action.SideEffect, ExternalOperationPolicy: action.ExternalOperationPolicy,
			BindingTool: &ModelActionBindingTool{Name: tool.Name, InputArgument: spec.InputArgument, Arguments: fixed},
		})
	}
	return actions
}

// validateDeploymentBindingSlugs refuses a write that would give two enabled
// bindings of one deployment the same model-name slug.
func validateDeploymentBindingSlugs(candidate *Binding, existing []*Binding) error {
	if !bindingClaimsSlug(candidate) {
		return nil
	}
	slug := BindingSlug(candidate)
	for _, other := range existing {
		if other == nil || other.ID == candidate.ID || !bindingClaimsSlug(other) {
			continue
		}
		if BindingSlug(other) == slug {
			return fmt.Errorf("%w: binding slug %q is already used by binding %s", ErrBindingInvalid, slug, other.ID)
		}
	}
	return nil
}

// validateBindingToolActionNames makes a model action name shared with a
// binding tool a hard activation error instead of a first-match or last-wins
// routing choice. Same-name definition actions of different bindings are
// left to the host projection, which may deliberately prefer one binding.
func validateBindingToolActionNames(actions []ModelAction) error {
	seen := make(map[string]ModelAction, len(actions))
	for _, action := range actions {
		if previous, duplicate := seen[action.Name]; duplicate && (previous.BindingTool != nil || action.BindingTool != nil) {
			return fmt.Errorf("%w: model action %q is exposed by bindings %s and %s", ErrBindingAmbiguous, action.Name, previous.BindingID, action.BindingID)
		}
		seen[action.Name] = action
	}
	return nil
}
