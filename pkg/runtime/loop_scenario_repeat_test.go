package runtime

// Run-loop scenarios for repeats and the doom loop (run-loop section 12 §5.4,
// section 08 §5 / DI-8, D9). The model, the action dispatcher and the host
// tools are the kit's fakes; the loop, the tool dispatcher and the repeat
// guard are production code. Counting comes from the session log only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// repeatLookupArgs is the lookup every doom-loop scenario repeats.
var repeatLookupArgs = map[string]any{"id": "6815"}

// stepPurpose matches ordinary model steps (not the explanation step).
func stepPurpose(v looptest.StepView) bool { return v.Purpose() == models.ModelStepPurposeStep }

// rawCall is a tool call whose argument JSON is sent byte for byte, so a
// scenario can vary whitespace and key order.
func rawCall(narration, name, arguments string) models.ModelStepResult {
	return models.ModelStepResult{
		Narration: narration,
		Assistant: models.AssistantEntry{
			Text: narration, StopReason: models.ModelStopToolCalls,
			ToolCalls: []models.ModelToolCall{{Name: name, Arguments: json.RawMessage(arguments)}},
		},
	}
}

// repeatToolEntries is the Run's tool entries whose tool name ends in action,
// in log order.
func repeatToolEntries(f *looptest.Fixture, runID, action string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		if entry.Type == models.SessionEntryTool && entry.Tool != nil && strings.HasSuffix(entry.Tool.Name, action) && entry.RunID == runID {
			out = append(out, entry)
		}
	}
	return out
}

// repeatNotes is the Run's D9 notes, in log order.
func repeatNotes(f *looptest.Fixture, runID string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		if entry.Type == models.SessionEntryNote && entry.Note != nil && entry.Note.Kind == models.NoteRepeat && entry.RunID == runID {
			out = append(out, entry)
		}
	}
	return out
}

// repeatNoteAfter is the D9 note written after tool entry id, if any.
func repeatNoteAfter(f *looptest.Fixture, runID, entryID string) *models.SessionEntry {
	f.T.Helper()
	for _, note := range repeatNotes(f, runID) {
		if note.SourceKey == runloop.RepeatNoteSourceKey(entryID) {
			return note
		}
	}
	return nil
}

func repeatResultText(entry *models.SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil {
		return ""
	}
	return looptest.PartsText(entry.Tool.Result.Content)
}

func repeatGuard(entry *models.SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Guard == nil {
		return ""
	}
	return entry.Tool.Guard.Kind
}

// repeatCallsByStatus is the Run's action_calls rows of action with status.
func repeatCallsByStatus(f *looptest.Fixture, runID, action string, status models.ActionCallStatus) []*models.ActionCall {
	f.T.Helper()
	var out []*models.ActionCall
	for _, call := range f.Calls(runID) {
		if call.Action == action && call.Status == status {
			out = append(out, call)
		}
	}
	return out
}

// The repeat guard's model-facing texts, spelled out so the scenarios pin
// what the model reads. times is "twice" or "<n> times".
func wantRepeatNote(priorStep int64) string {
	return fmt.Sprintf("You already made this exact call at step %d and got the same result. Use that result instead of calling again.", priorStep)
}

func wantRepeatRefused(times string, priorStep int64) string {
	return fmt.Sprintf("Not run: this exact call was already made %s in this run with the same result (last at step %d). Use the earlier result, try a different step, or say what is blocking you.", times, priorStep)
}

func wantRepeatLimit(times string) string {
	return fmt.Sprintf("Not run: this exact call has now been repeated %s without progress, so this run is stopping.", times)
}

func wantRepeatSuppressed(priorStep int64) string {
	return fmt.Sprintf("Not run: this exact call already succeeded at step %d, so it was not repeated. If it really has to happen again, tell the user; a new request can do it. Its result was:", priorStep)
}

