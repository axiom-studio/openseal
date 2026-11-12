package modelsteptest

import (
	"encoding/json"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// SessionScope is the scope of the fixture sessions.
var SessionScope = models.Scope{Kind: "org", ID: "o1"}

// AssistantEntry is a finished assistant entry calling the lookup tool once
// per call id.
func AssistantEntry(calls ...string) *models.AssistantEntry {
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

// ValidModelStepRequest is a valid step request over a short session.
func ValidModelStepRequest() models.ModelStepRequest {
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
			userView("e1", 1, "hello"),
			{ID: "e2", Sequence: 2, Type: models.SessionEntryAssistant, Assistant: AssistantEntry("c1", "c2")},
			toolView("e3", 3, "c1", models.ToolEntryCompleted),
			userView("e4", 4, "steer"),
			toolView("e5", 5, "c2", models.ToolEntryError),
		},
		Stream: models.ModelStepStream{FinalAnswerTool: "turn_final_answer", AnswerPreview: true},
	}
}

func userView(id string, seq int64, text string) models.SessionEntryView {
	return models.SessionEntryView{ID: id, Sequence: seq, Type: models.SessionEntryUser, CreatedAt: time.Unix(seq, 0).UTC(),
		User: &models.UserEntryView{Author: &models.EntryAuthor{Kind: models.EntryAuthorPerson, ID: "u1", Label: "Ana"}, Content: []models.ContentPart{{Type: models.ContentPartText, Text: text}}}}
}

func toolView(id string, seq int64, callID string, state models.ToolEntryState) models.SessionEntryView {
	return models.SessionEntryView{ID: id, Sequence: seq, Type: models.SessionEntryTool, Tool: &models.ToolEntryView{
		CallID: callID, Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), State: state,
		Result: models.ToolResultView{Content: []models.ContentPart{{Type: models.ContentPartText, Text: "ok"}}, IsError: state != models.ToolEntryCompleted},
	}}
}
