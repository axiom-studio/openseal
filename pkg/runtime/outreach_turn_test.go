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

func TestOutreachTurnRunnerUsesOnlyReviewedArgumentsEvidenceAndReceipt(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "research"}
	disclosure := "Disclosure: I work on OpenSeal."
	body := "What made installation difficult? " + disclosure
	lifecycle := &fixedOutreachTurnLifecycle{thread: &OutreachThread{
		ID: "thread-1", Scope: scope, ProjectID: "project-1", SourceObservationID: "observation-1", MonitorID: "monitor-1",
		TargetURI: "https://forum.example/thread/1", Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"},
		AssignedAgentID: "research-agent", Status: OutreachThreadOpen,
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Status: OutreachMessageDraft, Body: body,
			Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "1.0.0", Action: "reply", Arguments: map[string]interface{}{
				"url": "https://forum.example/thread/1", "body": body,
			}}}},
	}}
	runner, err := NewOutreachTurnRunner(lifecycle, []capability.ModelAction{{Name: "forum.reply", SkillID: "forum", Version: "1.0.0", Action: "reply"}})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run-1", Scope: scope, Owner: lifecycle.thread.Owner, AssignedAgentID: "research-agent", Context: map[string]interface{}{
		OutreachInvocationContextKey: map[string]interface{}{"threadId": "thread-1", "messageId": "message-1"},
	}}
	first, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-1"}})
	if err != nil || len(first.ProposedActions) != 1 || first.ProposedActions[0].IdempotencyKey != "outreach:thread-1:message-1" ||
		len(first.ProposedActions[0].EvidenceRefs) != 1 || first.ProposedActions[0].EvidenceRefs[0] != "observation-1" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	arguments, err := resolveTurnActionInput(first.ContinuationCheckpoint, first.ProposedActions[0].InputRef)
	if err != nil || arguments["body"] != body || arguments["url"] != lifecycle.thread.TargetURI {
		t.Fatalf("reviewed arguments=%#v err=%v", arguments, err)
	}
	deliveredAt := time.Date(2026, 7, 13, 17, 0, 0, 0, time.UTC)
	run.Checkpoint = first.ContinuationCheckpoint
	run.Checkpoint["lastAction"] = map[string]interface{}{
		"actionCallId": "action-1", "skillId": "forum", "skillVersion": "1.0.0", "action": "reply", "status": "succeeded",
		"result": map[string]interface{}{"outreachReceipt": map[string]interface{}{
			"provider": "forum", "externalId": "reply-1", "externalUri": "https://forum.example/thread/1/reply/1", "deliveredAt": deliveredAt.Format(time.RFC3339Nano),
		}},
	}
	second, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-2"}})
	if err != nil || second.NextRunStatus != AgentRunStatusCompleted || lifecycle.reconciled.ActionCallID != "action-1" ||
		lifecycle.reconciled.Receipt == nil || lifecycle.reconciled.Receipt.ExternalID != "reply-1" || second.ContinuationCheckpoint["lastAction"] != nil {
		t.Fatalf("second=%#v reconciled=%#v err=%v", second, lifecycle.reconciled, err)
	}
}

func TestOutreachTurnRunnerReconcilesDeniedAndCanceledActionsWithoutProviderResult(t *testing.T) {
	for _, terminal := range []OutreachMessageStatus{OutreachMessageDeclined, OutreachMessageCanceled} {
		t.Run(string(terminal), func(t *testing.T) {
			scope := Scope{Kind: "tenant", ID: "research"}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "research-agent"}
			lifecycle := &fixedOutreachTurnLifecycle{reconcileStatus: terminal, thread: &OutreachThread{
				ID: "thread-1", Scope: scope, SourceObservationID: "observation-1", TargetURI: "https://forum.example/thread/1",
				Owner: owner, AssignedAgentID: "research-agent", Status: OutreachThreadOpen,
				Messages: []OutreachMessage{{
					ID: "message-1", Direction: OutreachMessageOutbound, Status: OutreachMessagePendingApproval, ActionCallID: "action-1",
					Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "1", Action: "reply"},
				}},
			}}
			runner, err := NewOutreachTurnRunner(lifecycle, []capability.ModelAction{{Name: "forum.reply", SkillID: "forum", Version: "1", Action: "reply"}})
			if err != nil {
				t.Fatal(err)
			}
			run := &AgentRun{ID: "run-1", Scope: scope, Owner: owner, AssignedAgentID: "research-agent", Context: map[string]interface{}{
				OutreachInvocationContextKey: map[string]interface{}{"threadId": "thread-1", "messageId": "message-1"},
			}, Checkpoint: map[string]interface{}{"outreachActionInputs": map[string]interface{}{"reviewed": map[string]interface{}{"body": "reviewed"}}}}
			outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-2"}})
			if err != nil || outcome.NextRunStatus != AgentRunStatusFailed || outcome.RunError == "" || lifecycle.reconciled.ActionCallID != "action-1" ||
				outcome.ContinuationCheckpoint["outreachActionInputs"] != nil {
				t.Fatalf("terminal=%s outcome=%#v reconcile=%#v err=%v", terminal, outcome, lifecycle.reconciled, err)
			}
		})
	}
}

func TestOutreachTurnRunnerRejectsSmuggledInvocationAndUnboundCapability(t *testing.T) {
	lifecycle := &fixedOutreachTurnLifecycle{thread: &OutreachThread{ID: "thread-1", Scope: Scope{Kind: "tenant", ID: "one"},
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Status: OutreachThreadOpen,
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Status: OutreachMessageDraft, Capability: &OutreachCapability{SkillID: "forum", SkillVersion: "2", Action: "reply"}}},
	}}
	runner, _ := NewOutreachTurnRunner(lifecycle, []capability.ModelAction{{Name: "forum.reply", SkillID: "forum", Version: "1", Action: "reply"}})
	run := &AgentRun{Scope: lifecycle.thread.Scope, Owner: lifecycle.thread.Owner, AssignedAgentID: "agent", Context: map[string]interface{}{
		OutreachInvocationContextKey: map[string]interface{}{"threadId": "thread-1", "messageId": "message-1", "body": "smuggled"},
	}}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err == nil {
		t.Fatal("smuggled outreach invocation was accepted")
	}
	delete(run.Context[OutreachInvocationContextKey].(map[string]interface{}), "body")
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err == nil {
		t.Fatal("unbound outreach capability was accepted")
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
