package authoring

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestAuthoringResultSchemaOwnsExactEventWaitShape(t *testing.T) {
	schema, err := AuthoringResultJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	match := findAuthoringObjectSchema(schema, "type", "source", "subject", "attributes", "after", "timeoutDuration")
	if match == nil {
		t.Fatal("canonical EventMatch schema was not generated")
	}
	if match["additionalProperties"] != false {
		t.Fatalf("event match object is not closed: %#v", match)
	}
	wait := findAuthoringObjectSchema(schema, "duration", "event", "match", "resultPath", "timeoutNext", "next")
	if wait == nil {
		t.Fatal("canonical WaitStep schema was not generated")
	}
	wrapper := map[string]interface{}{
		"type": "object", "$defs": schema["$defs"],
		"properties": map[string]interface{}{"wait": wait}, "required": []string{"wait"},
	}
	payload, err := json.Marshal(authoringEventWait("done", "timeout").Wait)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]interface{}
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	if err := runbook.ValidateInterfaceInput(wrapper, map[string]interface{}{"wait": object}); err != nil {
		t.Fatalf("canonical event wait was rejected by generated schema: %v", err)
	}
	object["duration"] = float64(time.Minute)
	if err := runbook.ValidateInterfaceInput(wrapper, map[string]interface{}{"wait": object}); err == nil {
		t.Fatal("generated schema accepted competing duration and event match sources")
	}
}

func authoringEventWait(next, timeoutNext string) runbook.Step {
	return runbook.Step{Kind: runbook.StepWait, Wait: &runbook.WaitStep{
		Match: &runbook.EventMatch{Type: "conversation.message.received", Source: literalActionValue("binding:connection"), Subject: literalActionValue("thread:123"), TimeoutDuration: time.Hour},
		Next:  next, TimeoutNext: timeoutNext,
	}}
}

func TestRunbookTimeoutBranchCannotUseUnproducedActionResult(t *testing.T) {
	candidate := browserRunbookActionContractCandidate()
	definition := candidate.Agents[0].Runbook
	definition.Entrypoints["daily"] = "wait"
	definition.Steps["wait"] = authoringEventWait("start", "navigate")
	// The matched path creates a browser session. The timeout path skips that
	// producer, so navigate cannot safely read its result at the merge.
	if !runbookStepReaches(definition, "wait", "navigate") || runbookStepDominates(definition, "start", "navigate") {
		t.Fatal("timeout path was omitted from action dataflow analysis")
	}
	issues := validateRunbookActionContracts(&candidate, browserRunbookActionContractCatalog())
	if !hasValidationCode(issues, "runbook_action_reference_unavailable") {
		t.Fatalf("timeout branch accepted an unavailable action result: %#v", issues)
	}
}

func TestRunbookTimeoutBranchRequiresEnoughHostedBudget(t *testing.T) {
	definition := &runbook.Definition{
		Entrypoints: map[string]string{"operate": "wait"},
		Triggers:    map[string]runbook.Trigger{"operate": {Entrypoint: "operate", Budget: &runbook.BudgetAllocation{MaxActions: 1}}},
		Steps: map[string]runbook.Step{
			"wait": authoringEventWait("done", "work"),
			"work": {Kind: runbook.StepDelegate, Delegate: &runbook.DelegateStep{Next: "done"}},
			"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
		},
	}
	issues := validateHostedTriggerCapacity(0, definition, "work", &runbook.BudgetAllocation{MaxActions: 10}, runbook.BudgetAllocation{MaxActions: 10})
	if !hasValidationCode(issues, "hosted_parent_actions_insufficient") {
		t.Fatalf("underfunded timeout branch was accepted: %#v", issues)
	}
}
