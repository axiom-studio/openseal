package routine

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func initializeRunbookRuntime(checkpoint map[string]interface{}, run *models.AgentRun) {
	rootRunID := run.RootRunID
	if rootRunID == "" {
		rootRunID = run.ID
	}
	checkpoint["runtime"] = map[string]interface{}{
		"runId":       run.ID,
		"rootRunId":   rootRunID,
		"objectiveId": run.ObjectiveID,
	}
}

func initializeRunbookInput(checkpoint map[string]interface{}, run *models.AgentRun) {
	if _, ok := checkpoint["input"]; ok {
		return
	}
	input := maps.Clone(run.Context)
	if input == nil {
		input = map[string]interface{}{}
	}
	if trigger, ok := checkpoint["trigger"].(map[string]interface{}); ok {
		if payload, ok := trigger["payload"].(map[string]interface{}); ok {
			for key, value := range payload {
				input[key] = value
			}
		}
	}
	checkpoint["input"] = input
}

func decodeRunbookState(checkpoint map[string]interface{}) (runbookExecutionState, error) {
	var state runbookExecutionState
	if raw, ok := checkpoint["runbook"].(map[string]interface{}); ok {
		encoded, _ := json.Marshal(raw)
		if err := json.Unmarshal(encoded, &state); err != nil {
			return state, fmt.Errorf("decode runbook checkpoint: %w", err)
		}
	}
	return state, nil
}

func encodeRunbookState(checkpoint map[string]interface{}, state runbookExecutionState) {
	encoded, _ := json.Marshal(state)
	var raw map[string]interface{}
	_ = json.Unmarshal(encoded, &raw)
	checkpoint["runbook"] = raw
}

func resolveRunbookValue(root map[string]interface{}, value runbook.Value) (interface{}, error) {
	if value.Ref != "" {
		return getRunbookPointer(root, string(value.Ref))
	}
	if len(value.Template) > 0 {
		var builder strings.Builder
		for _, segment := range value.Template {
			if segment.Text != "" {
				builder.WriteString(segment.Text)
				continue
			}
			resolved, err := getRunbookPointer(root, string(segment.Ref))
			if err != nil {
				// Embedded templates preserve ordinary shell/UI interpolation
				// semantics: an absent optional value contributes an empty string.
				// Exact Ref values remain strict and fail above.
				continue
			}
			switch typed := resolved.(type) {
			case string:
				builder.WriteString(typed)
			case nil:
				builder.WriteString("null")
			default:
				encoded, encodeErr := json.Marshal(typed)
				if encodeErr != nil {
					return nil, encodeErr
				}
				builder.Write(encoded)
			}
		}
		return builder.String(), nil
	}
	var result interface{}
	if err := json.Unmarshal(value.Literal, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func getRunbookPointer(root map[string]interface{}, pointer string) (interface{}, error) {
	var current interface{} = root
	for _, encoded := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]interface{}:
			var ok bool
			current, ok = typed[segment]
			if !ok {
				return nil, fmt.Errorf("JSON Pointer %q does not exist", pointer)
			}
		case []interface{}:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("JSON Pointer %q has invalid array index", pointer)
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("JSON Pointer %q traverses a scalar", pointer)
		}
	}
	return current, nil
}

func setRunbookPointer(root map[string]interface{}, pointer string, value interface{}) error {
	segments := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	if pointer == "" || !strings.HasPrefix(pointer, "/") || len(segments) == 0 {
		return fmt.Errorf("invalid JSON Pointer %q", pointer)
	}
	current := root
	for _, encoded := range segments[:len(segments)-1] {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		child, ok := current[segment].(map[string]interface{})
		if !ok {
			child = map[string]interface{}{}
			current[segment] = child
		}
		current = child
	}
	last := strings.ReplaceAll(strings.ReplaceAll(segments[len(segments)-1], "~1", "/"), "~0", "~")
	current[last] = value
	return nil
}

func escapeRunbookPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func evaluateRunbookPredicate(root map[string]interface{}, predicate runbook.Predicate) (bool, error) {
	switch predicate.Operator {
	case runbook.PredicateAll:
		for _, child := range predicate.Operands {
			ok, err := evaluateRunbookPredicate(root, child)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	case runbook.PredicateAny:
		for _, child := range predicate.Operands {
			ok, err := evaluateRunbookPredicate(root, child)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case runbook.PredicateNot:
		ok, err := evaluateRunbookPredicate(root, predicate.Operands[0])
		return !ok, err
	}
	left, err := resolveRunbookValue(root, *predicate.Left)
	if err != nil {
		return false, err
	}
	if predicate.Operator == runbook.PredicateExists {
		return left != nil, nil
	}
	if predicate.Operator == runbook.PredicateTruthy {
		return truthy(left), nil
	}
	right, err := resolveRunbookValue(root, *predicate.Right)
	if err != nil {
		return false, err
	}
	switch predicate.Operator {
	case runbook.PredicateEqual:
		return reflect.DeepEqual(left, right), nil
	case runbook.PredicateNotEqual:
		return !reflect.DeepEqual(left, right), nil
	case runbook.PredicateContains:
		return strings.Contains(fmt.Sprint(left), fmt.Sprint(right)), nil
	}
	l, okL := numeric(left)
	rr, okR := numeric(right)
	if !okL || !okR {
		return false, errors.New("ordered predicates require numeric operands")
	}
	switch predicate.Operator {
	case runbook.PredicateGreater:
		return l > rr, nil
	case runbook.PredicateAtLeast:
		return l >= rr, nil
	case runbook.PredicateLess:
		return l < rr, nil
	case runbook.PredicateAtMost:
		return l <= rr, nil
	}
	return false, fmt.Errorf("unsupported predicate %q", predicate.Operator)
}

func truthy(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case []interface{}:
		return len(typed) > 0
	case map[string]interface{}:
		return len(typed) > 0
	default:
		return true
	}
}

func numeric(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		value, err := typed.Float64()
		return value, err == nil
	}
	return 0, false
}
