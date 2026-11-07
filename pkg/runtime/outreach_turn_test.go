package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
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
	input, err := ParseGovernedToolInput(outreachProducerAction, first.ToolCalls[0].Arguments, governedDirectiveOf(directive))
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

// outreachServiceLifecycle is the production lifecycle shape: the real
// OutreachService behind the producer's lifecycle port.
type outreachServiceLifecycle struct{ service *OutreachService }

func (l outreachServiceLifecycle) GetOutreachThread(ctx context.Context, scope Scope, id string) (*OutreachThread, error) {
	return l.service.Get(ctx, scope, id)
}

func (l outreachServiceLifecycle) ReconcileOutreachAction(ctx context.Context, scope Scope, id string, req ReconcileOutreachActionRequest) (*ReconcileOutreachActionResult, error) {
	return l.service.ReconcileAction(ctx, scope, id, req)
}

// The producer's directive must carry the thread's source observation as
// evidence: OutreachService.LinkAction refuses a call or approval without it.
func TestOutreachProducerCallCarriesSourceEvidenceThroughRealOutreachService(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "research"}
	project, runs := seedExecutableMonitorProject(t, store, scope)
	sources := NewSourceMonitorService(store, store, store, store)
	ingested, err := sources.Ingest(t.Context(), sourceObservationRequest(scope, project.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-7", "Setup is confusing"))
	if err != nil {
		t.Fatal(err)
	}
	actions := &fixedOutreachActionReader{}
	service := NewOutreachService(store, store, store, actions)
	disclosure := "Disclosure: I work on OpenSeal and am seeking product feedback."
	body := "What part of setup was hardest for you? " + disclosure
	arguments := map[string]interface{}{"threadUrl": ingested.Observation.SourceURI, "message": body}
	created, _, err := service.Create(t.Context(), CreateOutreachThreadRequest{IdempotencyKey: "draft-1", Actor: ActivityActor{Type: "user", ID: "operator"}, Thread: &OutreachThread{
		Scope: scope, ProjectID: project.ID, SourceObservationID: ingested.Observation.ID, MonitorID: "monitor-a",
		StableSourceID: "thread-7", TargetURI: ingested.Observation.SourceURI, Owner: project.Owner, AssignedAgentID: "researcher",
		SourcePolicyRef: "approved-forums", ApprovalPolicyRef: "review-outreach",
		Identity: OutreachIdentity{ProfileRef: "profile:openseal-research", DisplayName: "OpenSeal Research", Affiliation: "OpenSeal", Disclosure: disclosure},
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Intent: OutreachIntentRequestFeedback, Body: body, Status: OutreachMessageDraft,
			Capability: &OutreachCapability{SkillID: "forum-outreach", SkillVersion: "1.0.0", Action: "reply", TargetArgument: "threadUrl", BodyArgument: "message", Arguments: arguments}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := outreachServiceLifecycle{service: service}
	action := capability.ModelAction{Name: "forum-outreach.reply", SkillID: "forum-outreach", Version: "1.0.0", Action: "reply", SideEffect: capability.SideEffectExternal}
	calls := fixedActionCalls{}
	producer, err := NewOutreachStepProducer(lifecycle, []capability.ModelAction{action}, calls)
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-outreach", Scope: scope, Owner: project.Owner, AssignedAgentID: "researcher", Context: map[string]interface{}{
		OutreachInvocationContextKey: map[string]interface{}{"threadId": created.ID, "messageId": "message-1"},
	}}
	first, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-1", Sequence: 1}, Path: &AgentSessionPath{}})
	if err != nil || len(first.ToolCalls) != 1 {
		t.Fatalf("first = %#v err = %v", first, err)
	}
	toolCall := first.ToolCalls[0]
	input, err := ParseGovernedToolInput(action, toolCall.Arguments, governedDirectiveOf(toolCall.Directive))
	if err != nil {
		t.Fatal(err)
	}
	if len(input.EvidenceRefs) != 1 || input.EvidenceRefs[0] != ingested.Observation.ID {
		t.Fatalf("evidence refs = %#v", input.EvidenceRefs)
	}

	// The coordinator copies the input's evidence onto the call and approval.
	call := &ActionCall{ID: "action-reply", Scope: scope, RunID: run.ID, DeploymentID: "researcher", SkillID: "forum-outreach", SkillVersion: "1.0.0", Action: "reply",
		Status: ActionCallStatusWaitingApproval, SideEffect: skill.SideEffectExternal, Arguments: input.Arguments,
		EvidenceRefs: input.EvidenceRefs, IdempotencyKey: toolCall.Directive.IdempotencyKey, ApprovalID: "approval-reply"}
	actions.call = call
	actions.approval = &ApprovalCheckpoint{ID: "approval-reply", Scope: scope, RunID: run.ID, ActionCallID: call.ID, EvidenceRefs: input.EvidenceRefs}
	observer, _ := NewOutreachActionProposalObserver(lifecycle)
	if err := observer.ObserveActionProposal(t.Context(), run, nil, &ActionProposalResult{Call: call}); err != nil {
		t.Fatalf("proposal projection: %v", err)
	}
	linked, _ := service.Get(t.Context(), scope, created.ID)
	if linked.Messages[0].Status != OutreachMessagePendingApproval || linked.Messages[0].ActionCallID != call.ID {
		t.Fatalf("linked = %#v", linked.Messages[0])
	}

	digest := sha256.Sum256([]byte(body))
	call.Status = ActionCallStatusSucceeded
	actions.approval.Status = ApprovalStatusApproved
	call.Output = map[string]interface{}{"outreachReceipt": map[string]interface{}{
		"provider": "forum", "externalId": "reply-99", "externalUri": "https://forum.example/threads/thread-7#reply-99",
		"digest": "sha256:" + hex.EncodeToString(digest[:]), "deliveredAt": time.Date(2026, 7, 13, 17, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}}
	calls[call.ID] = call
	second, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-2", Sequence: 2},
		Path: producerToolPath(run.ID, outreachToolCallID, ToolEntryCompleted, call.ID)})
	if err != nil {
		t.Fatal(err)
	}
	requireValidDeterministicOutput(t, second)
	delivered, _ := service.Get(t.Context(), scope, created.ID)
	if second.Final == nil || second.Final.Status != AgentRunStatusCompleted || delivered.Messages[0].Status != OutreachMessageDelivered {
		t.Fatalf("second = %#v message = %#v", second, delivered.Messages[0])
	}
}

func TestOutreachProducerFailsWhenRefusedBeforeAnyActionCall(t *testing.T) {
	for _, state := range []ToolEntryState{ToolEntryError, ToolEntryInterrupted} {
		t.Run(string(state), func(t *testing.T) {
			lifecycle, run := outreachProducerFixture(OutreachMessageDraft, "")
			producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, fixedActionCalls{})
			out, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-2"},
				Path: producerToolPath("run-1", outreachToolCallID, state, "")})
			if err != nil {
				t.Fatal(err)
			}
			requireValidDeterministicOutput(t, out)
			if out.Failure == nil || out.Failure.Code != "outreach_delivery_failed" || lifecycle.reconciled.ActionCallID != "" {
				t.Fatalf("out = %#v reconciled = %#v", out, lifecycle.reconciled)
			}
		})
	}
}

