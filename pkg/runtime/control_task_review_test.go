package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// taskReviewHost answers review calls with scripted texts, in order.
type taskReviewHost struct {
	replies  []string
	requests []ModelStepRequest
}

func (h *taskReviewHost) ExecuteModelStep(_ context.Context, req ModelStepRequest) (ModelStepResult, error) {
	h.requests = append(h.requests, req)
	text := `{"decision":"approve","reason":"default"}`
	if len(h.replies) > 0 {
		text, h.replies = h.replies[0], h.replies[1:]
	}
	return ModelStepResult{
		APIVersion: ModelStepAPIVersion, StepID: req.StepID,
		Assistant: AssistantEntry{Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopEnd, Text: text},
		Model:     ModelStepModelInfo{Provider: ModelStepProviderAnthropic, Model: "claude-x"},
	}, nil
}

func (h *taskReviewHost) ExecuteHostTool(context.Context, HostToolRequest) (HostToolResult, error) {
	return HostToolResult{}, nil
}

const taskReviewGoal = "Compile the full list of open invoices with totals."

// newTaskReviewFixture makes the fixture's Run the work Run of an independent
// conversation task.
func newTaskReviewFixture(t *testing.T, calls ...LoopToolCall) *controlFixture {
	t.Helper()
	f := newFinalFixture(t, calls...)
	f.run.Kind = RunKindAgentWork
	f.run.Context = map[string]interface{}{ConversationTaskContextKey: "task-1"}
	task := &ConversationTask{
		ID: "task-1", Mode: ConversationTaskModeIndependent, Scope: f.run.Scope, Owner: f.run.Owner,
		ConversationID: "conv-1", SourceMessageID: "msg-1", ThreadRootID: "msg-1", SourceRunID: "source-run",
		TargetAgentID: "agent-1", WorkRunID: f.run.ID, TaskKey: "key-1", Goal: taskReviewGoal, CreatedAt: f.now,
	}
	putStoredConversationTask(f.t, f.store, task, f.run)
	return f
}

func (f *controlFixture) settleReviewed(callID string, host StepHost) ControlResult {
	f.t.Helper()
	cc := f.context(callID, ControlAuthority{})
	cc.Binding.Host = host
	result, err := f.exec.Settle(f.ctx, cc)
	if err != nil {
		f.t.Fatalf("settle %s: %v", callID, err)
	}
	return result
}

func TestTaskReviewApproveDelivers(t *testing.T) {
	f := newTaskReviewFixture(t, LoopToolCall{ID: "lookup", Name: "invoices_list", Arguments: json.RawMessage(`{}`)},
		finalCall("final", `{"answer":"Invoices: A 10, B 20. Total 30."}`))
	// A finished tool call of this Run is review evidence.
	lookup := f.entries["lookup"]
	if _, err := f.store.TransitionToolEntry(f.ctx, ToolEntryTransitionRequest{
		Scope: f.run.Scope, EntryID: lookup.ID, RunID: f.run.ID, Lease: AgentRunLeaseGuard{WorkerID: controlTestWorker, Now: f.now},
		From: ToolEntryPending, To: ToolEntryCompleted, Now: f.now,
		Result: &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{{Type: ContentPartText, Text: "A 10\nB 20\n" + strings.Repeat("x", 4096)}}},
	}); err != nil {
		t.Fatalf("complete lookup: %v", err)
	}
	host := &taskReviewHost{replies: []string{`{"decision":"approve","reason":"Both invoices and the total are listed."}`}}
	final := requireFinal(t, f.settleReviewed("final", host))
	if final.Answer != "Invoices: A 10, B 20. Total 30." || final.ToolEntryID != f.entries["final"].ID {
		t.Fatalf("final = %#v", final)
	}
	if len(host.requests) != 1 {
		t.Fatalf("review calls = %d", len(host.requests))
	}
	req := host.requests[0]
	if req.Purpose != ModelStepPurposeAcknowledgement || len(req.Tools) != 0 || req.SessionID != "" || req.Validate() != nil ||
		len(req.System.AgentInstructions) != 1 || req.System.AgentInstructions[0] != taskCompletionReviewInstructions {
		t.Fatalf("request = %#v", req)
	}
	var input taskReviewInput
	if err := json.Unmarshal([]byte(req.Entries[0].User.Content[0].Text), &input); err != nil {
		t.Fatalf("input: %v", err)
	}
	if input.Goal != taskReviewGoal || input.CandidateAnswer != final.Answer || len(input.ToolResults) != 1 ||
		input.ToolResults[0].Tool != "invoices_list" || input.ToolResults[0].Status != string(ToolResultSucceeded) ||
		len(input.ToolResults[0].Content) > taskReviewMaxToolResultBytes || !strings.HasPrefix(input.ToolResults[0].Content, "A 10\nB 20") {
		t.Fatalf("input = %#v", input)
	}
}