func describeRepeatEntries(entries []*models.SessionEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = fmt.Sprintf("{step:%d state:%s guard:%s result:%q}", e.Step, e.State, repeatGuard(e), repeatResultText(e))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestLoopIdenticalReadNoteThenRefusalThenFailure: the 2nd identical read
// with the same result gets a note naming step 1, the 3rd and 4th are
// refused with refused action_calls rows, the 5th fails the Run with
// repeat_limit, and the chat gets the explanation.
func TestLoopIdenticalReadNoteThenRefusalThenFailure(t *testing.T) {
	looptest.EachStore(t, "repeat_doom", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking.", looptest.Call(looptest.GovernedTool(v, "lookup"), repeatLookupArgs))
			}},
			&looptest.ModelRule{Name: "explain", When: func(v looptest.StepView) bool { return v.Purpose() == models.ModelStepPurposeExplanation },
				Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Say("I kept looking up issue 6815 and got the same answer, so I stopped.")
				}},
		))
		conversation, run := f.StartChat("what is the status of 6815?")
		done := f.DriveToRest(run.ID, 4)
		f.MustStatus(done, models.AgentRunStatusFailed)

		if got := f.Dispatch.Count("lookup"); got != 2 {
			t.Fatalf("lookup dispatched %d times, want 2", got)
		}
		lookups := repeatToolEntries(f, run.ID, "lookup")
		if len(lookups) != 5 {
			t.Fatalf("lookup tool entries = %d, want 5: %s", len(lookups), describeRepeatEntries(lookups))
		}
		for i, entry := range lookups[:2] {
			if entry.State != models.ToolEntryCompleted || repeatGuard(entry) != "" {
				t.Fatalf("call %d = %s, want completed and unguarded", i+1, describeRepeatEntries(lookups))
			}
		}
		if note := repeatNoteAfter(f, run.ID, lookups[0].ID); note != nil {
			t.Fatalf("the first call got a note: %q", note.Note.Text)
		}
		note := repeatNoteAfter(f, run.ID, lookups[1].ID)
		if note == nil {
			t.Fatalf("the second identical call got no note; log %v", f.EntryTypes(run.ID))
		}
		if want := wantRepeatNote(lookups[0].Step); !strings.HasPrefix(note.Note.Text, want) {
			t.Fatalf("note = %q, want it to start with %q", note.Note.Text, want)
		}
		if !strings.Contains(note.Note.Text, fmt.Sprintf("step %d", lookups[0].Step)) {
			t.Fatalf("note %q does not name step %d", note.Note.Text, lookups[0].Step)
		}
		if note.Sequence <= lookups[1].Sequence || note.Sequence >= lookups[2].Sequence {
			t.Fatalf("note at %d is not between the 2nd (%d) and 3rd (%d) calls", note.Sequence, lookups[1].Sequence, lookups[2].Sequence)
		}
		if len(repeatNotes(f, run.ID)) != 1 {
			t.Fatalf("repeat notes = %d, want 1", len(repeatNotes(f, run.ID)))
		}
		for i, entry := range lookups[2:4] {
			count := i + 3
			times := []string{"twice", "3 times"}[i]
			if entry.State != models.ToolEntryError || repeatGuard(entry) != action.RefusedLoopCallRepeat ||
				entry.Tool.Result.Status != models.ToolResultRefused || !entry.Tool.Result.IsError {
				t.Fatalf("call %d = %s, want refused", count, describeRepeatEntries(lookups))
			}
			if want := wantRepeatRefused(times, lookups[1].Step); repeatResultText(entry) != want {
				t.Fatalf("call %d text = %q, want %q", count, repeatResultText(entry), want)
			}
		}
		if !strings.Contains(repeatResultText(lookups[2]), "already made twice") {
			t.Fatalf("3rd call text = %q, want it to say the call was already made twice", repeatResultText(lookups[2]))
		}
		fifth := lookups[4]
		if fifth.State != models.ToolEntryError || repeatGuard(fifth) != action.RefusedLoopCallRepeatLimit || repeatResultText(fifth) != wantRepeatLimit("5 times") {
			t.Fatalf("5th call = %s, want the repeat_limit refusal", describeRepeatEntries(lookups[4:]))
		}

		if got := repeatCallsByStatus(f, run.ID, "lookup", models.ActionCallStatusSucceeded); len(got) != 2 {
			t.Fatalf("succeeded lookup rows = %d, want 2", len(got))
		}
		refused := repeatCallsByStatus(f, run.ID, "lookup", models.ActionCallStatusRefused)
		codes := map[string]int{}
		for _, row := range refused {
			codes[row.ErrorCode]++
			if row.Origin != models.ActionOriginLoop || row.DuplicateOfActionCallID == "" {
				t.Fatalf("refused row = %#v, want loop origin and its prior call", row)
			}
		}
		if codes[action.RefusedLoopCallRepeat] != 2 {
			t.Fatalf("repeat_refused rows = %d, want 2 (codes %v)", codes[action.RefusedLoopCallRepeat], codes)
		}
		for i, entry := range lookups[2:] {
			if entry.ActionCallID == "" {
				t.Fatalf("refused call %d names no action_calls row", i+3)
			}
		}
		if len(f.Events(run.ID, action.ActivityEventActionRefused)) != len(refused) {
			t.Fatalf("action.refused events = %d, want %d", len(f.Events(run.ID, action.ActivityEventActionRefused)), len(refused))
		}

		final := f.MustRun(run.ID)
		if final.FailureExplanation == nil || final.FailureExplanation.Code != action.RunFailureCodeRepeatLimit ||
			final.FailureExplanation.Kind != modelstep.RunFailureKindRepeatLimit || !final.FailureExplanation.Explained {
			t.Fatalf("failure = %#v, want an explained repeat_limit", final.FailureExplanation)
		}
		const repeatLimitMessage = "The agent repeated the same call without making progress, so the run was stopped."
		if final.Error != repeatLimitMessage {
			t.Fatalf("run error = %q, want %q", final.Error, repeatLimitMessage)
		}
		messages := f.Messages(conversation.ID)
		last := messages[len(messages)-1]
		if last.Sender.ID != looptest.FixtureAgent || !strings.Contains(last.Content, "so I stopped") {
			t.Fatalf("channel = %s, want the explanation posted", looptest.DescribeMessages(messages))
		}
		if steps := len(f.Model.Calls(models.ModelStepPurposeStep)); steps != 5 {
			t.Fatalf("model steps = %d, want 5", steps)
		}
	})
}

