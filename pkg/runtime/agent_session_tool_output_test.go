package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/toolfile"
)

type fakeToolOutputKeeper struct {
	calls    int
	payloads []toolfile.Payload
	paths    []string
	err      error
	tooLarge bool
}

func (k *fakeToolOutputKeeper) KeepToolOutput(_ context.Context, invocation ToolInvocation, files []toolfile.Payload) ([]string, error) {
	k.calls++
	if k.err != nil {
		return nil, k.err
	}
	k.payloads = append(k.payloads, files...)
	used := map[string]bool{}
	paths := make([]string, len(files))
	for i, file := range files {
		if k.tooLarge {
			continue
		}
		paths[i] = toolfile.Path(invocation.ActionCallID, file, used)
	}
	k.paths = append(k.paths, paths...)
	return paths, nil
}

func requireToolResultFitsCap(t *testing.T, result ToolResult) {
	t.Helper()
	if _, err := validateContentParts(result.Content, contentPartLimits{
		Field: "tool result", MaxParts: MaxToolResultParts, MaxContent: MaxToolResultContentBytes,
	}); err != nil {
		t.Fatalf("bounded result does not fit the cap: %v", err)
	}
}

func bigJSONValue(t *testing.T) json.RawMessage {
	t.Helper()
	items := make([]map[string]interface{}, 0, 400)
	for i := 0; i < 400; i++ {
		items = append(items, map[string]interface{}{"id": i, "body": strings.Repeat("é", 200)})
	}
	encoded, err := json.Marshal(map[string]interface{}{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= MaxToolResultContentBytes {
		t.Fatalf("fixture too small: %d", len(encoded))
	}
	return encoded
}

func TestBoundToolResultUnderCapUnchanged(t *testing.T) {
	keeper := &fakeToolOutputKeeper{}
	result := ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{
		{Type: ContentPartText, Text: "hello"},
		{Type: ContentPartJSON, Value: json.RawMessage(`{"a":1}`)},
		{Type: ContentPartFile, Path: "tool-output/c/x.png", MimeType: "image/png", SizeBytes: 9 << 20},
	}}
	got, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "call-1"}, result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, result) {
		t.Fatalf("result changed: %#v", got)
	}
	if keeper.calls != 0 {
		t.Fatalf("keeper called %d times", keeper.calls)
	}
}

func TestBoundToolResultKeepsFullOutputAsFile(t *testing.T) {
	keeper := &fakeToolOutputKeeper{}
	value := bigJSONValue(t)
	result := ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{
		{Type: ContentPartText, Text: "found 400 items"},
		{Type: ContentPartJSON, Value: value},
	}}
	got, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "call-1"}, result)
	if err != nil {
		t.Fatal(err)
	}
	if len(keeper.payloads) != 1 {
		t.Fatalf("keeper got %d payloads", len(keeper.payloads))
	}
	payload := keeper.payloads[0]
	if keeper.paths[0] != "tool-output/call-1/output.json" || payload.MimeType != "application/json" {
		t.Fatalf("payload kept at %q as %q", keeper.paths[0], payload.MimeType)
	}
	var parts []ContentPart
	if err := json.Unmarshal(payload.Data, &parts); err != nil || len(parts) != 2 || string(parts[1].Value) != string(value) {
		t.Fatalf("payload does not hold the full content: %v", err)
	}
	if got.Status != ToolResultSucceeded || len(got.Content) != 2 {
		t.Fatalf("unexpected result: %#v", got)
	}
	preview, file := got.Content[0], got.Content[1]
	if preview.Type != ContentPartText || !strings.HasPrefix(preview.Text, "found 400 items\n") ||
		!strings.Contains(preview.Text, "the full output is in tool-output/call-1/output.json") {
		t.Fatalf("unexpected preview: %.200q", preview.Text)
	}
	if !utf8Valid(preview.Text) {
		t.Fatal("preview splits a rune")
	}
	if file.Type != ContentPartFile || file.Path != "tool-output/call-1/output.json" || file.MimeType != "application/json" || file.SizeBytes != int64(len(payload.Data)) {
		t.Fatalf("unexpected file part: %#v", file)
	}
	requireToolResultFitsCap(t, got)
}

