package skill

import (
	"bytes"
	"encoding/json"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

type compiledSchema struct{ schema *jsonschema.Schema }

type compiledActionSchemas struct {
	input  *compiledSchema
	output *compiledSchema
}

func compileActionSchemas(skillID, version string, action Action) (*compiledActionSchemas, error) {
	input, err := compileSchema(skillID+"-"+version+"-"+action.Name+"-input.json", action.InputSchema)
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

func compileSchema(name string, value map[string]interface{}) (*compiledSchema, error) {
	if err := validateSchemaReferences(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
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
	return &compiledSchema{schema: compiled}, nil
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
