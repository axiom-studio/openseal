package authoring

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"strings"
	"sync"

	inferschema "github.com/invopop/jsonschema"
	validateschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const authoringResultSchemaResource = "openseal-authoring-result-v1.json"

var (
	authoringResultSchemaOnce sync.Once
	authoringResultSchemaDoc  map[string]interface{}
	authoringResultValidator  *validateschema.Schema
	authoringResultSchemaErr  error
)

// AuthoringResultJSONSchema returns a detached copy of the canonical schema
// inferred from AuthoringResult. It is suitable for a provider tool's
// parameters and remains owned and enforced by OpenSeal.
func AuthoringResultJSONSchema() (map[string]interface{}, error) {
	initializeAuthoringResultSchema()
	if authoringResultSchemaErr != nil {
		return nil, authoringResultSchemaErr
	}
	payload, err := json.Marshal(authoringResultSchemaDoc)
	if err != nil {
		return nil, err
	}
	var detached map[string]interface{}
	if err := json.Unmarshal(payload, &detached); err != nil {
		return nil, err
	}
	return detached, nil
}

func initializeAuthoringResultSchema() {
	authoringResultSchemaOnce.Do(func() {
		inferred := (&inferschema.Reflector{
			Anonymous:      true,
			ExpandedStruct: true,
			Namer: func(value reflect.Type) string {
				if value.Name() == "" {
					return fmt.Sprintf("anonymous_%x", sha256.Sum256([]byte(value.String())))
				}
				return path.Base(value.PkgPath()) + "_" + value.Name()
			},
		}).Reflect(AuthoringResult{})
		payload, err := json.Marshal(inferred)
		if err != nil {
			authoringResultSchemaErr = fmt.Errorf("marshal authoring result schema: %w", err)
			return
		}
		if err := json.Unmarshal(payload, &authoringResultSchemaDoc); err != nil {
			authoringResultSchemaErr = fmt.Errorf("decode authoring result schema: %w", err)
			return
		}
		properties, ok := authoringResultSchemaDoc["properties"].(map[string]interface{})
		if !ok {
			authoringResultSchemaErr = errors.New("authoring result schema has no object properties")
			return
		}
		properties["schemaVersion"] = map[string]interface{}{
			"type": "string", "const": AuthoringResultSchemaVersion,
			"description": "Version of the OpenSeal authoring-result contract used by this proposal.",
		}
		compiler := validateschema.NewCompiler()
		if err := compiler.AddResource(authoringResultSchemaResource, authoringResultSchemaDoc); err != nil {
			authoringResultSchemaErr = fmt.Errorf("register authoring result schema: %w", err)
			return
		}
		authoringResultValidator, authoringResultSchemaErr = compiler.Compile(authoringResultSchemaResource)
	})
}

type AuthoringSchemaViolation struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type AuthoringSchemaValidationError struct {
	Violations []AuthoringSchemaViolation `json:"violations"`
}

func (e *AuthoringSchemaValidationError) Error() string {
	if e == nil || len(e.Violations) == 0 {
		return "authoring result does not satisfy its schema"
	}
	payload, _ := json.Marshal(e.Violations)
	return "authoring result schema violations: " + string(payload)
}

func validateAuthoringResultDocument(payload []byte) error {
	initializeAuthoringResultSchema()
	if authoringResultSchemaErr != nil {
		return authoringResultSchemaErr
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return &AuthoringSchemaValidationError{Violations: []AuthoringSchemaViolation{{Path: "", Message: err.Error()}}}
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err == nil {
		return &AuthoringSchemaValidationError{Violations: []AuthoringSchemaViolation{{Path: "", Message: "must contain exactly one JSON value"}}}
	} else if !errors.Is(err, io.EOF) {
		return &AuthoringSchemaValidationError{Violations: []AuthoringSchemaViolation{{Path: "", Message: err.Error()}}}
	}
	if err := authoringResultValidator.Validate(document); err != nil {
		var validation *validateschema.ValidationError
		if errors.As(err, &validation) {
			violations := flattenAuthoringSchemaViolations(validation, nil)
			return &AuthoringSchemaValidationError{Violations: violations}
		}
		return err
	}
	return nil
}

func flattenAuthoringSchemaViolations(value *validateschema.ValidationError, into []AuthoringSchemaViolation) []AuthoringSchemaViolation {
	if len(value.Causes) == 0 {
		into = append(into, AuthoringSchemaViolation{Path: "/" + strings.Join(value.InstanceLocation, "/"), Message: value.Error()})
		return into
	}
	for _, cause := range value.Causes {
		into = flattenAuthoringSchemaViolations(cause, into)
	}
	return into
}
