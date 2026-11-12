package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// progressFixture is a control fixture running the real turn_report_progress
// handler with an activity service.
func progressFixture(t *testing.T, calls ...agentrun.LoopToolCall) *controlFixture {
	t.Helper()
	f := newControlFixture(t, calls)
	registry := Registry{}
	registry.register(models.ControlToolReportProgress, reportProgressHandler{})
	f.exec.handlers = registry
	f.exec.Activity = activity.NewRunActivityService(f.store, f.store)
	return f
}

func progressSpec(t *testing.T, authority agentrun.ControlAuthority) ControlToolSpec {
	t.Helper()
	for _, spec := range builtinControlSpecs(authority) {
		if spec.Name == models.ControlToolReportProgress {
			return spec
		}
	}
	t.Fatalf("turn_report_progress not offered under %#v", authority)
	return ControlToolSpec{}
}

func progressCall(id, args string) agentrun.LoopToolCall {
	return agentrun.LoopToolCall{ID: id, Name: string(models.ControlToolReportProgress), Arguments: json.RawMessage(args)}
}

func (f *controlFixture) progressEvents() []*models.ActivityEvent {
	f.t.Helper()
	events, err := f.store.ListActivity(f.ctx, models.ActivityFilter{Scope: f.run.Scope, RunID: f.run.ID, Limit: 500})
	if err != nil {
		f.t.Fatalf("list activity: %v", err)
	}
	var out []*models.ActivityEvent
	for _, event := range events {
		if event.EventType == "turn.commentary" || event.EventType == "turn.progress" {
			out = append(out, event)
		}
	}
	return out
}

func requirePosted(t *testing.T, result ControlResult) {
	t.Helper()
	if result.Kind != ControlContinue || result.Entry == nil || result.Entry.State != models.ToolEntryCompleted {
		t.Fatalf("result = %#v", result)
	}
	tr := result.Entry.Tool.Result
	if tr == nil || tr.IsError || tr.Status != models.ToolResultSucceeded || len(tr.Content) != 1 || string(tr.Content[0].Value) != `{"status":"posted"}` {
		t.Fatalf("tool result = %#v", tr)
	}
}

func TestReportProgressEmitsCommentary(t *testing.T) {
	f := progressFixture(t,
		progressCall("default", `{"summary":"  Checking the Q3 invoices now.  "}`),
		progressCall("trail", `{"summary":"Found 3 overdue invoices.","destination":"activity"}`),
		progressCall("bad", `{"summary":"x","destination":"slack"}`),
		progressCall("long", `{"summary":"`+strings.Repeat("é", 601)+`"}`),
	)
	authority := agentrun.ControlAuthority{CanReportProgress: true}
	spec := progressSpec(t, authority)
	if spec.Phase != ControlPhaseInline {
		t.Fatalf("phase = %s", spec.Phase)
	}
	props := spec.InputSchema["properties"].(map[string]interface{})
	if got := props["destination"].(map[string]interface{})["enum"]; !reflect.DeepEqual(got, []string{"commentary", "activity"}) {
		t.Fatalf("destination enum = %v", got)
	}
	result, err := f.exec.Inline(f.ctx, f.context("default", authority))
	if err != nil {
		t.Fatalf("inline: %v", err)
	}
	requirePosted(t, result)
	result, err = f.exec.Inline(f.ctx, f.context("trail", authority))
	if err != nil {
		t.Fatalf("inline: %v", err)
	}
	requirePosted(t, result)
	events := f.progressEvents()
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	first, second := events[0], events[1]
	if first.EventType != "turn.commentary" || first.Summary != "Checking the Q3 invoices now." ||
		first.TurnID != f.step.ID || first.AgentID != "agent-1" || first.Visibility != models.ActivityVisibilityScope ||
		first.Actor.ID != controlTestWorker || first.Payload["entryId"] != f.entries["default"].ID {
		t.Fatalf("commentary event = %#v", first)
	}
	if second.EventType != "turn.progress" || second.Summary != "Found 3 overdue invoices." {
		t.Fatalf("activity event = %#v", second)
	}
	for _, id := range []string{"bad", "long"} {
		result, err := f.exec.Inline(f.ctx, f.context(id, authority))
		if err != nil {
			t.Fatalf("inline %s: %v", id, err)
		}
		if result.Entry.State != models.ToolEntryError {
			t.Fatalf("%s result = %#v", id, result)
		}
		requireControlError(t, f.stored(id).Tool.Result, models.ToolResultFailed, "tool_arguments_invalid")
	}
	if len(f.progressEvents()) != 2 {
		t.Fatal("an invalid call published an event")
	}
	// Not offered without the authority.
	other := progressFixture(t, progressCall("p", `{"summary":"hi"}`))
	result, err = other.exec.Inline(other.ctx, other.context("p", agentrun.ControlAuthority{}))
	if err != nil || result.Entry.State != models.ToolEntryError || len(other.progressEvents()) != 0 {
		t.Fatalf("unoffered = %#v, %v", result, err)
	}
}

func TestReportProgressTaskWorkActivityOnly(t *testing.T) {
	f := progressFixture(t,
		progressCall("commentary", `{"summary":"Drafting the summary.","destination":"commentary"}`),
		progressCall("default", `{"summary":"Pulled the account history."}`),
	)
	authority := agentrun.ControlAuthority{CanReportProgress: true, TaskWork: true}
	spec := progressSpec(t, authority)
	props, _ := spec.InputSchema["properties"].(map[string]interface{})
	if props == nil || !reflect.DeepEqual(props["destination"].(map[string]interface{})["enum"], []string{"activity"}) ||
		!strings.Contains(spec.Description, "activity-trail") {
		t.Fatalf("task-work spec = %#v", spec)
	}
	for _, id := range []string{"commentary", "default"} {
		result, err := f.exec.Inline(f.ctx, f.context(id, authority))
		if err != nil {
			t.Fatalf("inline %s: %v", id, err)
		}
		requirePosted(t, result)
	}
	events := f.progressEvents()
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	for _, event := range events {
		if event.EventType != "turn.progress" {
			t.Fatalf("task work published %s", event.EventType)
		}
	}
}

func TestReportProgressRejectsRepeat(t *testing.T) {
	f := progressFixture(t,
		progressCall("first", `{"summary":"Found the duplicate charge."}`),
		progressCall("again", `{"summary":" Found the duplicate charge. ","destination":"activity"}`),
		progressCall("new", `{"summary":"Refund is pending approval."}`),
	)
	authority := agentrun.ControlAuthority{CanReportProgress: true}
	result, err := f.exec.Inline(f.ctx, f.context("first", authority))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	requirePosted(t, result)
	result, err = f.exec.Inline(f.ctx, f.context("again", authority))
	if err != nil {
		t.Fatalf("again: %v", err)
	}
	if result.Kind != ControlContinue || result.Entry.State != models.ToolEntryError {
		t.Fatalf("repeat result = %#v", result)
	}
	value := requireControlError(t, f.stored("again").Tool.Result, models.ToolResultRefused, reportProgressRepeatCode)
	if !strings.HasPrefix(value["message"].(string), "Already reported") {
		t.Fatalf("message = %v", value["message"])
	}
	result, err = f.exec.Inline(f.ctx, f.context("new", authority))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	requirePosted(t, result)
	if events := f.progressEvents(); len(events) != 2 || events[1].Summary != "Refund is pending approval." {
		t.Fatalf("events = %#v", events)
	}
}
