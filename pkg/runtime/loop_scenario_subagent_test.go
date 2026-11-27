package runtime

// Run-loop scenarios, subagents: background subagents with their end notes,
// the final-answer guard, wait_subagents, read_subagent, message_subagent
// (steering a running subagent, continuing a finished one on its session)
// and stop_subagents, driven through the real runloop.Loop by the child
// fixture's worker.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

func notParent(v looptest.StepView) bool { return !isForkBranch(v) }

func notesContain(v looptest.StepView, text string) bool {
	for _, note := range v.Notes() {
		if strings.Contains(note, text) {
			return true
		}
	}
	return false
}

func (c *childFixture) notesOf(runID string, kind models.NoteKind) []*models.SessionEntry {
	c.T.Helper()
	var out []*models.SessionEntry
	for _, e := range c.sessionEntries(c.MustRun(runID).SessionID) {
		if e.Type == models.SessionEntryNote && e.Note != nil && e.Note.Kind == kind && e.RunID == runID {
			out = append(out, e)
		}
	}
	return out
}

func hasTool(req models.ModelStepRequest, name string) bool {
	return looptest.StepView{Req: req}.HasTool(name)
}

func TestLoopBackgroundSubagentsGuardTheFinalAnswerUntilWaitedFor(t *testing.T) {
	looptest.EachStore(t, "subagent_background", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "subagent", When: isForkBranch, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Say("done: " + branchGoal(v))
			}},
			&looptest.ModelRule{Name: "start", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Starting both surveys.", childStartCall(true, "survey north", "survey south"))
			}},
			&looptest.ModelRule{Name: "too-early", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Say("All done.")
			}},
			&looptest.ModelRule{Name: "wait", Times: 1, When: func(v looptest.StepView) bool { return notParent(v) && notesContain(v, "still running") },
				Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Act("Waiting for them.", looptest.Call(string(models.ControlToolWaitSubagents), map[string]any{}))
				}},
			&looptest.ModelRule{Name: "answer", When: notParent, Reply: func(v looptest.StepView) models.ModelStepResult {
				content, _, _ := v.ToolResult(string(models.ControlToolWaitSubagents))
				if !strings.Contains(content, "done: survey north") || !strings.Contains(content, "done: survey south") {
					return looptest.Say("The surveys are lost.")
				}
				return looptest.Say("North and south are surveyed.")
			}},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Survey the regions")

		// One claim: start in the background, a refused answer, then the wait.
		if exit, _ := c.drive(parent.ID); exit.Reason != runloop.ExitParked {
			t.Fatalf("parent exit = %s", exit.Reason)
		}
		start := c.toolEntry(parent.ID, string(models.ControlToolStartSubagents))
		value := childResultJSON(t, start)
		started, _ := value["subagents"].([]interface{})
		if start.State != models.ToolEntryCompleted || len(started) != 2 || started[0].(map[string]interface{})["id"] != "s1" ||
			started[1].(map[string]interface{})["status"] != "running" {
			t.Fatalf("start entry = %s %s", start.State, childResultText(start))
		}
		var surfaced []string
		for _, tool := range start.Tool.SurfacedTools {
			surfaced = append(surfaced, tool.Name)
		}
		if strings.Join(surfaced, ",") != "wait_subagents,read_subagent,message_subagent,stop_subagents" {
			t.Fatalf("surfaced = %v", surfaced)
		}
		for _, raw := range strings.Split(childResultText(start), "\"") {
			for _, child := range c.children(parent.ID) {
				if raw == child.ID {
					t.Fatalf("the start result names Run %s", raw)
				}
			}
		}
		requests := c.requestsFor(parent.ID)
		if len(requests) != 3 || hasTool(requests[0], string(models.ControlToolWaitSubagents)) || !hasTool(requests[1], string(models.ControlToolWaitSubagents)) {
			t.Fatalf("parent requests = %d; follow-up tools first offered after the start", len(requests))
		}
		rejections := c.notesOf(parent.ID, models.NoteFinalRejection)
		if len(rejections) != 1 || rejections[0].Note.Text != "s1, s2 are still running: wait for them or stop them." {
			t.Fatalf("final rejections = %v", rejections)
		}
		wait := c.toolEntry(parent.ID, string(models.ControlToolWaitSubagents))
		waiting := c.MustRun(parent.ID)
		if wait.State != models.ToolEntryAwaitingChild || waiting.Status != models.AgentRunStatusWaitingForDependency ||
			waiting.WakeCondition == nil || waiting.WakeCondition.Reference != agentrun.WaitGroupID(parent.ID, wait.ID) {
			t.Fatalf("wait = %s, parent %s %#v", wait.State, waiting.Status, waiting.WakeCondition)
		}

		c.work(10)
		done := c.MustRun(parent.ID)
		c.MustStatus(done, models.AgentRunStatusCompleted)
		if got := done.Output[conversationrun.FinalOutputSummaryKey]; got != "North and south are surveyed." {
			t.Fatalf("parent answer = %v", got)
		}
		wait = c.toolEntry(parent.ID, string(models.ControlToolWaitSubagents))
		if wait.State != models.ToolEntryCompleted || !strings.Contains(childResultText(wait), "done: survey south") {
			t.Fatalf("wait entry = %s %s", wait.State, childResultText(wait))
		}
		// The wait reported both, so no note repeats them.
		if notes := c.notesOf(parent.ID, models.NoteSubagent); len(notes) != 0 {
			t.Fatalf("subagent notes = %d", len(notes))
		}
	})
}

