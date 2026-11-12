package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/axiom-studio/openseal/pkg/models"
	"strings"
	"testing"
	"time"
)

func modelStepTestAssistant(calls ...string) *models.AssistantEntry {
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

func userView(id string, seq int64, text string) models.SessionEntryView {
	return models.SessionEntryView{ID: id, Sequence: seq, Type: models.SessionEntryUser, CreatedAt: time.Unix(seq, 0).UTC(),
		User: &models.UserEntryView{Author: &models.EntryAuthor{Kind: models.EntryAuthorPerson, ID: "u1", Label: "Ana"}, Content: []models.ContentPart{{Type: models.ContentPartText, Text: text}}}}
}

func assistantView(id string, seq int64, calls ...string) models.SessionEntryView {
	return models.SessionEntryView{ID: id, Sequence: seq, Type: models.SessionEntryAssistant, Assistant: modelStepTestAssistant(calls...)}
}

func toolView(id string, seq int64, callID string, state models.ToolEntryState) models.SessionEntryView {
	return models.SessionEntryView{ID: id, Sequence: seq, Type: models.SessionEntryTool, Tool: &models.ToolEntryView{
		CallID: callID, Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), State: state,
		Result: models.ToolResultView{Content: []models.ContentPart{{Type: models.ContentPartText, Text: "ok"}}, IsError: state != models.ToolEntryCompleted},
	}}
}

func validModelStepRequest() models.ModelStepRequest {
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
			assistantView("e2", 2, "c1", "c2"),
			toolView("e3", 3, "c1", models.ToolEntryCompleted),
			userView("e4", 4, "steer"),
			toolView("e5", 5, "c2", models.ToolEntryError),
		},
		Stream: models.ModelStepStream{FinalAnswerTool: "turn_final_answer", AnswerPreview: true},
	}
}

func expectModelStepInvalid(t *testing.T, req models.ModelStepRequest, want string) {
	t.Helper()
	err := req.Validate()
	if err == nil {
		t.Fatalf("Validate() = nil, want error containing %q", want)
	}
	if !errors.Is(err, models.ErrInvalidModelStepRequest) {
		t.Fatalf("Validate() = %v, want ErrInvalidModelStepRequest", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Validate() = %v, want it to contain %q", err, want)
	}
}

func TestModelStepRequestValidateAcceptsValidStep(t *testing.T) {
	if err := validModelStepRequest().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestModelStepRequestValidateAcceptsCompactedPath(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = []models.SessionEntryView{
		{ID: "c", Sequence: 10, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "Earlier work.", FirstKeptEntryID: "e7", TokensBefore: 9000}},
		userView("e7", 7, "kept"),
		assistantView("e8", 8, "c1"),
		toolView("e9", 9, "c1", models.ToolEntryInterrupted),
		{ID: "e11", Sequence: 11, Type: models.SessionEntryNote, Note: &models.NoteEntryView{Kind: string(models.NoteHost), Text: "note"}},
		userView("e12", 12, "after"),
		{ID: "e13", Sequence: 13, Type: models.SessionEntryModelChange, ModelChange: &models.ModelChangeEntry{Provider: "openai-responses", Model: "gpt-x"}},
		assistantView("e14", 14),
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	// A compaction with nothing kept and nothing after it is still a path.
	req.Entries = []models.SessionEntryView{
		{ID: "c", Sequence: 10, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "All of it."}},
		userView("e11", 11, "next"),
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() without kept tail = %v", err)
	}
}

func TestModelStepRequestValidateRejectsCompactedPathOrder(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = []models.SessionEntryView{
		{ID: "c", Sequence: 10, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "s", FirstKeptEntryID: "e7"}},
		userView("e7", 7, "kept"),
		userView("e12", 12, "after"),
		userView("e8", 8, "late kept"),
	}
	expectModelStepInvalid(t, req, "kept tail must come before")

	req.Entries = []models.SessionEntryView{
		{ID: "c", Sequence: 10, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "s", FirstKeptEntryID: "e7"}},
		userView("e8", 8, "a"),
		userView("e7", 7, "b"),
	}
	expectModelStepInvalid(t, req, "out of sequence order")

	req.Entries = []models.SessionEntryView{
		{ID: "c", Sequence: 10, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "s", FirstKeptEntryID: "e6"}},
		userView("e7", 7, "a"),
	}
	expectModelStepInvalid(t, req, "kept tail does not start there")

	req = validModelStepRequest()
	req.Entries[2], req.Entries[3] = req.Entries[3], req.Entries[2]
	req.Entries[2].Sequence, req.Entries[3].Sequence = 3, 4
	if err := req.Validate(); err != nil {
		t.Fatalf("reordered steer is valid: %v", err)
	}
	req.Entries[3].Sequence = 2
	expectModelStepInvalid(t, req, "out of sequence order")
}

