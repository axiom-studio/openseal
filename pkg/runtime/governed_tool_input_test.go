package runtime

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func governedTestAction(sideEffect capability.SideEffect, risk capability.RiskLevel, policy capability.ExternalOperationPolicy) capability.ModelAction {
	return capability.ModelAction{
		Name: "demo.send", SkillID: "demo", Action: "send", Risk: risk, SideEffect: sideEffect,
		ExternalOperationPolicy: policy,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"text":           map[string]interface{}{"type": "string"},
				"idempotencyKey": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"text", "idempotencyKey"},
		},
	}
}

func schemaJSON(t *testing.T, schema map[string]interface{}) map[string]interface{} {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func requiredOf(schema map[string]interface{}) []string {
	var names []string
	for _, value := range schema["required"].([]interface{}) {
		names = append(names, value.(string))
	}
	return names
}

func expectRejection(t *testing.T, err error, code string) *ActionProposalRejection {
	t.Helper()
	var rejection *ActionProposalRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("error = %v, want *ActionProposalRejection %s", err, code)
	}
	if rejection.Code != code {
		t.Fatalf("code = %q (%s), want %q", rejection.Code, rejection.Message, code)
	}
	if strings.TrimSpace(rejection.Message) == "" {
		t.Fatal("rejection has no model-visible message")
	}
	return rejection
}

func TestGovernedSchemaReviewedEnvelope(t *testing.T) {
	action := governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationRequired)
	action.InputSchema["$defs"] = map[string]interface{}{"t": map[string]interface{}{"type": "string"}}
	action.InputSchema["properties"].(map[string]interface{})["ref"] = map[string]interface{}{"$ref": "#/$defs/t"}
	if !GovernedToolReviewed(action) || GovernedToolNeedsReviewSummary(action) {
		t.Fatal("external action must be reviewed and carry no _reviewSummary")
	}
	schema := schemaJSON(t, GovernedToolInputSchema(action))
	if schema["additionalProperties"] != false || schema["type"] != "object" {
		t.Fatalf("envelope not closed: %v", schema)
	}
	if got := requiredOf(schema); !reflect.DeepEqual(got, []string{"arguments", "reviewContext", "externalOperation"}) {
		t.Fatalf("required = %v", got)
	}
	properties := schema["properties"].(map[string]interface{})
	arguments := properties["arguments"].(map[string]interface{})
	if _, ok := arguments["properties"].(map[string]interface{})["idempotencyKey"]; ok {
		t.Fatal("idempotencyKey is host-owned and must not be offered")
	}
	if got := requiredOf(arguments); !reflect.DeepEqual(got, []string{"text"}) {
		t.Fatalf("arguments required = %v", got)
	}
	if ref := arguments["properties"].(map[string]interface{})["ref"].(map[string]interface{})["$ref"]; ref != "#/properties/arguments/$defs/t" {
		t.Fatalf("local ref not rebased: %v", ref)
	}
	if _, ok := action.InputSchema["properties"].(map[string]interface{})["idempotencyKey"]; !ok {
		t.Fatal("the action's own schema was mutated")
	}
	review := properties["reviewContext"].(map[string]interface{})
	if got := requiredOf(review); !reflect.DeepEqual(got, []string{"summary", "content"}) {
		t.Fatalf("reviewContext required = %v", got)
	}
	if max := review["properties"].(map[string]interface{})["summary"].(map[string]interface{})["maxLength"]; max != float64(2000) {
		t.Fatalf("summary maxLength = %v", max)
	}
	external := properties["externalOperation"].(map[string]interface{})
	if external["additionalProperties"] != false {
		t.Fatal("externalOperation not closed")
	}
	operation := external["properties"].(map[string]interface{})["operation"].(map[string]interface{})
	if operation["pattern"] != `^[^\r\n?#@/\\]+$` || operation["maxLength"] != float64(256) {
		t.Fatalf("operation schema = %v", operation)
	}
	refs := properties["evidenceRefs"].(map[string]interface{})["items"].(map[string]interface{})
	if refs["description"] != "An evidenceRef from an earlier tool result, e.g. action-call:<id>." {
		t.Fatalf("evidenceRefs items = %v", refs)
	}

	optional := governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationOptional)
	optionalSchema := schemaJSON(t, GovernedToolInputSchema(optional))
	if got := requiredOf(optionalSchema); !reflect.DeepEqual(got, []string{"arguments", "reviewContext"}) {
		t.Fatalf("optional required = %v", got)
	}
	if _, ok := optionalSchema["properties"].(map[string]interface{})["externalOperation"]; !ok {
		t.Fatal("optional policy must offer externalOperation")
	}
	forbidden := governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationForbidden)
	if _, ok := schemaJSON(t, GovernedToolInputSchema(forbidden))["properties"].(map[string]interface{})["externalOperation"]; ok {
		t.Fatal("forbidden policy must not offer externalOperation")
	}
	destructive := governedTestAction(capability.SideEffectDestructive, capability.RiskLevelDestructive, "")
	destructiveSchema := schemaJSON(t, GovernedToolInputSchema(destructive))
	if _, ok := destructiveSchema["properties"].(map[string]interface{})["externalOperation"]; ok {
		t.Fatal("destructive actions have no externalOperation")
	}
	if !GovernedToolReviewed(destructive) {
		t.Fatal("destructive action must be reviewed")
	}
}