func TestLoopBackgroundSubagentEndArrivesAsANote(t *testing.T) {
	looptest.EachStore(t, "subagent_note", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "subagent", When: isForkBranch, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Say("3 boxes")
			}},
			&looptest.ModelRule{Name: "start", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Counting in the background.", childStartCall(true, "count boxes"))
			}},
			&looptest.ModelRule{Name: "sleep", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Meanwhile, a pause.", looptest.Call(string(models.ControlToolWait), map[string]any{"seconds": 60}))
			}},
			&looptest.ModelRule{Name: "answer", When: func(v looptest.StepView) bool {
				return notParent(v) && notesContain(v, "Subagent s1 finished: 3 boxes")
			}, Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("There are 3 boxes.") }},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Count the stock")
		// The parent sleeps while its subagent works, then wakes to the note.
		c.work(10)
		c.MustStatus(c.MustRun(parent.ID), models.AgentRunStatusSleeping)
		c.Clock.Advance(61 * time.Second)
		if _, err := agentrun.NewWakeService(c.Store, c.Store, models.WithClock(c.Clock.Now)).WakeDueTimers(c.Ctx, c.Scope, c.Clock.Now()); err != nil {
			t.Fatalf("wake timers: %v", err)
		}
		c.work(4)
		done := c.MustRun(parent.ID)
		c.MustStatus(done, models.AgentRunStatusCompleted)
		notes := c.notesOf(parent.ID, models.NoteSubagent)
		if len(notes) != 1 || notes[0].Note.Text != "Subagent s1 finished: 3 boxes" {
			t.Fatalf("subagent notes = %v", notes)
		}
		var data map[string]interface{}
		if err := json.Unmarshal(notes[0].Note.Data, &data); err != nil || data["subagent"] != "s1" || data["status"] != "completed" {
			t.Fatalf("note data = %s", notes[0].Note.Data)
		}
	})
}