func TestModelStepRequestValidateRejectsUnpairedToolCall(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = req.Entries[:4] // c2 never answered
	expectModelStepInvalid(t, req, "tool call c2 of assistant entry e2 has no tool entry")

	req = validModelStepRequest()
	req.Entries = append(req.Entries[:5:5], assistantView("e6", 6), toolView("e7", 7, "c2", models.ToolEntryCompleted))
	expectModelStepInvalid(t, req, "answers no call of the preceding assistant entry")

	req = validModelStepRequest()
	req.Entries = append(req.Entries[:5:5], toolView("e6", 6, "c2", models.ToolEntryCompleted))
	expectModelStepInvalid(t, req, "a second time")

	req = validModelStepRequest()
	req.Entries = []models.SessionEntryView{
		userView("e1", 1, "hi"),
		assistantView("e2", 2, "c1"),
		assistantView("e3", 3),
		toolView("e4", 4, "c1", models.ToolEntryCompleted),
	}
	expectModelStepInvalid(t, req, "tool call c1 of assistant entry e2 has no tool entry")
}

func TestModelStepRequestValidateRejectsNonTerminalToolEntry(t *testing.T) {
	for _, state := range models.OpenToolEntryStateValues() {
		req := validModelStepRequest()
		req.Entries[2] = toolView("e3", 3, "c1", state)
		expectModelStepInvalid(t, req, "only finished tool entries are sent")
	}
	req := validModelStepRequest()
	req.Entries[2].Tool.Result.IsError = true
	expectModelStepInvalid(t, req, "isError disagrees")
}

func TestModelStepRequestValidateRejectsToolGroupOrder(t *testing.T) {
	req := validModelStepRequest()
	req.Tools[0], req.Tools[1] = req.Tools[1], req.Tools[0]
	expectModelStepInvalid(t, req, "out of order")

	req = validModelStepRequest()
	req.Tools[2].Group = ""
	expectModelStepInvalid(t, req, "unknown group")

	req = validModelStepRequest()
	req.Tools[1].Name = "turn_final_answer"
	expectModelStepInvalid(t, req, "duplicate tool name")

	req = validModelStepRequest()
	req.Tools[1].Name = "skill.lookup"
	expectModelStepInvalid(t, req, "invalid name")

	req = validModelStepRequest()
	req.Tools[1].InputSchema = map[string]interface{}{"type": "string"}
	expectModelStepInvalid(t, req, "type object")

	req = validModelStepRequest()
	req.Stream.FinalAnswerTool = "missing_tool"
	expectModelStepInvalid(t, req, "not a listed tool")

	// Several tools in the same group, and empty groups, are fine.
	req = validModelStepRequest()
	req.Tools = []models.ModelStepTool{req.Tools[0], req.Tools[2], {Name: "more", InputSchema: map[string]interface{}{"type": "object"}, Group: models.ModelStepToolLoaded}}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestModelStepRequestValidateRejectsCompactionNotFirst(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = []models.SessionEntryView{
		userView("e1", 1, "hi"),
		{ID: "c", Sequence: 2, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "s"}},
	}
	expectModelStepInvalid(t, req, "must be the first entry")

	req.Entries = []models.SessionEntryView{
		{ID: "c", Sequence: 5, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "s"}},
		userView("e6", 6, "hi"),
		{ID: "c2", Sequence: 7, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "s"}},
	}
	expectModelStepInvalid(t, req, "must be the first entry")
}

func TestModelStepRequestValidateRejectsLeadingToolEntry(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = []models.SessionEntryView{toolView("e1", 1, "c1", models.ToolEntryCompleted), userView("e2", 2, "hi")}
	expectModelStepInvalid(t, req, "cannot start with a tool entry")

	req.Entries = []models.SessionEntryView{
		{ID: "c", Sequence: 5, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "s", FirstKeptEntryID: "e3"}},
		toolView("e3", 3, "c1", models.ToolEntryCompleted),
	}
	expectModelStepInvalid(t, req, "cannot start with a tool entry")
}

func TestModelStepRequestValidateRejectsBadIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*models.ModelStepRequest)
		want   string
	}{
		{"version", func(r *models.ModelStepRequest) { r.APIVersion = "openseal.hosted-turn/v14" }, "apiVersion"},
		{"purpose", func(r *models.ModelStepRequest) { r.Purpose = "chat" }, "unknown purpose"},
		{"scope", func(r *models.ModelStepRequest) { r.Scope = models.Scope{} }, "scope"},
		{"run", func(r *models.ModelStepRequest) { r.RunID = " " }, "runId"},
		{"step", func(r *models.ModelStepRequest) { r.StepID = "" }, "stepId"},
		{"agent", func(r *models.ModelStepRequest) { r.AgentID = "" }, "agentId"},
		{"session", func(r *models.ModelStepRequest) { r.SessionID = "" }, "sessionId"},
		{"attempt", func(r *models.ModelStepRequest) { r.Attempt = -1 }, "attempt"},
		{"max output", func(r *models.ModelStepRequest) { r.MaxOutputTokens = -5 }, "maxOutputTokens"},
		{"stray instruction", func(r *models.ModelStepRequest) { r.SummaryInstruction = "Summarize." }, "only for summary"},
		{"section id", func(r *models.ModelStepRequest) { r.System.Sections[0].ID = "Progress Rules" }, "invalid id"},
		{"section dup", func(r *models.ModelStepRequest) {
			r.System.Sections = append(r.System.Sections, models.ModelStepSystemSection{ID: "progress_rules", Title: "Again"})
		}, "duplicate system section"},
		{"no entries", func(r *models.ModelStepRequest) { r.Entries = nil }, "entries are required"},
		{"dup entry", func(r *models.ModelStepRequest) { r.Entries[3].ID = "e1" }, "duplicate entry id"},
		{"zero sequence", func(r *models.ModelStepRequest) { r.Entries[0].Sequence = 0 }, "positive sequence"},
		{"wrong payload", func(r *models.ModelStepRequest) { r.Entries[0].Type = models.SessionEntryNote }, "exactly the note payload"},
		{"two payloads", func(r *models.ModelStepRequest) { r.Entries[0].Note = &models.NoteEntryView{Kind: "host", Text: "x"} }, "exactly the user payload"},
		{"assistant limits", func(r *models.ModelStepRequest) {
			r.Entries[1].Assistant.Limits = &models.AssistantModelLimits{ContextTokens: 1}
		}, "kernel-only"},
		{"author kind", func(r *models.ModelStepRequest) { r.Entries[0].User.Author.Kind = "user" }, "author kind"},
		{"tool args", func(r *models.ModelStepRequest) { r.Entries[2].Tool.Arguments = json.RawMessage(`[1]`) }, "JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validModelStepRequest()
			tc.mutate(&req)
			expectModelStepInvalid(t, req, tc.want)
		})
	}
}

func TestModelStepRequestValidatePurposes(t *testing.T) {
	t.Run("summary", func(t *testing.T) {
		req := validModelStepRequest()
		req.Purpose = models.ModelStepPurposeSummary
		expectModelStepInvalid(t, req, "needs summaryInstruction")
		req.SummaryInstruction = "Summarize the conversation so far."
		if err := req.Validate(); err != nil {
			t.Fatalf("summary with tools = %v", err)
		}
		req.Entries = req.Entries[:1]
		expectModelStepInvalid(t, req, "at least two entries")
	})
	t.Run("explanation", func(t *testing.T) {
		req := validModelStepRequest()
		req.Purpose = models.ModelStepPurposeExplanation
		expectModelStepInvalid(t, req, "sends no tools")
		req.Tools = nil
		expectModelStepInvalid(t, req, "not a listed tool")
		req.Stream = models.ModelStepStream{}
		if err := req.Validate(); err != nil {
			t.Fatalf("explanation = %v", err)
		}
		req.SessionID = ""
		expectModelStepInvalid(t, req, "sessionId")
	})
	t.Run("acknowledgement", func(t *testing.T) {
		req := validModelStepRequest()
		req.Purpose = models.ModelStepPurposeAcknowledgement
		req.SessionID = ""
		req.Stream = models.ModelStepStream{}
		expectModelStepInvalid(t, req, "sends no tools")
		req.Tools = []models.ModelStepTool{}
		req.Entries = []models.SessionEntryView{userView("ack-1", 1, "Please book the flight.")}
		if err := req.Validate(); err != nil {
			t.Fatalf("acknowledgement = %v", err)
		}
	})
}

func TestModelStepRequestValidateContentParts(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	req := validModelStepRequest()
	req.Entries[0].User.Content = append(req.Entries[0].User.Content,
		models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{"a":1}`)},
		models.ContentPart{Type: models.ContentPartFile, Path: "chat-files/report.pdf", MimeType: "application/pdf", SizeBytes: 900},
		models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", Detail: "low", DataBase64: png},
	)
	req.Entries[2].Tool.Result.Content = append(req.Entries[2].Tool.Result.Content,
		models.ContentPart{Type: models.ContentPartImage, MediaType: "image/webp", DataBase64: png, ActionCallID: "ac-1"})
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}

	cases := []struct {
		name string
		part models.ContentPart
		want string
	}{
		{"unknown type", models.ContentPart{Type: "audio"}, "unknown part type"},
		{"bad json", models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{`)}, "valid value"},
		{"image type", models.ContentPart{Type: models.ContentPartImage, MediaType: "image/gif", DataBase64: png}, "media type"},
		{"image detail", models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", Detail: "max", DataBase64: png}, "detail"},
		{"image without bytes", models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", ArtifactID: "a1"}, "base64 data"},
		{"image too large", models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(make([]byte, models.MaxModelStepMediaBytes+1))}, "max"},
		{"text with bytes", models.ContentPart{Type: models.ContentPartText, Text: "x", DataBase64: png}, "only image parts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validModelStepRequest()
			req.Entries[2].Tool.Result.Content = append(req.Entries[2].Tool.Result.Content, tc.part)
			expectModelStepInvalid(t, req, tc.want)
		})
	}

	req = validModelStepRequest()
	req.Entries[2].Tool.Result.Content = []models.ContentPart{{Type: models.ContentPartText, Text: strings.Repeat("x", models.MaxToolResultContentBytes+1)}}
	expectModelStepInvalid(t, req, "max")
}

