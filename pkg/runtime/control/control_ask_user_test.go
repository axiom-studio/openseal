package control

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TestAskUserWithoutConversationIsRefused(t *testing.T) {
	f := newControlFixture(t, []agentrun.LoopToolCall{{ID: "ask", Name: string(models.ControlToolAskUser), Arguments: json.RawMessage(`{"question":"Which one?"}`)}})
	f.exec.handlers = nil // the real registry
	f.exec.Reporting = f.store
	result, err := f.exec.Settle(f.ctx, f.context("ask", agentrun.ControlAuthority{CanAskUser: true}))
	if err != nil || result.Kind != ControlContinue || result.Entry.State != models.ToolEntryError {
		t.Fatalf("settle = %#v %v", result, err)
	}
	requireControlError(t, result.Entry.Tool.Result, models.ToolResultRefused, "cannot_post")
}

func TestAskUserArgumentsAreValidated(t *testing.T) {
	for _, raw := range []string{`{"question":"   "}`, `{"question":"x","extra":1}`, `{"question":"` + strings.Repeat("a", 4097) + `"}`} {
		if _, err := (askUserHandler{}).Decode(json.RawMessage(raw)); err == nil {
			t.Fatalf("decode accepted %s", raw[:min(len(raw), 40)])
		}
	}
	value, err := (askUserHandler{}).Decode(json.RawMessage(`{"question":"  Which one?  "}`))
	if err != nil || value.(*models.AskUserArgs).Question != "Which one?" {
		t.Fatalf("decode = %#v %v", value, err)
	}
}