// TestLoopIdenticalReadCountsResetAfterASuccessfulWrite: a successful write
// between identical reads starts the count again.
func TestLoopIdenticalReadCountsResetAfterASuccessfulWrite(t *testing.T) {
	looptest.EachStore(t, "repeat_reset", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", Times: 2, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking.", looptest.Call(looptest.GovernedTool(v, "lookup"), repeatLookupArgs))
			}},
			&looptest.ModelRule{Name: "update", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Reopening it.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "6815", "status": "open"}, "Reopen 6815"))
			}},
			&looptest.ModelRule{Name: "lookup again", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking again.", looptest.Call(looptest.GovernedTool(v, "lookup"), repeatLookupArgs))
			}},
			&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "6815 is open."})
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Reopen 6815 and confirm it")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		if got := f.Dispatch.Count("lookup"); got != 3 {
			t.Fatalf("lookup dispatched %d times, want 3", got)
		}
		if got := f.Dispatch.Count("update_status"); got != 1 {
			t.Fatalf("update_status dispatched %d times, want 1", got)
		}
		lookups := repeatToolEntries(f, run.ID, "lookup")
		if len(lookups) != 3 {
			t.Fatalf("lookup entries = %s", describeRepeatEntries(lookups))
		}
		for i, entry := range lookups {
			if entry.State != models.ToolEntryCompleted || repeatGuard(entry) != "" {
				t.Fatalf("lookup %d = %s, want completed", i+1, describeRepeatEntries(lookups))
			}
		}
		if repeatNoteAfter(f, run.ID, lookups[1].ID) == nil {
			t.Fatal("the second identical lookup got no note")
		}
		if note := repeatNoteAfter(f, run.ID, lookups[2].ID); note != nil {
			t.Fatalf("the lookup after the write got a note: %q", note.Note.Text)
		}
		if len(repeatNotes(f, run.ID)) != 1 {
			t.Fatalf("repeat notes = %d, want 1", len(repeatNotes(f, run.ID)))
		}
		if refused := repeatCallsByStatus(f, run.ID, "lookup", models.ActionCallStatusRefused); len(refused) != 0 {
			t.Fatalf("refused rows = %d, want 0", len(refused))
		}
	})
}