// encoding/json compacts a RawMessage on marshal (and escapes <, >, & as
// \u003c...), so "verbatim" holds for the compact, escaped JSON that provider
// SDKs return from RawJSON(). Whitespace-only differences are not
// significant to any provider.
func TestModelStepJSONRoundTripKeepsNativeVerbatim(t *testing.T) {
	anthropic := []json.RawMessage{
		json.RawMessage(`{"type":"thinking","thinking":"plan  it","signature":"c2lnbmF0dXJl+/=="}`),
		json.RawMessage(`{"type":"redacted_thinking","data":"ZGF0YQ=="}`),
		json.RawMessage(`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"z":1,"a":[2,3]}}`),
	}
	responses := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"gAAAA\u003cx\u003e","summary":[]}`),
		json.RawMessage(`{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"hi"}]}`),
	}
	result := models.ModelStepResult{
		APIVersion: models.ModelStepAPIVersion, StepID: "step-1",
		Assistant: models.AssistantEntry{
			Provider: models.ModelStepProviderAnthropic, Model: "claude-test", SystemDigest: "sha256:a", ToolsDigest: "sha256:b",
			Text: "Checking.", ToolCalls: []models.ModelToolCall{{ID: "toolu_1", Name: "lookup", Arguments: json.RawMessage(`{"z":1,"a":[2, 3]}`)}},
			Native:     &models.AssistantNative{Anthropic: anthropic, Responses: responses, ChatReasoningField: "reasoning_content", ChatReasoning: "r"},
			StopReason: models.ModelStopToolCalls, Usage: models.ModelStepUsage{InputTokens: 10, OutputTokens: 5, Attempts: 1},
		},
		Narration: "Checking.",
		Model:     models.ModelStepModelInfo{Provider: models.ModelStepProviderAnthropic, Model: "claude-test", ContextTokens: 200000},
		Render:    models.ModelStepRenderInfo{EstimatedInputTokens: 100, ReasoningCutoffSequence: 4},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded models.ModelStepResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for i := range anthropic {
		if !bytes.Equal(decoded.Assistant.Native.Anthropic[i], anthropic[i]) {
			t.Fatalf("anthropic block %d = %s, want %s", i, decoded.Assistant.Native.Anthropic[i], anthropic[i])
		}
	}
	for i := range responses {
		if !bytes.Equal(decoded.Assistant.Native.Responses[i], responses[i]) {
			t.Fatalf("responses item %d = %s, want %s", i, decoded.Assistant.Native.Responses[i], responses[i])
		}
	}
	again, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, encoded) {
		t.Fatalf("second round trip changed bytes:\n%s\n%s", again, encoded)
	}
	if decoded.Assistant.Native.ChatReasoning != "r" || decoded.Render.ReasoningCutoffSequence != 4 || decoded.Model.ContextTokens != 200000 {
		t.Fatalf("decoded result lost fields: %+v", decoded)
	}

	// The request side carries the same native bytes through an assistant view.
	req := validModelStepRequest()
	req.Entries[1].Assistant.Native = &models.AssistantNative{Anthropic: anthropic}
	req.Purpose = models.ModelStepPurposeSummary
	req.SummaryInstruction = "Summarize."
	encoded, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decodedReq models.ModelStepRequest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decodedReq); err != nil {
		t.Fatal(err)
	}
	for i := range anthropic {
		if !bytes.Equal(decodedReq.Entries[1].Assistant.Native.Anthropic[i], anthropic[i]) {
			t.Fatalf("request anthropic block %d changed", i)
		}
	}
	if decodedReq.SummaryInstruction != "Summarize." || decodedReq.Purpose != models.ModelStepPurposeSummary {
		t.Fatalf("request lost purpose fields: %+v", decodedReq)
	}
	if err := decodedReq.Validate(); err != nil {
		t.Fatalf("decoded request Validate() = %v", err)
	}
}
