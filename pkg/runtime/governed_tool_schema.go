package runtime

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
)

// GovernedReviewSummaryField is the one-sentence review summary a write
// governed tool carries. It is never passed to the tool.
const GovernedReviewSummaryField = "_reviewSummary"

const (
	maxGovernedReviewSummaryRunes   = 600
	maxExternalOperationResource    = 2048
	maxExternalOperationOperation   = 256
	governedEvidenceRefsDescription = "An evidenceRef from an earlier tool result, e.g. action-call:<id>."
	governedReviewSummaryText       = "One sentence a reviewer reads on the approval card: what this does and to what. Never passed to the tool."
)

// GovernedToolReviewed reports whether a governed call carries the review
// envelope {arguments, reviewContext, externalOperation?, evidenceRefs?}.
func GovernedToolReviewed(action capability.ModelAction) bool {
	return action.SideEffect == capability.SideEffectExternal || action.SideEffect == capability.SideEffectDestructive
}

// GovernedToolNeedsReviewSummary reports whether a governed call carries a
// required _reviewSummary next to its arguments: writes and non-read-risk
// actions whose input schema can be extended safely.
func GovernedToolNeedsReviewSummary(action capability.ModelAction) bool {
	if GovernedToolReviewed(action) {
		return false
	}
	if action.Risk == capability.RiskLevelRead &&
		(action.SideEffect == capability.SideEffectNone || action.SideEffect == capability.SideEffectRead) {
		return false
	}
	return governedSummaryExtendable(governedArgumentsSchema(action.InputSchema))
}

// governedExternalOperationOffered reports whether the reviewed envelope
// offers externalOperation.
func governedExternalOperationOffered(action capability.ModelAction) bool {
	return action.SideEffect == capability.SideEffectExternal && action.ExternalOperationPolicy != capability.ExternalOperationForbidden
}

func governedExternalOperationRequired(action capability.ModelAction) bool {
	return governedExternalOperationOffered(action) && action.ExternalOperationPolicy == capability.ExternalOperationRequired
}

// GovernedToolInputSchema is the input schema the step host offers for a
// governed action, before provider dialect lowering (atlas keeps lowering).
func GovernedToolInputSchema(action capability.ModelAction) map[string]interface{} {
	arguments := governedArgumentsSchema(action.InputSchema)
	switch {
	case GovernedToolReviewed(action):
		return governedReviewedEnvelopeSchema(action, arguments)
	case GovernedToolNeedsReviewSummary(action):
		return governedSchemaWithReviewSummary(arguments)
	default:
		return arguments
	}
}

// governedArgumentsSchema is a deep copy of the action's input schema
// without the host-owned idempotencyKey property. An empty schema is an
// open object.
func governedArgumentsSchema(input map[string]interface{}) map[string]interface{} {
	if len(input) == 0 {
		return map[string]interface{}{"type": "object"}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return map[string]interface{}{"type": "object"}
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(encoded, &schema); err != nil || schema == nil {
		return map[string]interface{}{"type": "object"}
	}
	if properties, ok := schema["properties"].(map[string]interface{}); ok {
		delete(properties, "idempotencyKey")
	}
	if required, ok := schema["required"].([]interface{}); ok {
		filtered := make([]interface{}, 0, len(required))
		for _, name := range required {
			if text, ok := name.(string); !ok || text != "idempotencyKey" {
				filtered = append(filtered, name)
			}
		}
		schema["required"] = filtered
	}
	return schema
}

func governedSchemaWithReviewSummary(arguments map[string]interface{}) map[string]interface{} {
	schema := make(map[string]interface{}, len(arguments)+1)
	for key, value := range arguments {
		schema[key] = value
	}
	properties := map[string]interface{}{}
	if original, ok := arguments["properties"].(map[string]interface{}); ok {
		for key, value := range original {
			properties[key] = value
		}
	}
	properties[GovernedReviewSummaryField] = map[string]interface{}{
		"type": "string", "minLength": 1, "maxLength": maxGovernedReviewSummaryRunes,
		"description": governedReviewSummaryText,
	}
	schema["properties"] = properties
	required := []interface{}{}
	if original, ok := arguments["required"].([]interface{}); ok {
		required = append(required, original...)
	}
	schema["required"] = append(required, GovernedReviewSummaryField)
	return schema
}

func governedReviewedEnvelopeSchema(action capability.ModelAction, arguments map[string]interface{}) map[string]interface{} {
	properties := map[string]interface{}{
		"arguments":     arguments,
		"reviewContext": runtimeaction.ReviewedActionReviewContextJSONSchema(),
		"evidenceRefs": map[string]interface{}{"type": "array", "items": map[string]interface{}{
			"type": "string", "description": governedEvidenceRefsDescription,
		}},
	}
	required := []string{"arguments", "reviewContext"}
	if governedExternalOperationOffered(action) {
		text := func(maxLength int, description string) map[string]interface{} {
			return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxLength, "description": description}
		}
		resource := text(maxExternalOperationResource, "Copy the actual stable destination or affected resource in its canonical format: HTTP(S) URL, single email address, channel handle, or opaque resource ID. An email target must be copied exactly as a bare address or mailto: address without display names or headers; email: and other invented semantic address prefixes are invalid. An API endpoint path such as /emails is not the recipient or affected resource. Preserve handles and opaque IDs instead of adding a URI scheme. At most 2048 UTF-8 bytes, including a canonical mailto: prefix for email targets; omit transient session IDs.")
		operation := text(maxExternalOperationOperation, "A non-blank, stable identifier for the semantic effect, at most 256 UTF-8 bytes. Keep the destination only in resource. Do not include URLs, email addresses, line breaks, or any of ?, #, @, /, or backslash in this identifier.")
		operation["pattern"] = `^[^\r\n?#@/\\]+$`
		properties["externalOperation"] = map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"resource": resource, "operation": operation},
			"required":   []string{"resource", "operation"},
		}
		if governedExternalOperationRequired(action) {
			required = append(required, "externalOperation")
		}
	}
	result := map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	// The domain schema moves under properties.arguments; local document
	// pointers must still reach it rather than the review envelope.
	governedRebaseSchemaReferences(arguments, "#/properties/arguments", governedSchemaDialect(arguments))
	if dialect, ok := arguments["$schema"]; ok {
		result["$schema"] = dialect
	}
	return result
}

