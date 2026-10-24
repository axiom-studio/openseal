package daemon

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// hostedTurnDecodingSchema strengthens the turn form schema for decoders that
// constrain output token by token. Grammar decoding tends to emit every
// optional property, so lifecycle rules the kernel enforces become one
// complete object branch per nextRunStatus group under a root oneOf, the only
// combinator form verified against llama.cpp grammar conversion. Strict
// decoding and every existing check remain authoritative.
func hostedTurnDecodingSchema(schema map[string]interface{}, completionEvidenceRefs []string) (map[string]interface{}, error) {
	base, err := cloneJSONSchemaObject(schema)
	if err != nil {
		return nil, err
	}
	defs, _ := base["$defs"].(map[string]interface{})
	delete(base, "$defs")
	delete(base, "$schema")
	properties, _ := base["properties"].(map[string]interface{})
	wake, err := resolveTurnSchemaRef(defs, properties["wakeCondition"])
	if err != nil {
		return nil, fmt.Errorf("wakeCondition: %w", err)
	}
	// ValidateHostedTurnLifecycle requires a non-blank type and allows
	// eventWait only on an event wake while waiting_for_event.
	plainWake, err := cloneJSONSchemaObject(wake)
	if err != nil {
		return nil, err
	}
	delete(plainWake["properties"].(map[string]interface{}), "eventWait")
	plainWake["properties"].(map[string]interface{})["type"] = map[string]interface{}{"type": "string", "minLength": 1, "pattern": `\S`}
	eventWake, err := cloneJSONSchemaObject(wake)
	if err != nil {
		return nil, err
	}
	eventWake["properties"].(map[string]interface{})["type"] = map[string]interface{}{"type": "string", "enum": []string{"event"}}
	eventWake["required"] = []interface{}{"type", "eventWait"}

	_, canStartTask := properties["proposedTask"]
	// group is one nextRunStatus branch. completed alone may carry generated
	// files, and only completion evidence is checked against receipts.
	type group struct {
		statuses      []runtime.AgentRunStatus
		wakeCondition interface{}
		requireWake   bool
		completed     bool
	}
	variant := func(g group) (map[string]interface{}, error) {
		branch, err := cloneJSONSchemaObject(base)
		if err != nil {
			return nil, err
		}
		branchProperties := branch["properties"].(map[string]interface{})
		branchProperties["nextRunStatus"] = map[string]interface{}{"type": "string", "enum": g.statuses}
		delete(branchProperties, "proposedTask")
		delete(branchProperties, "wakeCondition")
		if g.wakeCondition != nil {
			branchProperties["wakeCondition"] = g.wakeCondition
		}
		if g.requireWake {
			branch["required"] = append(branch["required"].([]interface{}), "wakeCondition")
		}
		if g.completed {
			refs := map[string]interface{}{"type": "array", "maxItems": 0}
			if len(completionEvidenceRefs) > 0 {
				refs = map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "enum": completionEvidenceRefs}}
			}
			branchProperties["completionEvidenceRefs"] = refs
		} else {
			// generatedFiles() rejects the key off completion whatever its
			// value, and a grammar cannot exclude one key of an open object,
			// so runOutput is closed to its declared fields here. It declares
			// the keys the kernel reads before completion: summary carries a
			// waiting run's clarification question, silent a quiet source
			// challenge. Other documented keys belong to completing turns.
			runOutput := branchProperties["runOutput"].(map[string]interface{})
			outputProperties := runOutput["properties"].(map[string]interface{})
			delete(outputProperties, "generatedFiles")
			outputProperties["summary"] = map[string]interface{}{"type": "string"}
			outputProperties["silent"] = map[string]interface{}{"type": "boolean"}
			runOutput["additionalProperties"] = false
		}
		return branch, nil
	}
	groups := []group{
		{statuses: []runtime.AgentRunStatus{runtime.AgentRunStatusCompleted}, completed: true},
		{statuses: []runtime.AgentRunStatus{runtime.AgentRunStatusRunning, runtime.AgentRunStatusPaused, runtime.AgentRunStatusFailed}},
		{statuses: []runtime.AgentRunStatus{runtime.AgentRunStatusSleeping, runtime.AgentRunStatusWaitingForDependency, runtime.AgentRunStatusWaitingForAgent}, wakeCondition: plainWake, requireWake: true},
		{statuses: []runtime.AgentRunStatus{runtime.AgentRunStatusWaitingForEvent}, wakeCondition: map[string]interface{}{"oneOf": []interface{}{plainWake, eventWake}}, requireWake: true},
		// The compiler rewrites waiting_for_approval to running and drops any
		// wake, so the wake stays optional and unconstrained.
		{statuses: []runtime.AgentRunStatus{runtime.AgentRunStatusWaitingForApproval}, wakeCondition: properties["wakeCondition"]},
	}
	branches := make([]interface{}, 0, len(groups)+1)
	for _, g := range groups {
		branch, err := variant(g)
		if err != nil {
			return nil, err
		}
		branches = append(branches, branch)
	}
	if canStartTask {
		// validateTaskProposalOutput: a task proposal leaves the Run running,
		// stands alone, and keeps every output empty until durable admission.
		task, err := variant(group{statuses: []runtime.AgentRunStatus{runtime.AgentRunStatusRunning}})
		if err != nil {
			return nil, err
		}
		taskProperties := task["properties"].(map[string]interface{})
		taskProperties["proposedTask"] = properties["proposedTask"]
		for _, name := range []string{"proposedAction", "proposedWorkspaceOperation", "proposedFork", "proposedDelegation", "proposedRunbook", "progressSummary"} {
			delete(taskProperties, name)
		}
		taskProperties["outputSummary"] = map[string]interface{}{"type": "string", "maxLength": 0}
		taskProperties["runError"] = map[string]interface{}{"type": "string", "maxLength": 0}
		taskProperties["runOutput"] = map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{}}
		task["required"] = append(task["required"].([]interface{}), "proposedTask")
		branches = append(branches, task)
	}
	decoding := map[string]interface{}{"$defs": defs, "oneOf": branches}
	adaptDecodingSchemaForGrammar(decoding)
	return decoding, nil
}