// TestLoopIdenticalReadWithChangedResultIsNotARepeat: an identical read whose
// result changed is progress, not a repeat.
func TestLoopIdenticalReadWithChangedResultIsNotARepeat(t *testing.T) {
	looptest.EachStore(t, "repeat_changed", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", Times: 4, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking.", looptest.Call(looptest.GovernedTool(v, "lookup"), repeatLookupArgs))
			}},
			&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "6815 keeps changing."})
			}},
		))
		var mu sync.Mutex
		statuses := []string{"open", "closed", "open", "closed"}
		f.Dispatch.On("lookup", func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			mu.Lock()
			defer mu.Unlock()
			status := statuses[0]
			statuses = statuses[1:]
			return map[string]interface{}{"id": "6815", "status": status}, nil
		})
		run := f.StartRun(models.RunKindAgentWork, "Watch 6815")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		if got := f.Dispatch.Count("lookup"); got != 4 {
			t.Fatalf("lookup dispatched %d times, want 4", got)
		}
		lookups := repeatToolEntries(f, run.ID, "lookup")
		for i, entry := range lookups {
			if entry.State != models.ToolEntryCompleted || repeatGuard(entry) != "" {
				t.Fatalf("lookup %d = %s, want completed", i+1, describeRepeatEntries(lookups))
			}
		}
		if notes := repeatNotes(f, run.ID); len(notes) != 0 {
			t.Fatalf("repeat notes = %d, want 0: %q", len(notes), notes[0].Note.Text)
		}
		if refused := repeatCallsByStatus(f, run.ID, "lookup", models.ActionCallStatusRefused); len(refused) != 0 {
			t.Fatalf("refused rows = %d, want 0", len(refused))
		}
	})
}

// TestLoopIdenticalReadsAreCanonicalisedByArguments: whitespace and key order
// do not make a call different; another value does.
func TestLoopIdenticalReadsAreCanonicalisedByArguments(t *testing.T) {
	looptest.EachStore(t, "repeat_canonical", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "compact", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return rawCall("Checking.", looptest.GovernedTool(v, "lookup"), `{"id":"6815"}`)
			}},
			&looptest.ModelRule{Name: "spaced", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return rawCall("Checking again.", looptest.GovernedTool(v, "lookup"), "{ \"id\" :\n \"6815\" }")
			}},
			&looptest.ModelRule{Name: "other", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return rawCall("Checking the next one.", looptest.GovernedTool(v, "lookup"), `{"id":"6816"}`)
			}},
			&looptest.ModelRule{Name: "close", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return rawCall("Closing it.", looptest.GovernedTool(v, "update_status"),
					`{"arguments":{"id":"6816","status":"closed"},"reviewContext":{"summary":"Close 6816","content":"Close 6816"}}`)
			}},
			&looptest.ModelRule{Name: "close reordered", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return rawCall("Closing it again.", looptest.GovernedTool(v, "update_status"),
					`{ "reviewContext" : {"content":"Close 6816","summary":"Close 6816"}, "arguments" : {"status":"closed","id":"6816"} }`)
			}},
			&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Done."})
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Check 6815 and close 6816")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		lookups := repeatToolEntries(f, run.ID, "lookup")
		if len(lookups) != 3 {
			t.Fatalf("lookup entries = %s", describeRepeatEntries(lookups))
		}
		if lookups[0].Tool.ArgumentsDigest == "" || lookups[0].Tool.ArgumentsDigest != lookups[1].Tool.ArgumentsDigest {
			t.Fatalf("respaced arguments have digests %q and %q, want equal", lookups[0].Tool.ArgumentsDigest, lookups[1].Tool.ArgumentsDigest)
		}
		if lookups[2].Tool.ArgumentsDigest == lookups[0].Tool.ArgumentsDigest {
			t.Fatal("id 6816 has the digest of id 6815")
		}
		if repeatNoteAfter(f, run.ID, lookups[1].ID) == nil {
			t.Fatal("the respaced identical lookup got no note")
		}
		if note := repeatNoteAfter(f, run.ID, lookups[2].ID); note != nil {
			t.Fatalf("the lookup of 6816 got a note: %q", note.Note.Text)
		}
		if got := f.Dispatch.Count("lookup"); got != 3 {
			t.Fatalf("lookup dispatched %d times, want 3", got)
		}

		updates := repeatToolEntries(f, run.ID, "update_status")
		if len(updates) != 2 {
			t.Fatalf("update_status entries = %s", describeRepeatEntries(updates))
		}
		if updates[0].Tool.ArgumentsDigest != updates[1].Tool.ArgumentsDigest {
			t.Fatal("reordered write arguments have different digests")
		}
		if updates[1].State != models.ToolEntryError || repeatGuard(updates[1]) != action.RefusedLoopCallDuplicateSuppressed {
			t.Fatalf("reordered write = %s, want duplicate_suppressed", describeRepeatEntries(updates))
		}
		if got := f.Dispatch.Count("update_status"); got != 1 {
			t.Fatalf("update_status dispatched %d times, want 1", got)
		}
	})
}