func TestTaskReviewReviseOnceThenRejectFails(t *testing.T) {
	f := newTaskReviewFixture(t, finalCall("first", `{"answer":"I will compile the list."}`),
		finalCall("second", `{"answer":"Still working on it."}`))
	host := &taskReviewHost{replies: []string{
		`{"decision":"revise","reason":"No invoices are listed."}`,
		`{"decision":"revise","reason":"The answer is still a plan."}`,
	}}
	value := requireRejected(t, f, f.settleReviewed("first", host), "first", taskCompletionReviseCode)
	if value["message"] != "The task result needs more work: No invoices are listed." {
		t.Fatalf("rejection = %#v", value)
	}
	result := f.settleReviewed("second", host)
	if result.Kind != ControlFailed || result.Failure == nil || result.Failure.Code != taskCompletionRejectedCode ||
		result.Failure.Kind != RunFailureKindTaskCompletion || !strings.Contains(result.Failure.Message, "still a plan") ||
		result.Entry == nil || result.Entry.State != ToolEntryError {
		t.Fatalf("second = %#v", result)
	}
	meta, err := DecodeControlEntryMeta(f.stored("second"))
	if err != nil || meta == nil || meta.ErrorCode != taskCompletionRejectedCode {
		t.Fatalf("meta = %#v, %v", meta, err)
	}
}

func TestTaskReviewInvalidVerdictAccepts(t *testing.T) {
	f := newTaskReviewFixture(t, finalCall("final", `{"answer":"Invoices: A 10."}`))
	host := &taskReviewHost{replies: []string{"Looks fine to me.", `{"decision":"maybe","reason":"x"}`}}
	final := requireFinal(t, f.settleReviewed("final", host))
	if final.Answer != "Invoices: A 10." || len(host.requests) != 2 ||
		host.requests[0].StepID != host.requests[1].StepID || host.requests[1].Attempt != 1 {
		t.Fatalf("final = %#v, requests = %d", final, len(host.requests))
	}
}

func TestTaskReviewInvalidThenValidVerdictCounts(t *testing.T) {
	f := newTaskReviewFixture(t, finalCall("final", `{"answer":"Plan."}`))
	host := &taskReviewHost{replies: []string{`{"decision":"revise","reason":"x","extra":1}`, `{"decision":"revise","reason":"Only a plan."}`}}
	requireRejected(t, f, f.settleReviewed("final", host), "final", taskCompletionReviseCode)
}

func TestTaskReviewSkipsRunsThatAreNotTaskWork(t *testing.T) {
	host := &taskReviewHost{replies: []string{`{"decision":"revise","reason":"no"}`}}
	// An ordinary Run.
	f := newFinalFixture(t, finalCall("final", `{"answer":"done"}`))
	requireFinal(t, f.settleReviewed("final", host))
	// A fork child of task work carries the hint but is not the task's work Run.
	g := newFinalFixture(t, finalCall("final", `{"answer":"part done"}`))
	g.run.Kind = RunKindAgentWork
	g.run.Context = map[string]interface{}{ConversationTaskContextKey: "task-1"}
	requireFinal(t, g.settleReviewed("final", host))
	if len(host.requests) != 0 {
		t.Fatalf("review calls = %d", len(host.requests))
	}
}

func TestTaskReviewPlainTextRevise(t *testing.T) {
	f := newTaskReviewFixture(t)
	host := &taskReviewHost{replies: []string{`{"decision":"revise","reason":"Nothing delivered."}`}}
	cc := f.plainContext("step-2", 2, ControlAuthority{})
	cc.Binding.Host = host
	result, err := f.exec.PlainTextFinal(f.ctx, cc, "I'll get to it.")
	if err != nil {
		t.Fatal(err)
	}
	requireRejectionNote(t, result, "step-2", taskCompletionReviseCode)
	if result.Kind != ControlContinue {
		t.Fatalf("result = %#v", result)
	}
}
