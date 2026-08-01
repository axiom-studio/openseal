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

	"github.com/axiom-studio/openseal/internal/domaincontract"
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
			Mapper:         canonicalAuthoringScalarSchema,
			Namer:          canonicalAuthoringTypeName,
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
		if err := applyAuthoringObjectContracts(authoringResultSchemaDoc, reflect.TypeOf(AuthoringResult{})); err != nil {
			authoringResultSchemaErr = fmt.Errorf("project authoring object contracts: %w", err)
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

func canonicalAuthoringTypeName(value reflect.Type) string {
	if value.Name() == "" {
		return fmt.Sprintf("anonymous_%x", sha256.Sum256([]byte(value.String())))
	}
	return path.Base(value.PkgPath()) + "_" + value.Name()
}

// applyAuthoringObjectContracts discovers canonical object contracts from the
// Go type graph and augments their exact reflected definitions. It does not
// know concrete types or infer contracts from coincidental field names.
func applyAuthoringObjectContracts(schema map[string]interface{}, root reflect.Type) error {
	definitions, _ := schema["$defs"].(map[string]interface{})
	contracts := map[string]domaincontract.ObjectVariants{}
	collectAuthoringObjectContracts(root, map[reflect.Type]bool{}, contracts)
	for name, contract := range contracts {
		definition, ok := definitions[name].(map[string]interface{})
		if !ok {
			return fmt.Errorf("canonical definition %s is unavailable", name)
		}
		variants := contract.ContractObjectVariants()
		if len(variants) == 0 {
			return fmt.Errorf("canonical definition %s has no object variants", name)
		}
		oneOf := make([]interface{}, 0, len(variants))
		for _, variant := range variants {
			properties := map[string]interface{}{}
			for field, expected := range variant.Match {
				properties[field] = map[string]interface{}{"const": expected}
			}
			for _, field := range variant.Forbidden {
				properties[field] = false
			}
			for field, minimum := range variant.MinItems {
				properties[field] = mergeAuthoringPropertyConstraint(properties[field], "minItems", minimum)
			}
			for field, maximum := range variant.MaxItems {
				properties[field] = mergeAuthoringPropertyConstraint(properties[field], "maxItems", maximum)
			}
			required := make([]interface{}, 0, len(variant.Match)+len(variant.Required))
			for field := range variant.Match {
				required = append(required, field)
			}
			for _, field := range variant.Required {
				required = append(required, field)
			}
			oneOf = append(oneOf, map[string]interface{}{"title": variant.Name, "properties": properties, "required": required})
		}
		definition["oneOf"] = oneOf
	}
	return nil
}

func mergeAuthoringPropertyConstraint(existing interface{}, keyword string, value uint64) map[string]interface{} {
	constraint, _ := existing.(map[string]interface{})
	if constraint == nil {
		constraint = map[string]interface{}{}
	}
	constraint[keyword] = value
	return constraint
}

func collectAuthoringObjectContracts(value reflect.Type, visited map[reflect.Type]bool, contracts map[string]domaincontract.ObjectVariants) {
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
		value = value.Elem()
	}
	if visited[value] {
		return
	}
	visited[value] = true
	if value.Kind() == reflect.Struct {
		instance := reflect.New(value).Elem().Interface()
		if contract, ok := instance.(domaincontract.ObjectVariants); ok {
			contracts[canonicalAuthoringTypeName(value)] = contract
		}
		for index := 0; index < value.NumField(); index++ {
			collectAuthoringObjectContracts(value.Field(index).Type, visited, contracts)
		}
		return
	}
	if value.Kind() == reflect.Map {
		collectAuthoringObjectContracts(value.Elem(), visited, contracts)
	}
}

// canonicalAuthoringScalarSchema projects provider-neutral contracts declared
// by canonical domain types. It deliberately never infers semantics from
// field names or object shapes and has no knowledge of concrete domain types.
func canonicalAuthoringScalarSchema(value reflect.Type) *inferschema.Schema {
	if value.Kind() != reflect.String {
		return nil
	}

	contract := reflect.New(value).Elem().Interface()
	var schema *inferschema.Schema
	if vocabulary, ok := contract.(domaincontract.StringVocabulary); ok {
		values := vocabulary.ContractValues()
		items := make([]interface{}, len(values))
		for index, value := range values {
			items[index] = value
		}
		schema = &inferschema.Schema{Type: "string", Enum: items}
	}
	if format, ok := contract.(domaincontract.StringFormat); ok {
		if schema == nil {
			schema = &inferschema.Schema{Type: "string"}
		}
		schema.Pattern = format.ContractPattern()
		if minimum := format.ContractMinLength(); minimum > 0 {
			schema.MinLength = &minimum
		}
	}
	return schema
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
