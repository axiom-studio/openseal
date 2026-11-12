package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// jsonbStyle is value as Postgres jsonb gives it back: one space after every
// ':' and ','. The compact form is what was written and checked.
func jsonbStyle(t *testing.T, entries int, valueBytes int) (compact, spaced json.RawMessage) {
	t.Helper()
	var c, s strings.Builder
	c.WriteString("{")
	s.WriteString("{")
	for i := 0; i < entries; i++ {
		if i > 0 {
			c.WriteString(",")
			s.WriteString(", ")
		}
		value := strings.Repeat("x", valueBytes)
		fmt.Fprintf(&c, `"k%d":["%s",1]`, i, value)
		fmt.Fprintf(&s, `"k%d": ["%s", 1]`, i, value)
	}
	c.WriteString("}")
	s.WriteString("}")
	return json.RawMessage(c.String()), json.RawMessage(s.String())
}

// A result written just under the cap is read back from jsonb larger than the
// cap (prod run f7804eab: 63130 bytes written, 67604 read). It still fits, and
// the model sees it compact.
func TestAResultReadBackFromJSONBStillFitsItsCap(t *testing.T) {
	compact, spaced := jsonbStyle(t, 1450, 30)
	if len(compact) > MaxToolResultContentBytes || len(spaced) <= MaxToolResultContentBytes {
		t.Fatalf("fixture: compact %d, spaced %d, cap %d", len(compact), len(spaced), MaxToolResultContentBytes)
	}
	result := &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartJSON, Value: spaced}}}
	entry := &SessionEntry{ID: "read-back", Type: SessionEntryTool, State: ToolEntryCompleted, StepID: "step-a", ToolCallID: "c1",
		Tool: &ToolEntry{Name: "lookup", SideEffect: ToolSideEffectRead, Result: result}}
	if err := entry.Validate(); err != nil {
		t.Fatalf("a result that fit when written is refused when read: %v", err)
	}

	f := NewRenderFixture(t, "lane")
	f.User("run-a", "look it up")
	f.Assistant("run-a", "step-a", "c1")
	tool := f.Tool("run-a", "step-a", "c1", ToolEntryCompleted, ContentPart{Type: ContentPartJSON, Value: spaced})
	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), NewRenderImageSources().Options(false))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	content := ViewByID(t, views, tool.ID).Tool.Result.Content
	if len(content) != 1 || string(content[0].Value) != string(compact) {
		t.Fatalf("the model is not shown the compact value (%d parts, %d bytes)", len(content), len(content[0].Value))
	}
	RequireRenderedStep(t, views)
}

// A stored result over the cap is shortened for the model instead of failing
// the step.
func TestAStoredResultOverTheCapIsShortenedNotFatal(t *testing.T) {
	compact, _ := jsonbStyle(t, 3000, 30)
	f := NewRenderFixture(t, "lane")
	f.User("run-a", "look it up")
	f.Assistant("run-a", "step-a", "c1")
	tool := f.Tool("run-a", "step-a", "c1", ToolEntryCompleted,
		ContentPart{Type: ContentPartText, Text: strings.Repeat("page text ", 4000)},
		ContentPart{Type: ContentPartJSON, Value: compact})
	views, err := modelstep.ProjectSessionPath(context.Background(), f.Path(), NewRenderImageSources().Options(false))
	if err != nil {
		t.Fatalf("projectSessionPath: %v", err)
	}
	content := ViewByID(t, views, tool.ID).Tool.Result.Content
	total := 0
	for _, part := range content {
		total += len(part.Text) + jsonPartBytes(part.Value)
	}
	if total > MaxToolResultContentBytes {
		t.Fatalf("the shown result is %d bytes (cap %d)", total, MaxToolResultContentBytes)
	}
	if !strings.Contains(fmt.Sprint(content), "truncated") && !strings.Contains(fmt.Sprint(content), "too large to show in full") {
		t.Fatalf("the shortened result does not say it was shortened: %.300v", content)
	}
	RequireRenderedStep(t, views)
}

// A model step refused for its content is not reported as a misconfigured host.
func TestAnInvalidStepRequestIsNotCalledAHostProblem(t *testing.T) {
	err := fmt.Errorf("%w: %w", modelstep.ErrTurnHostConfiguration, fmt.Errorf("%w: tool entry e1 result text and json are 70000 bytes (max 65536)", ErrInvalidModelStepRequest))
	if !errors.Is(err, modelstep.ErrTurnHostConfiguration) || !errors.Is(err, ErrInvalidModelStepRequest) {
		t.Fatal("the wrapped error lost a cause")
	}
}
