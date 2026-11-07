package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type fixedOutreachTurnLifecycle struct {
	thread          *OutreachThread
	reconciled      ReconcileOutreachActionRequest
	reconcileStatus OutreachMessageStatus
}

func (f *fixedOutreachTurnLifecycle) GetOutreachThread(context.Context, Scope, string) (*OutreachThread, error) {
	return cloneOutreachThread(f.thread), nil
}

func (f *fixedOutreachTurnLifecycle) ReconcileOutreachAction(_ context.Context, _ Scope, _ string, req ReconcileOutreachActionRequest) (*ReconcileOutreachActionResult, error) {
	f.reconciled = req
	if f.thread == nil {
		return &ReconcileOutreachActionResult{}, nil
	}
	thread := cloneOutreachThread(f.thread)
	if req.Receipt != nil {
		thread.Messages[0].Status = OutreachMessageDelivered
		thread.Messages[0].Receipt = cloneOutreachReceipt(req.Receipt)
	} else if f.reconcileStatus != "" {
		thread.Messages[0].Status = f.reconcileStatus
	}
	return &ReconcileOutreachActionResult{Thread: thread}, nil
}

func outreachProducerFixture(status OutreachMessageStatus, actionCallID string) (*fixedOutreachTurnLifecycle, *AgentRun) {
	scope := Scope{Kind: "tenant", ID: "research"}
	disclosure := "Disclosure: I work on OpenSeal."
	body := "What made installation difficult? " + disclosure
	lifecycle := &fixedOutreachTurnLifecycle{thread: &OutreachThread{
		ID: "thread-1", Scope: scope, ProjectID: "project-1", SourceObservationID: "observation-1", MonitorID: "monitor-1",
		TargetURI: "https://forum.example/thread/1", Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"},
		AssignedAgentID: "research-agent", Status: OutreachThreadOpen,
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Status: status, Body: body, ActionCallID: actionCallID,
			Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "1.0.0", Action: "reply", BindingID: "forum-account", BindingRevision: 2,
				Arguments: map[string]interface{}{"url": "https://forum.example/thread/1", "body": body}}}},
	}}
	run := &AgentRun{ID: "run-1", Scope: scope, Owner: lifecycle.thread.Owner, AssignedAgentID: "research-agent", Context: map[string]interface{}{
		OutreachInvocationContextKey: map[string]interface{}{"threadId": "thread-1", "messageId": "message-1"},
	}}
	return lifecycle, run
}

var outreachProducerAction = capability.ModelAction{Name: "forum.reply", SkillID: "forum", Version: "1.0.0", Action: "reply", BindingID: "forum-account", BindingRevision: 2}

func outreachCall(status ActionCallStatus, output map[string]interface{}) *ActionCall {
	return &ActionCall{ID: "action-1", RunID: "run-1", SkillID: "forum", SkillVersion: "1.0.0", Action: "reply",
		BindingID: "forum-account", BindingRevision: 2, Status: status, Output: output}
}

func TestOutreachProducerUsesReviewedArgumentsOnly(t *testing.T) {
	lifecycle, run := outreachProducerFixture(OutreachMessageDraft, "")
	calls := fixedActionCalls{}
	producer, err := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, calls)
	if err != nil {
		t.Fatal(err)
	}
	if producer.Kind() != StepProducerDeterministic {
		t.Fatalf("kind = %s", producer.Kind())
	}
	first, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-1", Sequence: 1}, Path: &AgentSessionPath{}})
	if err != nil {
		t.Fatal(err)
	}
	requireValidDeterministicOutput(t, first)
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].ID != outreachToolCallID || first.ToolCalls[0].Name != "forum.reply" {
		t.Fatalf("first = %#v", first)
	}
	directive := first.ToolCalls[0].Directive
	if directive == nil || directive.IdempotencyKey != "outreach:thread-1:message-1" || directive.BindingID != "forum-account" ||
		directive.BindingRevision != 2 || directive.ExpectedSkillVersion != "1.0.0" || directive.Summary != "Deliver reviewed outreach message" {
		t.Fatalf("directive = %#v", directive)
	}
	input, err := ParseGovernedToolInput(outreachProducerAction, first.ToolCalls[0].Arguments, &governedInputDirective{Summary: directive.Summary})
	reviewed := lifecycle.thread.Messages[0].Capability.Arguments
	if err != nil || len(input.Arguments) != 2 || input.Arguments["body"] != reviewed["body"] || input.Arguments["url"] != lifecycle.thread.TargetURI {
		t.Fatalf("reviewed arguments = %#v err = %v", input.Arguments, err)
	}

	deliveredAt := time.Date(2026, 7, 13, 17, 0, 0, 0, time.UTC)
	calls["action-1"] = outreachCall(ActionCallStatusSucceeded, map[string]interface{}{"outreachReceipt": map[string]interface{}{
		"provider": "forum", "externalId": "reply-1", "externalUri": "https://forum.example/thread/1/reply/1", "deliveredAt": deliveredAt.Format(time.RFC3339Nano),
	}})
	lifecycle.thread.Messages[0].Status, lifecycle.thread.Messages[0].ActionCallID = OutreachMessagePendingApproval, "action-1"
	second, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-2", Sequence: 2},
		Path: producerToolPath("run-1", outreachToolCallID, ToolEntryCompleted, "action-1")})
	if err != nil {
		t.Fatal(err)
	}
	requireValidDeterministicOutput(t, second)
	if second.Final == nil || second.Final.Status != AgentRunStatusCompleted || lifecycle.reconciled.ActionCallID != "action-1" ||
		lifecycle.reconciled.Receipt == nil || lifecycle.reconciled.Receipt.ExternalID != "reply-1" ||
		second.Final.Output["outreachThreadId"] != "thread-1" || second.Final.Output["outreachMessageId"] != "message-1" {
		t.Fatalf("second = %#v reconciled = %#v", second, lifecycle.reconciled)
	}
}

