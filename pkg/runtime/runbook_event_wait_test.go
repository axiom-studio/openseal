package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func runbookEventWaitFixture(t *testing.T, handleTimeout bool) (*RunbookTurnRunner, *AgentRun, time.Time) {
	t.Helper()
	wait := &runbook.WaitStep{Match: &runbook.EventMatch{
		Type: "conversation.message.received", Source: *runbookRef("/input/source"), Subject: *runbookRef("/input/subject"),
		Attributes: map[string]runbook.Value{"sender": *runbookRef("/input/sender"), "sequence": runbookLiteral(3)}, TimeoutDuration: time.Hour,
	}, ResultPath: "/state/reply", Next: "matched"}
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "follow-up", Version: "1", Name: "Follow-up", Entrypoints: map[string]string{"manual": "wait"}, Steps: map[string]runbook.Step{
		"wait":    {Kind: runbook.StepWait, Wait: wait},
		"matched": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"reply": *runbookRef("/state/reply")}}},
	}}
	if handleTimeout {
		wait.TimeoutNext = "timed-out"
		definition.Steps["timed-out"] = runbook.Step{Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{"timedOut": runbookLiteral(true), "reply": *runbookRef("/state/reply")}}}
	}
	runner, err := NewRunbookTurnRunner(definition, "manual")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	run := &AgentRun{ID: "follow-up-run", CreatedAt: now.Add(-time.Minute), Context: map[string]interface{}{"source": "binding:connection", "subject": "thread:123", "sender": "sender-1"}}
	return runner, run, now
}

func armRunbookEventWait(t *testing.T, runner *RunbookTurnRunner, run *AgentRun) *TurnOutcome {
	t.Helper()
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "first", Sequence: 1}})
	if err != nil || outcome.NextRunStatus != AgentRunStatusWaitingForEvent || outcome.WakeCondition == nil || outcome.WakeCondition.EventWait == nil {
		t.Fatalf("arm outcome=%#v err=%v", outcome, err)
	}
	run.Checkpoint = outcome.ContinuationCheckpoint
	return outcome
}

func eventForRunbookWait(spec *RunEventWaitSpec, at time.Time) map[string]interface{} {
	return eventContext(EventEnvelope{ID: "reply-event", Type: spec.Type, Source: spec.Source, Subject: spec.Subject, OccurredAt: at, Attributes: cloneMap(spec.Attributes), Payload: map[string]interface{}{"text": "Here is my update."}})
}

func TestRunbookEventWaitConsumesEarlyEventWithFrozenDynamicSelectors(t *testing.T) {
	runner, run, now := runbookEventWaitFixture(t, true)
	first := armRunbookEventWait(t, runner, run)
	spec := first.WakeCondition.EventWait
	if spec.Source != "binding:connection" || spec.Subject != "thread:123" || spec.Attributes["sender"] != "sender-1" || !spec.After.Equal(run.CreatedAt) || !spec.Deadline.Equal(now.Add(time.Hour)) {
		t.Fatalf("selector=%#v", spec)
	}
	// The reply arrived before execution entered the wait, while its preceding
	// action was running. It must remain eligible within the saved window.
	event := eventForRunbookWait(spec, now.Add(-30*time.Second))
	run.Checkpoint[runEventWaitCheckpointKey] = map[string]interface{}{"key": spec.Key, "status": string(RunEventWaitMatched), "event": event}
	run.Checkpoint["input"].(map[string]interface{})["subject"] = "different-thread"
	encoded, err := json.Marshal(run.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &run.Checkpoint); err != nil {
		t.Fatal(err)
	}
	second, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "second", Sequence: 2}})
	if err != nil {
		t.Fatal(err)
	}
	actualJSON, _ := json.Marshal(second.RunOutput["reply"])
	expectedJSON, _ := json.Marshal(event)
	if second.NextRunStatus != AgentRunStatusCompleted || string(actualJSON) != string(expectedJSON) {
		t.Fatalf("resume outcome=%#v err=%v", second, err)
	}
	if _, present := second.ContinuationCheckpoint[runEventWaitCheckpointKey]; present {
		t.Fatal("durable result was not consumed")
	}
	state, err := decodeRunbookState(second.ContinuationCheckpoint)
	if err != nil || state.WaitKey != "" || state.WaitSpec != nil || state.Waiting != "" {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	trace, err := RunbookTraceFromCheckpoint(second.ContinuationCheckpoint)
	if err != nil || len(trace.Entries) != 2 || trace.Entries[0].Status != RunbookStepTraceSucceeded || !reflect.DeepEqual(trace.Entries[0].InputRefs, []string{"/input/sender", "/input/source", "/input/subject"}) || !reflect.DeepEqual(trace.Entries[0].OutputRefs, []string{"/state/reply"}) {
		t.Fatalf("trace=%#v err=%v", trace, err)
	}
}

