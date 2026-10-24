package authoring

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	validateschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestAuthoringRepairIncludesPriorAnswerAndAuthorizedSkillChoices(t *testing.T) {
	invalid := `{"schemaVersion":"openseal.authoring-intent/v4","kind":"agent","name":"Slack assistant","purpose":"Help in Slack","agents":[{"key":"assistant","name":"Slack assistant","purpose":"Help in Slack","behavior":"Respond to Slack requests.","skills":[{"catalogId":"k","required":true}]}]}`
	valid := strings.Replace(invalid, `"catalogId":"k"`, `"catalogId":"skill-slack"`, 1)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var payload struct {
			Messages []map[string]string      `json:"messages"`
			Tools    []map[string]interface{} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(payload.Tools)
		if !strings.Contains(string(encoded), `"enum":["skill-slack"]`) {
			t.Errorf("provider schema lacks authorized Skill choices")
		}
		answer := invalid
		if attempts > 1 {
			feedback := payload.Messages[len(payload.Messages)-1]["content"]
			var repair struct {
				Previous string                     `json:"previousSemanticAnswers"`
				Errors   []AuthoringSchemaViolation `json:"validationErrors"`
			}
			_, raw, ok := strings.Cut(feedback, "\n")
			if !ok || json.Unmarshal([]byte(raw), &repair) != nil || repair.Previous != invalid || len(repair.Errors) == 0 {
				t.Errorf("repair omitted the rejected answer or its diagnostics")
			}
			answer = valid
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{
			"finish_reason": "stop", "message": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
				"type": "function", "function": map[string]interface{}{"name": "submit_authoring_intent", "arguments": answer},
			}}},
		}}})
	}))
	defer server.Close()
	g, err := NewOpenAICompatibleGenerator(server.URL, "secret", "model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := g.GenerateIntent(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a Slack bot", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"skill-slack": {ID: "skill-slack"}}}})
	if err != nil || attempts != 2 || len(intent.Agents) != 1 || intent.Agents[0].Skills[0].CatalogID != "skill-slack" {
		t.Fatalf("repair failed: attempts=%d err=%v intent=%#v", attempts, err, intent)
	}
}

func TestAuthoringSkillChoicesDoNotLeakBetweenCatalogs(t *testing.T) {
	for _, id := range []string{"tenant-a-skill", "tenant-b-skill"} {
		schema, err := authoringIntentContractForCatalog(CapabilityCatalog{Skills: map[string]SkillCapability{id: {ID: id}}}).Schema()
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(schema)
		if !strings.Contains(string(encoded), `"enum":["`+id+`"]`) {
			t.Fatal("missing catalog choices")
		}
		other := "tenant-a-skill"
		if id == other {
			other = "tenant-b-skill"
		}
		if strings.Contains(string(encoded), other) {
			t.Fatal("foreign catalog leaked into schema")
		}
	}
	base, err := AuthoringIntentJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(base)
	if strings.Contains(string(encoded), "tenant-a-skill") || strings.Contains(string(encoded), "tenant-b-skill") {
		t.Fatal("shared schema was mutated")
	}
}