// An interrupted entry whose call is still in flight is not a delivery
// failure: the producer reports an error and never fails the Run.
func TestOutreachProducerDoesNotFailOnInterruptedEntryWithPendingCall(t *testing.T) {
	for _, status := range []ActionCallStatus{ActionCallStatusWaitingApproval, ActionCallStatusRunning} {
		t.Run(string(status), func(t *testing.T) {
			lifecycle, run := outreachProducerFixture(OutreachMessagePendingApproval, "action-1")
			calls := fixedActionCalls{"action-1": outreachCall(status, nil)}
			producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, calls)
			out, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-2"},
				Path: producerToolPath("run-1", outreachToolCallID, ToolEntryInterrupted, "action-1")})
			if err == nil || out.Failure != nil {
				t.Fatalf("pending call reported as %#v err = %v", out, err)
			}
		})
	}
	// A terminal call whose reconciliation leaves the message non-terminal is
	// an error too, not a reported failure.
	lifecycle, run := outreachProducerFixture(OutreachMessagePendingApproval, "action-1")
	calls := fixedActionCalls{"action-1": outreachCall(ActionCallStatusFailed, nil)}
	producer, _ := NewOutreachStepProducer(lifecycle, []capability.ModelAction{outreachProducerAction}, calls)
	if out, err := producer.Produce(t.Context(), StepInput{Run: run, Step: &AgentTurn{ID: "step-2"},
		Path: producerToolPath("run-1", outreachToolCallID, ToolEntryError, "action-1")}); err == nil || out.Failure != nil {
		t.Fatalf("non-terminal reconciliation reported as %#v err = %v", out, err)
	}
}
