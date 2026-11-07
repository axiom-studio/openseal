package daemon

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

const renderModel = "deepseek-chat"

func userView(id string, seq int64, text string) runtime.SessionEntryView {
	return runtime.SessionEntryView{ID: id, Sequence: seq, Type: runtime.SessionEntryUser,
		User: &runtime.UserEntryView{Content: []runtime.ContentPart{{Type: runtime.ContentPartText, Text: text}}}}
}

func assistantView(id string, seq int64, model, text, reasoning string, calls ...runtime.ModelToolCall) runtime.SessionEntryView {
	a := &runtime.AssistantEntry{Provider: runtime.ModelStepProviderChat, Model: model, Text: text, ToolCalls: calls}
	if reasoning != "" {
		a.Native = &runtime.AssistantNative{ChatReasoningField: "reasoning_content", ChatReasoning: reasoning}
	}
	return runtime.SessionEntryView{ID: id, Sequence: seq, Type: runtime.SessionEntryAssistant, Assistant: a}
}

func toolCall(id, name, args string) runtime.ModelToolCall {
	return runtime.ModelToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func toolView(id string, seq int64, callID string, state runtime.ToolEntryState, parts ...runtime.ContentPart) runtime.SessionEntryView {
	return runtime.SessionEntryView{ID: id, Sequence: seq, Type: runtime.SessionEntryTool, Tool: &runtime.ToolEntryView{
		CallID: callID, Name: "lookup", Arguments: json.RawMessage(`{}`), State: state,
		Result: runtime.ToolResultView{Content: parts, IsError: state != runtime.ToolEntryCompleted},
	}}
}

func textPart(text string) runtime.ContentPart {
	return runtime.ContentPart{Type: runtime.ContentPartText, Text: text}
}

func noteView(id string, seq int64, text string) runtime.SessionEntryView {
	return runtime.SessionEntryView{ID: id, Sequence: seq, Type: runtime.SessionEntryNote, Note: &runtime.NoteEntryView{Kind: "run_context", Text: text}}
}

func compactionView(id string, seq int64, summary string) runtime.SessionEntryView {
	return runtime.SessionEntryView{ID: id, Sequence: seq, Type: runtime.SessionEntryCompaction, Compaction: &runtime.CompactionEntryView{Summary: summary}}
}

// wire marshals messages and decodes them back to generic maps, so tests
// assert on the exact JSON the provider receives.
func wire(t *testing.T, messages []openAIMessage) []map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func renderCode(t *testing.T, err error) string {
	t.Helper()
	var failure *runtime.TurnHostFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a TurnHostFailure", err)
	}
	if failure.Retryable {
		t.Fatalf("render failure %q is retryable", failure.Code)
	}
	return failure.Code
}

