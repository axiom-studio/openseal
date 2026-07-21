package capability

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

const (
	MaximumBindingConfigurationFields  = 128
	MaximumBindingConfigurationOptions = 256
	MaximumBindingConfigurationText    = 1024
)

// BindingConfigurationValue is the closed set of scalar values that a host
// may advertise for non-secret Skill binding configuration. Pointers preserve
// false, zero, and the empty string as intentional values. Exactly one member
// must be set.
type BindingConfigurationValue struct {
	String  *string  `json:"string,omitempty"`
	Integer *int64   `json:"integer,omitempty"`
	Number  *float64 `json:"number,omitempty"`
	Boolean *bool    `json:"boolean,omitempty"`
}

func (v BindingConfigurationValue) Value() (interface{}, string, error) {
	count := 0
	var value interface{}
	kind := ""
	if v.String != nil {
		count, value, kind = count+1, *v.String, "string"
	}
	if v.Integer != nil {
		count, value, kind = count+1, *v.Integer, "integer"
	}
	if v.Number != nil {
		count, value, kind = count+1, *v.Number, "number"
		if math.IsNaN(*v.Number) || math.IsInf(*v.Number, 0) {
			return nil, "", errors.New("binding configuration number must be finite")
		}
	}
	if v.Boolean != nil {
		count, value, kind = count+1, *v.Boolean, "boolean"
	}
	if count != 1 {
		return nil, "", errors.New("binding configuration value must set exactly one scalar member")
	}
	return value, kind, nil
}

// BindingConfigurationOption is one host-authorized, non-secret choice. The
// typed value is written to reviewed placement only after the operator selects
// it; it is never added to model-visible action inputs or credential context.
type BindingConfigurationOption struct {
	Label       string                    `json:"label"`
	Description string                    `json:"description,omitempty"`
	Value       BindingConfigurationValue `json:"value"`
}

// BindingConfigurationFieldChoice describes one dependency-satisfied question
// for a top-level property of a Skill's bindingConfigSchema. CatalogSkillID is
// the model-facing requirement id; Skill is the exact immutable runtime
// identity selected by the host.
type BindingConfigurationFieldChoice struct {
	CatalogSkillID string                       `json:"catalogSkillId"`
	Skill          SkillIdentity                `json:"skill"`
	Key            string                       `json:"key"`
	Type           string                       `json:"type"`
	Required       bool                         `json:"required,omitempty"`
	Prompt         string                       `json:"prompt"`
	Options        []BindingConfigurationOption `json:"options"`
}

// ValidateBindingConfigurationFields rejects ambiguous or unsafe capability
// advertisements before a prompt-first client consumes them.
func ValidateBindingConfigurationFields(fields []BindingConfigurationFieldChoice) error {
	if len(fields) > MaximumBindingConfigurationFields {
		return fmt.Errorf("binding configuration advertises more than %d fields", MaximumBindingConfigurationFields)
	}
	seenFields := make(map[string]bool, len(fields))
	for index, field := range fields {
		field.CatalogSkillID, field.Key, field.Type, field.Prompt = strings.TrimSpace(field.CatalogSkillID), strings.TrimSpace(field.Key), strings.TrimSpace(field.Type), strings.TrimSpace(field.Prompt)
		identity := field.Skill.Normalized()
		if field.CatalogSkillID == "" || !identity.Valid() || field.Skill != identity || field.Key == "" || field.Prompt == "" ||
			len(field.CatalogSkillID) > 256 || len(field.Key) > 256 || len(identity.ID) > 256 || len(identity.Version) > 128 || len(identity.SourceIdentity) > 2048 ||
			len(field.Prompt) > MaximumBindingConfigurationText || containsBindingConfigurationControl(field.CatalogSkillID+field.Key+identity.ID+identity.Version+identity.SourceIdentity) || strings.ContainsAny(field.Prompt, "\r\n\t") {
			return fmt.Errorf("binding configuration field %d has invalid identity, key, or prompt", index)
		}
		if secretLikeBindingConfigurationKey(field.Key) {
			return fmt.Errorf("binding configuration field %d uses secret-like key %s", index, field.Key)
		}
		switch field.Type {
		case "string", "integer", "number", "boolean":
		default:
			return fmt.Errorf("binding configuration field %d has unsupported type %s", index, field.Type)
		}
		fieldID := field.CatalogSkillID + "\x00" + identity.Key() + "\x00" + field.Key
		if seenFields[fieldID] {
			return fmt.Errorf("binding configuration field %d duplicates %s", index, field.Key)
		}
		seenFields[fieldID] = true
		if len(field.Options) == 0 || len(field.Options) > MaximumBindingConfigurationOptions {
			return fmt.Errorf("binding configuration field %d must advertise between 1 and %d options", index, MaximumBindingConfigurationOptions)
		}
		seenValues := make(map[string]bool, len(field.Options))
		for optionIndex, option := range field.Options {
			label, description := strings.TrimSpace(option.Label), strings.TrimSpace(option.Description)
			if label == "" || len(label) > MaximumBindingConfigurationText || len(description) > MaximumBindingConfigurationText || strings.ContainsAny(label+description, "\r\n\t") {
				return fmt.Errorf("binding configuration field %d option %d has invalid display text", index, optionIndex)
			}
			value, kind, err := option.Value.Value()
			if err != nil || kind != field.Type {
				return fmt.Errorf("binding configuration field %d option %d does not match type %s", index, optionIndex, field.Type)
			}
			if text, ok := value.(string); ok && (len(text) > MaximumBindingConfigurationText || containsBindingConfigurationControl(text)) {
				return fmt.Errorf("binding configuration field %d option %d has an invalid string value", index, optionIndex)
			}
			valueID := fmt.Sprintf("%s:%v", kind, value)
			if seenValues[valueID] {
				return fmt.Errorf("binding configuration field %d has duplicate option value", index)
			}
			seenValues[valueID] = true
		}
	}
	return nil
}

