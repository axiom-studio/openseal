package runtime

// Compaction and overflow journeys (run-loop section 12 §5.5, tests-T09).
// The real RunLoop drives the kernel LoopCompactor, which the fixture's
// binding wires as the catalog resolver does (budget included); only the
// model, the host tools and the action dispatcher are fakes. Tool results are sized through the kernel estimate
// (estimateEntryTokens), so the thresholds the scenarios cross are exact.

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fixture glue

// bigLookups makes every lookup result about tokens kernel-estimated tokens.
func bigLookups(f *loopFixture, tokens int) {
	f.dispatch.on("lookup", func(in ActionDispatchInput) (map[string]interface{}, error) {
		id, _ := in.Arguments["id"].(string)
		return map[string]interface{}{"id": id, "status": "open", "notes": strings.Repeat("note ", tokens*4/5)}, nil
	})
}

// lookupScript calls lookup{1..n}, one per step, then answers. Its step
// counter lives in the script, not the rendered path, because a compaction
// shortens the path.
func lookupScript(n int, answer string) *modelRule {
	done := 0
	return &modelRule{name: "lookups", when: isStepPurpose, reply: func(v stepView) ModelStepResult {
		if done < n {
			done++
			return act(fmt.Sprintf("Looking up %d.", done), call(kitGovernedTool(v, "lookup"), map[string]any{"id": fmt.Sprint(done)}))
		}
		return say(answer)
	}}
}

func isStepPurpose(v stepView) bool { return v.Purpose() == ModelStepPurposeStep }

// summaryRule answers every compaction summary call; the i-th summary is
// "Summary <i>: ...".
func summaryRule() *modelRule {
	n := 0
	return &modelRule{name: "summary",
		when: func(v stepView) bool { return v.Purpose() == ModelStepPurposeSummary },
		reply: func(stepView) ModelStepResult {
			n++
			return say(fmt.Sprintf("Summary %d: the earlier lookups found open issues.", n))
		}}
}

func contextExceeded() error {
	return &TurnHostFailure{Code: TurnHostContextExceeded, Message: "the prompt is longer than the model's context"}
}

// compactionEntries is the Run's compaction entries in log order.
func compactionEntries(f *loopFixture, runID string) []*SessionEntry {
	var out []*SessionEntry
	for _, entry := range f.entries(runID) {
		if entry.Type == SessionEntryCompaction {
			out = append(out, entry)
		}
	}
	return out
}

func entryIndex(entries []*SessionEntry, id string) int {
	return slices.IndexFunc(entries, func(e *SessionEntry) bool { return e.ID == id })
}

// summarizedActionCallIDs is the action calls of the tool entries before
// compaction's first kept entry and after the previous compaction's first
// kept entry (or the log start), in log order.
func summarizedActionCallIDs(t *testing.T, log []*SessionEntry, previous, compaction *SessionEntry) []string {
	t.Helper()
	from := 0
	if previous != nil {
		from = entryIndex(log, previous.Compaction.FirstKeptEntryID)
	}
	to := entryIndex(log, compaction.Compaction.FirstKeptEntryID)
	if from < 0 || to < 0 {
		t.Fatalf("first kept entries %v not in the log", []*SessionEntry{previous, compaction})
	}
	var ids []string
	for _, entry := range log[from:to] {
		if entry.Type == SessionEntryTool && entry.ActionCallID != "" {
			ids = append(ids, entry.ActionCallID)
		}
	}
	return ids
}