func TestBoundToolResultTextUsesTxt(t *testing.T) {
	keeper := &fakeToolOutputKeeper{}
	text := strings.Repeat("line of log output\n", 5000)
	got, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "entry-9"}, ToolResult{
		Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if keeper.paths[0] != "tool-output/entry-9/output.txt" || keeper.payloads[0].MimeType != "text/plain" || string(keeper.payloads[0].Data) != text {
		t.Fatalf("unexpected payload at %q", keeper.paths[0])
	}
	if !strings.HasPrefix(got.Content[0].Text, text[:toolOutputPreviewBytes]) || got.Content[1].Path != "tool-output/entry-9/output.txt" {
		t.Fatalf("unexpected content: %#v", got.Content[1])
	}
	requireToolResultFitsCap(t, got)
}

func TestBoundToolResultNoChatCompactsToCap(t *testing.T) {
	value := bigJSONValue(t)
	text := strings.Repeat("ü", 50<<10)
	result := ToolResult{Status: ToolResultFailed, IsError: true, Content: []ContentPart{
		{Type: ContentPartText, Text: text},
		{Type: ContentPartJSON, Value: value},
	}}
	for name, keeper := range map[string]ToolOutputKeeper{
		"nil keeper": nil,
		"no chat":    &fakeToolOutputKeeper{err: ErrToolOutputNoChat},
		"too large":  &fakeToolOutputKeeper{tooLarge: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "call-1"}, result)
			if err != nil {
				t.Fatal(err)
			}
			requireToolResultFitsCap(t, got)
			if got.Status != ToolResultFailed || !got.IsError || len(got.Content) != 2 {
				t.Fatalf("unexpected result: %#v", got)
			}
			if !strings.Contains(got.Content[0].Text, "(truncated: 102400 bytes in total;") || !utf8Valid(got.Content[0].Text) {
				t.Fatalf("text not marked truncated: %q", got.Content[0].Text[len(got.Content[0].Text)-120:])
			}
			var wrapped struct {
				Truncated         bool            `json:"truncated"`
				OriginalSizeBytes int             `json:"originalSizeBytes"`
				Value             json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(got.Content[1].Value, &wrapped); err != nil || !wrapped.Truncated || wrapped.OriginalSizeBytes != len(value) || len(wrapped.Value) == 0 {
				t.Fatalf("json not wrapped as truncated: %v %s", err, got.Content[1].Value[:80])
			}
		})
	}
	unshortenable := ToolResult{Status: ToolResultSucceeded, Content: make([]ContentPart, 0, MaxToolResultParts)}
	// Keys are never shortened, so long keys cannot be compacted to fit.
	longKeys := map[string]int{}
	for i := 0; i < 4; i++ {
		longKeys[strings.Repeat(string(rune('a'+i)), 20<<10)] = i
	}
	longKeyValue, _ := json.Marshal(longKeys)
	for i := 0; i < 2; i++ {
		unshortenable.Content = append(unshortenable.Content, ContentPart{Type: ContentPartJSON, Value: longKeyValue})
	}
	got, err := BoundToolResult(context.Background(), nil, ToolInvocation{}, unshortenable)
	if err != nil {
		t.Fatal(err)
	}
	requireToolResultFitsCap(t, got)
	if len(got.Content) != 1 || !strings.Contains(got.Content[0].Text, "could not be shortened") {
		t.Fatalf("expected a could-not-shorten note, got %d parts", len(got.Content))
	}
}

