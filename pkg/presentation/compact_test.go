package presentation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestCompactRenderer(t *testing.T) {
	named := compactRenderer{
		patterns: map[string]string{"^[a-z]+$": "word"},
		shapes:   map[string]string{canonicalSchema(closedObject([]interface{}{"x"}, map[string]interface{}{"x": number()})): "Point"},
	}
	for _, test := range []struct {
		name     string
		renderer compactRenderer
		schema   map[string]interface{}
		want     string
	}{
		{"string", compactRenderer{}, map[string]interface{}{"type": "string"}, "str"},
		{"string max", compactRenderer{}, str(40), "str..40"},
		{"string range", compactRenderer{}, strRange(1, 40), "str1..40"},
		{"string min", compactRenderer{}, map[string]interface{}{"type": "string", "minLength": 2}, "str2.."},
		{"pattern", compactRenderer{}, map[string]interface{}{"type": "string", "pattern": "^[A-Z]{3}$"}, "str/^[A-Z]{3}$/"},
		{"named pattern", named, map[string]interface{}{"type": "string", "pattern": "^[a-z]+$", "maxLength": 9}, "word..9"},
		{"enum", compactRenderer{}, enum([]string{"bar", "line", "run_now"}), "bar|line|run_now"},
		{"enum needing quotes", compactRenderer{}, map[string]interface{}{"enum": []interface{}{"a b", "12", 3, nil}}, `"a b"|"12"|3|null`},
		{"const", compactRenderer{}, map[string]interface{}{"const": "fixed"}, `"fixed"`},
		{"integer range", compactRenderer{}, integer(1, 24), "int1..24"},
		{"integer min", compactRenderer{}, map[string]interface{}{"type": "integer", "minimum": 0}, "int0.."},
		{"number fraction", compactRenderer{}, map[string]interface{}{"type": "number", "minimum": -1.5, "maximum": 0.25}, "num-1.5..0.25"},
		{"exclusive", compactRenderer{}, map[string]interface{}{"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1}, "num>0,<1"},
		{"exclusive with inclusive", compactRenderer{}, map[string]interface{}{"type": "number", "exclusiveMinimum": 0, "maximum": 5}, "num..5,>0"},
		{"boolean", compactRenderer{}, boolean(), "bool"},
		{"type list", compactRenderer{}, map[string]interface{}{"type": []interface{}{"string", "null"}, "maxLength": 3}, "str..3|null"},
		{"array", compactRenderer{}, array(str(10), 2, 5), "[str..10]2..5"},
		{"array max only", compactRenderer{}, array(number(), 0, 4), "[num]..4"},
		{"array unbounded", compactRenderer{}, map[string]interface{}{"type": "array", "items": boolean()}, "[bool]"},
		{"array min only", compactRenderer{}, map[string]interface{}{"type": "array", "items": boolean(), "minItems": 1}, "[bool]1.."},
		{"closed object", compactRenderer{}, closedObject([]interface{}{"b", "a"}, map[string]interface{}{"a": number(), "b": boolean(), "d": str(0), "c": str(0)}),
			"{b:bool, a:num, c?:str, d?:str}"},
		{"open object", compactRenderer{}, map[string]interface{}{"type": "object"}, "object"},
		{"open object with keys", compactRenderer{}, map[string]interface{}{"type": "object", "properties": map[string]interface{}{"a": number()}}, "{a?:num, ...}"},
		{"empty closed object", compactRenderer{}, closedObject(nil, map[string]interface{}{}), "{}"},
		{"map", compactRenderer{}, map[string]interface{}{"type": "object", "additionalProperties": str(80)}, "{*:str..80}"},
		{"quoted key", compactRenderer{}, closedObject([]interface{}{"a b"}, map[string]interface{}{"a b": number()}), `{"a b":num}`},
		{"required without property", compactRenderer{}, closedObject([]interface{}{"x"}, map[string]interface{}{}), "{x:any}"},
		{"anyOf", compactRenderer{}, map[string]interface{}{"anyOf": []interface{}{str(5), number()}}, "str..5 | num"},
		{"oneOf", compactRenderer{}, map[string]interface{}{"oneOf": []interface{}{boolean(), map[string]interface{}{"type": "null"}}}, "bool | null"},
		{"ref", compactRenderer{}, componentRef(), "Component"},
		{"description", compactRenderer{}, describe(str(3), "Short."), "str..3 (Short.)"},
		{"nested descriptions", compactRenderer{}, describe(closedObject([]interface{}{"a"}, map[string]interface{}{"a": describe(number(), "Inner.")}), "Outer."),
			"{a:num (Inner.)} (Outer.)"},
		{"named shape", named, array(closedObject([]interface{}{"x"}, map[string]interface{}{"x": number()}), 1, 2), "[Point]1..2"},
		{"described shape is not the shape", named, describe(closedObject([]interface{}{"x"}, map[string]interface{}{"x": number()}), "Here."), "{x:num} (Here.)"},
		{"unknown keyword kept", compactRenderer{}, map[string]interface{}{"type": "string", "format": "email", "uniqueItems": true}, `str <format="email"> <uniqueItems=true>`},
		{"untyped", compactRenderer{}, map[string]interface{}{}, "any"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.renderer.render(test.schema); got != test.want {
				t.Fatalf("render(%s) = %s, want %s", compactJSON(test.schema), got, test.want)
			}
		})
	}
}

// TestVisualFormatKeepsEverything walks the strict component schema and the
// shared shapes and checks that every key, enum value, bound, pattern and
// description reaches the compact format, so nothing drops silently.
func TestVisualFormatKeepsEverything(t *testing.T) {
	format := VisualFormat()
	notation := visualNotation()
	if strings.Contains(format, " <") {
		t.Fatalf("format falls back on unknown keywords:\n%s", format)
	}
	for _, name := range TypeNames() {
		if !strings.Contains(format, "\n"+name+":") && !strings.Contains(format, "\n"+name+"*:") {
			t.Fatalf("format has no line for %s", name)
		}
	}
	for _, pattern := range visualPatterns() {
		if !strings.Contains(format, pattern.name+" = str/"+pattern.pattern+"/") {
			t.Fatalf("format does not define %s", pattern.name)
		}
	}
	for _, shape := range visualShapes() {
		if !strings.Contains(format, shape.name+" = "+notation.define(shape.schema)) {
			t.Fatalf("format does not define %s", shape.name)
		}
	}
	for _, icon := range Icons {
		if !strings.Contains(format, " "+icon+",") && !strings.Contains(format, " "+icon+";") {
			t.Fatalf("format lacks icon %s", icon)
		}
	}
	roots := map[string]map[string]interface{}{"component": componentJSONSchema(), "visual": visualSchema(componentRef(), "")}
	for _, shape := range visualShapes() {
		roots[shape.name] = shape.schema
	}
	checked := 0
	for root, schema := range roots {
		var visit func(path string, node interface{}, depth int)
		visit = func(path string, node interface{}, depth int) {
			schema, ok := node.(map[string]interface{})
			if !ok {
				return
			}
			checked++
			for keyword := range schema {
				if !compactKeywords[keyword] {
					t.Errorf("%s uses %q, which the compact notation does not know", path, keyword)
				}
			}
			// Below the root, each sub-schema renders to an exact
			// substring of the format (the component's type list is
			// spelled out as one line per type instead).
			if depth > 0 && path != "component.type" {
				if rendered := notation.render(schema); !strings.Contains(format, rendered) {
					t.Errorf("%s renders as %q, which the format lacks", path, rendered)
				}
			}
			if description, ok := schema["description"].(string); ok && !strings.Contains(format, "("+description+")") {
				t.Errorf("%s description %q is missing", path, description)
			}
			if values, ok := schema["enum"].([]interface{}); ok {
				for _, value := range values {
					if !strings.Contains(format, compactEnumValue(value)) {
						t.Errorf("%s enum value %v is missing", path, value)
					}
				}
			}
			if pattern, ok := schema["pattern"].(string); ok && !strings.Contains(format, "/"+pattern+"/") {
				t.Errorf("%s pattern %s is missing", path, pattern)
			}
			for _, keyword := range []string{"minimum", "maximum", "exclusiveMinimum", "minLength", "maxLength", "minItems", "maxItems"} {
				if value, ok := compactNumber(schema[keyword]); ok && !strings.Contains(format, value) {
					t.Errorf("%s %s %s is missing", path, keyword, value)
				}
			}
			properties, _ := schema["properties"].(map[string]interface{})
			keys := make([]string, 0, len(properties))
			for key := range properties {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if !strings.Contains(format, key+":") && !strings.Contains(format, key+"?:") {
					t.Errorf("%s.%s is missing", path, key)
				}
				visit(path+"."+key, properties[key], depth+1)
			}
			if additional, ok := schema["additionalProperties"].(map[string]interface{}); ok {
				visit(path+".*", additional, depth+1)
			}
			if items, ok := schema["items"]; ok {
				visit(path+"[]", items, depth+1)
			}
			if members, ok := schema["anyOf"].([]interface{}); ok {
				for index, member := range members {
					visit(fmt.Sprintf("%s|%d", path, index), member, depth+1)
				}
			}
		}
		visit(root, schema, 0)
	}
	if checked < 300 {
		t.Fatalf("visited only %d sub-schemas", checked)
	}
}

// compactKeywords are the schema keywords the visual builders emit; the
// notation renders each of them.
var compactKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"minItems": true, "maxItems": true, "minLength": true, "maxLength": true, "pattern": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "enum": true, "anyOf": true,
	"description": true, "$ref": true,
}

// TestModelFacingVisualBudget bounds what every turn sends: the answer's
// visual field (loose schema plus the description with the whole format)
// and the update_visual input schema.
func TestModelFacingVisualBudget(t *testing.T) {
	field := VisualJSONSchema()
	field["description"] = VisualFieldDescription()
	encodedField, err := json.Marshal(field)
	if err != nil {
		t.Fatal(err)
	}
	encodedUpdate, err := json.Marshal(UpdateVisualInputSchema())
	if err != nil {
		t.Fatal(err)
	}
	// 24 KB held through Wave 2 (22,714 bytes). Wave 3's ten types need
	// 25,840 bytes even after the format stopped repeating notes and named
	// the shared Format, Tone, Binding, Side and Rating shapes, so the
	// ceiling moved to 26 KB (recorded in the Wave 3 contract). Wave 5's
	// map and locations fit under it at 26,575 bytes.
	const ceiling = 26 * 1024
	if total := len(encodedField) + len(encodedUpdate); total > ceiling {
		t.Fatalf("visual field %d + update_visual %d = %d bytes; the ceiling is %d", len(encodedField), len(encodedUpdate), total, ceiling)
	}
}
