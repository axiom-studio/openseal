package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	validateschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// decodeTurnAgainstBothContracts sends document as the provider reply and
// returns the decoding schema's verdict alongside the real turn pipeline's.
func decodeTurnAgainstBothContracts(t *testing.T, request runtime.HostedTurnRequest, document string) (schemaErr, hostErr error) {
	t.Helper()
	var decoding map[string]interface{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResponseFormat struct {
				JSONSchema struct {
					Schema map[string]interface{} `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		decoding = body.ResponseFormat.JSONSchema.Schema
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": "stop", "message": map[string]interface{}{"content": document}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
	}))
	defer provider.Close()
	host, err := NewProviderTurnHostWithOptions(provider.URL, "key", "model", ProviderTurnHostOptions{StructuredOutput: ProviderStructuredOutputJSONSchema})
	if err != nil {
		t.Fatal(err)
	}
	_, hostErr = host.ExecuteHostedTurn(t.Context(), request)
	encoded, _ := json.Marshal(decoding)
	schemaDocument, err := validateschema.UnmarshalJSON(strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	compiler := validateschema.NewCompiler()
	if err := compiler.AddResource("https://openseal.test/turn.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("https://openseal.test/turn.json")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := validateschema.UnmarshalJSON(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Validate(instance), hostErr
}

func TestHostedTurnDecodingSchemaRejectsOnlyWhatTheTurnChecksReject(t *testing.T) {
	turn := func(fields string) string {
		return `{"schemaVersion":"` + runtime.HostedTurnFormSchemaVersion + `","skillSelections":[],"decisions":[],"continuationCheckpoint":{},"runError":"","completionEvidenceRefs":[],"evidenceClaims":[],` + fields + `}`
	}
	plain := runtime.HostedTurnRequest{APIVersion: runtime.HostedTurnAPIVersion, InvocationID: "turn", Goal: "Answer"}
	task := runtime.HostedTurnRequest{
		APIVersion: runtime.HostedTurnAPIVersion, InvocationID: "turn", Goal: "Reply", AgentID: "agent",
		InputContext:      map[string]interface{}{"conversationTasks": map[string]interface{}{"canStart": true}},
		ConversationTasks: &runtime.HostedConversationTaskContext{ConversationID: "conversation", CanStart: true},
	}
	files := `"runOutput":{"generatedFiles":[{"name":"notes.md","mediaType":"text/markdown","text":"Notes"}]}`
	for _, test := range []struct {
		name     string
		request  runtime.HostedTurnRequest
		document string
		valid    bool
	}{
		{name: "completed reply", request: plain, document: turn(`"nextRunStatus":"completed","outputSummary":"Done","runOutput":{"reply":"Four"}`), valid: true},
		{name: "completed with files", request: plain, document: turn(`"nextRunStatus":"completed","outputSummary":"Done",` + files), valid: true},
		{name: "running", request: plain, document: turn(`"nextRunStatus":"running","outputSummary":"Working","runOutput":{}`), valid: true},
		{name: "sleeping with timer", request: plain, document: turn(`"nextRunStatus":"sleeping","outputSummary":"Later","runOutput":{},"wakeCondition":{"type":"timer","wakeAt":"2030-01-01T09:00:00Z"}`), valid: true},
		{name: "waiting for a user message", request: plain, document: turn(`"nextRunStatus":"waiting_for_event","outputSummary":"Asked","runOutput":{},"wakeCondition":{"type":"user_message"}`), valid: true},
		{name: "waiting with a clarification question", request: plain, document: turn(`"nextRunStatus":"waiting_for_event","outputSummary":"Asked","runOutput":{"summary":"Which repository should I review?"},"wakeCondition":{"type":"user_message"}`), valid: true},
		{name: "quiet waiting challenge", request: plain, document: turn(`"nextRunStatus":"waiting_for_event","outputSummary":"Waiting","runOutput":{"silent":true},"wakeCondition":{"type":"user_message"}`), valid: true},
		{name: "approval without wake", request: plain, document: turn(`"nextRunStatus":"waiting_for_approval","outputSummary":"Needs approval","runOutput":{}`), valid: true},
		{name: "task proposal", request: task, document: turn(`"nextRunStatus":"running","outputSummary":"","runOutput":{},"proposedTask":{"taskKey":"digest","goal":"Write the digest","acknowledgment":"I will write the digest."}`), valid: true},
		{name: "completed with wake", request: plain, document: turn(`"nextRunStatus":"completed","outputSummary":"Done","runOutput":{"reply":"Four"},"wakeCondition":{"type":"manual"}`)},
		{name: "running with wake", request: plain, document: turn(`"nextRunStatus":"running","outputSummary":"Working","runOutput":{},"wakeCondition":{"type":"None"}`)},
		{name: "sleeping without wake", request: plain, document: turn(`"nextRunStatus":"sleeping","outputSummary":"Later","runOutput":{}`)},
		{name: "blank wake type", request: plain, document: turn(`"nextRunStatus":"sleeping","outputSummary":"Later","runOutput":{},"wakeCondition":{"type":" "}`)},
		{name: "event wait off waiting_for_event", request: plain, document: turn(`"nextRunStatus":"waiting_for_dependency","outputSummary":"Later","runOutput":{},"wakeCondition":{"type":"event","eventWait":{"source":"webhook","subject":"build"}}`)},
		{name: "invented completion evidence", request: plain, document: turn(`"nextRunStatus":"completed","outputSummary":"Done","runOutput":{"reply":"Four"},"completionEvidenceRefs":["action-call:invented"]`)},
		{name: "files off completion", request: plain, document: turn(`"nextRunStatus":"running","outputSummary":"Working","runOutput":{"generatedFiles":[]}`)},
		{name: "skill selection without prompts", request: plain, document: strings.Replace(turn(`"nextRunStatus":"completed","outputSummary":"Done","runOutput":{"reply":"Four"}`), `"skillSelections":[]`, `"skillSelections":[{"skillRef":"invented","disposition":"applied","summary":"Used it"}]`, 1)},
		{name: "task with output", request: task, document: turn(`"nextRunStatus":"running","outputSummary":"Starting","runOutput":{},"proposedTask":{"taskKey":"digest","goal":"Write the digest","acknowledgment":"I will write the digest."}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			schemaErr, hostErr := decodeTurnAgainstBothContracts(t, test.request, test.document)
			if test.valid != (schemaErr == nil) || test.valid != (hostErr == nil) {
				t.Fatalf("decoding schema error=%v turn error=%v want valid=%v", schemaErr, hostErr, test.valid)
			}
		})
	}
}