func TestRunbookEventWaitContinuationSharesKernelScalarSemantics(t *testing.T) {
	for _, item := range []struct {
		name     string
		expected interface{}
		literal  bool
		value    interface{}
		matches  bool
	}{
		{name: "decimal number", value: json.Number("3.0"), matches: true},
		{name: "exponent number", value: json.Number("3e0"), matches: true},
		{name: "integer", value: int64(3), matches: true},
		{name: "float", value: float64(3), matches: true},
		{name: "exact large integer", expected: json.Number("9007199254740993"), value: json.Number("9007199254740993.0"), matches: true},
		{name: "exact large literal", expected: json.Number("9007199254740993"), literal: true, value: json.Number("9007199254740993e0"), matches: true},
		{name: "numeric string", value: "3", matches: false},
		{name: "boolean", value: true, matches: false},
		{name: "different number", value: json.Number("3.1"), matches: false},
	} {
		t.Run(item.name, func(t *testing.T) {
			runner, run, now := runbookEventWaitFixture(t, true)
			run.Scope = Scope{Kind: "tenant", ID: "scalar-tests"}
			run.Context["source"] = "connector:verified"
			if item.expected != nil {
				if item.literal {
					runner.definition.Steps["wait"].Wait.Match.Attributes["sequence"] = runbookLiteral(item.expected)
				} else {
					runner.definition.Steps["wait"].Wait.Match.Attributes["sequence"] = *runbookRef("/input/sequence")
					run.Context["sequence"] = item.expected
				}
			}
			first := armRunbookEventWait(t, runner, run)
			run.Status = AgentRunStatusWaitingForEvent
			run.WakeCondition = first.WakeCondition
			// Exercise persisted checkpoint decoding before event resolution.
			run = cloneAgentRun(run)
			wait, err := runEventWaitForRun(run)
			if err != nil {
				t.Fatal(err)
			}
			attributes := cloneMap(wait.Spec.Attributes)
			attributes["sequence"] = item.value
			receipt := &RunEventReceipt{ReceivedAt: now, Event: EventEnvelope{
				ID: "scalar-event", Scope: run.Scope, Type: wait.Spec.Type, Source: wait.Spec.Source, Subject: wait.Spec.Subject,
				OccurredAt: now, Attributes: attributes, Payload: map[string]interface{}{"text": "An update."},
			}}
			if got := runEventWaitMatches(wait, receipt); got != item.matches {
				t.Fatalf("kernel match=%t want=%t", got, item.matches)
			}
			if item.matches {
				// Exercise the same durable result construction used by every
				// store, then resume the Runbook rather than testing its helper.
				resolved, _, _, err := prepareRunEventWaitResolution(run, wait, receipt, now)
				if err != nil {
					t.Fatal(err)
				}
				run = resolved
				run = cloneAgentRun(run)
			} else {
				// Even a malformed continuation result must keep JSON scalar
				// types separate, matching the kernel's rejection above.
				run.Checkpoint[runEventWaitCheckpointKey] = map[string]interface{}{
					"key": wait.Spec.Key, "status": string(RunEventWaitMatched), "event": eventContext(receipt.Event),
				}
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "scalar-resume", Sequence: 2}})
			if err != nil {
				t.Fatal(err)
			}
			if item.matches {
				if outcome.NextRunStatus != AgentRunStatusCompleted {
					t.Fatalf("kernel-matched numeric equivalent failed continuation: %#v", outcome)
				}
			} else if outcome.NextRunStatus != AgentRunStatusFailed || !strings.Contains(outcome.RunError, "does not match") {
				t.Fatalf("different scalar type/value satisfied continuation: %#v", outcome)
			}
		})
	}
}

func TestRunbookEventWaitDoesNotAdvanceOnUnrelatedResume(t *testing.T) {
	runner, run, now := runbookEventWaitFixture(t, true)
	first := armRunbookEventWait(t, runner, run)
	runner.now = func() time.Time { return now.Add(10 * time.Minute) }
	for _, staleResult := range []interface{}{nil, map[string]interface{}{"key": "different-visit", "status": string(RunEventWaitMatched)}} {
		run.Checkpoint[runEventWaitCheckpointKey] = staleResult
		outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "manual-resume", Sequence: 2}})
		if err != nil || outcome.NextRunStatus != AgentRunStatusWaitingForEvent || !sameRunEventWaitSpec(outcome.WakeCondition.EventWait, first.WakeCondition.EventWait) {
			t.Fatalf("unrelated resume outcome=%#v err=%v", outcome, err)
		}
		trace, err := RunbookTraceFromCheckpoint(outcome.ContinuationCheckpoint)
		if err != nil || len(trace.Entries) != 1 || trace.Entries[0].Status != RunbookStepTraceWaiting {
			t.Fatalf("trace=%#v err=%v", trace, err)
		}
	}
}

