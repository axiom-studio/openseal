package presentation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// The host reads the model's visual leniently (decision 2026-11-20,
// "visuals are read, not policed"); the format the model is told stays
// strict. A slip with one obvious reading is read that way: a scalar of
// another type is converted (coerceScalar) and a list or text over its
// maximum is cut. Whatever still fails costs only the component it is in,
// and every component left out is reported as a Drop. Both readings are
// driven by the schema the format is rendered from, plus the coerce and
// validate hooks of the type specs for what a schema cannot state, so a new
// component type gets them by declaring its schema. Validate, Encode and
// Decode stay strict: what Admit returns passes them.

// Drop is a part of a visual the host left out because it failed its
// checks. Path and ComponentID name the component; a drop with no path is
// the whole visual.
type Drop struct {
	Path        string `json:"path,omitempty"`
	ComponentID string `json:"componentId,omitempty"`
	Reason      string `json:"reason"`
}

// DecodeVisual reads a visual the model sent, without host checks. It
// returns the visual with every slip read and every failing component left
// out, and the drops. It fails only when nothing renderable remains; the
// drops then end with one for the whole visual.
func DecodeVisual(raw []byte) (*Visual, []Drop, error) {
	document, err := visualDocument(raw)
	if err != nil {
		_, drops, _, err := admitted(nil, nil, nil, err)
		return nil, drops, err
	}
	visual, drops, _, err := admitted(admit(context.Background(), document, ValidateOptions{SkipHostChecks: true}))
	return visual, drops, err
}

// visualDocument is the surface document of a visual's JSON.
func visualDocument(raw []byte) (map[string]interface{}, error) {
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
		// The host sets schema and state; a visual that sends them has
		// them ignored like any other key it does not have.
		if key != "schema" && key != "state" {
			surface[key] = value
		}
	}
	return surface, nil
}

// Admit reads a decoded visual as DecodeVisual does, with the host checks
// of opts: a component whose binding, link, image or place fails them is
// left out on its own.
func Admit(ctx context.Context, visual Visual, opts ValidateOptions) (*Visual, []Drop, []Warning, error) {
	encoded, err := json.Marshal(visual.Surface(StateReady))
	if err != nil {
		return admitted(nil, nil, nil, fmt.Errorf("encode visual: %w", err))
	}
	document, err := surfaceDocument(encoded, false)
	if err != nil {
		return admitted(nil, nil, nil, fmt.Errorf("read visual: %w", err))
	}
	fields, _ := document.(map[string]interface{})
	return admitted(admit(ctx, fields, opts))
}

// admitted adds the drop of the whole visual when there is no visual. When
// it is gone because every component was left out, the error names why each
// was, so whoever sent it can fix them.
func admitted(visual *Visual, drops []Drop, warnings []Warning, err error) (*Visual, []Drop, []Warning, error) {
	if err != nil {
		if len(drops) > 0 {
			reasons := make([]string, 0, len(drops))
			for _, drop := range drops {
				reasons = append(reasons, strings.TrimSpace(drop.Path+" "+drop.Reason))
			}
			err = fmt.Errorf("%w; every component was left out: %s", err, strings.Join(reasons, "; "))
		}
		return nil, append(drops, Drop{Reason: err.Error()}), nil, err
	}
	return visual, drops, warnings, nil
}

// admit reads a surface document: the schema's reading (repairDocument),
// then the checks only Go makes, leaving out each component that fails
// them. A component left out can make another fail (an action whose target
// is gone, a section left without children), so both run again until
// nothing more is left out.
func admit(ctx context.Context, document map[string]interface{}, opts ValidateOptions) (*Visual, []Drop, []Warning, error) {
	dropNulls(document)
	var drops []Drop
	for pass := 0; pass < maxRepairPasses; pass++ {
		dropped, err := repairDocument(document)
		drops = append(drops, dropped...)
		if err != nil {
			return nil, drops, nil, err
		}
		var visual Visual
		if err := decodeDrawn(document, &visual); err != nil {
			return nil, drops, nil, fmt.Errorf("decode the visual: %w", err)
		}
		surface := visual.Surface(StateReady)
		warnings, failures := inspect(ctx, surface, opts)
		if len(failures) == 0 {
			if encoded, err := json.Marshal(surface); err != nil || len(encoded) > MaxSurfaceBytes {
				return nil, drops, nil, &ValidationError{Message: fmt.Sprintf("the visual is %d bytes of JSON; the limit is %d. Use fewer or smaller components", len(encoded), MaxSurfaceBytes)}
			}
			return &visual, drops, warnings, nil
		}
		found := map[string]Drop{}
		for _, failure := range failures {
			if _, seen := found[failure.path]; !seen {
				found[failure.path] = Drop{Path: failure.path, ComponentID: failure.err.ComponentID, Reason: failure.err.Message}
			}
		}
		removed := outermost(found)
		drops = append(drops, removed...)
		document["components"] = removeComponents(document["components"], "components", removed)
	}
	return nil, drops, nil, errors.New("the visual could not be read")
}