func TestGovernedSchemaWriteAddsRequiredReviewSummary(t *testing.T) {
	for _, action := range []capability.ModelAction{
		governedTestAction(capability.SideEffectWrite, capability.RiskLevelWrite, ""),
		governedTestAction(capability.SideEffectRead, capability.RiskLevelProduction, ""),
	} {
		if !GovernedToolNeedsReviewSummary(action) {
			t.Fatalf("%s/%s must need a review summary", action.SideEffect, action.Risk)
		}
		schema := schemaJSON(t, GovernedToolInputSchema(action))
		if got := requiredOf(schema); !reflect.DeepEqual(got, []string{"text", GovernedReviewSummaryField}) {
			t.Fatalf("required = %v", got)
		}
		field := schema["properties"].(map[string]interface{})[GovernedReviewSummaryField].(map[string]interface{})
		if field["type"] != "string" || field["minLength"] != float64(1) || field["maxLength"] != float64(600) ||
			field["description"] != "One sentence a reviewer reads on the approval card: what this does and to what. Never passed to the tool." {
			t.Fatalf("_reviewSummary schema = %v", field)
		}
		if _, ok := action.InputSchema["properties"].(map[string]interface{})[GovernedReviewSummaryField]; ok {
			t.Fatal("the action's own schema was mutated")
		}
	}
}

func TestGovernedSchemaReadUnchanged(t *testing.T) {
	for _, sideEffect := range []capability.SideEffect{capability.SideEffectRead, capability.SideEffectNone} {
		action := governedTestAction(sideEffect, capability.RiskLevelRead, "")
		if GovernedToolReviewed(action) || GovernedToolNeedsReviewSummary(action) {
			t.Fatalf("%s read must be offered as is", sideEffect)
		}
		schema := schemaJSON(t, GovernedToolInputSchema(action))
		properties := schema["properties"].(map[string]interface{})
		if _, ok := properties[GovernedReviewSummaryField]; ok {
			t.Fatal("read got _reviewSummary")
		}
		if _, ok := properties["text"]; !ok || len(properties) != 1 {
			t.Fatalf("read properties = %v", properties)
		}
	}
	empty := capability.ModelAction{Name: "demo.list", Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead}
	if got := GovernedToolInputSchema(empty); !reflect.DeepEqual(got, map[string]interface{}{"type": "object"}) {
		t.Fatalf("empty schema = %v", got)
	}
}

func TestGovernedSchemaNonExtendableUnchanged(t *testing.T) {
	cases := map[string]func(map[string]interface{}){
		"open object": func(s map[string]interface{}) { s["additionalProperties"] = true },
		"collision": func(s map[string]interface{}) {
			s["properties"].(map[string]interface{})[GovernedReviewSummaryField] = map[string]interface{}{"type": "string"}
		},
		"object keyword": func(s map[string]interface{}) { s["minProperties"] = 1 },
		"root ref": func(s map[string]interface{}) {
			s["properties"].(map[string]interface{})["self"] = map[string]interface{}{"$ref": "#"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			action := governedTestAction(capability.SideEffectWrite, capability.RiskLevelWrite, "")
			mutate(action.InputSchema)
			if GovernedToolNeedsReviewSummary(action) {
				t.Fatal("non-extendable schema must not need _reviewSummary")
			}
			if _, ok := GovernedToolInputSchema(action)["properties"].(map[string]interface{})[GovernedReviewSummaryField]; ok && name != "collision" {
				t.Fatal("non-extendable schema got _reviewSummary")
			}
			// A plain call is accepted and _reviewSummary (if a real field) is kept.
			input, err := ParseGovernedToolInput(action, json.RawMessage(`{"text":"hi","_reviewSummary":"x"}`), nil)
			if err != nil || input.ReviewSummary != "" || input.Arguments[GovernedReviewSummaryField] != "x" {
				t.Fatalf("parse = %+v, %v", input, err)
			}
		})
	}
}