func TestHostedTurnDecodingSchemaClosesRunOutputOffCompletion(t *testing.T) {
	// The one deliberate narrowing: off completion runOutput is closed to its
	// declared fields so generatedFiles cannot appear under any value. The
	// turn checks accept other extra keys; no documented flow emits one before
	// completing.
	turn := func(status string) string {
		return `{"schemaVersion":"` + runtime.HostedTurnFormSchemaVersion + `","skillSelections":[],"decisions":[],"continuationCheckpoint":{},"runError":"","completionEvidenceRefs":[],"evidenceClaims":[],"nextRunStatus":"` + status + `","outputSummary":"Working","runOutput":{"progress":{"step":1},"note":"half way"}}`
	}
	request := runtime.HostedTurnRequest{APIVersion: runtime.HostedTurnAPIVersion, InvocationID: "turn", Goal: "Answer"}
	schemaErr, hostErr := decodeTurnAgainstBothContracts(t, request, turn("running"))
	if schemaErr == nil || hostErr != nil {
		t.Fatalf("running extra keys: schema error=%v turn error=%v", schemaErr, hostErr)
	}
	if schemaErr, hostErr = decodeTurnAgainstBothContracts(t, request, turn("completed")); schemaErr != nil || hostErr != nil {
		t.Fatalf("completed extra keys: schema error=%v turn error=%v", schemaErr, hostErr)
	}
	files := strings.Replace(turn("running"), `{"progress":{"step":1},"note":"half way"}`, `{"generatedFiles":"["}`, 1)
	if schemaErr, hostErr = decodeTurnAgainstBothContracts(t, request, files); schemaErr == nil || hostErr == nil {
		t.Fatalf("scalar generatedFiles off completion: schema error=%v turn error=%v", schemaErr, hostErr)
	}
}

func TestHostedTurnDecodingSchemaAdaptsBoundsForLlamaGrammar(t *testing.T) {
	action := capability.ModelAction{Name: "notes.publish", SideEffect: capability.SideEffectExternal, InputSchema: map[string]interface{}{"type": "object"}}
	schema, err := runtime.HostedTurnFormJSONSchema([]capability.ModelAction{action}, runtime.HostedTurnFormAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	schema["properties"].(map[string]interface{})["runOutput"] = generatedOutputSchema()
	canonical, _ := json.Marshal(schema)
	if !strings.Contains(string(canonical), `"maxLength":2000`) {
		t.Fatal("fixture no longer carries the reviewContext bound this guards against")
	}
	decoding, err := hostedTurnDecodingSchema(schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	var walk func(path string, value interface{})
	walk = func(path string, value interface{}) {
		switch node := value.(type) {
		case map[string]interface{}:
			for _, keyword := range []string{"maxLength", "maxItems"} {
				if bound, ok := node[keyword].(float64); ok && bound >= largeDecodingBound {
					t.Errorf("%s keeps %s %v", path, keyword, bound)
				}
			}
			if _, bounded := node["maxItems"]; bounded && node["items"] == nil {
				t.Errorf("%s bounds items without an items schema, which llama.cpp ignores", path)
			}
			for key, child := range node {
				walk(path+"."+key, child)
			}
		case []interface{}:
			for _, child := range node {
				walk(path+"[]", child)
			}
		}
	}
	walk("", decoding)
	branch := decoding["oneOf"].([]interface{})[0].(map[string]interface{})["properties"].(map[string]interface{})
	files, _ := json.Marshal(branch["runOutput"].(map[string]interface{})["properties"].(map[string]interface{})["generatedFiles"].(map[string]interface{})["maxItems"])
	if string(files) != "8" {
		t.Fatalf("small bounds must survive: generatedFiles maxItems = %s", files)
	}
}
