package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func modelStepTestAssistant(calls ...string) *AssistantEntry {
	a := &AssistantEntry{
		Provider: ModelStepProviderAnthropic, Model: "claude-test",
		SystemDigest: "sha256:aa", ToolsDigest: "sha256:bb",
		StopReason: ModelStopEnd, Usage: ModelStepUsage{Attempts: 1},
	}
	for _, id := range calls {
		a.ToolCalls = append(a.ToolCalls, ModelToolCall{ID: id, Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)})
	}
	if len(calls) > 0 {
		a.StopReason = ModelStopToolCalls
	}
	return a
}

func userView(id string, seq int64, text string) SessionEntryView {
	return SessionEntryView{ID: id, Sequence: seq, Type: SessionEntryUser, CreatedAt: time.Unix(seq, 0).UTC(),
		User: &UserEntryView{Author: &EntryAuthor{Kind: EntryAuthorPerson, ID: "u1", Label: "Ana"}, Content: []ContentPart{{Type: ContentPartText, Text: text}}}}
}

func assistantView(id string, seq int64, calls ...string) SessionEntryView {
	return SessionEntryView{ID: id, Sequence: seq, Type: SessionEntryAssistant, Assistant: modelStepTestAssistant(calls...)}
}

func toolView(id string, seq int64, callID string, state ToolEntryState) SessionEntryView {
	return SessionEntryView{ID: id, Sequence: seq, Type: SessionEntryTool, Tool: &ToolEntryView{
		CallID: callID, Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), State: state,
		Result: ToolResultView{Content: []ContentPart{{Type: ContentPartText, Text: "ok"}}, IsError: state != ToolEntryCompleted},
	}}
}

func validModelStepRequest() ModelStepRequest {
	return ModelStepRequest{
		APIVersion: ModelStepAPIVersion,
		Purpose:    ModelStepPurposeStep,
		Scope:      Scope{Kind: "tenant", ID: "t1"},
		RunID:      "run-1", StepID: "step-1", SessionID: "session-1", AgentID: "agent-1",
		DefinitionID: "def-1", DefinitionVersion: "1",
		System: ModelStepSystem{
			AgentInstructions: []string{"Be helpful."},
			Sections:          []ModelStepSystemSection{{ID: "progress_rules", Title: "Progress", Text: "Report."}},
		},
		Tools: []ModelStepTool{
			{Name: "turn_final_answer", Description: "Final.", InputSchema: map[string]interface{}{"type": "object"}, Group: ModelStepToolHarness},
			{Name: "lookup", Description: "Look up.", InputSchema: map[string]interface{}{"type": "object"}, Group: ModelStepToolCore},
			{Name: "skill-live-browser_open", Description: "Open.", InputSchema: map[string]interface{}{"type": "object"}, Group: ModelStepToolLoaded, Capability: "skill-live-browser.open"},
		},
		Entries: []SessionEntryView{
			userView("e1", 1, "hello"),
			assistantView("e2", 2, "c1", "c2"),
			toolView("e3", 3, "c1", ToolEntryCompleted),
			userView("e4", 4, "steer"),
			toolView("e5", 5, "c2", ToolEntryError),
		},
		Stream: ModelStepStream{FinalAnswerTool: "turn_final_answer", AnswerPreview: true},
	}
}

func expectModelStepInvalid(t *testing.T, req ModelStepRequest, want string) {
	t.Helper()
	err := req.Validate()
	if err == nil {
		t.Fatalf("Validate() = nil, want error containing %q", want)
	}
	if !errors.Is(err, ErrInvalidModelStepRequest) {
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
	req.Entries = []SessionEntryView{
		{ID: "c", Sequence: 10, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "Earlier work.", FirstKeptEntryID: "e7", TokensBefore: 9000}},
		userView("e7", 7, "kept"),
		assistantView("e8", 8, "c1"),
		toolView("e9", 9, "c1", ToolEntryInterrupted),
		{ID: "e11", Sequence: 11, Type: SessionEntryNote, Note: &NoteEntryView{Kind: string(NoteHost), Text: "note"}},
		userView("e12", 12, "after"),
		{ID: "e13", Sequence: 13, Type: SessionEntryModelChange, ModelChange: &ModelChangeEntry{Provider: "openai-responses", Model: "gpt-x"}},
		assistantView("e14", 14),
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	// A compaction with nothing kept and nothing after it is still a path.
	req.Entries = []SessionEntryView{
		{ID: "c", Sequence: 10, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "All of it."}},
		userView("e11", 11, "next"),
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() without kept tail = %v", err)
	}
}