// requireCompactionKeepsToolPairs fails when compaction's kept tail starts
// at a tool entry, or keeps a tool entry without the assistant entry that
// called it, or summarizes away a tool entry whose call it keeps.
func requireCompactionKeepsToolPairs(t *testing.T, log []*SessionEntry, compaction *SessionEntry) {
	t.Helper()
	first := entryIndex(log, compaction.Compaction.FirstKeptEntryID)
	end := entryIndex(log, compaction.ID)
	if first < 0 || end < 0 || first >= end {
		t.Fatalf("first kept entry %q is not before compaction %s", compaction.Compaction.FirstKeptEntryID, compaction.ID)
	}
	if log[first].Type == SessionEntryTool {
		t.Fatalf("kept tail starts at tool entry %s", log[first].ID)
	}
	// Scripted call ids restart after a compaction (stepView.Step counts the
	// rendered path), so a call is keyed by its step row too.
	callers := map[string]int{} // step id + tool call id -> index of its assistant entry
	for i, entry := range log[:end] {
		if entry.Type == SessionEntryAssistant && entry.Assistant != nil {
			for _, call := range entry.Assistant.ToolCalls {
				callers[entry.StepID+"/"+call.ID] = i
			}
		}
	}
	for i, entry := range log[:end] {
		if entry.Type != SessionEntryTool {
			continue
		}
		caller, ok := callers[entry.StepID+"/"+entry.ToolCallID]
		if !ok {
			t.Fatalf("tool entry %s has no assistant call %q", entry.ID, entry.ToolCallID)
		}
		if (i >= first) != (caller >= first) {
			t.Fatalf("compaction splits tool call %q: assistant at %d, result at %d, first kept at %d", entry.ToolCallID, caller, i, first)
		}
	}
}

func viewEntryIDs(views []SessionEntryView) []string {
	out := make([]string, len(views))
	for i, view := range views {
		out[i] = view.ID
	}
	return out
}

// ---------------------------------------------------------------------------
// Scenarios