func TestOutreachProducerRejectsMissingReceipt(t *testing.T) {
	lifecycle, run := outreachProducerFixture(OutreachMessagePendingApproval, "action-1")
	calls := fixedActionCalls{"action-1": outreachCall(ActionCallStatusSucceeded, map[string]interface{}{"ok": true})}
	producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, calls)
	if _, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-2"},
		Path: producerToolPath("run-1", outreachToolCallID, ToolEntryCompleted, "action-1")}); err == nil {
		t.Fatal("a succeeded call without a provider receipt completed the outreach")
	}
}

func TestOutreachProducerReconcilesUnsuccessfulCalls(t *testing.T) {
	for _, tc := range []struct {
		call    ActionCallStatus
		message OutreachMessageStatus
		code    string
	}{
		{ActionCallStatusDenied, OutreachMessageDeclined, "outreach_delivery_declined"},
		{ActionCallStatusCanceled, OutreachMessageCanceled, "outreach_delivery_canceled"},
		{ActionCallStatusFailed, OutreachMessageFailed, "outreach_delivery_failed"},
	} {
		t.Run(string(tc.call), func(t *testing.T) {
			lifecycle, run := outreachProducerFixture(OutreachMessagePendingApproval, "action-1")
			lifecycle.reconcileStatus = tc.message
			calls := fixedActionCalls{"action-1": outreachCall(tc.call, nil)}
			producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, calls)
			out, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-2"},
				Path: producerToolPath("run-1", outreachToolCallID, ToolEntryError, "action-1")})
			if err != nil {
				t.Fatal(err)
			}
			requireValidDeterministicOutput(t, out)
			if out.Failure == nil || out.Failure.Code != tc.code || out.Failure.Explainable ||
				lifecycle.reconciled.ActionCallID != "action-1" || lifecycle.reconciled.Receipt != nil {
				t.Fatalf("out = %#v reconcile = %#v", out, lifecycle.reconciled)
			}
		})
	}
}

func TestOutreachProducerReconcilesActionLinkedOutsideTheRunSession(t *testing.T) {
	for _, terminal := range []OutreachMessageStatus{OutreachMessageDeclined, OutreachMessageCanceled} {
		t.Run(string(terminal), func(t *testing.T) {
			lifecycle, run := outreachProducerFixture(OutreachMessagePendingApproval, "action-1")
			lifecycle.reconcileStatus = terminal
			producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, fixedActionCalls{})
			out, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-1"}, Path: &AgentSessionPath{}})
			if err != nil {
				t.Fatal(err)
			}
			requireValidDeterministicOutput(t, out)
			if out.Failure == nil || len(out.ToolCalls) != 0 || lifecycle.reconciled.ActionCallID != "action-1" {
				t.Fatalf("terminal = %s out = %#v reconcile = %#v", terminal, out, lifecycle.reconciled)
			}
		})
	}
	// Still pending elsewhere: never proposed a second time.
	lifecycle, run := outreachProducerFixture(OutreachMessagePendingApproval, "action-1")
	producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, fixedActionCalls{})
	if out, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-1"}}); err == nil {
		t.Fatalf("pending linked message was proposed again: %#v", out)
	}
}

func TestOutreachProducerRejectsSmuggledInvocationAndUnboundCapability(t *testing.T) {
	lifecycle := &fixedOutreachTurnLifecycle{thread: &OutreachThread{ID: "thread-1", Scope: Scope{Kind: "tenant", ID: "one"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Status: OutreachThreadOpen,
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Status: OutreachMessageDraft, Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "2", Action: "reply"}}},
	}}
	producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{{Name: "forum.reply", SkillID: "forum", Version: "1", Action: "reply"}}, fixedActionCalls{})
	run := &AgentRun{ID: "run", Scope: lifecycle.thread.Scope, Owner: lifecycle.thread.Owner, AssignedAgentID: "agent", Context: map[string]interface{}{
		OutreachInvocationContextKey: map[string]interface{}{"threadId": "thread-1", "messageId": "message-1", "body": "smuggled"},
	}}
	if _, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step"}}); err == nil {
		t.Fatal("smuggled outreach invocation was accepted")
	}
	delete(run.Context[OutreachInvocationContextKey].(map[string]interface{}), "body")
	if _, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step"}}); err == nil {
		t.Fatal("unbound outreach capability was accepted")
	}
	run.AssignedAgentID = "other-agent"
	lifecycle.thread.Messages[0].Capability.SkillVersion = "1"
	if _, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step"}}); err == nil {
		t.Fatal("outreach for another Agent was accepted")
	}
}

func TestOutreachActionProposalObserverProjectsCommittedAction(t *testing.T) {
	lifecycle := &fixedOutreachTurnLifecycle{}
	observer, err := NewOutreachActionProposalObserver(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{Scope: Scope{Kind: "tenant", ID: "one"}, Context: map[string]interface{}{
		OutreachInvocationContextKey: map[string]interface{}{"threadId": "thread-1", "messageId": "message-1"},
	}}
	if err := observer.ObserveActionProposal(t.Context(), run, nil, &ActionProposalResult{Call: &ActionCall{ID: "action-1"}}); err != nil {
		t.Fatal(err)
	}
	if lifecycle.reconciled.ActionCallID != "action-1" || lifecycle.reconciled.MessageID != "message-1" {
		t.Fatalf("projection=%#v", lifecycle.reconciled)
	}
}
