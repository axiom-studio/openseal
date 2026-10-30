package presentation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The compact notation renders a JSON Schema as dense text a model reads in
// a fraction of the tokens of the JSON form, keeping every constraint:
//
//	{a:T, b?:T}     object with exactly these keys; b is optional
//	{a:T, ...}      object that also allows other keys
//	{*:T}           object mapping any key to T
//	object          any object
//	[T]1..12        list of 1 to 12 T
//	str..40         text of at most 40 characters; str1..40 at least 1
//	str/re/         text matching the regular expression re
//	int1..24 num>0  numbers in an inclusive (or, with < and >, exclusive) range
//	a|b|c           one of these values; "x" a constant
//	A | B           any of these schemas
//	T (note)        a description
//	Name            a named pattern or shape, defined once elsewhere
//
// A keyword the notation does not know is kept as <keyword=json>, so nothing
// is ever dropped silently.
const compactLegend = "Notation: {a:T, b?:T} is an object with exactly these keys, b optional; {*:T} maps any key to T; [T]1..12 is a list of 1-12 T; " +
	"a..b is an inclusive range (characters for str, the value for int and num, items for lists) and a missing end is open; " +
	"x|y is one of these values; A | B is any of these; (…) is a note on the field."

// compactRenderer renders schemas in the compact notation. patterns names
// string patterns (regex -> name); shapes names whole sub-schemas by their
// canonical JSON.
type compactRenderer struct {
	patterns map[string]string
	shapes   map[string]string
}

// namedPattern is a string pattern the visual notation names instead of
// printing its regex.
type namedPattern struct {
	name, pattern, note string
}

// namedShape is a shared sub-schema the visual notation defines once.
type namedShape struct {
	name   string
	schema map[string]interface{}
}

func visualPatterns() []namedPattern {
	return []namedPattern{
		{"id", opaqueIDPattern, ""},
		{"itemId", itemIDPattern, "lowercase letters, digits, - and _; unique within its component"},
		{"iconName", iconPattern.String(), ""},
		{"date", isoDate.String(), "YYYY-MM-DD"},
		{"time", clockTimePattern, "HH:MM, 24-hour"},
		{"placeId", PlaceIDPattern, "from find_place"},
	}
}

func visualShapes() []namedShape {
	return []namedShape{
		{"ImageRef", imageRefSchema()},
		{"Currency", currencySchema()},
		{"Amount", amountSchema()},
		{"Price", priceSchema()},
		{"Icon", iconSchema()},
		{"Rating", ratingSchema()},
		{"Format", enum(formatNames())},
		{"Tone", enum(toneNames())},
		{"Binding", bindingSchema()},
		{"Side", beforeAfterSideSchema()},
	}
}

// visualNotation is the renderer for the visual catalogue: its named
// patterns and shapes are spelled out in the visual field's description.
func visualNotation() compactRenderer {
	renderer := compactRenderer{patterns: map[string]string{}, shapes: map[string]string{}}
	for _, pattern := range visualPatterns() {
		renderer.patterns[pattern.pattern] = pattern.name
	}
	for _, shape := range visualShapes() {
		renderer.shapes[canonicalSchema(shape.schema)] = shape.name
	}
	return renderer
}

// CompactSchema renders schema in the compact notation, naming the shared
// visual patterns and shapes.
func CompactSchema(schema map[string]interface{}) string {
	return visualNotation().render(schema)
}