const validReviewedInput = `{"arguments":{"text":"Hello"},"reviewContext":{"summary":"Post hello to #general","content":"Hello"},"externalOperation":{"resource":"#general","operation":"post_message"},"evidenceRefs":["action-call:1"]}`

func TestParseReviewedEnvelopeStrict(t *testing.T) {
	required := governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationRequired)
	input, err := ParseGovernedToolInput(required, json.RawMessage(validReviewedInput), nil)
	if err != nil {
		t.Fatal(err)
	}
	if input.Arguments["text"] != "Hello" || input.ReviewSummary != "Post hello to #general" ||
		input.ExternalOperation == nil || input.ExternalOperation.Operation != "post_message" ||
		!reflect.DeepEqual(input.EvidenceRefs, []string{"action-call:1"}) || input.ReviewContext.Content != "Hello" {
		t.Fatalf("input = %+v", input)
	}
	forbidden := governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationForbidden)
	for name, tc := range map[string]struct {
		action capability.ModelAction
		raw    string
		code   string
	}{
		"not an object":               {required, `["x"]`, ProposalRejectionToolArgumentsInvalid},
		"unknown key":                 {required, `{"arguments":{},"reviewContext":{"summary":"s","content":"c"},"externalOperation":{"resource":"#g","operation":"p"},"extra":1}`, ProposalRejectionToolArgumentsInvalid},
		"missing arguments":           {required, `{"reviewContext":{"summary":"s","content":"c"},"externalOperation":{"resource":"#g","operation":"p"}}`, ProposalRejectionToolArgumentsInvalid},
		"missing reviewContext":       {required, `{"arguments":{},"externalOperation":{"resource":"#g","operation":"p"}}`, ProposalRejectionReviewInvalid},
		"invalid reviewContext":       {required, `{"arguments":{},"reviewContext":{"summary":" ","content":"c"},"externalOperation":{"resource":"#g","operation":"p"}}`, ProposalRejectionReviewInvalid},
		"missing externalOperation":   {required, `{"arguments":{},"reviewContext":{"summary":"s","content":"c"}}`, ProposalRejectionExternalIdentityInvalid},
		"forbidden externalOperation": {forbidden, `{"arguments":{},"reviewContext":{"summary":"s","content":"c"},"externalOperation":{"resource":"#g","operation":"p"}}`, ProposalRejectionExternalIdentityInvalid},
		"invalid externalOperation":   {required, `{"arguments":{},"reviewContext":{"summary":"s","content":"c"},"externalOperation":{"resource":"#g","operation":"a/b"}}`, ProposalRejectionExternalIdentityInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseGovernedToolInput(tc.action, json.RawMessage(tc.raw), nil)
			expectRejection(t, err, tc.code)
		})
	}
}

func TestReviewedActionRequiresContent(t *testing.T) {
	for _, action := range []capability.ModelAction{
		governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationOptional),
		governedTestAction(capability.SideEffectDestructive, capability.RiskLevelDestructive, ""),
	} {
		for _, raw := range []string{
			`{"arguments":{"text":"x"},"reviewContext":{"summary":"Delete the draft"}}`,
			`{"arguments":{"text":"x"},"reviewContext":{"summary":"Delete the draft","content":"  "}}`,
		} {
			_, err := ParseGovernedToolInput(action, json.RawMessage(raw), nil)
			rejection := expectRejection(t, err, ProposalRejectionReviewInvalid)
			if !strings.Contains(rejection.Message, "content") || !errors.Is(err, ErrInvalidApprovalReviewContext) {
				t.Fatalf("rejection = %q (%v)", rejection.Message, err)
			}
		}
		input, err := ParseGovernedToolInput(action, json.RawMessage(`{"arguments":{"text":"x"},"reviewContext":{"summary":"Delete the draft","content":"Draft: Q3 plan"}}`), nil)
		if err != nil || input.ReviewContext.Content != "Draft: Q3 plan" {
			t.Fatalf("parse = %+v, %v", input, err)
		}
	}
}

