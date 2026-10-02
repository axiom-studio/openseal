package runbook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func eventWaitDefinition() *Definition {
	return &Definition{APIVersion: APIVersion, ID: "event-follow-up", Version: "1", Name: "Event follow-up", Entrypoints: map[string]string{"manual": "wait"}, Steps: map[string]Step{
		"wait": {Kind: StepWait, Wait: &WaitStep{
			Match:      &EventMatch{Type: "conversation.message.received", Source: ref("/input/source"), Subject: ref("/input/subject"), Attributes: map[string]Value{"sender": ref("/input/sender")}, TimeoutDuration: time.Hour},
			ResultPath: "/state/reply", Next: "matched", TimeoutNext: "timed-out",
		}},
		"matched": {Kind: StepEnd, End: &EndStep{}}, "timed-out": {Kind: StepEnd, End: &EndStep{}},
	}}
}

func TestValidateEventWaitIncludesTimeoutGraphEdge(t *testing.T) {
	definition := eventWaitDefinition()
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("valid event wait diagnostics=%#v", diagnostics)
	}
	definition.Steps["timed-out"] = Step{Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"/state/retry": literal(true)}, Next: "wait"}}
	diagnostics := Validate(definition)
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "graph.implicit_cycle" {
			return
		}
	}
	t.Fatalf("timeout branch cycle was not rejected: %#v", diagnostics)
}

func TestValidateEventWaitRejectsUnboundedOrInvalidSelectors(t *testing.T) {
	cases := []struct {
		name string
		edit func(*WaitStep)
		code string
	}{
		{"wildcard type", func(w *WaitStep) { w.Match.Type = "*" }, "wait.event_type_invalid"},
		{"wildcard source", func(w *WaitStep) { w.Match.Source = literal("*") }, "wait.selector_invalid"},
		{"empty subject", func(w *WaitStep) { w.Match.Subject = literal("") }, "wait.selector_invalid"},
		{"missing source", func(w *WaitStep) { w.Match.Source = Value{} }, "value.source"},
		{"null attribute", func(w *WaitStep) { w.Match.Attributes["sender"] = literal(nil) }, "wait.attribute_scalar"},
		{"object attribute", func(w *WaitStep) { w.Match.Attributes["sender"] = literal(map[string]string{"id": "sender"}) }, "wait.attribute_scalar"},
		{"invalid attribute name", func(w *WaitStep) { w.Match.Attributes["bad\nname"] = literal("sender") }, "wait.attribute_name_invalid"},
		{"zero timeout", func(w *WaitStep) { w.Match.TimeoutDuration = 0 }, "wait.timeout_invalid"},
		{"oversized timeout", func(w *WaitStep) { w.Match.TimeoutDuration = 30*24*time.Hour + time.Second }, "wait.timeout_invalid"},
		{"invalid after", func(w *WaitStep) { value := literal("yesterday"); w.Match.After = &value }, "wait.after_invalid"},
		{"non-string after", func(w *WaitStep) { value := literal(42); w.Match.After = &value }, "wait.after_invalid"},
		{"ambiguous wait", func(w *WaitStep) { w.Duration = time.Minute }, "wait.source"},
		{"unknown timeout branch", func(w *WaitStep) { w.TimeoutNext = "missing" }, "step.reference_unknown"},
		{"invalid result path", func(w *WaitStep) { w.ResultPath = "reply" }, "pointer.invalid"},
		{"reserved result path", func(w *WaitStep) { w.ResultPath = "/runbook/waitKey" }, "wait.result_reserved"},
		{"reserved wait result", func(w *WaitStep) { w.ResultPath = "/lastEventWait" }, "wait.result_reserved"},
		{"too many attributes", func(w *WaitStep) {
			w.Match.Attributes = map[string]Value{}
			for index := range 17 {
				w.Match.Attributes[strings.Repeat("a", index+1)] = literal(true)
			}
		}, "wait.attributes_limit"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			definition := eventWaitDefinition()
			item.edit(definition.Steps["wait"].Wait)
			for _, diagnostic := range Validate(definition) {
				if diagnostic.Code == item.code {
					return
				}
			}
			t.Fatalf("missing diagnostic %s: %#v", item.code, Validate(definition))
		})
	}
}

func TestAuthoringSchemaProjectsEventMatchFields(t *testing.T) {
	const prefix = "Portable Runbook schema projection (generated from canonical OpenSeal types): "
	var projection authoringSchemaProjection
	if err := json.Unmarshal([]byte(strings.TrimPrefix(AuthoringSchemaProjection(), prefix)), &projection); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"type", "source", "subject", "attributes", "after", "timeoutDuration"} {
		if !containsField(projection.EventMatchFields, name) {
			t.Fatalf("missing event match field %s: %#v", name, projection)
		}
	}
	for _, name := range []string{"match", "resultPath", "timeoutNext"} {
		if !containsField(projection.StepKinds[StepWait].PayloadFields, name) {
			t.Fatalf("missing wait field %s: %#v", name, projection)
		}
	}
}