func canonicalSchema(schema map[string]interface{}) string {
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func (r compactRenderer) render(value interface{}) string {
	schema, ok := value.(map[string]interface{})
	if !ok {
		return compactJSON(value)
	}
	if name, named := r.shapes[canonicalSchema(schema)]; named {
		return name
	}
	return r.define(schema)
}

// define renders schema itself even when it is a named shape.
func (r compactRenderer) define(schema map[string]interface{}) string {
	used := map[string]bool{"description": true}
	text := r.body(schema, used)
	var extra []string
	for keyword := range schema {
		if !used[keyword] {
			extra = append(extra, "<"+keyword+"="+compactJSON(schema[keyword])+">")
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		text += " " + strings.Join(extra, " ")
	}
	if description, ok := schema["description"].(string); ok && description != "" {
		text += " (" + description + ")"
	}
	return text
}

func (r compactRenderer) body(schema map[string]interface{}, used map[string]bool) string {
	mark := func(keywords ...string) {
		for _, keyword := range keywords {
			used[keyword] = true
		}
	}
	if ref, ok := schema["$ref"].(string); ok {
		mark("$ref")
		return ref[strings.LastIndex(ref, "/")+1:]
	}
	if value, ok := schema["const"]; ok {
		mark("const", "type")
		return compactJSON(value)
	}
	if values, ok := schema["enum"].([]interface{}); ok {
		mark("enum", "type")
		parts := make([]string, len(values))
		for index, value := range values {
			parts[index] = compactEnumValue(value)
		}
		return strings.Join(parts, "|")
	}
	for _, keyword := range []string{"anyOf", "oneOf"} {
		if members, ok := schema[keyword].([]interface{}); ok {
			mark(keyword)
			parts := make([]string, len(members))
			for index, member := range members {
				parts[index] = r.render(member)
			}
			return strings.Join(parts, " | ")
		}
	}
	mark("type")
	switch kind := schema["type"].(type) {
	case string:
		return r.typed(kind, schema, mark)
	case []interface{}:
		parts := make([]string, len(kind))
		for index, member := range kind {
			name, _ := member.(string)
			parts[index] = r.typed(name, schema, mark)
		}
		return strings.Join(parts, "|")
	case nil:
		if schema["properties"] != nil || schema["additionalProperties"] != nil {
			return r.object(schema, mark)
		}
		return "any"
	default:
		used["type"] = false
		return "any"
	}
}

func (r compactRenderer) typed(kind string, schema map[string]interface{}, mark func(...string)) string {
	switch kind {
	case "object":
		return r.object(schema, mark)
	case "array":
		mark("items", "minItems", "maxItems")
		items := "any"
		if schema["items"] != nil {
			items = r.render(schema["items"])
		}
		return "[" + items + "]" + compactRange(schema, "minItems", "maxItems", "", "")
	case "string":
		mark("pattern", "minLength", "maxLength")
		base := "str"
		if pattern, ok := schema["pattern"].(string); ok {
			if name, named := r.patterns[pattern]; named {
				base = name
			} else {
				base = "str/" + pattern + "/"
			}
		}
		return base + compactRange(schema, "minLength", "maxLength", "", "")
	case "integer", "number":
		mark("minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum")
		base := "num"
		if kind == "integer" {
			base = "int"
		}
		return base + compactRange(schema, "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum")
	case "boolean":
		return "bool"
	case "null":
		return "null"
	default:
		return kind
	}
}

func (r compactRenderer) object(schema map[string]interface{}, mark func(...string)) string {
	mark("properties", "required", "additionalProperties")
	properties, _ := schema["properties"].(map[string]interface{})
	additional, hasAdditional := schema["additionalProperties"]
	closed := hasAdditional && additional == false
	open := !hasAdditional || additional == true
	if len(properties) == 0 && open {
		return "object"
	}
	required := map[string]bool{}
	var keys []string
	if list, ok := schema["required"].([]interface{}); ok {
		for _, entry := range list {
			key := fmt.Sprint(entry)
			required[key] = true
			keys = append(keys, key)
		}
	}
	var optional []string
	for key := range properties {
		if !required[key] {
			optional = append(optional, key)
		}
	}
	sort.Strings(optional)
	keys = append(keys, optional...)
	entries := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		marker := ":"
		if !required[key] {
			marker = "?:"
		}
		property, ok := properties[key]
		value := "any"
		if ok {
			value = r.render(property)
		}
		entries = append(entries, compactKey(key)+marker+value)
	}
	switch {
	case closed:
	case open:
		entries = append(entries, "...")
	default:
		entries = append(entries, "*:"+r.render(additional))
	}
	return "{" + strings.Join(entries, ", ") + "}"
}

// compactRange renders inclusive bounds as a..b (either end may be open)
// and exclusive bounds as >a and <b.
func compactRange(schema map[string]interface{}, minimum, maximum, exclusiveMinimum, exclusiveMaximum string) string {
	low, hasLow := compactNumber(schema[minimum])
	high, hasHigh := compactNumber(schema[maximum])
	var text string
	if hasLow || hasHigh {
		text = low + ".." + high
	}
	if exclusiveMinimum != "" {
		if value, ok := compactNumber(schema[exclusiveMinimum]); ok {
			text = joinBound(text, ">"+value)
		}
	}
	if exclusiveMaximum != "" {
		if value, ok := compactNumber(schema[exclusiveMaximum]); ok {
			text = joinBound(text, "<"+value)
		}
	}
	return text
}

func joinBound(text, bound string) string {
	if text == "" {
		return bound
	}
	return text + "," + bound
}

func compactNumber(value interface{}) (string, bool) {
	switch number := value.(type) {
	case int:
		return strconv.Itoa(number), true
	case int64:
		return strconv.FormatInt(number, 10), true
	case float64:
		return strconv.FormatFloat(number, 'f', -1, 64), true
	case json.Number:
		return number.String(), true
	}
	return "", false
}

var compactBare = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.+-]*$`)

func compactEnumValue(value interface{}) string {
	if text, ok := value.(string); ok && compactBare.MatchString(text) {
		return text
	}
	return compactJSON(value)
}

func compactKey(key string) string {
	if compactBare.MatchString(key) {
		return key
	}
	return compactJSON(key)
}

func compactJSON(value interface{}) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}