// TestLoopCompactsAtStepStartWhenOverThreshold: a 64k chat crosses the 48k
// threshold after three 12k lookups, so step 4 starts with a threshold
// compaction whose summary replaces everything before the kept tail.
func TestLoopCompactsAtStepStartWhenOverThreshold(t *testing.T) {
	eachStore(t, "compaction_threshold", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withContextWindow(64_000), withRules(summaryRule(), lookupScript(3, "Issues 1 to 3 are open.")))
		bigLookups(f, 12_000)
		conversation, run := f.startChat("check issues 1 to 3")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)

		log := f.entries(run.ID)
		compactions := compactionEntries(f, run.ID)
		if len(compactions) != 1 {
			t.Fatalf("compactions = %d, want 1; log %v", len(compactions), f.entryTypes(run.ID))
		}
		compaction := compactions[0]
		payload := compaction.Compaction
		if payload.Trigger != CompactionTriggerThreshold {
			t.Fatalf("trigger = %q, want threshold", payload.Trigger)
		}
		if payload.Summary != "Summary 1: the earlier lookups found open issues." {
			t.Fatalf("summary = %q", payload.Summary)
		}
		// The estimate crossed the threshold only after the third lookup.
		if payload.TokensBefore <= 48_000 || payload.TokensBefore > 56_000 {
			t.Fatalf("tokensBefore = %d, want about 52k", payload.TokensBefore)
		}

		// The kept tail is the newest ~12k (compactionKeepTokens(64k)): step 3's
		// call and its result, cut at the assistant entry.
		first := entryIndex(log, payload.FirstKeptEntryID)
		if first < 0 || log[first].Type != SessionEntryAssistant || log[first].Step != 3 {
			t.Fatalf("first kept entry = %#v, want step 3's assistant entry", log[max(first, 0)])
		}
		var kept int64
		for _, entry := range log[first:entryIndex(log, compaction.ID)] {
			kept += entry.Tokens
		}
		// A 64k window keeps min(15k, a quarter of its 48k threshold).
		if keep := int64(12_000); kept < keep {
			t.Fatalf("kept tail = %d tokens, want at least %d", kept, keep)
		}
		requireCompactionKeepsToolPairs(t, log, compaction)

		// actionCallIds are the summarized lookups' calls, derived by the kernel.
		calls := f.calls(run.ID)
		if len(calls) != 3 {
			t.Fatalf("action calls = %d, want 3", len(calls))
		}
		if want := summarizedActionCallIDs(t, log, nil, compaction); len(want) != 2 || !slices.Equal(payload.ActionCallIDs, want) {
			t.Fatalf("actionCallIds = %v, want the summarized lookups %v", payload.ActionCallIDs, want)
		}

		// The summary call is the step's own request as a summary: same step,
		// System and Tools, every rendered entry of the path, no preview.
		summaries := f.model.calls(ModelStepPurposeSummary)
		steps := f.model.calls(ModelStepPurposeStep)
		if len(summaries) != 1 || len(steps) != 4 {
			t.Fatalf("model calls: %d summaries, %d steps; want 1 and 4", len(summaries), len(steps))
		}
		summary, after := summaries[0], steps[3]
		if summary.StepID != after.StepID || summary.SummaryInstruction == "" || summary.Stream.AnswerPreview {
			t.Fatalf("summary request = step %s instruction %q preview %v; step 4 is %s",
				summary.StepID, summary.SummaryInstruction, summary.Stream.AnswerPreview, after.StepID)
		}
		if !reflect.DeepEqual(summary.System, after.System) || !slices.Equal(toolNames(summary.Tools), toolNames(after.Tools)) {
			t.Fatalf("summary System/Tools differ from the step's: tools %v vs %v", toolNames(summary.Tools), toolNames(after.Tools))
		}
		summarized := viewEntryIDs(summary.Entries)
		for _, entry := range log[:first] {
			if entry.Type != SessionEntryNote && !slices.Contains(summarized, entry.ID) {
				t.Fatalf("summary request lacks summarized entry %s (%s): %v", entry.ID, entry.Type, summarized)
			}
		}

		// The next step renders the compaction, then exactly the kept tail.
		rendered := viewEntryIDs(after.Entries)
		if len(rendered) == 0 || rendered[0] != compaction.ID {
			t.Fatalf("step 4 path = %v, want the compaction first", rendered)
		}
		var wantTail []string
		for _, entry := range log[first:] {
			if entry.ID == compaction.ID || entry.Step == 4 && entry.Type == SessionEntryAssistant {
				continue
			}
			wantTail = append(wantTail, entry.ID)
		}
		if !slices.Equal(rendered[1:], wantTail) {
			t.Fatalf("step 4 path after the compaction = %v, want the kept tail %v", rendered[1:], wantTail)
		}
		if got, ok := (stepView{req: after}).Compaction(); !ok || !strings.Contains(got, payload.Summary) {
			t.Fatalf("step 4 compaction view = %q", got)
		}

		// Log: the compaction sits after every lookup and before step 4's answer.
		at := entryIndex(log, compaction.ID)
		for i, entry := range log {
			if entry.Type == SessionEntryTool && i > at {
				t.Fatalf("tool entry %s after the compaction", entry.ID)
			}
			if entry.Type == SessionEntryAssistant && entry.Step == 4 && i < at {
				t.Fatalf("step 4's assistant entry precedes the compaction")
			}
		}
		if log[len(log)-1].Type != SessionEntryAssistant || log[len(log)-1].Step != 4 {
			t.Fatalf("log = %v, want step 4's answer last", f.entryTypes(run.ID))
		}

		// Compaction writes no step row and announces itself once.
		if n := len(f.steps(run.ID)); n != 4 {
			t.Fatalf("step rows = %d, want 4", n)
		}
		events := f.events(run.ID, "session.compacted")
		if len(events) != 1 || events[0].Payload["entryId"] != compaction.ID || events[0].Payload["trigger"] != CompactionTriggerThreshold {
			t.Fatalf("compaction events = %#v", events)
		}
		messages := f.messages(conversation.ID)
		if last := messages[len(messages)-1]; last.Sender.ID != loopFixtureAgent || !strings.Contains(last.Content, "Issues 1 to 3 are open.") {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
	})
}