// TestLoopIdenticalWriteAfterSuccessIsSuppressed: a write identical to one
// that succeeded is not run again; the model gets step 1's result.
func TestLoopIdenticalWriteAfterSuccessIsSuppressed(t *testing.T) {
	looptest.EachStore(t, "repeat_write_ok", func(t *testing.T, store models.KernelStore) {
		sendArgs := map[string]any{"to": "ops", "text": "6815 is fixed"}
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "send", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Telling ops.", looptest.ReviewedCall(looptest.GovernedTool(v, "send"), sendArgs, "Tell ops 6815 is fixed"))
			}},
			&looptest.ModelRule{Name: "lookup", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking.", looptest.Call(looptest.GovernedTool(v, "lookup"), repeatLookupArgs))
			}},
			&looptest.ModelRule{Name: "send again", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Telling ops again.", looptest.ReviewedCall(looptest.GovernedTool(v, "send"), sendArgs, "Tell ops 6815 is fixed"))
			}},
			&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Ops know."})
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Tell ops 6815 is fixed")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		if got := f.Dispatch.Count("send"); got != 1 {
			t.Fatalf("send dispatched %d times, want 1", got)
		}
		sends := repeatToolEntries(f, run.ID, "send")
		if len(sends) != 2 || sends[0].State != models.ToolEntryCompleted {
			t.Fatalf("send entries = %s", describeRepeatEntries(sends))
		}
		second := sends[1]
		if second.State != models.ToolEntryError || repeatGuard(second) != action.RefusedLoopCallDuplicateSuppressed ||
			second.Tool.Result.Status != models.ToolResultDuplicateSuppressed || second.Tool.DuplicateOfEntryID != sends[0].ID {
			t.Fatalf("second send = %s (dup of %q), want duplicate_suppressed of the first", describeRepeatEntries(sends[1:]), second.Tool.DuplicateOfEntryID)
		}
		text := repeatResultText(second)
		if !strings.HasPrefix(text, wantRepeatSuppressed(sends[0].Step)) || !strings.Contains(text, fmt.Sprintf("already succeeded at step %d", sends[0].Step)) {
			t.Fatalf("suppressed text = %q", text)
		}
		if !strings.Contains(text, repeatResultText(sends[0])) || !strings.Contains(text, "sent") {
			t.Fatalf("suppressed text %q does not carry step %d's result %q", text, sends[0].Step, repeatResultText(sends[0]))
		}
		rows := repeatCallsByStatus(f, run.ID, "send", models.ActionCallStatusRefused)
		if len(rows) != 1 || rows[0].ErrorCode != action.RefusedLoopCallDuplicateSuppressed || rows[0].ID != second.ActionCallID {
			t.Fatalf("refused send rows = %#v, want one duplicate_suppressed row named by the entry", rows)
		}
		succeeded := repeatCallsByStatus(f, run.ID, "send", models.ActionCallStatusSucceeded)
		if len(succeeded) != 1 || rows[0].DuplicateOfActionCallID != succeeded[0].ID {
			t.Fatalf("suppressed row duplicates %q, want the succeeded send", rows[0].DuplicateOfActionCallID)
		}
		if refusedEvents := f.Events(run.ID, action.ActivityEventActionRefused); len(refusedEvents) != 1 || refusedEvents[0].Summary != "Already done; not repeated" {
			t.Fatalf("action.refused events = %d", len(refusedEvents))
		}
	})
}