func TestBoundToolResultKeepsExistingFileBlocks(t *testing.T) {
	existing := []ContentPart{
		{Type: ContentPartFile, Path: "tool-output/call-1/report.pdf", MimeType: "application/pdf", SizeBytes: 12345},
		{Type: ContentPartImage, MediaType: "image/png", ActionCallID: "call-1"},
	}
	content := append([]ContentPart{{Type: ContentPartText, Text: strings.Repeat("x", 70<<10)}}, existing...)
	for name, keeper := range map[string]ToolOutputKeeper{"kept": &fakeToolOutputKeeper{}, "no chat": nil} {
		t.Run(name, func(t *testing.T) {
			got, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "call-1"}, ToolResult{Status: ToolResultSucceeded, Content: content})
			if err != nil {
				t.Fatal(err)
			}
			tail := got.Content[len(got.Content)-2:]
			if !reflect.DeepEqual(tail, existing) {
				t.Fatalf("existing parts changed: %#v", tail)
			}
			requireToolResultFitsCap(t, got)
		})
	}
	keeper := &fakeToolOutputKeeper{}
	if _, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "call-1"}, ToolResult{Status: ToolResultSucceeded, Content: content}); err != nil {
		t.Fatal(err)
	}
	if keeper.payloads[0].Name != "output.txt" || len(keeper.payloads[0].Data) != 70<<10 {
		t.Fatalf("payload should hold only the text: %q %d", keeper.payloads[0].Name, len(keeper.payloads[0].Data))
	}
}

func TestBoundToolResultKeeperErrorFails(t *testing.T) {
	boom := errors.New("chat files unavailable")
	keeper := &fakeToolOutputKeeper{err: boom}
	_, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "call-1"}, ToolResult{
		Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: strings.Repeat("x", 70<<10)}},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected keeper error, got %v", err)
	}
	if _, err := BoundToolResult(context.Background(), &fakeToolOutputKeeper{}, ToolInvocation{}, ToolResult{
		Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: strings.Repeat("x", 70<<10)}},
	}); err == nil {
		t.Fatal("expected an error without an action call id")
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// A result over the inline size but within the cap (a 30 KiB page snapshot)
// is kept as a chat file with a short preview, so later steps do not resend it.
func TestBoundToolResultKeepsAMidSizeResultAsAFile(t *testing.T) {
	keeper := &fakeToolOutputKeeper{}
	text := strings.Repeat("page text ", 3<<10)
	got, err := BoundToolResult(context.Background(), keeper, ToolInvocation{ActionCallID: "call-7"}, ToolResult{
		Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) <= toolOutputInlineBytes || len(text) > MaxToolResultContentBytes {
		t.Fatalf("fixture: %d bytes", len(text))
	}
	if keeper.calls != 1 || string(keeper.payloads[0].Data) != text || len(got.Content) != 2 || got.Content[1].Path != "tool-output/call-7/output.txt" {
		t.Fatalf("not kept as a file: calls %d, content %d", keeper.calls, len(got.Content))
	}
	if len(got.Content[0].Text) > toolOutputPreviewBytes+200 || !strings.Contains(got.Content[0].Text, "the full output is in tool-output/call-7/output.txt") {
		t.Fatalf("preview is %d bytes", len(got.Content[0].Text))
	}
}

// When a mid-size result cannot be kept as a file it stays whole, as before:
// the lower inline size never shortens or fails a result that fits the cap.
func TestBoundToolResultLeavesAMidSizeResultWholeWithoutAFile(t *testing.T) {
	result := ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: strings.Repeat("page text ", 3<<10)}}}
	for name, c := range map[string]struct {
		keeper ToolOutputKeeper
		call   string
	}{
		"nil keeper":        {nil, "call-1"},
		"no chat":           {&fakeToolOutputKeeper{err: ErrToolOutputNoChat}, "call-1"},
		"keeper fails":      {&fakeToolOutputKeeper{err: errors.New("chat files unavailable")}, "call-1"},
		"file too large":    {&fakeToolOutputKeeper{tooLarge: true}, "call-1"},
		"no action call id": {&fakeToolOutputKeeper{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BoundToolResult(context.Background(), c.keeper, ToolInvocation{ActionCallID: c.call}, result)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, result) {
				t.Fatal("the result changed")
			}
		})
	}
}