// TestLoopLaterCompactionCarriesThePreviousSummary: after a first compaction
// the path grows past the threshold again; the second summary call reads the
// first summary and its entry's actionCallIds are the union.
func TestLoopLaterCompactionCarriesThePreviousSummary(t *testing.T) {
	eachStore(t, "compaction_later", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withContextWindow(64_000), withRules(summaryRule(), lookupScript(5, "Issues 1 to 5 are open.")))
		bigLookups(f, 12_000)
		run := f.startRun(RunKindAgentWork, "Check issues 1 to 5")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)

		log := f.entries(run.ID)
		compactions := compactionEntries(f, run.ID)
		if len(compactions) != 2 {
			t.Fatalf("compactions = %d, want 2; log %v", len(compactions), f.entryTypes(run.ID))
		}
		first, second := compactions[0], compactions[1]
		summaries := f.model.calls(ModelStepPurposeSummary)
		if len(summaries) != 2 {
			t.Fatalf("summary calls = %d, want 2", len(summaries))
		}
		if len(summaries[1].Entries) == 0 || summaries[1].Entries[0].ID != first.ID ||
			!strings.Contains(summaries[1].Entries[0].Compaction.Summary, first.Compaction.Summary) {
			t.Fatalf("second summary path = %v, want the first compaction first", viewEntryIDs(summaries[1].Entries))
		}
		if second.Compaction.Summary != "Summary 2: the earlier lookups found open issues." {
			t.Fatalf("second summary = %q", second.Compaction.Summary)
		}
		for _, compaction := range compactions {
			requireCompactionKeepsToolPairs(t, log, compaction)
		}
		want := append(append([]string(nil), first.Compaction.ActionCallIDs...), summarizedActionCallIDs(t, log, first, second)...)
		if len(want) <= len(first.Compaction.ActionCallIDs) || !slices.Equal(second.Compaction.ActionCallIDs, want) {
			t.Fatalf("second actionCallIds = %v, want the union %v", second.Compaction.ActionCallIDs, want)
		}
		// The step after the second compaction renders only it and its tail.
		steps := f.model.calls(ModelStepPurposeStep)
		last := steps[len(steps)-1]
		if rendered := viewEntryIDs(last.Entries); len(rendered) == 0 || rendered[0] != second.ID || slices.Contains(rendered, first.ID) {
			t.Fatalf("last step path = %v, want the second compaction first and not the first", rendered)
		}
		if n := len(f.steps(run.ID)); n != 6 {
			t.Fatalf("step rows = %d, want 6", n)
		}
	})
}

// overflowScript is three 5k lookups, then a step the host rejects for
// length (rejections times; 0 = always), then "ok" once a compaction renders.
func overflowScript(rejections int) []*modelRule {
	lookups := 0
	return []*modelRule{
		summaryRule(),
		{name: "lookups", when: func(v stepView) bool { return isStepPurpose(v) && lookups < 3 },
			reply: func(v stepView) ModelStepResult {
				lookups++
				return act(fmt.Sprintf("Looking up %d.", lookups), call(kitGovernedTool(v, "lookup"), map[string]any{"id": fmt.Sprint(lookups)}))
			}},
		{name: "too long", when: isStepPurpose, err: contextExceeded(), times: rejections},
		{name: "ok", when: func(v stepView) bool {
			_, compacted := v.Compaction()
			return isStepPurpose(v) && compacted
		}, reply: func(stepView) ModelStepResult { return say("ok") }},
	}
}