// maxRepairPasses bounds admit and repairDocument; every pass converts,
// cuts or leaves out something, so a visual within MaxSurfaceBytes needs far
// fewer.
const maxRepairPasses = 512

// repairDocument reads a surface document by its schema. Each pass first
// lets the type specs coerce what only Go can type, then checks the
// schema: a failure with one obvious reading (a scalar of another type, a
// list or text over its maximum) is repaired in place; when none is left,
// each component that still fails is left out. It fails when the surface
// itself fails (its title, or no component left to draw).
func repairDocument(document map[string]interface{}) ([]Drop, error) {
	compiled, err := compiledSurfaceSchema()
	if err != nil {
		return nil, fmt.Errorf("compile the visual schema: %w", err)
	}
	var drops []Drop
	for pass := 0; pass < maxRepairPasses; pass++ {
		coerceComponents(document["components"])
		failure := compiled.schema.Validate(document)
		if failure == nil {
			return drops, nil
		}
		validation, ok := failure.(*jsonschema.ValidationError)
		if !ok {
			return drops, failure
		}
		leaves := schemaLeaves(validation)
		repaired := false
		for _, leaf := range leaves {
			if repairLeaf(document, leaf) {
				repaired = true
			}
		}
		if repaired {
			continue
		}
		sort.SliceStable(leaves, func(i, j int) bool { return locationLess(leaves[i].InstanceLocation, leaves[j].InstanceLocation) })
		found := map[string]Drop{}
		for _, leaf := range leaves {
			place := placeOf(leaf.InstanceLocation, document)
			if _, seen := found[place.componentPath]; seen || place.componentPath == "" {
				continue
			}
			reason := leaf.Error()
			if worded := wordSchemaError(leaf, compiled.document, document); len(worded) > 0 {
				reason = worded[0].Message
			}
			found[place.componentPath] = Drop{Path: place.componentPath, ComponentID: place.componentID(), Reason: reason}
		}
		if len(found) == 0 {
			return drops, checkSchema(document)
		}
		removed := outermost(found)
		drops = append(drops, removed...)
		document["components"] = removeComponents(document["components"], "components", removed)
	}
	return drops, errors.New("the visual could not be read")
}

// coerceComponents runs the coerce hook of each component's type on its
// payload, nested components included.
func coerceComponents(value interface{}) {
	list, _ := value.([]interface{})
	for _, entry := range list {
		component, _ := entry.(map[string]interface{})
		name, _ := component["type"].(string)
		spec, known := catalogue[name]
		if !known {
			continue
		}
		if payload, ok := component[spec.payload].(map[string]interface{}); ok && spec.coerce != nil {
			spec.coerce(payload)
		}
		coerceComponents(component["children"])
		for _, tab := range objectsAt(component, "tabs") {
			coerceComponents(tab["children"])
		}
	}
}

// repairLeaf repairs one schema failure in place when it has one obvious
// reading, and reports whether it did.
func repairLeaf(document map[string]interface{}, leaf *jsonschema.ValidationError) bool {
	value, replace, ok := locate(document, leaf.InstanceLocation)
	if !ok {
		return false
	}
	switch failure := leaf.ErrorKind.(type) {
	case *kind.Type:
		for _, want := range failure.Want {
			if coerced, ok := coerceScalar(value, want); ok {
				replace(coerced)
				return true
			}
		}
	case *kind.MaxLength:
		if text, isText := value.(string); isText && failure.Want > 0 {
			replace(cutText(text, failure.Want))
			return true
		}
	case *kind.MaxItems:
		if list, isList := value.([]interface{}); isList && len(list) > failure.Want {
			replace(list[:failure.Want])
			return true
		}
	case *kind.MaxContains:
		// Only the components of a known type count; the list is cut
		// before the first one past the maximum.
		if list, isList := value.([]interface{}); isList && len(failure.Got) > failure.Want && failure.Got[failure.Want] < len(list) {
			replace(list[:failure.Got[failure.Want]])
			return true
		}
	}
	return false
}

// locate finds the value at a schema failure's location and a function
// that replaces it.
func locate(document map[string]interface{}, location []string) (interface{}, func(interface{}), bool) {
	if len(location) == 0 {
		return nil, nil, false
	}
	var parent interface{} = document
	for _, token := range location[:len(location)-1] {
		next, ok := child(parent, token)
		if !ok {
			return nil, nil, false
		}
		parent = next
	}
	last := location[len(location)-1]
	value, ok := child(parent, last)
	if !ok {
		return nil, nil, false
	}
	switch node := parent.(type) {
	case map[string]interface{}:
		return value, func(replacement interface{}) { node[last] = replacement }, true
	case []interface{}:
		index, _ := strconv.Atoi(last)
		return value, func(replacement interface{}) { node[index] = replacement }, true
	}
	return nil, nil, false
}

