package skill

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	MaxSkillConfigFields           = 128
	MaxSkillConfigLabelRunes       = 60
	MaxSkillConfigDescriptionRunes = 200
	MaxSkillConfigEnumValues       = 256
	MaxSkillConfigEnumValueBytes   = 256
	MaxSkillConfigPatternBytes     = 1024
	MaxSkillSetupSteps             = 32
	MaxSkillSetupTitleRunes        = 120
	MaxSkillSetupDescriptionRunes  = 500
	MaxSkillSetupLinks             = 16
	MaxSkillSetupLinkLabelRunes    = 60
	MaxSkillSetupLinkURLBytes      = 2048
)

var skillConfigKeyPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9]{0,63}$`)

// validateSkillConfig enforces the operator-config contract: field shapes,
// cross-field references, the health action, setup steps, and that no config
// key can also be set as an action argument, binding configuration key, or
// credential.
func validateSkillConfig(definition *Definition) error {
	config := definition.Config
	if config == nil {
		return nil
	}
	if len(config.Scopes) == 0 {
		return errors.New("scopes must name at least one of global, tenant")
	}
	sectionScopes := make(map[SkillConfigScope]bool, len(config.Scopes))
	for _, scope := range config.Scopes {
		if scope != capability.SkillConfigScopeGlobal && scope != capability.SkillConfigScopeTenant {
			return fmt.Errorf("scope %q is not global or tenant", scope)
		}
		if sectionScopes[scope] {
			return fmt.Errorf("scope %q is duplicated", scope)
		}
		sectionScopes[scope] = true
	}
	if len(config.Fields) == 0 {
		return errors.New("at least one field is required")
	}
	if len(config.Fields) > MaxSkillConfigFields {
		return fmt.Errorf("at most %d fields are allowed", MaxSkillConfigFields)
	}
	fields := make(map[string]SkillConfigField, len(config.Fields))
	for index, field := range config.Fields {
		if !skillConfigKeyPattern.MatchString(field.Key) {
			return fmt.Errorf("field %d key %q must match %s", index, field.Key, skillConfigKeyPattern)
		}
		if _, exists := fields[field.Key]; exists {
			return fmt.Errorf("field key %s is duplicated", field.Key)
		}
		if err := validateSkillConfigField(field, sectionScopes); err != nil {
			return fmt.Errorf("field %s: %w", field.Key, err)
		}
		fields[field.Key] = field
	}
	for _, field := range config.Fields {
		keys := make([]string, 0, len(field.RequiredWhen))
		for key := range field.RequiredWhen {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			referenced, ok := fields[key]
			if !ok {
				return fmt.Errorf("field %s requiredWhen references undeclared field %s", field.Key, key)
			}
			if key == field.Key {
				return fmt.Errorf("field %s requiredWhen references itself", field.Key)
			}
			if referenced.Type == capability.SkillConfigFieldSecret {
				return fmt.Errorf("field %s requiredWhen cannot compare secret field %s", field.Key, key)
			}
			if err := capability.ValidateSkillConfigValue(referenced, field.RequiredWhen[key]); err != nil {
				return fmt.Errorf("field %s requiredWhen value for %s is invalid: %w", field.Key, key, err)
			}
		}
	}
	if config.Health != nil {
		action, ok := definition.Actions[config.Health.Action]
		if !ok {
			return fmt.Errorf("health action %q is not an action of this Skill", config.Health.Action)
		}
		if action.SideEffect != SideEffectRead {
			return fmt.Errorf("health action %s must declare sideEffect read", config.Health.Action)
		}
	}
	if len(config.Setup) > MaxSkillSetupSteps {
		return fmt.Errorf("at most %d setup steps are allowed", MaxSkillSetupSteps)
	}
	for index, step := range config.Setup {
		if err := validateSkillSetupStep(step, fields); err != nil {
			return fmt.Errorf("setup step %d: %w", index, err)
		}
	}
	return validateSkillConfigCollisions(definition)
}

func validateSkillConfigField(field SkillConfigField, sectionScopes map[SkillConfigScope]bool) error {
	if err := validateSkillConfigText("label", field.Label, true, MaxSkillConfigLabelRunes); err != nil {
		return err
	}
	if err := validateSkillConfigText("description", field.Description, false, MaxSkillConfigDescriptionRunes); err != nil {
		return err
	}
	if !capability.ValidSkillConfigFieldType(field.Type) {
		return fmt.Errorf("type %q must be one of string, enum, bool, int, secret, url", field.Type)
	}
	if field.Type == capability.SkillConfigFieldEnum {
		if len(field.Enum) == 0 || len(field.Enum) > MaxSkillConfigEnumValues {
			return fmt.Errorf("enum must list between 1 and %d values", MaxSkillConfigEnumValues)
		}
		seen := make(map[string]bool, len(field.Enum))
		for _, option := range field.Enum {
			if option == "" || len(option) > MaxSkillConfigEnumValueBytes || seen[option] {
				return fmt.Errorf("enum value %q is empty, too long, or duplicated", option)
			}
			seen[option] = true
		}
	} else if field.Enum != nil {
		return errors.New("enum is only allowed for type enum")
	}
	if (field.Min != nil || field.Max != nil) && field.Type != capability.SkillConfigFieldInt {
		return errors.New("min and max are only allowed for type int")
	}
	if field.Min != nil && field.Max != nil && *field.Min > *field.Max {
		return errors.New("min must not exceed max")
	}
	if field.Pattern != "" {
		if field.Type != capability.SkillConfigFieldString {
			return errors.New("pattern is only allowed for type string")
		}
		if len(field.Pattern) > MaxSkillConfigPatternBytes {
			return fmt.Errorf("pattern must be at most %d bytes", MaxSkillConfigPatternBytes)
		}
		if _, err := regexp.Compile(field.Pattern); err != nil {
			return fmt.Errorf("pattern does not compile: %w", err)
		}
	}
	if field.Default != nil {
		if field.Type == capability.SkillConfigFieldSecret {
			return errors.New("default is not allowed for type secret")
		}
		if err := capability.ValidateSkillConfigValue(field, field.Default); err != nil {
			return fmt.Errorf("default is invalid: %w", err)
		}
	}
	if field.RequiredWhen != nil && len(field.RequiredWhen) == 0 {
		return errors.New("requiredWhen must name at least one field")
	}
	seenScopes := make(map[SkillConfigScope]bool, len(field.Scopes))
	for _, scope := range field.Scopes {
		if !sectionScopes[scope] {
			return fmt.Errorf("scope %q is not one of the section's scopes", scope)
		}
		if seenScopes[scope] {
			return fmt.Errorf("scope %q is duplicated", scope)
		}
		seenScopes[scope] = true
	}
	switch field.Audience {
	case "", capability.SkillConfigAudienceSkill, capability.SkillConfigAudienceHost:
	default:
		return fmt.Errorf("audience %q must be skill or host", field.Audience)
	}
	return nil
}

func validateSkillSetupStep(step SkillSetupStep, fields map[string]SkillConfigField) error {
	if err := validateSkillConfigText("title", step.Title, true, MaxSkillSetupTitleRunes); err != nil {
		return err
	}
	if err := validateSkillConfigText("description", step.Description, false, MaxSkillSetupDescriptionRunes); err != nil {
		return err
	}
	if len(step.Links) > MaxSkillSetupLinks {
		return fmt.Errorf("at most %d links are allowed", MaxSkillSetupLinks)
	}
	for index, link := range step.Links {
		if err := validateSkillConfigText(fmt.Sprintf("link %d label", index), link.Label, true, MaxSkillSetupLinkLabelRunes); err != nil {
			return err
		}
		parsed, err := url.Parse(link.URL)
		if err != nil || len(link.URL) > MaxSkillSetupLinkURLBytes || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return fmt.Errorf("link %d url must be an absolute https URL", index)
		}
	}
	if step.Copy != nil {
		switch {
		case step.Copy.Field != "" && step.Copy.Platform != "":
			return errors.New("copy must set exactly one of field or platform, not both")
		case step.Copy.Field != "":
			field, ok := fields[step.Copy.Field]
			if !ok {
				return fmt.Errorf("copy field %s is not a declared config field", step.Copy.Field)
			}
			if field.Type == capability.SkillConfigFieldSecret {
				return fmt.Errorf("copy field %s is a secret and is never readable", step.Copy.Field)
			}
		case step.Copy.Platform != "":
			if !capability.ValidSkillSetupPlatformValue(step.Copy.Platform) {
				return fmt.Errorf("copy platform %q must be one of %s", step.Copy.Platform, strings.Join(capability.SkillSetupPlatformValues(), ", "))
			}
		default:
			return errors.New("copy must set exactly one of field or platform")
		}
	}
	return nil
}

func validateSkillConfigText(name, value string, required bool, maxRunes int) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return fmt.Errorf("%s is required", name)
	}
	if trimmed != value || utf8.RuneCountInString(value) > maxRunes || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be trimmed and at most %d characters", name, maxRunes)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%s must not contain control characters", name)
		}
	}
	return nil
}

// validateSkillConfigCollisions keeps every setting settable in exactly one
// place: a config key may not also be an action argument, a binding
// configuration key, or a credential name of the same Skill.
func validateSkillConfigCollisions(definition *Definition) error {
	owners := make(map[string]string)
	claim := func(name, owner string) {
		if _, exists := owners[name]; !exists {
			owners[name] = owner
		}
	}
	actionNames := make([]string, 0, len(definition.Actions))
	for name := range definition.Actions {
		actionNames = append(actionNames, name)
	}
	sort.Strings(actionNames)
	for _, name := range actionNames {
		action := definition.Actions[name]
		properties, _ := action.InputSchema["properties"].(map[string]interface{})
		for property := range properties {
			claim(property, fmt.Sprintf("an input argument of action %s", name))
		}
		for _, credential := range action.Credentials {
			claim(credential.Name, fmt.Sprintf("a credential of action %s", name))
		}
	}
	if properties, ok := definition.BindingConfigSchema["properties"].(map[string]interface{}); ok {
		for property := range properties {
			claim(property, "a binding configuration key")
		}
	}
	if definition.Prompt != nil {
		for _, credential := range definition.Prompt.Credentials {
			claim(credential.Name, "a prompt credential")
		}
	}
	adapterIDs := make([]string, 0, len(definition.ConversationAdapters))
	for id := range definition.ConversationAdapters {
		adapterIDs = append(adapterIDs, id)
	}
	sort.Strings(adapterIDs)
	for _, id := range adapterIDs {
		adapter := definition.ConversationAdapters[id]
		names := append(append([]string(nil), adapter.Transport.IngressCredentials...), adapter.Transport.DeliveryCredentials...)
		for _, credential := range adapter.Credentials {
			names = append(names, credential.Name)
		}
		for _, name := range names {
			claim(name, fmt.Sprintf("a credential of conversation adapter %s", id))
		}
	}
	callbackIDs := make([]string, 0, len(definition.CallbackAdapters))
	for id := range definition.CallbackAdapters {
		callbackIDs = append(callbackIDs, id)
	}
	sort.Strings(callbackIDs)
	for _, id := range callbackIDs {
		adapter := definition.CallbackAdapters[id]
		for _, credential := range adapter.Credentials {
			claim(credential.Name, fmt.Sprintf("a credential of callback adapter %s", id))
		}
		for _, name := range adapter.Transport.IngressCredentials {
			claim(name, fmt.Sprintf("a credential of callback adapter %s", id))
		}
		if adapter.Transport.Connection != nil {
			for _, name := range adapter.Transport.Connection.Credentials {
				claim(name, fmt.Sprintf("a credential of callback adapter %s", id))
			}
		}
	}
	for _, field := range definition.Config.Fields {
		if owner, exists := owners[field.Key]; exists {
			return fmt.Errorf("field key %s collides with %s; a setting must be declared in exactly one place", field.Key, owner)
		}
	}
	return nil
}