// TestLoopIdenticalWriteAfterFailureMayRetry: a write that failed was not
// done, so the identical call runs again.
func TestLoopIdenticalWriteAfterFailureMayRetry(t *testing.T) {
	looptest.EachStore(t, "repeat_write_failed", func(t *testing.T, store models.KernelStore) {
		sendArgs := map[string]any{"to": "ops", "text": "6815 is fixed"}
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "send", Times: 2, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Telling ops.", looptest.ReviewedCall(looptest.GovernedTool(v, "send"), sendArgs, "Tell ops 6815 is fixed"))
			}},
			&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Ops know."})
			}},
		))
		var mu sync.Mutex
		attempts := 0
		f.Dispatch.On("send", func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			if attempts == 1 {
				return nil, errors.New("the mail relay is down")
			}
			return map[string]interface{}{"sent": true}, nil
		})
		run := f.StartRun(models.RunKindAgentWork, "Tell ops 6815 is fixed")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		if got := f.Dispatch.Count("send"); got != 2 {
			t.Fatalf("send dispatched %d times, want 2", got)
		}
		sends := repeatToolEntries(f, run.ID, "send")
		if len(sends) != 2 || sends[0].State != models.ToolEntryError || repeatGuard(sends[0]) != "" ||
			sends[1].State != models.ToolEntryCompleted || repeatGuard(sends[1]) != "" {
			t.Fatalf("send entries = %s, want a failure then a success", describeRepeatEntries(sends))
		}
		if sends[0].Tool.ArgumentsDigest != sends[1].Tool.ArgumentsDigest {
			t.Fatal("the retried send is not identical to the failed one")
		}
		if rows := repeatCallsByStatus(f, run.ID, "send", models.ActionCallStatusRefused); len(rows) != 0 {
			t.Fatalf("refused send rows = %d, want 0", len(rows))
		}
	})
}

// TestLoopRepeatsAreCountedAcrossClaimsAndParks: the count comes from the
// session log, so a yield between claims does not reset it.
func TestLoopRepeatsAreCountedAcrossClaimsAndParks(t *testing.T) {
	looptest.EachStore(t, "repeat_claims", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithMaxSteps(2), looptest.WithRules(
			&looptest.ModelRule{Name: "done", When: func(v looptest.StepView) bool {
				_, isError, ok := v.ToolResult(looptest.GovernedTool(v, "lookup"))
				return ok && isError
			}, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "6815 is open."})
			}},
			&looptest.ModelRule{Name: "lookup", Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking.", looptest.Call(looptest.GovernedTool(v, "lookup"), repeatLookupArgs))
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Check 6815")
		exit, after := f.RunClaim(run.ID)
		if exit.Reason != runloop.ExitYielded || after.Status != models.AgentRunStatusQueued {
			t.Fatalf("first claim exited %s with %s, want a yield", exit.Reason, after.Status)
		}
		if got := len(repeatToolEntries(f, run.ID, "lookup")); got != 2 {
			t.Fatalf("lookups in the first claim = %d, want 2", got)
		}
		// A new worker process: nothing survives in memory.
		f.WorkerID = looptest.FixtureWorker2
		f.Build()
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)

		if got := f.Dispatch.Count("lookup"); got != 2 {
			t.Fatalf("lookup dispatched %d times, want 2", got)
		}
		lookups := repeatToolEntries(f, run.ID, "lookup")
		if len(lookups) != 3 || repeatGuard(lookups[2]) != action.RefusedLoopCallRepeat || lookups[2].State != models.ToolEntryError {
			t.Fatalf("lookups = %s, want the 3rd refused in the second claim", describeRepeatEntries(lookups))
		}
		if lookups[2].StepID == lookups[1].StepID {
			t.Fatal("the refused lookup ran in the same step as the second")
		}
		if rows := repeatCallsByStatus(f, run.ID, "lookup", models.ActionCallStatusRefused); len(rows) != 1 || rows[0].ErrorCode != action.RefusedLoopCallRepeat {
			t.Fatalf("refused rows = %#v", rows)
		}
	})
}