func containsBindingConfigurationControl(value string) bool {
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

// ValidateBindingConfigurationFieldSchema proves that a host-authored field is
// an exact projection of one top-level scalar property in the canonical Skill
// binding schema. The full selected object is still validated by the binding
// service before activation.
func ValidateBindingConfigurationFieldSchema(field BindingConfigurationFieldChoice, schema map[string]interface{}) error {
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return errors.New("binding configuration schema has no object properties")
	}
	property, ok := properties[field.Key].(map[string]interface{})
	if !ok {
		return fmt.Errorf("binding configuration schema does not declare field %s", field.Key)
	}
	typeName, _ := property["type"].(string)
	if typeName != field.Type {
		return fmt.Errorf("binding configuration field %s type does not match schema", field.Key)
	}
	required := false
	switch values := schema["required"].(type) {
	case []interface{}:
		for _, value := range values {
			if value == field.Key {
				required = true
				break
			}
		}
	case []string:
		for _, value := range values {
			if value == field.Key {
				required = true
				break
			}
		}
	}
	if required != field.Required {
		return fmt.Errorf("binding configuration field %s required state does not match schema", field.Key)
	}
	for index, option := range field.Options {
		value, _, err := option.Value.Value()
		if err != nil || !bindingConfigurationOptionMatchesProperty(value, property) {
			return fmt.Errorf("binding configuration field %s option %d does not satisfy schema", field.Key, index)
		}
	}
	return nil
}

func bindingConfigurationOptionMatchesProperty(value interface{}, property map[string]interface{}) bool {
	if constant, exists := property["const"]; exists && !equalBindingConfigurationValue(value, constant) {
		return false
	}
	if enum, exists := property["enum"].([]interface{}); exists {
		matched := false
		for _, candidate := range enum {
			if equalBindingConfigurationValue(value, candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if text, ok := value.(string); ok {
		if minimum, ok := schemaNumericValue(property["minLength"]); ok && float64(len([]rune(text))) < minimum {
			return false
		}
		if maximum, ok := schemaNumericValue(property["maxLength"]); ok && float64(len([]rune(text))) > maximum {
			return false
		}
		if pattern, ok := property["pattern"].(string); ok {
			compiled, err := regexp.Compile(pattern)
			if err != nil || !compiled.MatchString(text) {
				return false
			}
		}
	}
	if number, ok := schemaNumericValue(value); ok {
		if minimum, ok := schemaNumericValue(property["minimum"]); ok && number < minimum {
			return false
		}
		if maximum, ok := schemaNumericValue(property["maximum"]); ok && number > maximum {
			return false
		}
		if minimum, ok := schemaNumericValue(property["exclusiveMinimum"]); ok && number <= minimum {
			return false
		}
		if maximum, ok := schemaNumericValue(property["exclusiveMaximum"]); ok && number >= maximum {
			return false
		}
	}
	return true
}

func equalBindingConfigurationValue(left, right interface{}) bool {
	leftNumber, leftNumeric := schemaNumericValue(left)
	rightNumber, rightNumeric := schemaNumericValue(right)
	if leftNumeric || rightNumeric {
		return leftNumeric && rightNumeric && leftNumber == rightNumber
	}
	return fmt.Sprint(left) == fmt.Sprint(right) && fmt.Sprintf("%T", left) == fmt.Sprintf("%T", right)
}

func schemaNumericValue(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	default:
		return 0, false
	}
}

func secretLikeBindingConfigurationKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.TrimSpace(key)))
	for _, fragment := range []string{"password", "passwd", "secret", "token", "apikey", "privatekey", "credential"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}