func TestLoopSubagentReadAndFollowUpContinueItsSession(t *testing.T) {
	looptest.EachStore(t, "subagent_follow_up", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "follow-up", When: func(v looptest.StepView) bool {
				return isForkBranch(v) && strings.Contains(v.LastUserText(), "Also count the crates")
			}, Reply: func(v looptest.StepView) models.ModelStepResult { return looptest.Say("2 crates") }},
			&looptest.ModelRule{Name: "subagent", When: isForkBranch, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Say("3 boxes")
			}},
			&looptest.ModelRule{Name: "start", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Counting.", childForkCall("count boxes"))
			}},
			&looptest.ModelRule{Name: "read", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Checking its work.", looptest.Call(string(models.ControlToolReadSubagent), map[string]any{"id": "s1"}))
			}},
			&looptest.ModelRule{Name: "message", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("One more thing.", looptest.Call(string(models.ControlToolMessageSubagent), map[string]any{"id": "s1", "message": "Also count the crates."}))
			}},
			&looptest.ModelRule{Name: "answer", When: notParent, Reply: func(v looptest.StepView) models.ModelStepResult {
				content, _, _ := v.ToolResult(string(models.ControlToolMessageSubagent))
				return looptest.Say("Boxes and " + map[bool]string{true: "2 crates", false: "no crates"}[strings.Contains(content, "2 crates")] + ".")
			}},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Count the stock")
		c.work(12)
		done := c.MustRun(parent.ID)
		c.MustStatus(done, models.AgentRunStatusCompleted)
		if got := done.Output[conversationrun.FinalOutputSummaryKey]; got != "Boxes and 2 crates." {
			t.Fatalf("parent answer = %v", got)
		}

		// read_subagent shows the subagent's own entries from its brief on.
		read := childResultJSON(t, c.toolEntry(parent.ID, string(models.ControlToolReadSubagent)))
		entries, _ := read["entries"].([]interface{})
		if read["status"] != "completed" || len(entries) != 2 {
			t.Fatalf("read = %v", read)
		}
		brief, answer := entries[0].(map[string]interface{}), entries[1].(map[string]interface{})
		if brief["type"] != "brief" || !strings.Contains(brief["text"].(string), "count boxes") ||
			answer["type"] != "assistant" || answer["text"] != "3 boxes" {
			t.Fatalf("read entries = %v", entries)
		}
		if strings.Contains(childResultText(c.toolEntry(parent.ID, string(models.ControlToolReadSubagent))), "Count the stock") {
			t.Fatal("read_subagent returned the inherited parent path")
		}

		// The follow-up is a second Run on the first one's session.
		children := c.children(parent.ID)
		if len(children) != 2 {
			t.Fatalf("children = %d", len(children))
		}
		first, second := children[0], children[1]
		if first.Goal != "count boxes" {
			first, second = second, first
		}
		session := c.sessionByID(first.SessionID)
		if second.SessionID != first.SessionID || session.OwnerRunID != second.ID || second.Goal != "Also count the crates." ||
			c.MustRun(second.ID).Status != models.AgentRunStatusCompleted {
			t.Fatalf("follow-up = session %q (first %q, owner %q) goal %q", second.SessionID, first.SessionID, session.OwnerRunID, second.Goal)
		}
		requests := c.requestsFor(second.ID)
		if len(requests) != 1 {
			t.Fatalf("follow-up requests = %d", len(requests))
		}
		var sawFirstAnswer bool
		for _, entry := range requests[0].Entries {
			sawFirstAnswer = sawFirstAnswer || entry.Type == models.SessionEntryAssistant && entry.Assistant != nil && entry.Assistant.Text == "3 boxes"
		}
		if !sawFirstAnswer {
			t.Fatalf("the follow-up lost the subagent's context: %s", looptest.StepView{Req: requests[0]}.Describe())
		}
		message := c.toolEntry(parent.ID, string(models.ControlToolMessageSubagent))
		if message.State != models.ToolEntryCompleted || !strings.Contains(childResultText(message), "2 crates") {
			t.Fatalf("message entry = %s %s", message.State, childResultText(message))
		}
	})
}

