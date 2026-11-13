package presentation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// Every rule a JSON Schema can state (lengths, counts, ranges, enums,
// patterns, required and unknown keys) lives only in the strict schema the
// visual format is rendered from; Validate checks a surface against it here
// and keeps Go code for the rules a schema cannot state.

// notBlankPattern is the pattern of every text with a minimum length: at
// least one character that is not a space. The compact notation shows it
// as a plain str with a lower bound.
const notBlankPattern = `\S`

const surfaceSchemaURL = "openseal-surface.json"

// surfaceSchema is the strict schema of a stored surface: the model's visual
// (title and components) with its schema and state, each component checked
// against the strict schema of its type.
func surfaceSchema() map[string]interface{} {
	branches := make([]interface{}, 0, len(catalogue))
	for _, name := range TypeNames() {
		branches = append(branches, map[string]interface{}{
			"if":   map[string]interface{}{"properties": map[string]interface{}{"type": map[string]interface{}{"const": name}}, "required": []interface{}{"type"}},
			"then": componentTypeSchema(name, componentRef()),
		})
	}
	component := map[string]interface{}{
		"type": "object", "required": []interface{}{"id", "type"},
		"properties": map[string]interface{}{"type": enum(TypeNames())}, "allOf": branches,
	}
	root := closedObject([]interface{}{"schema", "title", "state", "components"}, map[string]interface{}{
		"schema": map[string]interface{}{"const": Schema}, "title": strRange(1, MaxTitleRunes),
		// The count of components includes nested ones, which Validate
		// checks itself.
		"state": enum([]string{string(StateStreaming), string(StateReady)}), "components": map[string]interface{}{"type": "array", "minItems": 1, "items": componentRef()},
	})
	root["$defs"] = map[string]interface{}{"Component": component}
	return root
}

type compiledSurface struct {
	schema *jsonschema.Schema
	// document is the schema as plain JSON, read to word errors.
	document interface{}
}

// compiledSurfaceSchema compiles the surface schema once per process.
var compiledSurfaceSchema = sync.OnceValues(func() (*compiledSurface, error) {
	encoded, err := json.Marshal(surfaceSchema())
	if err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(surfaceSchemaURL, document); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile(surfaceSchemaURL)
	if err != nil {
		return nil, err
	}
	return &compiledSurface{schema: schema, document: document}, nil
})

// DecodeVisual decodes a visual the model sent. Its JSON is checked against
// the schema first, so a key the format does not have or a value of the
// wrong type is named by its path. Validate still checks the decoded visual.
func DecodeVisual(raw []byte) (*Visual, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("the visual is not valid JSON: %w", err)
	}
	fields, ok := document.(map[string]interface{})
	if !ok {
		return nil, &ValidationError{Message: "the visual must be an object {title, components}"}
	}
	surface := map[string]interface{}{"schema": Schema, "state": string(StateReady)}
	for key, value := range fields {
		if key == "schema" || key == "state" {
			return nil, &ValidationError{Path: key, Message: "is not part of a visual; send only title and components"}
		}
		surface[key] = value
	}
	dropNulls(surface)
	if err := checkSchema(surface); err != nil {
		return nil, err
	}
	var visual Visual
	if err := json.Unmarshal(raw, &visual); err != nil {
		return nil, fmt.Errorf("decode the visual: %w", err)
	}
	return &visual, nil
}

// surfaceDocument is a surface's JSON as the schema checks it. A stored
// surface keeps the fields the host stamped in from find_place, which the
// model never sends; they are left out of the schema check and checked by
// Validate itself.
func surfaceDocument(encoded []byte, stamped bool) (interface{}, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	dropNulls(document)
	if stamped {
		removeStampedFields(document)
	}
	return document, nil
}

// dropNulls removes keys whose value is null: a null field is a field left
// out, as it decodes in Go.
func dropNulls(value interface{}) {
	switch node := value.(type) {
	case map[string]interface{}:
		for key, child := range node {
			if child == nil {
				delete(node, key)
				continue
			}
			dropNulls(child)
		}
	case []interface{}:
		for _, child := range node {
			dropNulls(child)
		}
	}
}