// TestLoopLengthRejectionCompactsAndRetriesOnce: the host rejects step 4 for
// length once; the loop compacts (overflow) and retries the same step.
func TestLoopLengthRejectionCompactsAndRetriesOnce(t *testing.T) {
	eachStore(t, "compaction_overflow_retry", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(overflowScript(1)...))
		bigLookups(f, 5_000)
		run := f.startRun(RunKindAgentWork, "Check issues 1 to 3")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)

		var order []string
		for _, req := range f.model.calls("") {
			order = append(order, fmt.Sprintf("%s#%d", req.Purpose, req.Attempt))
		}
		if got, want := strings.Join(order, ","), "step#0,step#0,step#0,step#0,summary#0,step#1"; got != want {
			t.Fatalf("model calls = %s, want %s", got, want)
		}
		requests := f.model.calls("")
		rejected, summary, retry := requests[3], requests[4], requests[5]
		if rejected.StepID != summary.StepID || retry.StepID != rejected.StepID {
			t.Fatalf("rejected %s, summary %s, retry %s: want one step", rejected.StepID, summary.StepID, retry.StepID)
		}
		compactions := compactionEntries(f, run.ID)
		if len(compactions) != 1 || compactions[0].Compaction.Trigger != CompactionTriggerOverflow {
			t.Fatalf("compactions = %#v, want one overflow compaction", compactions)
		}
		compaction := compactions[0]
		log := f.entries(run.ID)
		requireCompactionKeepsToolPairs(t, log, compaction)
		// Overflow summarizes only the span before the kept tail.
		first := entryIndex(log, compaction.Compaction.FirstKeptEntryID)
		for _, id := range viewEntryIDs(summary.Entries) {
			if i := entryIndex(log, id); i < 0 || i >= first {
				t.Fatalf("overflow summary sent kept entry %s (first kept at %d)", id, first)
			}
		}
		if rendered := viewEntryIDs(retry.Entries); len(rendered) == 0 || rendered[0] != compaction.ID {
			t.Fatalf("retry path = %v, want the compaction first", rendered)
		}
		// The retry belongs to the rejected step: no extra step row.
		if n := len(f.steps(run.ID)); n != 4 {
			t.Fatalf("step rows = %d, want 4", n)
		}
		requireCapabilityNotesThroughProducer(t, f, run.ID, requests, compaction)
	})
}

// requireCapabilityNotesThroughProducer checks the capabilities note the real
// ModelStepProducer writes: one full note, the last entry of the first
// request; none on later steps; one full note again after the compaction
// summarized the first, the last entry of the retry; never a duplicate.
func requireCapabilityNotesThroughProducer(t *testing.T, f *loopFixture, runID string, requests []ModelStepRequest, compaction *SessionEntry) {
	t.Helper()
	var notes []*SessionEntry
	for _, entry := range f.entries(runID) {
		if entry.Type == SessionEntryNote && entry.Note != nil && entry.Note.Kind == NoteCapabilities {
			notes = append(notes, entry)
		}
	}
	if len(notes) != 2 {
		t.Fatalf("capabilities notes = %d, want 2 (first step and after the compaction)", len(notes))
	}
	for _, note := range notes {
		if !strings.HasPrefix(note.Note.Text, "<capabilities>") || !strings.Contains(note.Note.Text, "action:") {
			t.Fatalf("note %s is not a full listing: %q", note.ID, note.Note.Text)
		}
	}
	if notes[0].Sequence > compaction.Sequence || notes[1].Sequence < compaction.Sequence {
		t.Fatalf("notes at %d and %d around compaction %d", notes[0].Sequence, notes[1].Sequence, compaction.Sequence)
	}
	last := func(req ModelStepRequest) string { return req.Entries[len(req.Entries)-1].ID }
	if got := last(requests[0]); got != notes[0].ID {
		t.Fatalf("first request ends at %s, want the note %s", got, notes[0].ID)
	}
	retry := requests[len(requests)-1]
	if got := last(retry); got != notes[1].ID {
		t.Fatalf("retry ends at %s, want the note %s", got, notes[1].ID)
	}
	for _, req := range requests[1 : len(requests)-1] {
		if req.Purpose != ModelStepPurposeStep {
			continue
		}
		for _, view := range req.Entries {
			if view.Note != nil && view.Note.Kind == string(NoteCapabilities) && view.ID != notes[0].ID {
				t.Fatalf("step %s sees an extra capabilities note %s", req.StepID, view.ID)
			}
		}
	}
}

