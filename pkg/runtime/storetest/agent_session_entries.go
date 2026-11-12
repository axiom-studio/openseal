package storetest

// Session entries the store suites and runtime tests write.

import (
	"encoding/json"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

// TextPart is a text content part.
func TextPart(text string) models.ContentPart {
	return models.ContentPart{Type: models.ContentPartText, Text: text}
}

// UserSessionEntry is a user entry with text, from channel message m-<id>.
func UserSessionEntry(id, text string) *models.SessionEntry {
	return &models.SessionEntry{ID: id, Type: models.SessionEntryUser, User: &models.UserEntry{
		Origin: models.UserEntryChannelMessage, Author: models.SessionAuthor{Type: "user", ID: "u1"},
		Content: []models.ContentPart{TextPart(text)}, MessageID: "m-" + id,
	}}
}

// AssistantSessionEntry is an assistant entry of stepID that calls each of
// callIDs.
func AssistantSessionEntry(id, stepID string, callIDs ...string) *models.SessionEntry {
	entry := &models.SessionEntry{ID: id, StepID: stepID, Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{
		Provider: models.ModelStepProviderAnthropic, Model: "claude-x", StopReason: models.ModelStopEnd, Text: "ok",
	}}
	for _, callID := range callIDs {
		entry.Assistant.ToolCalls = append(entry.Assistant.ToolCalls, models.ModelToolCall{ID: callID, Name: "linear_get_issue", Arguments: json.RawMessage(`{"id":"1"}`)})
		entry.Assistant.StopReason = models.ModelStopToolCalls
	}
	return entry
}

// ToolSessionEntry is the tool entry of callID in state, with the result or
// wait that state needs.
func ToolSessionEntry(id, stepID, callID string, state models.ToolEntryState) *models.SessionEntry {
	entry := &models.SessionEntry{ID: id, StepID: stepID, Type: models.SessionEntryTool, State: state, ToolCallID: callID, Tool: &models.ToolEntry{
		Name: "linear_get_issue", Arguments: json.RawMessage(`{"id":"1"}`), SideEffect: models.ToolSideEffectRead,
	}}
	switch {
	case state.Terminal():
		status := models.ToolResultSucceeded
		if state == models.ToolEntryError {
			status = models.ToolResultFailed
		}
		if state == models.ToolEntryInterrupted {
			status = models.ToolResultInterrupted
		}
		entry.Tool.Result = &models.ToolResult{Status: status, IsError: status != models.ToolResultSucceeded, Content: []models.ContentPart{TextPart("done")}}
	case state.Awaiting():
		entry.Tool.Wait = &models.ToolEntryWait{ApprovalID: "ap1"}
	}
	return entry
}

// PathEntryIDs joins the IDs of the path's entries with commas.
func PathEntryIDs(path *models.AgentSessionPath) string {
	ids := make([]string, 0, len(path.Entries))
	for _, entry := range path.Entries {
		ids = append(ids, entry.ID)
	}
	return strings.Join(ids, ",")
}
