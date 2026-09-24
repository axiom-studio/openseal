package authoring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	inferschema "github.com/invopop/jsonschema"
	validateschema "github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	authoringIntentSchemaOnce sync.Once
	authoringIntentSchemaDoc  map[string]interface{}
	authoringIntentValidator  *validateschema.Schema
	authoringIntentSchemaErr  error
)

// Schema resources carry absolute, non-file URLs. The validator resolves a
// bare name against the process working directory as a file:// URL, and a
// directory with a space in its name (the desktop daemon runs from
// ~/Library/Application Support/...) is percent-encoded on lookup but not on
// registration, so the in-memory document is never found and the compiler
// tries to read it from disk. See TestAuthoringSchemasCompileFromCwdWithSpace.
const authoringIntentSchemaResource = "https://openseal.dev/schemas/openseal-authoring-intent-v1.json"

// AuthoringIntentJSONSchema is the only schema sent to an authoring model.
// Canonical runtime and resource types must never become reachable from it.
func AuthoringIntentJSONSchema() (map[string]interface{}, error) {
	initializeAuthoringIntentSchema()
	if authoringIntentSchemaErr != nil {
		return nil, authoringIntentSchemaErr
	}
	payload, err := json.Marshal(authoringIntentSchemaDoc)
	if err != nil {
		return nil, err
	}
	var detached map[string]interface{}
	if err := json.Unmarshal(payload, &detached); err != nil {
		return nil, err
	}
	return detached, nil
}

func initializeAuthoringIntentSchema() {
	authoringIntentSchemaOnce.Do(func() {
		inferred := (&inferschema.Reflector{
			Anonymous:      true,
			ExpandedStruct: true,
			Mapper:         canonicalAuthoringScalarSchema,
			Namer:          canonicalAuthoringTypeName,
		}).Reflect(AuthoringIntent{})
		payload, err := json.Marshal(inferred)
		if err != nil {
			authoringIntentSchemaErr = fmt.Errorf("marshal authoring intent schema: %w", err)
			return
		}
		if err := json.Unmarshal(payload, &authoringIntentSchemaDoc); err != nil {
			authoringIntentSchemaErr = fmt.Errorf("decode authoring intent schema: %w", err)
			return
		}
		if err := applyAuthoringObjectContracts(authoringIntentSchemaDoc, reflect.TypeOf(AuthoringIntent{})); err != nil {
			authoringIntentSchemaErr = fmt.Errorf("project authoring intent contracts: %w", err)
			return
		}
		properties, ok := authoringIntentSchemaDoc["properties"].(map[string]interface{})
		if !ok {
			authoringIntentSchemaErr = errors.New("authoring intent schema has no object properties")
			return
		}
		properties["schemaVersion"] = map[string]interface{}{
			"type": "string", "const": AuthoringIntentSchemaVersion,
			"description": "Version of the semantic authoring answer contract.",
		}
		compiler := validateschema.NewCompiler()
		if err := compiler.AddResource(authoringIntentSchemaResource, authoringIntentSchemaDoc); err != nil {
			authoringIntentSchemaErr = fmt.Errorf("register authoring intent schema: %w", err)
			return
		}
		authoringIntentValidator, authoringIntentSchemaErr = compiler.Compile(authoringIntentSchemaResource)
	})
}

func decodeAuthoringIntent(payload []byte) (AuthoringIntent, error) {
	initializeAuthoringIntentSchema()
	if authoringIntentSchemaErr != nil {
		return AuthoringIntent{}, authoringIntentSchemaErr
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return AuthoringIntent{}, &AuthoringSchemaValidationError{Violations: []AuthoringSchemaViolation{{Path: "", Message: err.Error()}}}
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err == nil {
		return AuthoringIntent{}, &AuthoringSchemaValidationError{Violations: []AuthoringSchemaViolation{{Path: "", Message: "must contain exactly one JSON value"}}}
	} else if !errors.Is(err, io.EOF) {
		return AuthoringIntent{}, &AuthoringSchemaValidationError{Violations: []AuthoringSchemaViolation{{Path: "", Message: err.Error()}}}
	}
	// Some OpenAI-compatible gateways preserve a model's JSON-encoded array as
	// a string inside otherwise valid tool arguments. Unwrap only values that
	// decode to the exact composite type required by the contract; arbitrary
	// prose and scalar coercions remain schema violations.
	normalizeJSONEncodedAuthoringIntentArrays(document)
	if err := authoringIntentValidator.Validate(document); err != nil {
		var validation *validateschema.ValidationError
		if errors.As(err, &validation) {
			return AuthoringIntent{}, &AuthoringSchemaValidationError{Violations: flattenAuthoringSchemaViolations(validation, nil)}
		}
		return AuthoringIntent{}, err
	}
	normalizedPayload, err := json.Marshal(document)
	if err != nil {
		return AuthoringIntent{}, fmt.Errorf("encode normalized authoring intent: %w", err)
	}
	strict := json.NewDecoder(bytes.NewReader(normalizedPayload))
	strict.DisallowUnknownFields()
	var result AuthoringIntent
	if err := strict.Decode(&result); err != nil {
		return AuthoringIntent{}, err
	}
	return result, nil
}

func normalizeJSONEncodedAuthoringIntentArrays(document interface{}) {
	object, ok := document.(map[string]interface{})
	if !ok {
		return
	}
	for _, field := range []string{"agents", "conversations", "assumptions", "clarifications"} {
		encoded, ok := object[field].(string)
		if !ok || strings.TrimSpace(encoded) == "" {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		var decoded interface{}
		if err := decoder.Decode(&decoded); err != nil {
			continue
		}
		if _, ok := decoded.([]interface{}); !ok {
			continue
		}
		var trailing interface{}
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			continue
		}
		object[field] = decoded
	}
}
