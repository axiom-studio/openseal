package capability

import (
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
)

// SkillConfig declares operator-level settings for a Skill: one global value
// per field with an optional per-tenant override. Values are never per Agent
// or per user, and never ride on action calls. Hosts store, audit and deliver
// values; the kernel only validates and stores this declaration with the
// immutable Skill definition.
type SkillConfig struct {
	Scopes []SkillConfigScope `json:"scopes"`
	Fields []SkillConfigField `json:"fields"`
	Health *SkillConfigHealth `json:"health,omitempty"`
	Setup  []SkillSetupStep   `json:"setup,omitempty"`
}

type SkillConfigScope string

const (
	SkillConfigScopeGlobal SkillConfigScope = "global"
	SkillConfigScopeTenant SkillConfigScope = "tenant"
)

type SkillConfigFieldType string

const (
	SkillConfigFieldString SkillConfigFieldType = "string"
	SkillConfigFieldEnum   SkillConfigFieldType = "enum"
	SkillConfigFieldBool   SkillConfigFieldType = "bool"
	SkillConfigFieldInt    SkillConfigFieldType = "int"
	// SkillConfigFieldSecret values are write-only: encrypted at rest and
	// never returned by any API.
	SkillConfigFieldSecret SkillConfigFieldType = "secret"
	SkillConfigFieldURL    SkillConfigFieldType = "url"
)

type SkillConfigAudience string

const (
	// SkillConfigAudienceSkill fields are delivered to the Skill's runtime.
	SkillConfigAudienceSkill SkillConfigAudience = "skill"
	// SkillConfigAudienceHost fields are used by the platform on the Skill's
	// behalf and are never delivered to the Skill.
	SkillConfigAudienceHost SkillConfigAudience = "host"
)

