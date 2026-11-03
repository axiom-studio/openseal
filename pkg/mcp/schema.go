package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const maxSchemaDepth = 64

// Digest is the stable SHA-256 of v's JSON encoding. Maps encode with sorted
// keys, so a tool definition hashes the same however its server orders it.
func Digest(v interface{}) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type schema struct{ compiled *jsonschema.Schema }

// compileSchema supports a reference-free-across-documents subset of JSON
// Schema (draft-07 or 2020-12, default 2020-12). It never loads a URL, and it
// rejects keywords it does not list instead of silently ignoring assertions.
func compileSchema(value map[string]interface{}) (*schema, error) {
	if value == nil {
		return nil, errors.New("schema required")
	}
	if err := checkSchema(value, 0); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaxBytes {
		return nil, errors.New("schema is not JSON or exceeds 2 MiB")
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, errors.New("schema is not JSON")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err := compiler.AddResource("mcp-tool-schema.json", document); err != nil {
		return nil, errors.New("invalid JSON Schema")
	}
	compiled, err := compiler.Compile("mcp-tool-schema.json")
	if err != nil {
		return nil, errors.New("invalid JSON Schema")
	}
	return &schema{compiled: compiled}, nil
}

func (s *schema) validate(value interface{}) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return errors.New("value is not JSON")
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil || s.compiled.Validate(document) != nil {
		return errors.New("value does not match pinned schema")
	}
	return nil
}

func checkSchema(s map[string]interface{}, depth int) error {
	if depth > maxSchemaDepth {
		return errors.New("schema nesting exceeds limit")
	}
	for k, v := range s {
		switch k {
		case "$schema":
			if v != "http://json-schema.org/draft-07/schema#" && v != "https://json-schema.org/draft/2020-12/schema" {
				return errors.New("unsupported schema dialect")
			}
		case "$ref":
			// Only references inside this document; never a URL.
			if ref, ok := v.(string); !ok || !strings.HasPrefix(ref, "#") {
				return errors.New("external schema references are not allowed")
			}
		case "x-mcp-header":
			if name, ok := v.(string); !ok || !headerName.MatchString(name) {
				return errors.New("invalid MCP parameter header")
			}
		case "type", "title", "description", "default", "examples", "enum", "const", "required", "minimum", "maximum",
			"exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "pattern", "minItems", "maxItems",
			"uniqueItems", "minProperties", "maxProperties", "format", "$comment", "readOnly", "writeOnly", "deprecated",
			"contentEncoding", "contentMediaType":
		case "properties", "patternProperties", "$defs", "definitions":
			m, ok := v.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid schema %s", k)
			}
			for _, sub := range m {
				if err := checkSubschema(sub, depth); err != nil {
					return err
				}
			}
		case "items", "additionalProperties", "not", "contains", "if", "then", "else":
			if err := checkSubschema(v, depth); err != nil {
				return err
			}
		case "allOf", "anyOf", "oneOf":
			a, ok := v.([]interface{})
			if !ok {
				return errors.New("invalid schema combinator")
			}
			for _, sub := range a {
				if err := checkSubschema(sub, depth); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported schema keyword %q", k)
		}
	}
	return nil
}

func checkSubschema(v interface{}, depth int) error {
	if _, ok := v.(bool); ok {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return errors.New("invalid subschema")
	}
	return checkSchema(m, depth+1)
}
