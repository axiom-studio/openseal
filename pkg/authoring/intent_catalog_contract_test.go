package authoring

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