// removeStampedFields drops lat, lon and a pin's address from a surface
// document.
func removeStampedFields(document interface{}) {
	surface, _ := document.(map[string]interface{})
	var visit func(components interface{})
	visit = func(components interface{}) {
		list, _ := components.([]interface{})
		for _, entry := range list {
			component, _ := entry.(map[string]interface{})
			if component == nil {
				continue
			}
			for _, item := range objectsAt(component, "map", "pins") {
				delete(item, "lat")
				delete(item, "lon")
				delete(item, "address")
			}
			if place, ok := component["place"].(map[string]interface{}); ok {
				delete(place, "lat")
				delete(place, "lon")
			}
			for _, day := range objectsAt(component, "itinerary", "days") {
				for _, item := range objectsAt(day, "items") {
					delete(item, "lat")
					delete(item, "lon")
				}
			}
			visit(component["children"])
			for _, tab := range objectsAt(component, "tabs") {
				visit(tab["children"])
			}
		}
	}
	visit(surface["components"])
}

// objectsAt is the list of objects at keys below object.
func objectsAt(object map[string]interface{}, keys ...string) []map[string]interface{} {
	var value interface{} = object
	for _, key := range keys {
		next, _ := value.(map[string]interface{})
		if next == nil {
			return nil
		}
		value = next[key]
	}
	list, _ := value.([]interface{})
	out := make([]map[string]interface{}, 0, len(list))
	for _, entry := range list {
		if item, ok := entry.(map[string]interface{}); ok {
			out = append(out, item)
		}
	}
	return out
}

// maxReportedSchemaErrors bounds how many schema errors one rejection
// lists, so the model can fix several at once without a wall of text.
const maxReportedSchemaErrors = 5

// checkSchema validates a surface document against the schema and words
// the failures as Validate does: the component's path and id, then the
// field and what is wrong with it.
func checkSchema(document interface{}) error {
	compiled, err := compiledSurfaceSchema()
	if err != nil {
		return fmt.Errorf("compile the visual schema: %w", err)
	}
	failure := compiled.schema.Validate(document)
	if failure == nil {
		return nil
	}
	validation, ok := failure.(*jsonschema.ValidationError)
	if !ok {
		return failure
	}
	var found []*ValidationError
	seen := map[string]bool{}
	leaves := schemaLeaves(validation)
	sort.SliceStable(leaves, func(i, j int) bool { return locationLess(leaves[i].InstanceLocation, leaves[j].InstanceLocation) })
	for _, leaf := range leaves {
		for _, worded := range wordSchemaError(leaf, compiled.document, document) {
			if key := worded.Error(); !seen[key] {
				seen[key] = true
				found = append(found, worded)
			}
		}
	}
	if len(found) == 0 {
		return &ValidationError{Message: validation.Error()}
	}
	first := found[0]
	var also []string
	for _, other := range found[1:min(len(found), maxReportedSchemaErrors)] {
		also = append(also, other.Error())
	}
	if len(also) > 0 {
		first.Message += "; also " + strings.Join(also, "; ")
	}
	return first
}

// schemaLeaves are the innermost failures. Of the members of an anyOf, only
// those of the value's own type are kept, so a long text cell is reported
// as too long rather than also as "not a number".
func schemaLeaves(failure *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(failure.Causes) == 0 {
		return []*jsonschema.ValidationError{failure}
	}
	causes := failure.Causes
	switch failure.ErrorKind.(type) {
	case *kind.AnyOf, *kind.OneOf:
		var sameType []*jsonschema.ValidationError
		var wanted []string
		for _, cause := range causes {
			if mismatch, ok := cause.ErrorKind.(*kind.Type); ok && len(cause.Causes) == 0 {
				wanted = append(wanted, mismatch.Want...)
				continue
			}
			sameType = append(sameType, cause)
		}
		if len(sameType) == 0 {
			got := ""
			if mismatch, ok := causes[0].ErrorKind.(*kind.Type); ok {
				got = mismatch.Got
			}
			return []*jsonschema.ValidationError{{SchemaURL: failure.SchemaURL, InstanceLocation: failure.InstanceLocation, ErrorKind: &kind.Type{Got: got, Want: wanted}}}
		}
		causes = sameType
	}
	var leaves []*jsonschema.ValidationError
	for _, cause := range causes {
		leaves = append(leaves, schemaLeaves(cause)...)
	}
	return leaves
}

