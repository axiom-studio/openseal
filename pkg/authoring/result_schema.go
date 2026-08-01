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
		constrainAuthoringRefinementVocabulary(authoringResultSchemaDoc)
		constrainAuthoringRunbookPointers(authoringResultSchemaDoc)
		compiler := validateschema.NewCompiler()
		if err := compiler.AddResource(authoringResultSchemaResource, authoringResultSchemaDoc); err != nil {
			authoringResultSchemaErr = fmt.Errorf("register authoring result schema: %w", err)
			return
		}
		authoringResultValidator, authoringResultSchemaErr = compiler.Compile(authoringResultSchemaResource)
	})
}

// constrainAuthoringRunbookPointers projects the portable Runbook validator's
// JSON Pointer invariant into the provider-facing schema. Runtime validation
// remains authoritative; this earlier boundary gives structured-output repair
// an exact field path before an invalid Runbook can become a review task.
func constrainAuthoringRunbookPointers(value interface{}) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if properties, ok := typed["properties"].(map[string]interface{}); ok {
			if _, hasResultPath := properties["resultPath"]; hasResultPath {
				if _, hasNext := properties["next"]; hasNext {
					properties["resultPath"] = map[string]interface{}{
						"type": "string", "minLength": 1, "pattern": "^/",
						"description": "Non-empty JSON Pointer where this step stores its durable result.",
					}
				}
			}
		}
		for _, child := range typed {
			constrainAuthoringRunbookPointers(child)
		}
	case []interface{}:
		for _, child := range typed {
			constrainAuthoringRunbookPointers(child)
		}
	}
}

// constrainAuthoringRefinementVocabulary adds the finite domain vocabulary
// that Go's string aliases cannot communicate to the schema reflector. The
// surrounding object shape and requiredness still come exclusively from the Go
// domain types; OpenSeal supplies only the enum values enforced by the same
// semantic validator.
func constrainAuthoringRefinementVocabulary(value interface{}) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if properties, ok := typed["properties"].(map[string]interface{}); ok {
			if _, hasCategory := properties["category"]; hasCategory {
				if _, hasBlocking := properties["blocking"]; hasBlocking {
					if _, hasAnswer := properties["answer"]; hasAnswer {
						properties["category"] = map[string]interface{}{
							"type": "string", "enum": []interface{}{
								string(RefinementCategoryCredential), string(RefinementCategorySkill), string(RefinementCategoryScope),
								string(RefinementCategoryPolicy), string(RefinementCategoryAuthority), string(RefinementCategoryDestination),
								string(RefinementCategoryBudget), string(RefinementCategoryApproval), string(RefinementCategoryOther),
							},
						}
						properties["blocking"] = map[string]interface{}{
							"type": "array", "minItems": 1,
							"items": map[string]interface{}{
								"type": "string", "enum": []interface{}{
									string(RefinementBlocksCandidate), string(RefinementBlocksEvaluation), string(RefinementBlocksApply),
								},
							},
						}
					}
				}
			}
			if _, hasOptions := properties["options"]; hasOptions {
				if _, hasMinimum := properties["minimum"]; hasMinimum {
					if _, hasKind := properties["kind"]; hasKind {
						properties["kind"] = map[string]interface{}{
							"type": "string", "enum": []interface{}{
								string(RefinementAnswerText), string(RefinementAnswerStringList), string(RefinementAnswerSingleSelect),
								string(RefinementAnswerMultiSelect), string(RefinementAnswerBoolean),
								string(RefinementAnswerCredentialReference), string(RefinementAnswerSkillSelection),
							},
						}
					}
				}
			}
		}
		for _, child := range typed {
			constrainAuthoringRefinementVocabulary(child)
		}
	case []interface{}:
		for _, child := range typed {
			constrainAuthoringRefinementVocabulary(child)
		}
	}
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