func TestRunbookEventWaitRoutesTimeoutOrFailsWithoutHandler(t *testing.T) {
	for _, handle := range []bool{true, false} {
		t.Run(map[bool]string{true: "handler", false: "no handler"}[handle], func(t *testing.T) {
			runner, run, now := runbookEventWaitFixture(t, handle)
			first := armRunbookEventWait(t, runner, run)
			runner.now = func() time.Time { return now.Add(time.Hour) }
			run.Checkpoint["state"] = map[string]interface{}{"reply": "stale prior iteration"}
			run.Checkpoint[runEventWaitCheckpointKey] = map[string]interface{}{"key": first.WakeCondition.EventWait.Key, "status": string(RunEventWaitTimedOut)}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "timeout", Sequence: 2}})
			if err != nil {
				t.Fatal(err)
			}
			if handle {
				if outcome.NextRunStatus != AgentRunStatusCompleted || outcome.RunOutput["timedOut"] != true || outcome.RunOutput["reply"] != nil {
					t.Fatalf("timeout outcome=%#v", outcome)
				}
			} else if outcome.NextRunStatus != AgentRunStatusFailed || !strings.Contains(outcome.RunError, "deadline elapsed") {
				t.Fatalf("unhandled timeout outcome=%#v", outcome)
			}
		})
	}
}

func TestRunbookEventWaitRejectsIncorrectDurablePayload(t *testing.T) {
	runner, run, now := runbookEventWaitFixture(t, true)
	first := armRunbookEventWait(t, runner, run)
	event := eventForRunbookWait(first.WakeCondition.EventWait, now)
	event["subject"] = "unrelated-thread"
	run.Checkpoint[runEventWaitCheckpointKey] = map[string]interface{}{"key": first.WakeCondition.EventWait.Key, "status": string(RunEventWaitMatched), "event": event}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "wrong-event", Sequence: 2}})
	if err != nil || outcome.NextRunStatus != AgentRunStatusFailed || !strings.Contains(outcome.RunError, "does not match") {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
}

func TestRunbookEventWaitRejectsDynamicNonScalarAndExpiredWindows(t *testing.T) {
	for _, name := range []string{"object attribute", "expired default window"} {
		t.Run(name, func(t *testing.T) {
			runner, run, now := runbookEventWaitFixture(t, true)
			if name == "object attribute" {
				run.Context["sender"] = map[string]interface{}{"id": "sender-1"}
			} else {
				run.CreatedAt = now.Add(-31 * 24 * time.Hour)
			}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "arm", Sequence: 1}})
			if err != nil || outcome.NextRunStatus != AgentRunStatusFailed || outcome.RunError == "" || outcome.WakeCondition != nil {
				t.Fatalf("outcome=%#v err=%v", outcome, err)
			}
		})
	}
}

func TestRunbookEventWaitUsesDistinctIdentityAcrossLoopVisits(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	definition := &runbook.Definition{APIVersion: runbook.APIVersion, ID: "loop-follow-up", Version: "1", Name: "Loop follow-up", Entrypoints: map[string]string{"manual": "loop"}, Steps: map[string]runbook.Step{
		"loop":  {Kind: runbook.StepForEach, ForEach: &runbook.ForEachStep{Items: *runbookRef("/input/people"), ItemName: "person", MaxIterations: 2, Body: "wait", Next: "done"}},
		"wait":  {Kind: runbook.StepWait, Wait: &runbook.WaitStep{Match: &runbook.EventMatch{Type: "conversation.message.received", Source: runbookLiteral("binding:connection"), Subject: runbookLiteral("thread:123"), Attributes: map[string]runbook.Value{"sender": *runbookRef("/loop/person")}, TimeoutDuration: time.Hour}, Next: "again"}},
		"again": {Kind: runbook.StepLoopReturn, LoopReturn: &runbook.LoopReturnStep{ForEach: "loop"}}, "done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
	}}
	runner, err := NewRunbookTurnRunner(definition, "manual")
	if err != nil {
		t.Fatal(err)
	}
	runner.now = func() time.Time { return now }
	run := &AgentRun{ID: "loop-run", CreatedAt: now, Context: map[string]interface{}{"people": []interface{}{"person-1", "person-2"}}}
	first := armRunbookEventWait(t, runner, run)
	spec := first.WakeCondition.EventWait
	run.Checkpoint[runEventWaitCheckpointKey] = map[string]interface{}{"key": spec.Key, "status": string(RunEventWaitMatched), "event": eventForRunbookWait(spec, now)}
	second, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "next-person", Sequence: 2}})
	if err != nil || second.NextRunStatus != AgentRunStatusWaitingForEvent || second.WakeCondition.EventWait.Key == spec.Key || second.WakeCondition.EventWait.Attributes["sender"] != "person-2" {
		t.Fatalf("second visit outcome=%#v err=%v", second, err)
	}
	if _, present := second.ContinuationCheckpoint[runEventWaitCheckpointKey]; present {
		t.Fatal("prior visit's result survived into the next wait")
	}
}