func TestModelStepRequestValidateRejectsCompactedPathOrder(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = []SessionEntryView{
		{ID: "c", Sequence: 10, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "s", FirstKeptEntryID: "e7"}},
		userView("e7", 7, "kept"),
		userView("e12", 12, "after"),
		userView("e8", 8, "late kept"),
	}
	expectModelStepInvalid(t, req, "kept tail must come before")

	req.Entries = []SessionEntryView{
		{ID: "c", Sequence: 10, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "s", FirstKeptEntryID: "e7"}},
		userView("e8", 8, "a"),
		userView("e7", 7, "b"),
	}
	expectModelStepInvalid(t, req, "out of sequence order")

	req.Entries = []SessionEntryView{
		{ID: "c", Sequence: 10, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "s", FirstKeptEntryID: "e6"}},
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
	req.Entries = append(req.Entries[:5:5], assistantView("e6", 6), toolView("e7", 7, "c2", ToolEntryCompleted))
	expectModelStepInvalid(t, req, "answers no call of the preceding assistant entry")

	req = validModelStepRequest()
	req.Entries = append(req.Entries[:5:5], toolView("e6", 6, "c2", ToolEntryCompleted))
	expectModelStepInvalid(t, req, "a second time")

	req = validModelStepRequest()
	req.Entries = []SessionEntryView{
		userView("e1", 1, "hi"),
		assistantView("e2", 2, "c1"),
		assistantView("e3", 3),
		toolView("e4", 4, "c1", ToolEntryCompleted),
	}
	expectModelStepInvalid(t, req, "tool call c1 of assistant entry e2 has no tool entry")
}

func TestModelStepRequestValidateRejectsNonTerminalToolEntry(t *testing.T) {
	for _, state := range OpenToolEntryStates {
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
	req.Tools = []ModelStepTool{req.Tools[0], req.Tools[2], {Name: "more", InputSchema: map[string]interface{}{"type": "object"}, Group: ModelStepToolLoaded}}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestModelStepRequestValidateRejectsCompactionNotFirst(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = []SessionEntryView{
		userView("e1", 1, "hi"),
		{ID: "c", Sequence: 2, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "s"}},
	}
	expectModelStepInvalid(t, req, "must be the first entry")

	req.Entries = []SessionEntryView{
		{ID: "c", Sequence: 5, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "s"}},
		userView("e6", 6, "hi"),
		{ID: "c2", Sequence: 7, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "s"}},
	}
	expectModelStepInvalid(t, req, "must be the first entry")
}

func TestModelStepRequestValidateRejectsLeadingToolEntry(t *testing.T) {
	req := validModelStepRequest()
	req.Entries = []SessionEntryView{toolView("e1", 1, "c1", ToolEntryCompleted), userView("e2", 2, "hi")}
	expectModelStepInvalid(t, req, "cannot start with a tool entry")

	req.Entries = []SessionEntryView{
		{ID: "c", Sequence: 5, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "s", FirstKeptEntryID: "e3"}},
		toolView("e3", 3, "c1", ToolEntryCompleted),
	}
	expectModelStepInvalid(t, req, "cannot start with a tool entry")
}

func TestModelStepRequestValidateRejectsBadIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ModelStepRequest)
		want   string
	}{
		{"version", func(r *ModelStepRequest) { r.APIVersion = "openseal.hosted-turn/v14" }, "apiVersion"},
		{"purpose", func(r *ModelStepRequest) { r.Purpose = "chat" }, "unknown purpose"},
		{"scope", func(r *ModelStepRequest) { r.Scope = Scope{} }, "scope"},
		{"run", func(r *ModelStepRequest) { r.RunID = " " }, "runId"},
		{"step", func(r *ModelStepRequest) { r.StepID = "" }, "stepId"},
		{"agent", func(r *ModelStepRequest) { r.AgentID = "" }, "agentId"},
		{"session", func(r *ModelStepRequest) { r.SessionID = "" }, "sessionId"},
		{"attempt", func(r *ModelStepRequest) { r.Attempt = -1 }, "attempt"},
		{"max output", func(r *ModelStepRequest) { r.MaxOutputTokens = -5 }, "maxOutputTokens"},
		{"stray instruction", func(r *ModelStepRequest) { r.SummaryInstruction = "Summarize." }, "only for summary"},
		{"section id", func(r *ModelStepRequest) { r.System.Sections[0].ID = "Progress Rules" }, "invalid id"},
		{"section dup", func(r *ModelStepRequest) {
			r.System.Sections = append(r.System.Sections, ModelStepSystemSection{ID: "progress_rules", Title: "Again"})
		}, "duplicate system section"},
		{"no entries", func(r *ModelStepRequest) { r.Entries = nil }, "entries are required"},
		{"dup entry", func(r *ModelStepRequest) { r.Entries[3].ID = "e1" }, "duplicate entry id"},
		{"zero sequence", func(r *ModelStepRequest) { r.Entries[0].Sequence = 0 }, "positive sequence"},
		{"wrong payload", func(r *ModelStepRequest) { r.Entries[0].Type = SessionEntryNote }, "exactly the note payload"},
		{"two payloads", func(r *ModelStepRequest) { r.Entries[0].Note = &NoteEntryView{Kind: "host", Text: "x"} }, "exactly the user payload"},
		{"assistant limits", func(r *ModelStepRequest) { r.Entries[1].Assistant.Limits = &AssistantModelLimits{ContextTokens: 1} }, "kernel-only"},
		{"author kind", func(r *ModelStepRequest) { r.Entries[0].User.Author.Kind = "user" }, "author kind"},
		{"tool args", func(r *ModelStepRequest) { r.Entries[2].Tool.Arguments = json.RawMessage(`[1]`) }, "JSON object"},
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
		req.Purpose = ModelStepPurposeSummary
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
		req.Purpose = ModelStepPurposeExplanation
		expectModelStepInvalid(t, req, "sends no tools")
		req.Tools = nil
		expectModelStepInvalid(t, req, "not a listed tool")
		req.Stream = ModelStepStream{}
		if err := req.Validate(); err != nil {
			t.Fatalf("explanation = %v", err)
		}
		req.SessionID = ""
		expectModelStepInvalid(t, req, "sessionId")
	})
	t.Run("acknowledgement", func(t *testing.T) {
		req := validModelStepRequest()
		req.Purpose = ModelStepPurposeAcknowledgement
		req.SessionID = ""
		req.Stream = ModelStepStream{}
		expectModelStepInvalid(t, req, "sends no tools")
		req.Tools = []ModelStepTool{}
		req.Entries = []SessionEntryView{userView("ack-1", 1, "Please book the flight.")}
		if err := req.Validate(); err != nil {
			t.Fatalf("acknowledgement = %v", err)
		}
	})
}