func locationLess(a, b []string) bool {
	for index := 0; index < len(a) && index < len(b); index++ {
		if a[index] == b[index] {
			continue
		}
		x, xErr := strconv.Atoi(a[index])
		y, yErr := strconv.Atoi(b[index])
		if xErr == nil && yErr == nil {
			return x < y
		}
		return a[index] < b[index]
	}
	return len(a) < len(b)
}

// schemaPlace is where a failure is: the component (its path and id) and
// the field inside it, or a top-level field of the surface.
type schemaPlace struct {
	componentPath string
	component     map[string]interface{}
	field         string
}

func placeOf(location []string, document interface{}) schemaPlace {
	surface, _ := document.(map[string]interface{})
	if len(location) < 2 || location[0] != "components" {
		return schemaPlace{field: fieldPath(location)}
	}
	list, _ := surface["components"].([]interface{})
	place := schemaPlace{componentPath: "components[" + location[1] + "]"}
	place.component = indexObject(list, location[1])
	rest := location[2:]
	for place.component != nil {
		switch {
		case len(rest) >= 2 && rest[0] == "children":
			children, _ := place.component["children"].([]interface{})
			place.componentPath += ".children[" + rest[1] + "]"
			place.component = indexObject(children, rest[1])
			rest = rest[2:]
			continue
		case len(rest) >= 4 && rest[0] == "tabs" && rest[2] == "children":
			tabs, _ := place.component["tabs"].([]interface{})
			tab := indexObject(tabs, rest[1])
			children, _ := tab["children"].([]interface{})
			place.componentPath += ".tabs[" + rest[1] + "].children[" + rest[3] + "]"
			place.component = indexObject(children, rest[3])
			rest = rest[4:]
			continue
		}
		break
	}
	place.field = fieldPath(rest)
	return place
}

func indexObject(list []interface{}, token string) map[string]interface{} {
	index, err := strconv.Atoi(token)
	if err != nil || index < 0 || index >= len(list) {
		return nil
	}
	object, _ := list[index].(map[string]interface{})
	return object
}

