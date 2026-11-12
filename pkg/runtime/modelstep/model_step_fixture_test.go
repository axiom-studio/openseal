package modelstep

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// The base fixtures of the model step tests: copies of runtime's session,
// model step and store test helpers, which stay with runtime's tests.

var stepTestScope = models.Scope{Kind: "org", ID: "o1"}

func newModelStepTestStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func stepTestAssistant(calls ...string) *models.AssistantEntry {
	a := &models.AssistantEntry{
		Provider: models.ModelStepProviderAnthropic, Model: "claude-test",
		SystemDigest: "sha256:aa", ToolsDigest: "sha256:bb",
		StopReason: models.ModelStopEnd, Usage: models.ModelStepUsage{Attempts: 1},
	}
	for _, id := range calls {
		a.ToolCalls = append(a.ToolCalls, models.ModelToolCall{ID: id, Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)})
	}
	if len(calls) > 0 {
		a.StopReason = models.ModelStopToolCalls
	}
	return a
}

func validStepRequest() models.ModelStepRequest {
	return models.ModelStepRequest{
		APIVersion: models.ModelStepAPIVersion,
		Purpose:    models.ModelStepPurposeStep,
		Scope:      models.Scope{Kind: "tenant", ID: "t1"},
		RunID:      "run-1", StepID: "step-1", SessionID: "session-1", AgentID: "agent-1",
		DefinitionID: "def-1", DefinitionVersion: "1",
		System: models.ModelStepSystem{
			AgentInstructions: []string{"Be helpful."},
			Sections:          []models.ModelStepSystemSection{{ID: "progress_rules", Title: "Progress", Text: "Report."}},
		},
		Tools: []models.ModelStepTool{
			{Name: "turn_final_answer", Description: "Final.", InputSchema: map[string]interface{}{"type": "object"}, Group: models.ModelStepToolHarness},
			{Name: "lookup", Description: "Look up.", InputSchema: map[string]interface{}{"type": "object"}, Group: models.ModelStepToolCore},
			{Name: "skill-live-browser_open", Description: "Open.", InputSchema: map[string]interface{}{"type": "object"}, Group: models.ModelStepToolLoaded, Capability: "skill-live-browser.open"},
		},
		Entries: []models.SessionEntryView{
			stepUserView("e1", 1, "hello"),
			{ID: "e2", Sequence: 2, Type: models.SessionEntryAssistant, Assistant: stepTestAssistant("c1", "c2")},
			stepToolView("e3", 3, "c1", models.ToolEntryCompleted),
			stepUserView("e4", 4, "steer"),
			stepToolView("e5", 5, "c2", models.ToolEntryError),
		},
		Stream: models.ModelStepStream{FinalAnswerTool: "turn_final_answer", AnswerPreview: true},
	}
}

func stepUserView(id string, seq int64, text string) models.SessionEntryView {
	return models.SessionEntryView{ID: id, Sequence: seq, Type: models.SessionEntryUser, CreatedAt: time.Unix(seq, 0).UTC(),
		User: &models.UserEntryView{Author: &models.EntryAuthor{Kind: models.EntryAuthorPerson, ID: "u1", Label: "Ana"}, Content: []models.ContentPart{{Type: models.ContentPartText, Text: text}}}}
}

func stepToolView(id string, seq int64, callID string, state models.ToolEntryState) models.SessionEntryView {
	return models.SessionEntryView{ID: id, Sequence: seq, Type: models.SessionEntryTool, Tool: &models.ToolEntryView{
		CallID: callID, Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), State: state,
		Result: models.ToolResultView{Content: []models.ContentPart{{Type: models.ContentPartText, Text: "ok"}}, IsError: state != models.ToolEntryCompleted},
	}}
}