func compileAuthoringTestSchema(t *testing.T, schema map[string]interface{}) *validateschema.Schema {
	t.Helper()
	encoded, _ := json.Marshal(schema)
	document, err := validateschema.UnmarshalJSON(strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	compiler := validateschema.NewCompiler()
	if err := compiler.AddResource("https://openseal.test/intent.json", document); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("https://openseal.test/intent.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func authoringTestSchemaPath(t *testing.T, schema map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	node := schema
	for _, segment := range path {
		node, _ = node[segment].(map[string]interface{})
		if node != nil && segment != "properties" {
			resolved, err := resolveSchemaRef(schema, node)
			if err != nil {
				t.Fatal(err)
			}
			node = resolved
		}
		if node == nil {
			t.Fatalf("schema path %v missing at %s", path, segment)
		}
	}
	return node
}

func TestAuthoringIntentProviderSchemaEncodesValidatorCountsAndCatalogReferences(t *testing.T) {
	schema, err := authoringIntentContractForCatalog(CapabilityCatalog{Skills: map[string]SkillCapability{"notes": {ID: "notes"}, "mail": {ID: "mail"}}}).Schema()
	if err != nil {
		t.Fatal(err)
	}
	ids := `["mail","notes"]`
	for _, test := range []struct {
		path []string
		key  string
		want string
	}{
		{path: []string{"properties", "agents"}, key: "minItems", want: "1"},
		{path: []string{"properties", "agents", "items", "properties", "skills", "items", "properties", "catalogId"}, key: "enum", want: ids},
		{path: []string{"properties", "agents", "items", "properties", "operations", "items", "properties", "skillCatalogIds", "items"}, key: "enum", want: ids},
		{path: []string{"properties", "team", "properties", "roles"}, key: "minItems", want: "1"},
		{path: []string{"properties", "team", "properties", "roles", "items", "properties", "agentKeys"}, key: "minItems", want: "1"},
		{path: []string{"properties", "team", "properties", "roles", "items", "properties", "skillCatalogIds", "items"}, key: "enum", want: ids},
	} {
		value, _ := json.Marshal(authoringTestSchemaPath(t, schema, test.path...)[test.key])
		if string(value) != test.want {
			t.Errorf("%v %s = %s want %s", test.path, test.key, value, test.want)
		}
	}
	// Array definitions are shared by many lists; narrowing must copy them.
	for name, definition := range schema["$defs"].(map[string]interface{}) {
		encoded, _ := json.Marshal(definition)
		if definition.(map[string]interface{})["type"] == "array" && (strings.Contains(string(encoded), "enum") || strings.Contains(string(encoded), "Items")) {
			t.Errorf("shared array definition %s was narrowed in place: %s", name, encoded)
		}
	}

	empty, err := authoringIntentContractForCatalog(CapabilityCatalog{}).Schema()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"properties", "agents", "items", "properties", "skills"},
		{"properties", "agents", "items", "properties", "operations", "items", "properties", "skillCatalogIds"},
		{"properties", "team", "properties", "roles", "items", "properties", "skillCatalogIds"},
	} {
		if maximum := authoringTestSchemaPath(t, empty, path...)["maxItems"]; maximum != 0 {
			t.Errorf("empty catalog %v maxItems = %v", path, maximum)
		}
	}
}

func TestAuthoringIntentProviderSchemaDerivesConversationChoicesFromAdapters(t *testing.T) {
	operationPath := []string{"properties", "agents", "items", "properties", "operations", "items"}
	countOperationBranches := func(contract authoringProviderContract) int {
		decoding, err := contract.DecodingSchema()
		if err != nil {
			t.Fatal(err)
		}
		branch := decoding["oneOf"].([]interface{})[0].(map[string]interface{})
		branch["$defs"] = decoding["$defs"]
		return len(authoringTestSchemaPath(t, branch, operationPath...)["oneOf"].([]interface{}))
	}

	withoutAdapters := authoringIntentContractForCatalog(CapabilityCatalog{Skills: map[string]SkillCapability{"notes": {ID: "notes"}}})
	schema, err := withoutAdapters.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if maximum := authoringTestSchemaPath(t, schema, "properties", "conversations")["maxItems"]; maximum != 0 {
		t.Fatalf("conversations without adapters maxItems = %v", maximum)
	}
	delivery, _ := json.Marshal(authoringTestSchemaPath(t, schema, append(operationPath, "properties", "approvalDelivery")...)["enum"])
	if string(delivery) != `["platform"]` {
		t.Fatalf("approval delivery without adapters = %s", delivery)
	}
	if branches := countOperationBranches(withoutAdapters); branches != 3 {
		t.Fatalf("operation branches without adapters = %d, want wake x platform", branches)
	}

	withAdapter := authoringIntentContractForCatalog(CapabilityCatalog{Skills: map[string]SkillCapability{
		"notes": {ID: "notes"},
		"chat":  {ID: "chat", ConversationAdapters: []ConversationAdapterCapability{{ID: "conversations", Provider: " slack "}}},
	}})
	schema, err = withAdapter.Schema()
	if err != nil {
		t.Fatal(err)
	}
	conversations := authoringTestSchemaPath(t, schema, "properties", "conversations")
	providers, _ := json.Marshal(authoringTestSchemaPath(t, schema, "properties", "conversations", "items", "properties", "provider")["enum"])
	if conversations["maxItems"] != nil || string(providers) != `["chat","slack"]` {
		t.Fatalf("conversation providers = %s maxItems = %v", providers, conversations["maxItems"])
	}
	if branches := countOperationBranches(withAdapter); branches != 6 {
		t.Fatalf("operation branches with adapters = %d, want wake x delivery", branches)
	}
}

