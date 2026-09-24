package runbook

import (
	"bytes"
	"encoding/json"
	"errors"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateInterfaceInput compiles the portable JSON Schema and validates one
// credential-free invocation. External references are deliberately rejected
// so an immutable Agent definition is self-contained.
func ValidateInterfaceInput(schema map[string]interface{}, input map[string]interface{}) error {
	compiled, err := compileInterfaceSchema(schema)
	if err != nil {
		return err
	}
	if input == nil {
		input = map[string]interface{}{}
	}
	return compiled.Validate(input)
}

func compileInterfaceSchema(schema map[string]interface{}) (*jsonschema.Schema, error) {
	if len(schema) == 0 {
		return nil, errors.New("runbook interface schema is required")
	}
	if err := rejectExternalSchemaReferences(schema); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	// Absolute URL: a bare name resolves against the working directory and
	// breaks when that path contains a space. See pkg/authoring/intent_schema.go.
	const resource = "https://openseal.dev/schemas/runbook-interface.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

func rejectExternalSchemaReferences(value interface{}) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if key == "$ref" {
				reference, ok := child.(string)
				if !ok || len(reference) == 0 || reference[0] != '#' {
					return errors.New("external JSON Schema references are not allowed")
				}
			}
			if err := rejectExternalSchemaReferences(child); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range typed {
			if err := rejectExternalSchemaReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}