// fieldPath writes a location as Validate's paths: keys joined by dots and
// list indexes in brackets.
func fieldPath(location []string) string {
	var b strings.Builder
	for _, token := range location {
		if _, err := strconv.Atoi(token); err == nil {
			b.WriteString("[" + token + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteString(".")
		}
		b.WriteString(token)
	}
	return b.String()
}

func (p schemaPlace) componentType() string {
	value, _ := p.component["type"].(string)
	return value
}

func (p schemaPlace) componentID() string {
	value, _ := p.component["id"].(string)
	return value
}

// at is the error for message at field (a path below the place's field).
func (p schemaPlace) at(field, message string) *ValidationError {
	if p.componentPath == "" {
		return &ValidationError{Path: field, Message: message}
	}
	if field != "" {
		message = field + " " + message
	}
	return &ValidationError{Path: p.componentPath, ComponentID: p.componentID(), Message: message}
}

func join(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// wordSchemaError words one schema failure in the terms of the visual
// format.
func wordSchemaError(leaf *jsonschema.ValidationError, schemaDocument, document interface{}) []*ValidationError {
	place := placeOf(leaf.InstanceLocation, document)
	schema := schemaAt(schemaDocument, leaf.SchemaURL)
	field := place.field
	componentRoot := place.componentPath != "" && field == ""
	switch failure := leaf.ErrorKind.(type) {
	case *kind.Required:
		var out []*ValidationError
		for _, missing := range failure.Missing {
			out = append(out, place.missing(missing, propertySchema(schema, missing)))
		}
		return out
	case *kind.AdditionalProperties:
		var out []*ValidationError
		for _, extra := range failure.Properties {
			switch {
			case componentRoot && extra == "children":
				out = append(out, place.at("", place.componentType()+" cannot have children; only section, grid and carousel can"))
			case componentRoot && isPayloadKey(extra):
				out = append(out, place.at("", fmt.Sprintf("%s cannot carry a %q payload", place.componentType(), extra)))
			case componentRoot && extra == "binding":
				out = append(out, place.at("", place.componentType()+" takes no component binding"))
			case extra == "lat" || extra == "lon":
				out = append(out, place.at(join(field, "lat"), "and .lon are filled in from find_place; leave them out and never type coordinates"))
			case extra == "address":
				out = append(out, place.at(join(field, "address"), "is filled in from find_place; leave it out"))
			default:
				subject := field
				if subject == "" && place.componentPath == "" {
					subject = "the visual"
				}
				out = append(out, place.at(subject, fmt.Sprintf("has no field %q (unknown field %q); leave it out", extra, extra)))
			}
		}
		return out
	case *kind.Enum:
		if componentRoot || field == "type" && place.componentPath != "" {
			return []*ValidationError{place.at("", fmt.Sprintf("unsupported type %q; use one of %s", fmt.Sprint(failure.Got), strings.Join(TypeNames(), ", ")))}
		}
		if field == "binding.kind" && place.componentPath != "" {
			return []*ValidationError{place.at(field, fmt.Sprintf("%q is not allowed on %s; use %s", fmt.Sprint(failure.Got), place.componentType(), joinValues(failure.Want)))}
		}
		return []*ValidationError{place.at(field, fmt.Sprintf("%q is not one of %s", fmt.Sprint(failure.Got), joinValues(failure.Want)))}
	case *kind.Const:
		return []*ValidationError{place.at(field, fmt.Sprintf("must be %s", compactJSON(failure.Want)))}
	case *kind.Type:
		return []*ValidationError{place.at(field, fmt.Sprintf("must be %s, not %s", strings.Join(failure.Want, " or "), failure.Got))}
	case *kind.Pattern:
		if failure.Want == notBlankPattern {
			return []*ValidationError{place.at(field, "must not be blank")}
		}
		if phrase, ok := patternPhrases[failure.Want]; ok {
			return []*ValidationError{place.at(field, fmt.Sprintf("%q %s", failure.Got, phrase))}
		}
		return []*ValidationError{place.at(field, fmt.Sprintf("%q must match %s", failure.Got, failure.Want))}
	case *kind.MinLength:
		return []*ValidationError{place.at(field, lengthMessage(schema, failure.Got))}
	case *kind.MaxLength:
		return []*ValidationError{place.at(field, lengthMessage(schema, failure.Got))}
	case *kind.MinItems:
		return []*ValidationError{place.count(field, schema, failure.Got)}
	case *kind.MaxItems:
		return []*ValidationError{place.count(field, schema, failure.Got)}
	case *kind.Minimum:
		return []*ValidationError{place.at(field, rangeMessage(schema, failure.Got))}
	case *kind.Maximum:
		return []*ValidationError{place.at(field, rangeMessage(schema, failure.Got))}
	case *kind.ExclusiveMinimum:
		return []*ValidationError{place.at(field, fmt.Sprintf("must be above %s (got %s)", ratString(failure.Want), ratString(failure.Got)))}
	}
	return []*ValidationError{place.at(field, leaf.Error())}
}

// patternPhrases say in words what the named patterns mean.
var patternPhrases = map[string]string{
	isoDate.String():     "must be a date as YYYY-MM-DD",
	clockTimePattern:     "must be a 24-hour time as HH:MM",
	PlaceIDPattern:       "must be a place id returned by find_place, such as place:4b2f9c0e1a77",
	iconPattern.String(): `must be a lowercase Lucide icon name such as "circle-check"`,
	currencyPattern:      "is not an ISO 4217 code such as GBP, USD or EUR",
	itemIDPattern:        "must match " + itemIDPattern + " (lowercase letters, digits, - and _)",
	explorerNamePattern:  "must match " + explorerNamePattern + ": expressions use it as a name",
}

// missing words a required key that is absent.
func (p schemaPlace) missing(key string, schema map[string]interface{}) *ValidationError {
	field := join(p.field, key)
	if p.componentPath != "" && p.field == "" {
		name := p.componentType()
		spec := catalogue[name]
		switch {
		case key == "summary":
			return p.at("", fmt.Sprintf("summary is required for %s: one plain sentence saying what it shows", name))
		case key == "binding" && len(spec.bindings) > 0:
			return p.at("", fmt.Sprintf("binding is required: {kind: %s, id} names the %s this card shows", joinBindingKinds(spec.bindings), bindingNoun(spec.bindings[0])))
		case key == spec.payload && schema["type"] == "object":
			return p.at("", name+" payload is required")
		}
	}
	if p.componentPath == "" && key == "components" {
		return p.at(key, "at least one component is required")
	}
	switch schema["type"] {
	case "array":
		return p.count(field, schema, 0)
	case "string":
		return p.at(field, lengthMessage(schema, 0))
	}
	return p.at(field, "is required")
}

// count words a list length outside its bounds, as bounded does.
func (p schemaPlace) count(field string, schema map[string]interface{}, got int) *ValidationError {
	minimum, hasMinimum := schemaInt(schema, "minItems")
	maximum, hasMaximum := schemaInt(schema, "maxItems")
	if p.componentPath != "" && field == "children" {
		return p.at("", fmt.Sprintf("%s needs %d-%d children (got %d)", p.componentType(), minimum, maximum, got))
	}
	if p.componentPath == "" && field == "components" && got == 0 {
		return p.at(field, "at least one component is required")
	}
	switch {
	case hasMinimum && hasMaximum && minimum > 0:
		return p.at(field, fmt.Sprintf("needs %d-%d (got %d)", minimum, maximum, got))
	case hasMaximum:
		return p.at(field, fmt.Sprintf("allows at most %d (got %d)", maximum, got))
	default:
		return p.at(field, fmt.Sprintf("needs at least %d (got %d)", minimum, got))
	}
}

// lengthMessage words a text length outside its bounds, as boundedText
// does.
func lengthMessage(schema map[string]interface{}, got int) string {
	minimum, _ := schemaInt(schema, "minLength")
	maximum, hasMaximum := schemaInt(schema, "maxLength")
	switch {
	case minimum > 0 && hasMaximum:
		return fmt.Sprintf("must contain between %d and %d characters (got %d)", minimum, maximum, got)
	case hasMaximum:
		return fmt.Sprintf("must be at most %d characters (got %d)", maximum, got)
	default:
		return fmt.Sprintf("must contain at least %d characters (got %d)", minimum, got)
	}
}

// rangeMessage words a number outside its inclusive bounds.
func rangeMessage(schema map[string]interface{}, got *big.Rat) string {
	low, hasLow := schema["minimum"]
	high, hasHigh := schema["maximum"]
	switch {
	case hasLow && hasHigh:
		return fmt.Sprintf("must be %v-%v (got %s)", low, high, ratString(got))
	case hasLow:
		return fmt.Sprintf("must be %v or more (got %s)", low, ratString(got))
	default:
		return fmt.Sprintf("must be at most %v (got %s)", high, ratString(got))
	}
}

func ratString(value *big.Rat) string {
	if value == nil {
		return "?"
	}
	if value.IsInt() {
		return value.Num().String()
	}
	number, _ := value.Float64()
	return strconv.FormatFloat(number, 'f', -1, 64)
}

func joinValues(values []interface{}) string {
	text := make([]string, len(values))
	for index, value := range values {
		text[index] = fmt.Sprint(value)
	}
	return strings.Join(text, ", ")
}

func isPayloadKey(key string) bool {
	for _, spec := range catalogue {
		if spec.payload == key {
			return true
		}
	}
	return false
}

// schemaAt resolves the JSON pointer of a schema URL in the schema
// document.
func schemaAt(document interface{}, url string) map[string]interface{} {
	_, fragment, _ := strings.Cut(url, "#")
	node := document
	for _, token := range strings.Split(strings.TrimPrefix(fragment, "/"), "/") {
		if token == "" {
			continue
		}
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch value := node.(type) {
		case map[string]interface{}:
			node = value[token]
		case []interface{}:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(value) {
				return nil
			}
			node = value[index]
		default:
			return nil
		}
	}
	schema, _ := node.(map[string]interface{})
	return schema
}

func propertySchema(schema map[string]interface{}, key string) map[string]interface{} {
	properties, _ := schema["properties"].(map[string]interface{})
	property, _ := properties[key].(map[string]interface{})
	return property
}

func schemaInt(schema map[string]interface{}, keyword string) (int, bool) {
	switch value := schema[keyword].(type) {
	case json.Number:
		number, err := value.Int64()
		return int(number), err == nil
	case float64:
		return int(value), true
	case int:
		return value, true
	}
	return 0, false
}
