package mcp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
)

// headerValue sends value verbatim when it is safe ASCII, otherwise as
// =?base64?...?= (2026-07-28 parameter and name headers).
func headerValue(value string) string {
	safe := strings.TrimSpace(value) == value && !(strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?="))
	for _, c := range value {
		if (c < 32 && c != '\t') || c > 126 {
			safe = false
		}
	}
	if safe {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

// Only x-mcp-header annotations reached through properties may become
// protocol headers; one under items, combinators or $defs is invalid.
func walkHeaders(schema map[string]interface{}, path []string, seen map[string]bool, visit func(string, []string, string) error) error {
	if annotation, exists := schema["x-mcp-header"]; exists {
		name, ok := annotation.(string)
		typ, _ := schema["type"].(string)
		if !ok || len(path) == 0 || !headerName.MatchString(name) || seen[strings.ToLower(name)] || (typ != "string" && typ != "integer" && typ != "boolean") {
			return errors.New("invalid MCP parameter header annotation")
		}
		seen[strings.ToLower(name)] = true
		if err := visit(name, path, typ); err != nil {
			return err
		}
	}
	for key, value := range schema {
		switch key {
		case "properties":
			properties, ok := value.(map[string]interface{})
			if !ok {
				return errors.New("invalid MCP schema properties")
			}
			for name, sub := range properties {
				if child, ok := sub.(map[string]interface{}); ok {
					next := append(append([]string(nil), path...), name)
					if err := walkHeaders(child, next, seen, visit); err != nil {
						return err
					}
				}
			}
		case "items", "additionalProperties", "patternProperties", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "$defs", "definitions", "contains", "prefixItems":
			if err := rejectAnnotations(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectAnnotations(v interface{}) error {
	switch x := v.(type) {
	case map[string]interface{}:
		if _, ok := x["x-mcp-header"]; ok {
			return errors.New("MCP header annotation is not reachable through properties")
		}
		for _, child := range x {
			if err := rejectAnnotations(child); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range x {
			if err := rejectAnnotations(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateHeaderSchema(schema map[string]interface{}) error {
	return walkHeaders(schema, nil, map[string]bool{}, func(string, []string, string) error { return nil })
}

// mirrorHeaders copies annotated arguments into Mcp-Param-<name> headers.
func mirrorHeaders(schema map[string]interface{}, args map[string]interface{}, headers http.Header) error {
	return walkHeaders(schema, nil, map[string]bool{}, func(name string, path []string, typ string) error {
		var value interface{} = args
		for _, key := range path {
			object, ok := value.(map[string]interface{})
			if !ok {
				return nil
			}
			if value, ok = object[key]; !ok {
				return nil
			}
		}
		var text string
		switch typ {
		case "string":
			s, ok := value.(string)
			if !ok {
				return errors.New("invalid MCP header argument")
			}
			text = s
		case "boolean":
			b, ok := value.(bool)
			if !ok {
				return errors.New("invalid MCP header argument")
			}
			text = fmt.Sprint(b)
		case "integer":
			n, ok := number(value)
			if !ok || math.Trunc(n) != n || math.Abs(n) > 9007199254740991 {
				return errors.New("MCP header integer exceeds safe range")
			}
			text = fmt.Sprintf("%.0f", n)
		}
		headers.Set("Mcp-Param-"+name, headerValue(text))
		return nil
	})
}

func number(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
