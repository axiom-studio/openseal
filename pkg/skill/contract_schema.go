package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/fileinput"
	"github.com/axiom-studio/openseal/pkg/toolfile"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

type compiledSchema struct{ schema *jsonschema.Schema }

type compiledActionSchemas struct {
	input  *compiledSchema
	output *compiledSchema
}

func compileActionSchemas(skillID, version string, action Action) (*compiledActionSchemas, error) {
	input, err := compileSchema(skillID+"-"+version+"-"+action.Name+"-input.json", fileinput.Schema(action.InputSchema))
	if err != nil {
		return nil, err
	}
	var output *compiledSchema
	if action.OutputSchema != nil {
		output, err = compileSchema(skillID+"-"+version+"-"+action.Name+"-output.json", action.OutputSchema)
		if err != nil {
			return nil, err
		}
	}
	return &compiledActionSchemas{input: input, output: output}, nil
}

// The process-wide cache of compiled schemas is bounded by entry count and
// holds only schemas up to compiledSchemaCacheMaxBytes when encoded. Compiled
// schemas are immutable and keyed by their exact content.
const (
	compiledSchemaCacheEntries  = 2048
	compiledSchemaCacheMaxBytes = 16 << 10
)

var compiledSchemas = newLRUCache[*compiledSchema](compiledSchemaCacheEntries)

func compileSchema(name string, value map[string]interface{}) (*compiledSchema, error) {
	if err := validateSchemaReferences(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return compileSchemaJSON(name, encoded)
}

// compileSchemaJSON compiles the JSON encoding of a schema whose references
// were already checked by validateSchemaReferences. Equal schemas compile
// once per process: many bindings of one connected service share them.
func compileSchemaJSON(name string, encoded []byte) (*compiledSchema, error) {
	digest := sha256.Sum256(encoded)
	key := name + "\x00" + hex.EncodeToString(digest[:])
	if cached, ok := compiledSchemas.get(key); ok {
		return cached, nil
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(name, document); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(name)
	if err != nil {
		return nil, err
	}
	result := &compiledSchema{schema: compiled}
	if len(encoded) <= compiledSchemaCacheMaxBytes {
		compiledSchemas.put(key, result)
	}
	return result, nil
}

func validateSchemaReferences(value interface{}) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || len(ref) == 0 || ref[0] != '#' {
					return fmt.Errorf("external JSON Schema references are not allowed")
				}
			}
			if err := validateSchemaReferences(child); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range typed {
			if err := validateSchemaReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *compiledSchema) validate(value interface{}) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	if err := s.schema.Validate(document); err != nil {
		return fmt.Errorf("json schema validation failed: %w", err)
	}
	return nil
}

func valuesEqual(left, right interface{}) bool {
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return bytes.Equal(l, r)
}

func containsValue(values []interface{}, expected interface{}) bool {
	for _, value := range values {
		if valuesEqual(value, expected) {
			return true
		}
	}
	return false
}

func cloneMap(value map[string]interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	var result map[string]interface{}
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return result
}

func cloneValue(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func modelVisibleInputSchema(schema map[string]interface{}) map[string]interface{} {
	return modelVisibleInputSchemaForBinding(schema, nil)
}

func modelVisibleInputSchemaForBinding(schema map[string]interface{}, bindings map[string]BindingArgumentValue) map[string]interface{} {
	projected, _ := projectModelSchemaValue(cloneMap(fileinput.Schema(schema))).(map[string]interface{})
	projected = toolfile.ModelSchema(projected)
	if len(bindings) == 0 || projected == nil {
		return projected
	}
	properties, _ := projected["properties"].(map[string]interface{})
	for name := range bindings {
		delete(properties, name)
	}
	if required, ok := projected["required"].([]interface{}); ok {
		visible := make([]interface{}, 0, len(required))
		for _, value := range required {
			name, _ := value.(string)
			if _, hidden := bindings[name]; !hidden {
				visible = append(visible, value)
			}
		}
		projected["required"] = visible
	}
	return projected
}

func projectModelSchemaValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		hidden := make(map[string]bool)
		if properties, ok := typed["properties"].(map[string]interface{}); ok {
			for name, property := range properties {
				if definition, ok := property.(map[string]interface{}); ok {
					hidden[name], _ = definition[SchemaExtensionKernelResolved].(bool)
				}
			}
		}
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			if key == SchemaExtensionKernelResolved || key == SchemaExtensionKernelSource {
				continue
			}
			switch key {
			case "properties":
				properties, _ := child.(map[string]interface{})
				visible := make(map[string]interface{}, len(properties))
				for name, property := range properties {
					if !hidden[name] {
						visible[name] = projectModelSchemaValue(property)
					}
				}
				result[key] = visible
			case "required":
				required, _ := child.([]interface{})
				visible := make([]interface{}, 0, len(required))
				for _, name := range required {
					if text, ok := name.(string); !ok || !hidden[text] {
						visible = append(visible, name)
					}
				}
				result[key] = visible
			default:
				result[key] = projectModelSchemaValue(child)
			}
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			result[index] = projectModelSchemaValue(child)
		}
		return result
	default:
		return typed
	}
}

func cloneDefinition(value *Definition) *Definition {
	if value == nil {
		return nil
	}
	var result Definition
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneBinding(value *Binding) *Binding {
	if value == nil {
		return nil
	}
	var result Binding
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}