// SkillConfigField is one operator setting. Empty Scopes means the section's
// scopes; empty Audience means SkillConfigAudienceSkill.
type SkillConfigField struct {
	Key          string                 `json:"key"`
	Label        string                 `json:"label"`
	Description  string                 `json:"description,omitempty"`
	Type         SkillConfigFieldType   `json:"type"`
	Enum         []string               `json:"enum,omitempty"`
	Default      interface{}            `json:"default,omitempty"`
	Required     bool                   `json:"required,omitempty"`
	RequiredWhen map[string]interface{} `json:"requiredWhen,omitempty"`
	Scopes       []SkillConfigScope     `json:"scopes,omitempty"`
	Audience     SkillConfigAudience    `json:"audience,omitempty"`
	// Summary includes the effective value in the operator list summary. For
	// a secret field the summary only says whether it is set.
	Summary bool   `json:"summary,omitempty"`
	Min     *int64 `json:"min,omitempty"`
	Max     *int64 `json:"max,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// EffectiveScopes returns the field's scopes, defaulting to the section's.
func (f SkillConfigField) EffectiveScopes(section []SkillConfigScope) []SkillConfigScope {
	if len(f.Scopes) > 0 {
		return append([]SkillConfigScope(nil), f.Scopes...)
	}
	return append([]SkillConfigScope(nil), section...)
}

// EffectiveAudience returns the field's audience, defaulting to skill.
func (f SkillConfigField) EffectiveAudience() SkillConfigAudience {
	if f.Audience == "" {
		return SkillConfigAudienceSkill
	}
	return f.Audience
}

// SkillConfigHealth names an optional read-only action of the same Skill that
// checks the effective configuration.
type SkillConfigHealth struct {
	Action string `json:"action"`
}

// SkillSetupStep is operator guidance rendered generically by hosts.
type SkillSetupStep struct {
	Title       string           `json:"title"`
	Description string           `json:"description,omitempty"`
	Links       []SkillSetupLink `json:"links,omitempty"`
	Copy        *SkillSetupCopy  `json:"copy,omitempty"`
}

type SkillSetupLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// SkillSetupCopy shows one read-only value with a Copy affordance. Exactly one
// of Field (a declared non-secret field of the same Skill) or Platform (one of
// SkillSetupPlatformValues, resolved by the host) is set.
type SkillSetupCopy struct {
	Field    string `json:"field,omitempty"`
	Platform string `json:"platform,omitempty"`
}

// SkillSetupPlatformOAuthCallbackURL is the redirect URI the host uses for
// every OAuth sign-in.
const SkillSetupPlatformOAuthCallbackURL = "oauthCallbackUrl"

// SkillSetupPlatformValues returns the fixed set of platform values a setup
// step may copy. Hosts resolve each one read-only.
func SkillSetupPlatformValues() []string {
	return []string{SkillSetupPlatformOAuthCallbackURL}
}

// ValidSkillSetupPlatformValue reports whether name is a known platform value.
func ValidSkillSetupPlatformValue(name string) bool {
	for _, candidate := range SkillSetupPlatformValues() {
		if candidate == name {
			return true
		}
	}
	return false
}

// ValidSkillConfigFieldType reports whether value is a declared field type.
func ValidSkillConfigFieldType(value SkillConfigFieldType) bool {
	switch value {
	case SkillConfigFieldString, SkillConfigFieldEnum, SkillConfigFieldBool, SkillConfigFieldInt, SkillConfigFieldSecret, SkillConfigFieldURL:
		return true
	}
	return false
}

// ValidateSkillConfigValue checks one value against a field declaration: its
// type, enum, min/max and pattern. Secrets must be non-empty strings. It does
// not evaluate required or requiredWhen, which depend on the other values.
func ValidateSkillConfigValue(field SkillConfigField, value interface{}) error {
	if value == nil {
		return fmt.Errorf("config field %s value is null", field.Key)
	}
	switch field.Type {
	case SkillConfigFieldString:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("config field %s must be a string", field.Key)
		}
		if field.Pattern != "" {
			pattern, err := regexp.Compile(field.Pattern)
			if err != nil {
				return fmt.Errorf("config field %s pattern is invalid: %w", field.Key, err)
			}
			if !pattern.MatchString(text) {
				return fmt.Errorf("config field %s does not match its pattern", field.Key)
			}
		}
	case SkillConfigFieldEnum:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("config field %s must be a string", field.Key)
		}
		for _, option := range field.Enum {
			if option == text {
				return nil
			}
		}
		return fmt.Errorf("config field %s value %q is not one of its enum values", field.Key, text)
	case SkillConfigFieldBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("config field %s must be a boolean", field.Key)
		}
	case SkillConfigFieldInt:
		number, ok := skillConfigInteger(value)
		if !ok {
			return fmt.Errorf("config field %s must be an integer", field.Key)
		}
		if field.Min != nil && number < *field.Min {
			return fmt.Errorf("config field %s must be at least %d", field.Key, *field.Min)
		}
		if field.Max != nil && number > *field.Max {
			return fmt.Errorf("config field %s must be at most %d", field.Key, *field.Max)
		}
	case SkillConfigFieldSecret:
		text, ok := value.(string)
		if !ok || text == "" {
			return fmt.Errorf("config field %s must be a non-empty string", field.Key)
		}
	case SkillConfigFieldURL:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("config field %s must be a URL string", field.Key)
		}
		parsed, err := url.Parse(text)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return fmt.Errorf("config field %s must be an absolute http(s) URL", field.Key)
		}
	default:
		return fmt.Errorf("config field %s has unsupported type %q", field.Key, field.Type)
	}
	return nil
}

func skillConfigInteger(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || math.Abs(number) > 1<<53 {
			return 0, false
		}
		return int64(number), true
	}
	return 0, false
}

// SkillConfigValuesEqual compares two values of the same field after numeric
// normalization, so a JSON-decoded 3.0 equals a Go int 3.
func SkillConfigValuesEqual(field SkillConfigField, left, right interface{}) bool {
	if field.Type == SkillConfigFieldInt {
		a, aok := skillConfigInteger(left)
		b, bok := skillConfigInteger(right)
		return aok && bok && a == b
	}
	return reflect.DeepEqual(left, right)
}
