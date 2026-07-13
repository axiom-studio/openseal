package runtime

import (
	"errors"
	"fmt"
	"strings"
)

func resolveTurnActionInput(checkpoint map[string]interface{}, reference string) (map[string]interface{}, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return map[string]interface{}{}, nil
	}
	if strings.HasPrefix(reference, "#") {
		reference = strings.TrimPrefix(reference, "#")
	}
	// Hosted planners sometimes make the documented checkpoint root explicit.
	// The resolver already receives that object as its root, so accept only this
	// exact, credential-free alias before applying the normal bounded pointer.
	const checkpointRoot = "/continuationCheckpoint"
	if reference == checkpointRoot {
		return nil, errors.New("action inputRef must select an object inside continuationCheckpoint")
	}
	if strings.HasPrefix(reference, checkpointRoot+"/") {
		reference = strings.TrimPrefix(reference, checkpointRoot)
	}
	if !strings.HasPrefix(reference, "/") {
		return nil, errors.New("action inputRef must be a JSON Pointer into continuationCheckpoint")
	}
	var current interface{} = checkpoint
	for _, encoded := range strings.Split(strings.TrimPrefix(reference, "/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("action inputRef %q traverses a non-object", reference)
		}
		current, ok = object[segment]
		if !ok {
			return nil, fmt.Errorf("action inputRef %q does not exist", reference)
		}
	}
	arguments, ok := current.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("action inputRef %q must resolve to an object", reference)
	}
	return cloneMap(arguments), nil
}
