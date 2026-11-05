package skill

import (
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/fileinput"
	"github.com/axiom-studio/openseal/pkg/toolfile"
)

// checkFileInput returns input with each {"$file": path} value replaced by a
// placeholder of its field's shape, for schema validation, and where each one
// goes in the action input. A binding tool's input object is checked against
// the tool's own schema, every other field against the action's.
func checkFileInput(bound *BoundAction, input map[string]interface{}) (map[string]interface{}, []toolfile.Reference, error) {
	if bound == nil || bound.Definition == nil || bound.Binding == nil {
		return nil, nil, errors.New("bound skill action is required")
	}
	spec := bound.Definition.BindingTools
	rest := input
	var checkedArguments interface{}
	var references []toolfile.Reference
	if spec.RoutesAction(bound.Action.Name) {
		if arguments, ok := input[spec.InputArgument]; ok {
			var schema map[string]interface{}
			if tool := SelectedBindingTool(bound.Definition, bound.Binding, bound.Action.Name, input); tool != nil {
				schema = tool.InputSchema
			}
			checked, found, err := toolfile.Check(schema, arguments, "/"+toolfile.Escape(spec.InputArgument))
			if err != nil {
				return nil, nil, err
			}
			checkedArguments, references = checked, found
			rest = make(map[string]interface{}, len(input))
			for key, value := range input {
				if key != spec.InputArgument {
					rest[key] = value
				}
			}
		}
	}
	checked, found, err := toolfile.Check(fileinput.Schema(bound.Action.InputSchema), rest, "")
	if err != nil {
		return nil, nil, err
	}
	result, _ := checked.(map[string]interface{})
	if result == nil {
		result = map[string]interface{}{}
	}
	if checkedArguments != nil {
		result[spec.InputArgument] = checkedArguments
	}
	return result, append(references, found...), nil
}

// FileReferences are the {"$file": path} values of a bound action's input,
// by where each goes in the action's transport arguments. A file in a field
// that takes no file content is refused.
func FileReferences(bound *BoundAction, input map[string]interface{}) ([]toolfile.Reference, error) {
	_, references, err := checkFileInput(bound, input)
	if err != nil || len(references) == 0 {
		return nil, err
	}
	transport := bound.Definition.Transport
	if bound.Action.Transport != nil {
		transport = *bound.Action.Transport
	}
	if len(transport.Arguments) == 0 {
		return references, nil
	}
	// A transport that renames its arguments moves each file with its
	// argument; one it does not forward is not sent.
	mapped := make([]toolfile.Reference, 0, len(references))
	for _, reference := range references {
		first, rest, _ := strings.Cut(strings.TrimPrefix(reference.Pointer, "/"), "/")
		for name, mapping := range transport.Arguments {
			if mapping.SourceArgument != "" && toolfile.Escape(mapping.SourceArgument) == first {
				moved := reference
				moved.Pointer = "/" + toolfile.Escape(name)
				if rest != "" {
					moved.Pointer += "/" + rest
				}
				mapped = append(mapped, moved)
			}
		}
	}
	return mapped, nil
}