func TestRenderOpenAIMessagesOrdersSystemCompactionAndPath(t *testing.T) {
	entries := []runtime.SessionEntryView{
		compactionView("c", 10, "The conversation and work before this point were compacted.\n\nSummary."),
		userView("kept", 4, "kept request"),
		assistantView("a1", 11, renderModel, "Looking it up.", "", toolCall("call_1", "lookup", `{"q":"x"}`)),
		toolView("t1", 12, "call_1", runtime.ToolEntryCompleted, textPart("found"), runtime.ContentPart{Type: runtime.ContentPartJSON, Value: json.RawMessage(`{ "n": 1 }`)}),
		{ID: "mc", Sequence: 13, Type: runtime.SessionEntryModelChange, ModelChange: &runtime.ModelChangeEntry{Provider: "openai-chat", Model: renderModel}},
		assistantView("empty", 14, renderModel, "", ""),
		assistantView("a2", 15, renderModel, "Done.", ""),
	}
	messages, info, err := renderOpenAIMessages("SYSTEM", entries, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	got := wire(t, messages)
	want := []map[string]interface{}{
		{"role": "system", "content": "SYSTEM"},
		{"role": "user", "content": "The conversation and work before this point were compacted.\n\nSummary."},
		{"role": "user", "content": "kept request"},
		{"role": "assistant", "content": "Looking it up.", "tool_calls": []interface{}{map[string]interface{}{
			"id": "call_1", "type": "function", "function": map[string]interface{}{"name": "lookup", "arguments": `{"q":"x"}`}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "found\n{\"n\":1}"},
		{"role": "assistant", "content": "Done."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages\n got %#v\nwant %#v", got, want)
	}
	if info != (openAIRenderInfo{}) {
		t.Fatalf("info = %+v", info)
	}
}

func TestRenderOpenAIStepBuildsSystemSummaryAndTools(t *testing.T) {
	req := runtime.ModelStepRequest{
		Purpose: runtime.ModelStepPurposeSummary,
		System: runtime.ModelStepSystem{
			AgentInstructions: []string{"Be Rowan.", " "},
			SkillPrompts:      []runtime.HostedSkillPrompt{{Name: "Notes", Reference: "skill:notes@1", Instructions: "Use notes."}},
			Sections:          []runtime.ModelStepSystemSection{{ID: "progress_rules", Title: "Progress", Text: "Narrate."}, {ID: "empty", Title: "Empty", Text: ""}},
		},
		Tools:              []runtime.ModelStepTool{{Name: "turn_final_answer", InputSchema: map[string]interface{}{"type": "object"}, Group: runtime.ModelStepToolHarness}},
		Stream:             runtime.ModelStepStream{FinalAnswerTool: "turn_final_answer"},
		Entries:            []runtime.SessionEntryView{userView("u", 1, "hi"), assistantView("a", 2, renderModel, "hello", "")},
		SummaryInstruction: "Write a summary.",
	}
	out, err := renderOpenAIStep(req, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	system := *out.Messages[0].Content
	wantSystem := strings.Join([]string{
		openAIStepHarnessPrompt,
		"Call turn_final_answer instead of replying with plain text when your answer needs one of its fields: a visual, a report with evidence, files, staying silent, or the Skills you applied.",
		"## Agent\nBe Rowan.",
		"## Skill: Notes (reference skill:notes@1)\nUse notes.",
		"## Progress\nNarrate.",
	}, "\n\n")
	if system != wantSystem {
		t.Fatalf("system\n got %q\nwant %q", system, wantSystem)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "user" || *last.Content != "Write a summary." {
		t.Fatalf("summary instruction not last: %+v", last)
	}
	if out.ToolChoice != "none" || len(out.Tools) != 1 {
		t.Fatalf("summary tools = %+v choice %v", out.Tools, out.ToolChoice)
	}

	req.Purpose, req.SummaryInstruction, req.Tools, req.Stream = runtime.ModelStepPurposeExplanation, "", nil, runtime.ModelStepStream{}
	out, err = renderOpenAIStep(req, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	if out.Tools != nil || out.ToolChoice != nil || strings.Contains(*out.Messages[0].Content, "Call turn_final_answer") {
		t.Fatalf("tool-free step rendered tools: %+v", out)
	}
}

func TestRenderOpenAIMessagesAttributesUsersAndRendersNotes(t *testing.T) {
	alice := userView("u1", 1, "first line")
	alice.User.Author = &runtime.EntryAuthor{Kind: runtime.EntryAuthorPerson, ID: "p1", Label: "Alice"}
	alice.User.Content = append(alice.User.Content,
		textPart("second line"),
		runtime.ContentPart{Type: runtime.ContentPartFile, Path: "chat/report.csv", MimeType: "text/csv", SizeBytes: 42})
	blank := userView("u2", 2, "no author label")
	blank.User.Author = &runtime.EntryAuthor{Kind: runtime.EntryAuthorAgent, Label: " "}
	entries := []runtime.SessionEntryView{alice, blank, userView("u3", 3, "requester"), noteView("n", 4, "The task was saved.")}
	messages, _, err := renderOpenAIMessages("S", entries, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range messages[1:] {
		if m.Role != "user" {
			t.Fatalf("role %q, want user", m.Role)
		}
		got = append(got, *m.Content)
	}
	want := []string{
		"Alice: first line\nsecond line\nOutput saved to chat/report.csv (text/csv, 42 bytes).",
		"no author label",
		"requester",
		"Note from OpenSeal: The task was saved.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}

	image := userView("img", 5, "look")
	image.User.Content = append(image.User.Content, runtime.ContentPart{Type: runtime.ContentPartImage, MediaType: "image/png", DataBase64: "AA=="})
	if _, _, err := renderOpenAIMessages("S", []runtime.SessionEntryView{image}, renderModel); renderCode(t, err) != openAIRenderUnsupportedMedia {
		t.Fatalf("image user entry: %v", err)
	}
}

func TestRenderOpenAIMessagesReplaysReasoningOnlyInCurrentLoopForSameModel(t *testing.T) {
	entries := []runtime.SessionEntryView{
		userView("u1", 1, "earlier question"),
		assistantView("old", 2, renderModel, "old answer", "old thinking"),
		userView("u2", 3, "now"),
		assistantView("other", 4, "deepseek-reasoner", "", "foreign thinking", toolCall("c1", "lookup", `{}`)),
		toolView("t1", 5, "c1", runtime.ToolEntryCompleted, textPart("r1")),
		assistantView("same", 6, strings.ToUpper(renderModel), "", "current thinking", toolCall("c2", "lookup", `{}`)),
		toolView("t2", 7, "c2", runtime.ToolEntryCompleted, textPart("r2")),
	}
	entries[5].Assistant.Native.ChatReasoningField = "reasoning"
	messages, info, err := renderOpenAIMessages("S", entries, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	got := wire(t, messages)
	if _, ok := got[2]["reasoning_content"]; ok {
		t.Fatalf("reasoning before the last user entry was replayed: %v", got[2])
	}
	if _, ok := got[4]["reasoning_content"]; ok {
		t.Fatalf("another model's reasoning was replayed: %v", got[4])
	}
	if got[4]["content"] != nil {
		t.Fatalf("tool-only assistant content = %v, want null", got[4]["content"])
	}
	if got[6]["reasoning"] != "current thinking" {
		t.Fatalf("current loop reasoning not replayed under its field: %v", got[6])
	}
	if info.ReasoningCutoffSequence != 4 {
		t.Fatalf("cutoff = %d, want 4", info.ReasoningCutoffSequence)
	}

	// A later model change drops everything before it.
	changed := append(append([]runtime.SessionEntryView(nil), entries...),
		runtime.SessionEntryView{ID: "mc", Sequence: 8, Type: runtime.SessionEntryModelChange, ModelChange: &runtime.ModelChangeEntry{Provider: "openai-chat", Model: renderModel}})
	messages, info, err = renderOpenAIMessages("S", changed, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Reasoning != "" {
			t.Fatalf("reasoning replayed across a model change: %+v", m)
		}
	}
	if info.ReasoningCutoffSequence != 8 {
		t.Fatalf("cutoff = %d, want 8", info.ReasoningCutoffSequence)
	}

	// The kept tail before a compaction never replays reasoning.
	compacted := []runtime.SessionEntryView{
		compactionView("c", 20, "summary"),
		assistantView("kept", 9, renderModel, "", "kept thinking", toolCall("c9", "lookup", `{}`)),
		toolView("t9", 10, "c9", runtime.ToolEntryCompleted, textPart("r")),
	}
	messages, _, err = renderOpenAIMessages("S", compacted, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	if messages[2].Reasoning != "" {
		t.Fatalf("kept-tail reasoning replayed: %+v", messages[2])
	}
}

func TestRenderOpenAIMessagesRendersErrorsInterruptionsAndFileReferences(t *testing.T) {
	entries := []runtime.SessionEntryView{
		userView("u", 1, "go"),
		assistantView("a", 2, renderModel, "", "",
			toolCall("c1", "lookup", `{}`), toolCall("c2", "lookup", ``), toolCall("c3", "lookup", `{}`), toolCall("c4", "lookup", `{}`), toolCall("c5", "lookup", `{}`)),
		toolView("t1", 3, "c1", runtime.ToolEntryError, textPart("not allowed")),
		toolView("t2", 4, "c2", runtime.ToolEntryInterrupted, textPart("Error: already prefixed")),
		toolView("t3", 5, "c3", runtime.ToolEntryCompleted,
			runtime.ContentPart{Type: runtime.ContentPartFile, Path: "tool-output/ac_1.json", MimeType: "application/json", SizeBytes: 1200},
			runtime.ContentPart{Type: runtime.ContentPartImage, MediaType: "image/png", DataBase64: "AA=="}),
		toolView("t4", 6, "c4", runtime.ToolEntryCompleted),
		toolView("t5", 7, "c5", runtime.ToolEntryError),
	}
	messages, info, err := renderOpenAIMessages("S", entries, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	got := wire(t, messages)
	if args := got[2]["tool_calls"].([]interface{})[1].(map[string]interface{})["function"].(map[string]interface{})["arguments"]; args != "{}" {
		t.Fatalf("empty arguments rendered as %q", args)
	}
	var contents []string
	for _, m := range messages[3:] {
		if m.Role != "tool" {
			t.Fatalf("role %q", m.Role)
		}
		contents = append(contents, *m.Content)
	}
	want := []string{
		"Error: not allowed",
		"Error: already prefixed",
		"Output saved to tool-output/ac_1.json (application/json, 1200 bytes).\n[image omitted]",
		"(no output)",
		"Error: the tool failed without details.",
	}
	if !reflect.DeepEqual(contents, want) {
		t.Fatalf("got %q\nwant %q", contents, want)
	}
	if !info.ImagesOmitted {
		t.Fatal("ImagesOmitted not reported")
	}
}

func TestRenderOpenAIMessagesDefersUserRoleEntriesUntilToolResultsAreIn(t *testing.T) {
	entries := []runtime.SessionEntryView{
		userView("u", 1, "go"),
		assistantView("a", 2, renderModel, "", "", toolCall("c1", "lookup", `{}`), toolCall("c2", "lookup", `{}`)),
		toolView("t1", 3, "c1", runtime.ToolEntryCompleted, textPart("r1")),
		noteView("n", 4, "approval granted"),
		userView("steer", 5, "also check B"),
		toolView("t2", 6, "c2", runtime.ToolEntryCompleted, textPart("r2")),
	}
	messages, _, err := renderOpenAIMessages("S", entries, renderModel)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range messages {
		roles = append(roles, m.Role)
	}
	if want := []string{"system", "user", "assistant", "tool", "tool", "user", "user"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles %v, want %v", roles, want)
	}
	if *messages[5].Content != "Note from OpenSeal: approval granted" || *messages[6].Content != "also check B" {
		t.Fatalf("deferred order wrong: %q, %q", *messages[5].Content, *messages[6].Content)
	}
}

func TestRenderOpenAIMessagesRejectsUnresolvedOrUnpairedToolCalls(t *testing.T) {
	call := toolCall("c1", "lookup", `{}`)
	cases := map[string][]runtime.SessionEntryView{
		"unresolved": {userView("u", 1, "go"), assistantView("a", 2, renderModel, "", "", call),
			toolView("t", 3, "c1", runtime.ToolEntryRunning)},
		"missing before next assistant": {userView("u", 1, "go"), assistantView("a", 2, renderModel, "", "", call),
			assistantView("b", 3, renderModel, "done", "")},
		"missing at end": {userView("u", 1, "go"), assistantView("a", 2, renderModel, "", "", call)},
		"orphan":         {userView("u", 1, "go"), toolView("t", 2, "c1", runtime.ToolEntryCompleted, textPart("r"))},
		"answered twice": {userView("u", 1, "go"), assistantView("a", 2, renderModel, "", "", call),
			toolView("t", 3, "c1", runtime.ToolEntryCompleted, textPart("r")), toolView("t2", 4, "c1", runtime.ToolEntryCompleted, textPart("r"))},
		"wrong id": {userView("u", 1, "go"), assistantView("a", 2, renderModel, "", "", call),
			toolView("t", 3, "c9", runtime.ToolEntryCompleted, textPart("r"))},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := renderOpenAIMessages("S", entries, renderModel)
			if code := renderCode(t, err); code != openAIRenderInvalidSession {
				t.Fatalf("code %q, want %q", code, openAIRenderInvalidSession)
			}
		})
	}
}

func TestRenderOpenAIMessagesRejectsMisplacedCompaction(t *testing.T) {
	entries := []runtime.SessionEntryView{userView("u", 1, "go"), compactionView("c", 2, "summary")}
	_, _, err := renderOpenAIMessages("S", entries, renderModel)
	if code := renderCode(t, err); code != openAIRenderInvalidSession {
		t.Fatalf("code %q", code)
	}
}

func TestRenderOpenAIToolsValidatesNamesAndSchemas(t *testing.T) {
	object := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}}}
	tools, err := renderOpenAITools([]runtime.ModelStepTool{
		{Name: "turn_final_answer", Description: "Answer.", InputSchema: object, Group: runtime.ModelStepToolHarness},
		{Name: "notes-search", InputSchema: map[string]interface{}{"type": "object"}, Group: runtime.ModelStepToolCore},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tools)
	want := `[{"type":"function","function":{"name":"turn_final_answer","description":"Answer.","parameters":{"properties":{"q":{"type":"string"}},"type":"object"}}},{"type":"function","function":{"name":"notes-search","parameters":{"type":"object"}}}]`
	if string(raw) != want {
		t.Fatalf("tools\n got %s\nwant %s", raw, want)
	}
	if tools, err := renderOpenAITools(nil); err != nil || tools != nil {
		t.Fatalf("empty tools = %v, %v", tools, err)
	}
	bad := map[string][]runtime.ModelStepTool{
		"bad name":   {{Name: "notes.search", InputSchema: object}},
		"long name":  {{Name: strings.Repeat("a", 65), InputSchema: object}},
		"duplicate":  {{Name: "a", InputSchema: object}, {Name: "a", InputSchema: object}},
		"not object": {{Name: "a", InputSchema: map[string]interface{}{"type": "string"}}},
		"no schema":  {{Name: "a"}},
	}
	for name, tools := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := renderOpenAITools(tools)
			if code := renderCode(t, err); code != openAIRenderInvalidTool {
				t.Fatalf("code %q", code)
			}
		})
	}
}

func TestRenderOpenAIToolChoice(t *testing.T) {
	tools := []runtime.ModelStepTool{{Name: "turn_final_answer"}, {Name: "lookup"}}
	for choice, want := range map[string]interface{}{"": "auto", "auto": "auto", "required": "required", "none": "none"} {
		got, err := renderOpenAIToolChoice(choice, tools)
		if err != nil || got != want {
			t.Fatalf("choice %q = %v, %v; want %v", choice, got, err, want)
		}
	}
	got, err := renderOpenAIToolChoice("lookup", tools)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `{"function":{"name":"lookup"},"type":"function"}` {
		t.Fatalf("forced choice = %s", raw)
	}
	_, err = renderOpenAIToolChoice("missing", tools)
	if code := renderCode(t, err); code != openAIRenderInvalidTool {
		t.Fatalf("code %q", code)
	}
}
