package authoring

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	inferschema "github.com/invopop/jsonschema"
)

var (
	authoringIntentSchemaOnce sync.Once
	authoringIntentSchemaDoc  map[string]interface{}
	authoringIntentSchemaErr  error
)

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
	})
}