func child(node interface{}, token string) (interface{}, bool) {
	switch value := node.(type) {
	case map[string]interface{}:
		entry, ok := value[token]
		return entry, ok
	case []interface{}:
		index, err := strconv.Atoi(token)
		if err != nil || index < 0 || index >= len(value) {
			return nil, false
		}
		return value[index], true
	}
	return nil, false
}

// outermost is the drops to make, in path order, leaving out those inside
// another one: a component left out takes its children with it.
func outermost(found map[string]Drop) []Drop {
	paths := make([]string, 0, len(found))
	for path := range found {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var out []Drop
	for _, path := range paths {
		inside := false
		for _, other := range paths {
			if other != path && strings.HasPrefix(path, other+".") {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, found[path])
		}
	}
	return out
}

// removeComponents is a list of components without those at the dropped
// paths, which are paths in the list as it is.
func removeComponents(value interface{}, prefix string, drops []Drop) []interface{} {
	gone := make(map[string]bool, len(drops))
	for _, drop := range drops {
		gone[drop.Path] = true
	}
	var remove func(value interface{}, prefix string) []interface{}
	remove = func(value interface{}, prefix string) []interface{} {
		list, _ := value.([]interface{})
		kept := make([]interface{}, 0, len(list))
		for index, entry := range list {
			path := fmt.Sprintf("%s[%d]", prefix, index)
			if gone[path] {
				continue
			}
			if component, ok := entry.(map[string]interface{}); ok {
				if children, ok := component["children"].([]interface{}); ok {
					component["children"] = remove(children, path+".children")
				}
				if tabs, ok := component["tabs"].([]interface{}); ok {
					for tabIndex, entry := range tabs {
						if tab, ok := entry.(map[string]interface{}); ok {
							if children, ok := tab["children"].([]interface{}); ok {
								tab["children"] = remove(children, fmt.Sprintf("%s.tabs[%d].children", path, tabIndex))
							}
						}
					}
				}
			}
			kept = append(kept, entry)
		}
		return kept
	}
	return remove(value, prefix)
}

// coerceScalar reads a JSON value as the type want when it is a value of
// another type with exactly one obvious reading:
//   - string: a number as written, or true or false;
//   - number: a string that is a finite number once spaces around it and
//     thousands separators (1,250) are taken out; integer only when whole;
//   - boolean: the string true or false;
//   - array: any single value, as a list of one.
//
// It reports false when value already has the type or has no such reading.
// The schema's reading and the coerce hooks of the type specs both use it.
func coerceScalar(value interface{}, want string) (interface{}, bool) {
	switch want {
	case "string":
		switch scalar := value.(type) {
		case json.Number:
			return scalar.String(), true
		case float64:
			return strconv.FormatFloat(scalar, 'f', -1, 64), true
		case bool:
			return strconv.FormatBool(scalar), true
		}
	case "number", "integer":
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		number, ok := parseNumber(text)
		if !ok || want == "integer" && number != math.Trunc(number) {
			return nil, false
		}
		return json.Number(strconv.FormatFloat(number, 'f', -1, 64)), true
	case "boolean":
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	case "array":
		if _, isList := value.([]interface{}); !isList {
			return []interface{}{value}, true
		}
	}
	return nil, false
}

var (
	plainNumber     = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	thousandsNumber = regexp.MustCompile(`^[+-]?\d{1,3}(,\d{3})+(\.\d*)?$`)
)

// parseNumber reads text as a finite number. Commas are thousands
// separators only between groups of three digits: "1,5" has two readings
// and is not a number.
func parseNumber(text string) (float64, bool) {
	text = strings.TrimSpace(text)
	if thousandsNumber.MatchString(text) {
		text = strings.ReplaceAll(text, ",", "")
	}
	if !plainNumber.MatchString(text) {
		return 0, false
	}
	number, err := strconv.ParseFloat(text, 64)
	return number, err == nil && !math.IsInf(number, 0) && !math.IsNaN(number)
}

// cutText cuts text to at most maximum characters, at the last word
// boundary when there is one in its second half, and ends it with "…".
func cutText(text string, maximum int) string {
	runes := []rune(text)
	if len(runes) <= maximum {
		return text
	}
	if maximum < 2 {
		return string(runes[:maximum])
	}
	cut := runes[:maximum-1]
	for index := len(cut) - 1; index > len(cut)/2; index-- {
		if unicode.IsSpace(cut[index]) {
			cut = cut[:index]
			break
		}
	}
	kept := strings.TrimRightFunc(string(cut), func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;:-", r) })
	return kept + "…"
}
