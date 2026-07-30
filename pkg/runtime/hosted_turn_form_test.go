package runtime

import (
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestHostedTurnFormCompilesAndRoundTripsExactActionArguments(t *testing.T) {
	action := capability.ModelAction{Name: "browser.click", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"target": map[string]interface{}{"type": "string", "pattern": "^s[1-9][0-9]*:e[1-9][0-9]*$"},
			"intent": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"target", "intent"},
	}}
	form := HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, SkillSelections: []HostedSkillSelection{}, Decisions: []TurnDecision{},
		OutputSummary: "Open a real post", ContinuationCheckpoint: map[string]interface{}{"kept": true},
		NextRunStatus: AgentRunStatusRunning, RunOutput: map[string]interface{}{}, CompletionEvidenceRefs: []string{}, EvidenceClaims: []EvidenceClaim{},
		ProposedAction: &HostedTurnActionForm{
			Capability: action.Name, Summary: "Open the selected post", IdempotencyKey: "open-current-post",
			Arguments: map[string]interface{}{"target": "s3:e40", "intent": "Read the selected post"},
		},
	}
	response, err := CompileHostedTurnForm(form, []capability.ModelAction{action})
	if err != nil {
		t.Fatal(err)
	}
	if response.ProposedAction == nil || response.ProposedAction.InputRef != "/actionInputs/proposed" || response.ProposedAction.Capability != action.Name {
		t.Fatalf("response = %#v", response)
	}
	roundTrip, err := HostedTurnFormFromResponse(*response)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.ProposedAction.Arguments, form.ProposedAction.Arguments) {
		t.Fatalf("arguments = %#v", roundTrip.ProposedAction.Arguments)
	}
	if _, leaked := form.ContinuationCheckpoint["actionInputs"]; leaked {
		t.Fatal("compilation mutated the provider form checkpoint")
	}
}

func TestHostedTurnFormRejectsMissingOrInventedActionValues(t *testing.T) {
	action := capability.ModelAction{Name: "browser.click", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"target": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"target"},
	}}
	base := HostedTurnForm{SchemaVersion: HostedTurnFormSchemaVersion, OutputSummary: "open", NextRunStatus: AgentRunStatusRunning}
	base.ProposedAction = &HostedTurnActionForm{Capability: action.Name, Summary: "open", IdempotencyKey: "open-1", Arguments: map[string]interface{}{}}
	if _, err := CompileHostedTurnForm(base, []capability.ModelAction{action}); err == nil {
		t.Fatal("missing evidence-derived target was accepted")
	}
	base.ProposedAction.Arguments = map[string]interface{}{"target": "invented", "authority": "widened"}
	if _, err := CompileHostedTurnForm(base, []capability.ModelAction{action}); err == nil {
		t.Fatal("unknown action input was accepted")
	}
}

func TestHostedTurnFormSchemaUsesExactAuthorizedActionInputs(t *testing.T) {
	action := capability.ModelAction{Name: "browser.click", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"target": map[string]interface{}{"type": "string", "pattern": "^s[1-9]"}},
		"required":   []interface{}{"target"},
	}}
	schema, err := HostedTurnFormJSONSchema([]capability.ModelAction{action})
	if err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]interface{})
	if properties["schemaVersion"].(map[string]interface{})["const"] != HostedTurnFormSchemaVersion {
		t.Fatalf("schema version = %#v", properties["schemaVersion"])
	}
	branches := properties["proposedAction"].(map[string]interface{})["oneOf"].([]interface{})
	arguments := branches[0].(map[string]interface{})["properties"].(map[string]interface{})["arguments"].(map[string]interface{})
	if arguments["additionalProperties"] != false || arguments["properties"].(map[string]interface{})["target"].(map[string]interface{})["pattern"] != "^s[1-9]" {
		t.Fatalf("arguments schema = %#v", arguments)
	}
}

func TestHostedTurnFormSchemaOmitsUnavailableProposalFamilies(t *testing.T) {
	schema, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]interface{})
	for _, name := range []string{"proposedAction", "proposedDelegation", "proposedFork", "proposedRunbook"} {
		if _, exists := properties[name]; exists {
			t.Fatalf("unavailable proposal %s remains in schema", name)
		}
	}

	schema, err = HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{CanDelegate: true, CanInvokeRunbook: true})
	if err != nil {
		t.Fatal(err)
	}
	properties = schema["properties"].(map[string]interface{})
	for _, name := range []string{"proposedDelegation", "proposedFork", "proposedRunbook"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("authorized proposal %s was removed from schema", name)
		}
	}
}

func TestHostedTurnFormSchemaRequiresExactOfferedSkillDispositions(t *testing.T) {
	schema, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{
		SkillPromptReferences: []string{"skill:summarize@1", "skill:research@2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selections := schema["properties"].(map[string]interface{})["skillSelections"].(map[string]interface{})
	if selections["minItems"] != 2 || selections["maxItems"] != 2 {
		t.Fatalf("selection cardinality = %#v", selections)
	}
	branches := selections["items"].(map[string]interface{})["oneOf"].([]interface{})
	if got := branches[0].(map[string]interface{})["properties"].(map[string]interface{})["skillRef"].(map[string]interface{})["const"]; got != "skill:summarize@1" {
		t.Fatalf("first Skill reference = %#v", got)
	}
	if _, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{SkillPromptReferences: []string{"skill:summarize@1", "skill:summarize@1"}}); err == nil {
		t.Fatal("duplicate Skill prompt references were accepted")
	}
}