func TestLoopMessageSteersARunningSubagent(t *testing.T) {
	looptest.EachStore(t, "subagent_steer", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "subagent", When: isForkBranch, Reply: func(v looptest.StepView) models.ModelStepResult {
				if strings.Contains(v.LastUserText(), "Only the north wing") {
					return looptest.Say("north wing: 4 boxes")
				}
				return looptest.Say("all wings: 9 boxes")
			}},
			&looptest.ModelRule{Name: "start", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Counting.", childStartCall(true, "count boxes"))
			}},
			&looptest.ModelRule{Name: "steer", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Narrowing it.", looptest.Call(string(models.ControlToolMessageSubagent), map[string]any{"id": "s1", "message": "Only the north wing.", "wait": false}))
			}},
			&looptest.ModelRule{Name: "wait", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Waiting.", looptest.Call(string(models.ControlToolWaitSubagents), map[string]any{"ids": []string{"s1"}}))
			}},
			&looptest.ModelRule{Name: "answer", When: notParent, Reply: func(v looptest.StepView) models.ModelStepResult {
				content, _, _ := v.ToolResult(string(models.ControlToolWaitSubagents))
				return looptest.Say(map[bool]string{true: "4 boxes in the north wing.", false: "Not narrowed."}[strings.Contains(content, "north wing: 4 boxes")])
			}},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Count the stock")
		c.drive(parent.ID)
		children := c.children(parent.ID)
		if len(children) != 1 {
			t.Fatalf("children = %d", len(children))
		}
		steered := c.MustRun(children[0].ID)
		if len(steered.PendingInterventions) != 1 || steered.PendingInterventions[0].Instruction != "Only the north wing." ||
			steered.PendingInterventions[0].Actor.Type != "agent" {
			t.Fatalf("child interventions = %#v", steered.PendingInterventions)
		}
		c.work(10)
		done := c.MustRun(parent.ID)
		c.MustStatus(done, models.AgentRunStatusCompleted)
		if got := done.Output[conversationrun.FinalOutputSummaryKey]; got != "4 boxes in the north wing." {
			t.Fatalf("parent answer = %v", got)
		}
		if n := len(c.children(parent.ID)); n != 1 {
			t.Fatalf("steering a running subagent started %d Runs", n)
		}
	})
}

func TestLoopStopSubagentEndsItWithoutANote(t *testing.T) {
	looptest.EachStore(t, "subagent_stop", func(t *testing.T, store models.KernelStore) {
		c := newChildFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "subagent", When: isForkBranch, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Say("still watching")
			}},
			&looptest.ModelRule{Name: "start", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Watching the queue.", childStartCall(true, "watch the queue"))
			}},
			&looptest.ModelRule{Name: "stop", Times: 1, When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Act("Not needed after all.", looptest.Call(string(models.ControlToolStopSubagents), map[string]any{"ids": []string{"s1"}}))
			}},
			&looptest.ModelRule{Name: "answer", When: notParent, Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.Say("Stopped watching.")
			}},
		))
		parent := c.StartRun(models.RunKindAgentWork, "Watch the queue")
		c.work(6)
		done := c.MustRun(parent.ID)
		c.MustStatus(done, models.AgentRunStatusCompleted)
		children := c.children(parent.ID)
		if len(children) != 1 || c.MustRun(children[0].ID).Status != models.AgentRunStatusCanceled {
			t.Fatalf("children = %v", children)
		}
		stop := c.toolEntry(parent.ID, string(models.ControlToolStopSubagents))
		if stop.State != models.ToolEntryCompleted || !strings.Contains(childResultText(stop), `"status":"stopped"`) {
			t.Fatalf("stop entry = %s %s", stop.State, childResultText(stop))
		}
		if notes := c.notesOf(parent.ID, models.NoteSubagent); len(notes) != 0 {
			t.Fatalf("a stopped subagent was reported again: %v", notes[0].Note.Text)
		}
		meta := childStartMeta(t, c, parent.ID)
		group, err := c.Raw.(models.RunDependencyStore).GetRunDependencyGroup(c.Ctx, c.Scope, meta)
		if err != nil || group == nil || !group.Detached || group.Status == models.RunDependencyGroupWaiting {
			t.Fatalf("group = %#v, %v", group, err)
		}
	})
}

// childStartMeta is the dependency group of the Run's start_subagents call.
func childStartMeta(t *testing.T, c *childFixture, runID string) string {
	t.Helper()
	var meta struct {
		GroupID string `json:"groupId"`
	}
	entry := c.toolEntry(runID, string(models.ControlToolStartSubagents))
	if err := json.Unmarshal(entry.Tool.Control, &meta); err != nil || meta.GroupID == "" {
		t.Fatalf("start meta = %s, %v", entry.Tool.Control, err)
	}
	return meta.GroupID
}