// governedSummaryExtendable reports whether _reviewSummary can be added to
// schema without changing its meaning: a closed object with plain keywords,
// no collision with the field and no reference to the document root.
func governedSummaryExtendable(schema map[string]interface{}) bool {
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		return false
	}
	for keyword := range schema {
		switch keyword {
		case "type", "properties", "required", "additionalProperties", "$defs", "definitions", "$schema", "$comment", "title", "description", "default", "examples", "deprecated", "readOnly", "writeOnly":
		default:
			return false
		}
	}
	properties, ok := schema["properties"].(map[string]interface{})
	if _, present := schema["properties"]; present && !ok {
		return false
	}
	if _, collision := properties[GovernedReviewSummaryField]; collision {
		return false
	}
	switch required := schema["required"].(type) {
	case nil:
	case []string:
		for _, name := range required {
			if name == GovernedReviewSummaryField {
				return false
			}
		}
	case []interface{}:
		for _, value := range required {
			name, valid := value.(string)
			if !valid || name == GovernedReviewSummaryField {
				return false
			}
		}
	default:
		return false
	}
	return !governedSchemaReferencesDocumentRoot(schema, governedSchemaDialect(schema))
}

func governedSchemaReferencesDocumentRoot(schema map[string]interface{}, dialect int) bool {
	if governedSchemaStartsResource(schema, dialect) {
		return false
	}
	for _, keyword := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
		reference, _ := schema[keyword].(string)
		if strings.HasPrefix(reference, "#") {
			fragment, err := url.PathUnescape(reference[1:])
			if err == nil && fragment == "" {
				return true
			}
		}
	}
	found := false
	governedVisitChildSchemas(schema, func(child map[string]interface{}) {
		if !found {
			found = governedSchemaReferencesDocumentRoot(child, dialect)
		}
	})
	return found
}

func governedRebaseSchemaReferences(schema map[string]interface{}, prefix string, dialect int) {
	// An embedded resource keeps its own URI and document-root pointers.
	if governedSchemaStartsResource(schema, dialect) {
		return
	}
	for _, keyword := range []string{"$ref", "$dynamicRef"} {
		reference, _ := schema[keyword].(string)
		if !strings.HasPrefix(reference, "#") {
			continue
		}
		fragment, err := url.PathUnescape(reference[1:])
		if err == nil && (fragment == "" || strings.HasPrefix(fragment, "/")) {
			schema[keyword] = prefix + reference[1:]
		}
	}
	governedVisitChildSchemas(schema, func(child map[string]interface{}) {
		governedRebaseSchemaReferences(child, prefix, dialect)
	})
}

// governedVisitChildSchemas visits only JSON Schema locations: default,
// examples and const are user data even when they contain a $ref key.
func governedVisitChildSchemas(schema map[string]interface{}, visit func(map[string]interface{})) {
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"} {
		children, _ := schema[keyword].(map[string]interface{})
		for _, child := range children {
			if nested, ok := child.(map[string]interface{}); ok {
				visit(nested)
			}
		}
	}
	for _, keyword := range []string{"items", "additionalItems", "additionalProperties", "unevaluatedItems", "unevaluatedProperties", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema", "allOf", "anyOf", "oneOf", "prefixItems"} {
		switch child := schema[keyword].(type) {
		case map[string]interface{}:
			visit(child)
		case []interface{}:
			for _, item := range child {
				if nested, ok := item.(map[string]interface{}); ok {
					visit(nested)
				}
			}
		}
	}
}

// governedSchemaDialect is the schema's declared draft; subschemas inherit
// it. Before draft 2019 a $ref object ignores its siblings.
func governedSchemaDialect(schema map[string]interface{}) int {
	dialect, _ := schema["$schema"].(string)
	dialect = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSuffix(dialect, "#"), "http://"), "https://")
	switch dialect {
	case "json-schema.org/draft-04/schema":
		return 4
	case "json-schema.org/draft-06/schema":
		return 6
	case "json-schema.org/draft-07/schema":
		return 7
	case "json-schema.org/draft/2019-09/schema":
		return 2019
	default:
		return 2020
	}
}

func governedSchemaResourceID(schema map[string]interface{}, dialect int) string {
	if dialect < 2019 {
		if _, reference := schema["$ref"]; reference {
			return ""
		}
	}
	identifier, _ := schema["$id"].(string)
	if dialect == 4 {
		identifier, _ = schema["id"].(string)
	}
	// Fragment-only IDs name an anchor in the existing document.
	return strings.SplitN(identifier, "#", 2)[0]
}

func governedSchemaStartsResource(schema map[string]interface{}, dialect int) bool {
	// A nested dialect declaration is effective only if it defines a resource.
	if _, declared := schema["$schema"].(string); declared && governedSchemaResourceID(schema, governedSchemaDialect(schema)) != "" {
		return true
	}
	return governedSchemaResourceID(schema, dialect) != ""
}