func TestModelStepRequestValidateContentParts(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	req := validModelStepRequest()
	req.Entries[0].User.Content = append(req.Entries[0].User.Content,
		ContentPart{Type: ContentPartJSON, Value: json.RawMessage(`{"a":1}`)},
		ContentPart{Type: ContentPartFile, Path: "chat-files/report.pdf", MimeType: "application/pdf", SizeBytes: 900},
		ContentPart{Type: ContentPartImage, MediaType: "image/png", Detail: "low", DataBase64: png},
	)
	req.Entries[2].Tool.Result.Content = append(req.Entries[2].Tool.Result.Content,
		ContentPart{Type: ContentPartImage, MediaType: "image/webp", DataBase64: png, ActionCallID: "ac-1"})
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}

	cases := []struct {
		name string
		part ContentPart
		want string
	}{
		{"unknown type", ContentPart{Type: "audio"}, "unknown part type"},
		{"bad json", ContentPart{Type: ContentPartJSON, Value: json.RawMessage(`{`)}, "valid value"},
		{"image type", ContentPart{Type: ContentPartImage, MediaType: "image/gif", DataBase64: png}, "media type"},
		{"image detail", ContentPart{Type: ContentPartImage, MediaType: "image/png", Detail: "max", DataBase64: png}, "detail"},
		{"image without bytes", ContentPart{Type: ContentPartImage, MediaType: "image/png", ArtifactID: "a1"}, "base64 data"},
		{"image too large", ContentPart{Type: ContentPartImage, MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(make([]byte, maxModelStepMediaBytes+1))}, "max"},
		{"text with bytes", ContentPart{Type: ContentPartText, Text: "x", DataBase64: png}, "only image parts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validModelStepRequest()
			req.Entries[2].Tool.Result.Content = append(req.Entries[2].Tool.Result.Content, tc.part)
			expectModelStepInvalid(t, req, tc.want)
		})
	}

	req = validModelStepRequest()
	req.Entries[2].Tool.Result.Content = []ContentPart{{Type: ContentPartText, Text: strings.Repeat("x", MaxToolResultContentBytes+1)}}
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
	result := ModelStepResult{
		APIVersion: ModelStepAPIVersion, StepID: "step-1",
		Assistant: AssistantEntry{
			Provider: ModelStepProviderAnthropic, Model: "claude-test", SystemDigest: "sha256:a", ToolsDigest: "sha256:b",
			Text: "Checking.", ToolCalls: []ModelToolCall{{ID: "toolu_1", Name: "lookup", Arguments: json.RawMessage(`{"z":1,"a":[2, 3]}`)}},
			Native:     &AssistantNative{Anthropic: anthropic, Responses: responses, ChatReasoningField: "reasoning_content", ChatReasoning: "r"},
			StopReason: ModelStopToolCalls, Usage: ModelStepUsage{InputTokens: 10, OutputTokens: 5, Attempts: 1},
		},
		Narration: "Checking.",
		Model:     ModelStepModelInfo{Provider: ModelStepProviderAnthropic, Model: "claude-test", ContextTokens: 200000},
		Render:    ModelStepRenderInfo{EstimatedInputTokens: 100, ReasoningCutoffSequence: 4},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ModelStepResult
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
	req.Entries[1].Assistant.Native = &AssistantNative{Anthropic: anthropic}
	req.Purpose = ModelStepPurposeSummary
	req.SummaryInstruction = "Summarize."
	encoded, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decodedReq ModelStepRequest
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
	if decodedReq.SummaryInstruction != "Summarize." || decodedReq.Purpose != ModelStepPurposeSummary {
		t.Fatalf("request lost purpose fields: %+v", decodedReq)
	}
	if err := decodedReq.Validate(); err != nil {
		t.Fatalf("decoded request Validate() = %v", err)
	}
}