// TestLoopVMNestedCallsAreNotCountedAsRepeats: identical calls made through
// the VM bridge inside one run_command are not loop tool calls.
func TestLoopVMNestedCallsAreNotCountedAsRepeats(t *testing.T) {
	looptest.EachStore(t, "repeat_vm", func(t *testing.T, store models.KernelStore) {
		var f *looptest.Fixture
		var nestedErr error
		command := modelstep.HostToolName(workspace.OperationRunCommand)
		f = looptest.NewFixture(t, store,
			looptest.WithHostTool(workspace.OperationRunCommand, func(ctx context.Context, req models.HostToolRequest) (models.HostToolResult, error) {
				capabilityName := ""
				for _, tool := range req.VMTools {
					if strings.HasSuffix(tool.Name, "lookup") {
						capabilityName = tool.Name
					}
				}
				if capabilityName == "" {
					nestedErr = errors.New("run_command was offered no lookup VM tool")
					return looptest.HostText("no lookup tool"), nil
				}
				for i := 1; i <= 3; i++ {
					nested, err := f.Actions.ProposeNested(ctx, action.NestedActionRequest{
						Scope: req.Scope, RunID: req.RunID, TurnID: req.StepID, Capability: capabilityName,
						Arguments: map[string]interface{}{"id": "6815"}, Origin: models.ActionOriginVM,
						OriginRef: req.CommandRef, IdempotencyKey: fmt.Sprintf("bridge-%d", i),
					})
					if err != nil {
						nestedErr = fmt.Errorf("nested lookup %d: %w", i, err)
						return looptest.HostText("bridge failed"), nil
					}
					if nested.Call.Status != models.ActionCallStatusReady {
						nestedErr = fmt.Errorf("nested lookup %d is %s, want ready", i, nested.Call.Status)
						return looptest.HostText("bridge failed"), nil
					}
					if _, err := f.Executor.RunOnce(ctx, req.Scope, "vm-action-worker", looptest.FixtureLease); err != nil {
						nestedErr = fmt.Errorf("run nested lookup %d: %w", i, err)
						return looptest.HostText("bridge failed"), nil
					}
				}
				return looptest.HostText("looked 6815 up three times: open"), nil
			}),
			looptest.WithRules(
				&looptest.ModelRule{Name: "command", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Act("Running the script.", looptest.Call(command, map[string]any{"command": "for i in 1 2 3; do seal call lookup 6815; done"}))
				}},
				&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": "6815 is open."})
				}},
			))
		run := f.StartRun(models.RunKindAgentWork, "Look 6815 up from a script")
		done := f.DriveToRest(run.ID, 3)
		if nestedErr != nil {
			t.Fatal(nestedErr)
		}
		f.MustStatus(done, models.AgentRunStatusCompleted)

		if got := f.Dispatch.Count("lookup"); got != 3 {
			t.Fatalf("lookup dispatched %d times, want 3", got)
		}
		var nested []*models.ActionCall
		for _, row := range f.Calls(run.ID) {
			if row.Action == "lookup" {
				nested = append(nested, row)
			}
		}
		if len(nested) != 3 {
			t.Fatalf("lookup rows = %d, want 3", len(nested))
		}
		for _, row := range nested {
			if row.Origin != models.ActionOriginVM || row.Status != models.ActionCallStatusSucceeded {
				t.Fatalf("nested row = origin %s status %s, want a succeeded vm call", row.Origin, row.Status)
			}
		}
		commands := repeatToolEntries(f, run.ID, command)
		if len(commands) != 1 || commands[0].State != models.ToolEntryCompleted || repeatGuard(commands[0]) != "" {
			t.Fatalf("run_command entries = %s", describeRepeatEntries(commands))
		}
		if len(repeatToolEntries(f, run.ID, "lookup")) != 0 {
			t.Fatal("nested calls became tool entries")
		}
		if notes := repeatNotes(f, run.ID); len(notes) != 0 {
			t.Fatalf("repeat notes = %d, want 0", len(notes))
		}
		if len(f.Events(run.ID, action.ActivityEventActionRefused)) != 0 {
			t.Fatal("a nested call was refused")
		}
	})
}

// TestLoopRepeatResultDigestIgnoresTheActionReference: two governed results
// that differ only in their own action reference are the same result.
func TestLoopRepeatResultDigestIgnoresTheActionReference(t *testing.T) {
	result := func(id, status string) *models.ToolResult {
		return &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{
			{Type: models.ContentPartJSON, Value: json.RawMessage(`{"evidenceRef":"` + runloop.GovernedEvidenceRef(id) + `","output":{"status":"` + status + `"}}`)},
			{Type: models.ContentPartImage, MediaType: "image/png", SHA256: "abc", ActionCallID: id},
		}}
	}
	if modelstep.ToolResultDigest(result("a", "open")) != modelstep.ToolResultDigest(result("b", "open")) {
		t.Fatal("results that differ only in their action reference have different digests")
	}
	if modelstep.ToolResultDigest(result("a", "open")) == modelstep.ToolResultDigest(result("a", "closed")) {
		t.Fatal("different outputs have the same digest")
	}
}