// largeDecodingBound is the smallest length or item bound dropped from the
// decoding schema. llama.cpp fails to parse some large repetition bounds (a
// maxLength or maxItems of exactly 2000, which reviewContext uses), and bounds
// this large give a model nothing usable. Local validation still enforces them.
const largeDecodingBound = 1000

// adaptDecodingSchemaForGrammar works around two llama.cpp grammar
// conversion gaps, both verified against Ollama: it drops large bounds, and it
// gives every bounded array an items schema, without which maxItems is
// silently ignored.
func adaptDecodingSchemaForGrammar(value interface{}) {
	switch node := value.(type) {
	case map[string]interface{}:
		for _, keyword := range []string{"maxLength", "maxItems"} {
			if bound, ok := node[keyword].(float64); ok && bound >= largeDecodingBound {
				delete(node, keyword)
			}
		}
		if _, bounded := node["maxItems"]; bounded && node["items"] == nil {
			node["items"] = map[string]interface{}{}
		}
		for _, child := range node {
			adaptDecodingSchemaForGrammar(child)
		}
	case []interface{}:
		for _, child := range node {
			adaptDecodingSchemaForGrammar(child)
		}
	}
}

func resolveTurnSchemaRef(defs map[string]interface{}, node interface{}) (map[string]interface{}, error) {
	object, ok := node.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("schema node %v is not an object", node)
	}
	ref, ok := object["$ref"].(string)
	if !ok {
		return object, nil
	}
	definition, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]interface{})
	if !ok || !strings.HasPrefix(ref, "#/$defs/") {
		return nil, fmt.Errorf("schema reference %q does not name a local definition", ref)
	}
	return definition, nil
}

// cloneJSONSchemaObject deep-copies a schema through its JSON form, which
// also normalizes typed slices such as []string to []interface{}.
func cloneJSONSchemaObject(schema map[string]interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode turn schema: %w", err)
	}
	var clone map[string]interface{}
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return nil, fmt.Errorf("decode turn schema: %w", err)
	}
	return clone, nil
}
