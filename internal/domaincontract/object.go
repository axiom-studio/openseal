package domaincontract

import (
	"encoding/json"
	"fmt"
)

// ObjectVariant describes one valid JSON object shape. Match contains exact
// string discriminator values. Required and Forbidden express field presence;
// item bounds apply to array fields. The same descriptor is consumed by schema
// projection and runtime validation.
type ObjectVariant struct {
	Name      string
	Match     map[string]string
	Required  []string
	Forbidden []string
	MinItems  map[string]uint64
	MaxItems  map[string]uint64
}

// ObjectVariants is implemented by canonical object types with mutually
// exclusive JSON shapes.
type ObjectVariants interface {
	ContractObjectVariants() []ObjectVariant
}

// ValidateObject verifies a canonical value using its JSON representation, so
// runtime presence semantics are identical to the generated JSON Schema.
func ValidateObject(value ObjectVariants) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode contract object: %w", err)
	}
	var object map[string]interface{}
	if err := json.Unmarshal(payload, &object); err != nil {
		return fmt.Errorf("decode contract object: %w", err)
	}
	matches := 0
	for _, variant := range value.ContractObjectVariants() {
		if objectMatchesVariant(object, variant) {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("object must match exactly one canonical variant; matched %d", matches)
	}
	return nil
}

func objectMatchesVariant(object map[string]interface{}, variant ObjectVariant) bool {
	for field, expected := range variant.Match {
		actual, ok := object[field].(string)
		if !ok || actual != expected {
			return false
		}
	}
	for _, field := range variant.Required {
		if _, ok := object[field]; !ok {
			return false
		}
	}
	for _, field := range variant.Forbidden {
		if _, ok := object[field]; ok {
			return false
		}
	}
	for field, minimum := range variant.MinItems {
		values, ok := object[field].([]interface{})
		if !ok || uint64(len(values)) < minimum {
			return false
		}
	}
	for field, maximum := range variant.MaxItems {
		values, ok := object[field].([]interface{})
		if !ok || uint64(len(values)) > maximum {
			return false
		}
	}
	return true
}