func TestParseStripsReviewSummaryAndIdempotencyKey(t *testing.T) {
	write := governedTestAction(capability.SideEffectWrite, capability.RiskLevelWrite, "")
	input, err := ParseGovernedToolInput(write, json.RawMessage(`{"text":"x","idempotencyKey":"k","_reviewSummary":"  Save the note to Inbox.  "}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if input.ReviewSummary != "Save the note to Inbox." || !reflect.DeepEqual(input.Arguments, map[string]interface{}{"text": "x"}) {
		t.Fatalf("input = %+v", input)
	}
	for name, raw := range map[string]string{
		"missing":  `{"text":"x"}`,
		"blank":    `{"text":"x","_reviewSummary":"   "}`,
		"not text": `{"text":"x","_reviewSummary":3}`,
		"too long": `{"text":"x","_reviewSummary":"` + strings.Repeat("é", 601) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseGovernedToolInput(write, json.RawMessage(raw), nil)
			rejection := expectRejection(t, err, ProposalRejectionReviewInvalid)
			if rejection.Message != "_reviewSummary must be one sentence of 1 to 600 characters." {
				t.Fatalf("message = %q", rejection.Message)
			}
		})
	}
	if _, err := ParseGovernedToolInput(write, json.RawMessage(`{"text":"x","_reviewSummary":"`+strings.Repeat("é", 600)+`"}`), nil); err != nil {
		t.Fatalf("600 runes rejected: %v", err)
	}
	reviewed := governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationOptional)
	input, err = ParseGovernedToolInput(reviewed, json.RawMessage(`{"arguments":{"text":"x","idempotencyKey":"k"},"reviewContext":{"summary":"s","content":"c"}}`), nil)
	if err != nil || !reflect.DeepEqual(input.Arguments, map[string]interface{}{"text": "x"}) {
		t.Fatalf("reviewed = %+v, %v", input, err)
	}
	read := governedTestAction(capability.SideEffectRead, capability.RiskLevelRead, "")
	input, err = ParseGovernedToolInput(read, json.RawMessage(`{"text":"x","idempotencyKey":"k"}`), nil)
	if err != nil || !reflect.DeepEqual(input.Arguments, map[string]interface{}{"text": "x"}) || input.ReviewSummary != "" {
		t.Fatalf("read = %+v, %v", input, err)
	}
}

func TestParseDirectiveOverrides(t *testing.T) {
	reviewed := governedTestAction(capability.SideEffectExternal, capability.RiskLevelExternal, capability.ExternalOperationRequired)
	directive := &governedInputDirective{
		Summary:           "Post the weekly report",
		ExternalOperation: &ExternalOperationIdentity{Resource: "#reports", Operation: "post_weekly"},
		ReviewContext:     &ApprovalReviewContext{Summary: "Weekly report"},
	}
	input, err := ParseGovernedToolInput(reviewed, json.RawMessage(`{"text":"report","idempotencyKey":"k"}`), directive)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input.Arguments, map[string]interface{}{"text": "report"}) || input.ReviewSummary != "Post the weekly report" ||
		input.ExternalOperation.Resource != "#reports" || input.ReviewContext.Summary != "Weekly report" {
		t.Fatalf("input = %+v", input)
	}
	if input.ExternalOperation == directive.ExternalOperation || input.ReviewContext == directive.ReviewContext {
		t.Fatal("directive values must be copied")
	}
	input, err = ParseGovernedToolInput(reviewed, json.RawMessage(`{"text":"report"}`), &governedInputDirective{
		ExternalOperation: directive.ExternalOperation, ReviewContext: directive.ReviewContext,
	})
	if err != nil || input.ReviewSummary != "Weekly report" {
		t.Fatalf("summary from review context = %+v, %v", input, err)
	}
	_, err = ParseGovernedToolInput(reviewed, json.RawMessage(`{"text":"report"}`), &governedInputDirective{Summary: "x"})
	expectRejection(t, err, ProposalRejectionExternalIdentityInvalid)
	write := governedTestAction(capability.SideEffectWrite, capability.RiskLevelWrite, "")
	input, err = ParseGovernedToolInput(write, json.RawMessage(`{"text":"x"}`), &governedInputDirective{Summary: "Save it"})
	if err != nil || input.ReviewSummary != "Save it" {
		t.Fatalf("write directive = %+v, %v", input, err)
	}
	_, err = ParseGovernedToolInput(write, json.RawMessage(`{"text":"x"}`), &governedInputDirective{ReviewContext: &ApprovalReviewContext{}})
	expectRejection(t, err, ProposalRejectionReviewInvalid)
}

func TestNarrationSummaryFirstSentence(t *testing.T) {
	for narration, want := range map[string]string{
		"":                                       "",
		"  I'll check the calendar. Then reply.": "I'll check the calendar.",
		"Looking it up! One sec.":                "Looking it up!",
		"Shall I? Maybe.":                        "Shall I?",
		"First line\nsecond line. More.":         "First line",
		"Version 1.2 is out":                     "Version 1.2 is out",
		"No ending":                              "No ending",
		strings.Repeat("ab", 400):                strings.Repeat("ab", 300),
	} {
		if got := narrationSummary(narration); got != want {
			t.Errorf("narrationSummary(%q) = %q, want %q", narration, got, want)
		}
	}
}