// TestLoopSecondLengthRejectionFailsVisibly: the retry after the overflow
// compaction is rejected again, so the Run fails with the context overflow
// code and message, without a second compaction or another model call.
func TestLoopSecondLengthRejectionFailsVisibly(t *testing.T) {
	eachStore(t, "compaction_overflow_fail", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(overflowScript(0)...))
		bigLookups(f, 5_000)
		run := f.startRun(RunKindAgentWork, "Check issues 1 to 3")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusFailed)

		const overflowMessage = "The conversation is too long for the model's context, even after compacting it."
		failure := done.FailureExplanation
		if failure == nil || failure.Code != TurnHostContextExceeded || failure.Kind != RunFailureKindContextOverflow ||
			failure.Message != overflowMessage {
			t.Fatalf("failure explanation = %#v", failure)
		}
		if !strings.Contains(done.Error, overflowMessage) {
			t.Fatalf("run error = %q, want the context overflow message", done.Error)
		}
		if n := len(compactionEntries(f, run.ID)); n != 1 {
			t.Fatalf("compactions = %d, want 1", n)
		}
		var order []string
		for _, req := range f.model.calls("") {
			order = append(order, fmt.Sprintf("%s#%d", req.Purpose, req.Attempt))
		}
		if got, want := strings.Join(order, ","), "step#0,step#0,step#0,step#0,summary#0,step#1"; got != want {
			t.Fatalf("model calls = %s, want %s", got, want)
		}
		steps := f.steps(run.ID)
		if len(steps) != 4 || steps[3].FailureCode != TurnHostContextExceeded {
			t.Fatalf("step rows = %d, last failure %q; want 4 ending in %s", len(steps), steps[len(steps)-1].FailureCode, TurnHostContextExceeded)
		}
	})
}

// TestLoopCompactionNeverSplitsAToolCallFromItsResult: the keep boundary
// falls between a step's two parallel tool results; the cut moves down to the
// step's assistant entry so the call and both results stay together.
func TestLoopCompactionNeverSplitsAToolCallFromItsResult(t *testing.T) {
	eachStore(t, "compaction_tool_pairs", func(t *testing.T, store KernelStore) {
		steps := 0
		f := newLoopFixture(t, store, withContextWindow(64_000), withRules(summaryRule(), &modelRule{name: "pairs", when: isStepPurpose,
			reply: func(v stepView) ModelStepResult {
				steps++
				if steps > 3 {
					return say("All six are open.")
				}
				a, b := fmt.Sprint(2*steps-1), fmt.Sprint(2*steps)
				return act("Looking up "+a+" and "+b+".",
					call(kitGovernedTool(v, "lookup"), map[string]any{"id": a}),
					call(kitGovernedTool(v, "lookup"), map[string]any{"id": b}))
			}}))
		// Each result is 6k: the newest one alone is under the 12k keep, the
		// newest two cross it, so the boundary falls between step 3's results.
		bigLookups(f, 6_000)
		run := f.startRun(RunKindAgentWork, "Check issues 1 to 6")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)

		log := f.entries(run.ID)
		compactions := compactionEntries(f, run.ID)
		if len(compactions) == 0 {
			t.Fatalf("no compaction; log %v", f.entryTypes(run.ID))
		}
		for _, compaction := range compactions {
			requireCompactionKeepsToolPairs(t, log, compaction)
		}
		first := log[entryIndex(log, compactions[0].Compaction.FirstKeptEntryID)]
		if first.Type != SessionEntryAssistant || first.Step != 3 || len(first.Assistant.ToolCalls) != 2 {
			t.Fatalf("first kept entry = %s step %d, want step 3's two-call assistant entry", first.Type, first.Step)
		}
		if f.dispatch.count("lookup") != 6 {
			t.Fatalf("lookups dispatched = %d, want 6", f.dispatch.count("lookup"))
		}
	})
}