func TestAuthoringIntentDecodingSchemaRejectsOnlyWhatTheValidatorRejects(t *testing.T) {
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{"notes": {ID: "notes", Actions: []string{"read"}}}}
	contract := authoringIntentContractForCatalog(catalog)
	decodingSchema, err := contract.DecodingSchema()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodingSchema["oneOf"]; !ok {
		t.Fatal("decoding schema has no kind variants")
	}
	decoding := compileAuthoringTestSchema(t, decodingSchema)
	canonical, err := contract.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := canonical["oneOf"]; ok {
		t.Fatal("tool and JSON transports must keep the combinator-free schema")
	}
	agent := func(key, skill, operations string) string {
		return `{"key":"` + key + `","name":"Digest","purpose":"Summarize notes","behavior":"Summarize notes daily.","skills":[{"catalogId":"` + skill + `","required":true}],"objectives":[{"key":"daily","title":"Daily digest","outcome":"A digest exists each day.","priority":1}],"operations":[` + operations + `]}`
	}
	operation := func(fields string) string {
		return `{"key":"run","name":"Run","goal":"Write the digest","objectiveKey":"daily","approval":"by_policy","reportProgress":false,` + fields + `}`
	}
	intent := func(kind, team string, agents ...string) string {
		return `{"schemaVersion":"openseal.authoring-intent/v4","kind":"` + kind + `","name":"Digest","purpose":"Summarize notes",` + team + `"agents":[` + strings.Join(agents, ",") + `]}`
	}
	single := func(operations string) string { return intent("agent", "", agent("digest", "notes", operations)) }
	team := `"team":{"key":"crew","name":"Crew","purpose":"Summarize notes","roles":[{"key":"lead","name":"Lead","purpose":"Lead","agentKeys":["digest"],"canSpeakInChannels":false}]},`
	for _, test := range []struct {
		name     string
		document string
		valid    bool
	}{
		{name: "single agent", document: single(operation(`"wake":"on_demand","approvalDelivery":"platform"`)), valid: true},
		{name: "scheduled", document: single(operation(`"wake":"schedule","schedule":"every weekday at 9","approvalDelivery":"platform"`)), valid: true},
		{name: "team", document: intent("team", team, agent("digest", "notes", ""), agent("other", "notes", "")), valid: true},
		{name: "agent with team", document: intent("agent", team, agent("digest", "notes", ""))},
		{name: "agent with two agents", document: intent("agent", "", agent("digest", "notes", ""), agent("other", "notes", ""))},
		{name: "team without team", document: intent("team", "", agent("digest", "notes", ""))},
		{name: "workforce without team", document: intent("workforce", "", agent("digest", "notes", ""))},
		{name: "no agents", document: intent("agent", "")},
		{name: "schedule without text", document: single(operation(`"wake":"schedule","approvalDelivery":"platform"`))},
		{name: "event without type", document: single(operation(`"wake":"event","eventType":" ","approvalDelivery":"platform"`))},
		{name: "channels without keys", document: single(operation(`"wake":"on_demand","approvalDelivery":"channels"`))},
		{name: "platform with keys", document: single(operation(`"wake":"on_demand","approvalDelivery":"platform","approvalChannelKeys":["ops"]`))},
		{name: "uncatalogued skill", document: intent("agent", "", agent("digest", "invented", ""))},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, err := validateschema.UnmarshalJSON(strings.NewReader(test.document))
			if err != nil {
				t.Fatal(err)
			}
			schemaErr := decoding.Validate(document)
			intent, validatorErr := decodeAuthoringIntent([]byte(test.document))
			if validatorErr == nil {
				validatorErr = validateAuthoringIntent(intent, catalog)
			}
			if test.valid != (schemaErr == nil) || test.valid != (validatorErr == nil) {
				t.Fatalf("decoding schema error=%v validator error=%v want valid=%v", schemaErr, validatorErr, test.valid)
			}
		})
	}
}
