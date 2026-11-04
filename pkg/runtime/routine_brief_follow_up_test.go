package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type routineBriefFixture struct {
	*conversationFollowUpFixture
	catalog *skill.Catalog
	clock   time.Time
}

func newRoutineBriefFixture(t *testing.T) *routineBriefFixture {
	t.Helper()
	f := newConversationFollowUpFixture(t)
	catalog := skill.NewCatalog()
	scope := capability.ScopeReference{Kind: f.conversation.Scope.Kind, ID: f.conversation.Scope.ID}
	shop := &skill.Definition{
		ID: "shop", Version: "1.0.0", Name: "Shop", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://shop.invalid"},
		Actions: map[string]skill.Action{"pay": {Name: "pay", Description: "Pay for the checkout", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
			Idempotency: skill.IdempotencySupported, ExternalOperationPolicy: skill.ExternalOperationForbidden, InputSchema: map[string]interface{}{"type": "object"}}},
	}
	for _, definition := range []*skill.Definition{RunbookManagementSkill(), shop} {
		if err := catalog.Register(t.Context(), definition); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "routines", Revision: 1, Scope: scope, DeploymentID: "agent-42", SkillID: RunbookManagementSkillID, SkillVersion: RunbookManagementSkillVersion, AllowedActions: []string{RunbookActionCreateTask}, MaximumRisk: skill.RiskLevelWrite}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "shop", Revision: 1, Scope: scope, DeploymentID: "agent-42", SkillID: "shop", SkillVersion: "1.0.0", AllowedActions: []string{"pay"}, MaximumRisk: skill.RiskLevelWrite}); err != nil {
		t.Fatal(err)
	}
	return &routineBriefFixture{conversationFollowUpFixture: f, catalog: catalog, clock: time.Now().UTC().Add(-time.Hour)}
}

func routineBriefArguments(timezone string) map[string]interface{} {
	return map[string]interface{}{
		"title": "Morning weather", "goal": "Read today's forecast and post a short summary.", "cron": "0 0 8 * * *", "timezone": timezone,
		"brief": map[string]interface{}{"summary": "Every day at 8:00 AM, reads the forecast and posts a summary here.", "grants": []interface{}{}},
	}
}

// propose claims the Run and proposes one action that waits for approval.
// The clock runs in the past so approvals predate messages posted later.
func (f *routineBriefFixture) propose(t *testing.T, run *AgentRun, skillID, action string, args map[string]interface{}) *ActionProposalResult {
	t.Helper()
	f.clock = f.clock.Add(time.Minute)
	claimed, err := f.store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, WorkerID: "worker", AssignedAgentID: "agent-42", Now: time.Now().Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := NewActionCoordinator(f.store, f.store, f.catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, Reason: "Review", EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "user"}}, ApprovalTTL: 24 * time.Hour}, nil
	}))
	now := f.clock
	coordinator.now = func() time.Time { return now }
	version := RunbookManagementSkillVersion
	if skillID != RunbookManagementSkillID {
		version = "1.0.0"
	}
	proposal, err := coordinator.Propose(t.Context(), ProposeActionRequest{
		Scope: run.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "agent-42", SkillID: skillID, SkillVersion: version,
		Action: action, Arguments: args, IdempotencyKey: fmt.Sprintf("%s-%d", action, now.UnixNano()), Summary: "Review " + action,
		Actor: ActivityActor{Type: "agent", ID: "agent-42"},
	})
	if err != nil || proposal.Approval == nil {
		t.Fatalf("proposal = %#v, %v", proposal, err)
	}
	return proposal
}

func (f *routineBriefFixture) approval(t *testing.T, id string) *ApprovalCheckpoint {
	t.Helper()
	approval, err := f.store.GetApproval(t.Context(), f.conversation.Scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func briefRunWaiting(t *testing.T, f *routineBriefFixture, skillID, action string, args map[string]interface{}) (*AgentRun, *ActionProposalResult) {
	t.Helper()
	run := f.schedule(t, f.post(t, "user", "Every morning at 8, tell me the weather"))
	proposal := f.propose(t, run, skillID, action, args)
	run = f.run(t, run.ID)
	if run.Status != AgentRunStatusWaitingForApproval || run.WakeCondition == nil || run.WakeCondition.Reference != proposal.Approval.ID {
		t.Fatalf("Run is not waiting on the approval: %#v", run)
	}
	return run, proposal
}

// A message sent while a routine brief waits replaces the brief: the Run
// resumes with the message as a follow-up instead of starting a new turn.
func TestMessageDuringPendingRoutineBriefSupersedesIt(t *testing.T) {
	f := newRoutineBriefFixture(t)
	run, proposal := briefRunWaiting(t, f, RunbookManagementSkillID, RunbookActionCreateTask, routineBriefArguments("Europe/London"))
	message := f.post(t, "user", "08:00 IST")
	delivered := f.schedule(t, message)
	if delivered.ID != run.ID || len(f.conversationRuns(t)) != 1 {
		t.Fatalf("message started another Run: %#v", delivered)
	}
	if delivered.Status != AgentRunStatusQueued || delivered.WakeCondition != nil || !conversationRunHasFollowUp(delivered, message.ID) || requiresFinalFailureExplanation(delivered.Checkpoint) {
		t.Fatalf("Run did not resume with the message: %#v", delivered)
	}
	approval := f.approval(t, proposal.Approval.ID)
	if approval.Status != ApprovalStatusSuperseded || approval.DecisionReason != ApprovalSupersededReason || approval.DecisionBy == nil ||
		*approval.DecisionBy != ApprovalSupersededPrincipal || approval.DecisionID != "superseded:"+approval.ID || approval.DecidedAt == nil {
		t.Fatalf("approval = %#v", approval)
	}
	if outcome := approvalOutcome(approval.Status); outcome != "Replaced by your message" {
		t.Fatalf("approval outcome = %q", outcome)
	}
	call, err := f.store.GetActionCall(t.Context(), run.Scope, proposal.Call.ID)
	if err != nil || call.Status != ActionCallStatusDenied {
		t.Fatalf("brief call = %#v, %v", call, err)
	}
	last, _ := delivered.Checkpoint["lastAction"].(map[string]interface{})
	if fmt.Sprint(last["approvalStatus"]) != string(ApprovalStatusSuperseded) || last["supersededByMessageId"] != message.ID {
		t.Fatalf("lastAction = %#v", last)
	}
	events, err := f.store.ListActivity(t.Context(), ActivityFilter{Scope: run.Scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, event := range events {
		audited = audited || event.EventType == "approval.superseded" && event.Actor == (ActivityActor{Type: "system", ID: "kernel"}) && event.CorrelationID == message.ID
	}
	if !audited {
		t.Fatalf("supersede was not audited: %#v", events)
	}
	if _, ok := governedConversationActionOutcome(delivered); ok {
		t.Fatal("a superseded brief resolved without the model")
	}

	// The resumed Turn sees the request and the message, in order.
	calls := 0
	runner := f.runner(t, func(input TurnExecutionContext) *TurnOutcome {
		calls++
		goal := decodeConversationFollowUpGoal(t, input.Run.Goal)
		if len(goal.FollowUpMessages) != 1 || goal.FollowUpMessages[0].Content != "08:00 IST" || !strings.Contains(goal.CurrentMessage.Content, "Every morning") {
			t.Fatalf("Turn did not see the brief reply: %#v", goal)
		}
		return completedConversationAnswer("Here is the revised routine.")
	})
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: delivered}); err != nil || calls != 1 {
		t.Fatalf("resumed Turn = %v, calls=%d", err, calls)
	}

	// The hosted request tells the Agent to re-propose with the change.
	var seen HostedTurnRequest
	host := &failureExplanationTestHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		seen = request
		return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model", NextRunStatus: AgentRunStatusCompleted, OutputSummary: "ok", RunOutput: map[string]interface{}{"summary": "ok"}}, nil
	}}
	hosted, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent-42", DefinitionID: "agent", DefinitionVersion: "1",
		Actions: []capability.ModelAction{{Name: "openseal.runbooks.create_task", SkillID: RunbookManagementSkillID, Action: RunbookActionCreateTask, Version: RunbookManagementSkillVersion}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hosted.RunTurn(t.Context(), TurnExecutionContext{Run: delivered, Turn: &AgentTurn{ID: "revise"}}); err != nil {
		t.Fatal(err)
	}
	instructions := strings.Join(seen.SystemInstructions, "\n")
	if !strings.Contains(instructions, SupersededRoutineBriefInstruction) || !strings.Contains(instructions, RoutineTimezoneInstruction) || !strings.Contains(instructions, RoutineDeliveryInstruction) || len(seen.Actions) != 1 {
		t.Fatalf("hosted request = %#v", seen)
	}
}

// Replays and a second quick message converge on one decision; a replay
// never closes the revised brief proposed after the message.
func TestRoutineBriefSupersedeIsIdempotent(t *testing.T) {
	f := newRoutineBriefFixture(t)
	run, proposal := briefRunWaiting(t, f, RunbookManagementSkillID, RunbookActionCreateTask, routineBriefArguments("Europe/London"))
	first := f.post(t, "user", "08:00 IST")
	second := f.post(t, "user", "and include the humidity")
	for _, message := range []*ChannelMessage{first, second, first} {
		if delivered := f.schedule(t, message); delivered.ID != run.ID {
			t.Fatalf("message %s started a parallel Run", message.ID)
		}
	}
	current := f.run(t, run.ID)
	if len(f.conversationRuns(t)) != 1 || len(conversationRunFollowUps(current)) != 2 || current.Status != AgentRunStatusQueued {
		t.Fatalf("Run = %#v", current)
	}
	resolved := f.approval(t, proposal.Approval.ID)
	if resolved.Status != ApprovalStatusSuperseded || resolved.Revision != proposal.Approval.Revision+1 {
		t.Fatalf("approval resolved more than once: %#v", resolved)
	}
	// A second supersede of the same approval is the same decision.
	replay, err := f.scheduler.approvals.ResolveSupersededByMessage(t.Context(), run.Scope, resolved.ID, proposal.Approval.Revision, second.ID)
	if err != nil || replay.Resolved {
		t.Fatalf("replayed supersede = %#v, %v", replay, err)
	}

	// The revised brief is newer than both messages; replaying them leaves it pending.
	f.clock = time.Now().UTC()
	revised := f.propose(t, current, RunbookManagementSkillID, RunbookActionCreateTask, routineBriefArguments("Asia/Kolkata"))
	for _, message := range []*ChannelMessage{first, second} {
		f.schedule(t, message)
	}
	if approval := f.approval(t, revised.Approval.ID); approval.Status != ApprovalStatusPending {
		t.Fatalf("replay closed the revised brief: %#v", approval)
	}
	if current := f.run(t, run.ID); current.Status != AgentRunStatusWaitingForApproval {
		t.Fatalf("revised brief Run = %#v", current)
	}
}

// Other approvals keep waiting for their decision; the message stays a
// follow-up for when the Run resumes.
func TestMessageDuringOtherPendingApprovalKeepsWaiting(t *testing.T) {
	f := newRoutineBriefFixture(t)
	run, proposal := briefRunWaiting(t, f, "shop", "pay", map[string]interface{}{})
	message := f.post(t, "user", "use the other card")
	delivered := f.schedule(t, message)
	if delivered.ID != run.ID || delivered.Status != AgentRunStatusWaitingForApproval || !conversationRunHasFollowUp(delivered, message.ID) {
		t.Fatalf("Run = %#v", delivered)
	}
	if approval := f.approval(t, proposal.Approval.ID); approval.Status != ApprovalStatusPending {
		t.Fatalf("payment approval changed: %#v", approval)
	}
}

// A decision that lands first wins; the message is still delivered.
func TestRoutineBriefDecidedConcurrentlyWithMessage(t *testing.T) {
	f := newRoutineBriefFixture(t)
	run, proposal := briefRunWaiting(t, f, RunbookManagementSkillID, RunbookActionCreateTask, routineBriefArguments("Europe/London"))
	message := f.post(t, "user", "08:00 IST")
	accepted, err := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{}).Resolve(t.Context(), ResolveApprovalRequest{
		Scope: run.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "accept",
		Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "user", ID: "user"},
	})
	if err != nil || accepted.Approval.Status != ApprovalStatusApproved {
		t.Fatalf("accept = %#v, %v", accepted, err)
	}
	// The kernel's supersede of the observed revision loses cleanly.
	if _, err := f.scheduler.approvals.ResolveSupersededByMessage(t.Context(), run.Scope, proposal.Approval.ID, proposal.Approval.Revision, message.ID); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("late supersede error = %v", err)
	}
	delivered := f.schedule(t, message)
	if delivered.ID != run.ID || delivered.Status != AgentRunStatusWaitingForDependency || !conversationRunHasFollowUp(delivered, message.ID) {
		t.Fatalf("Run = %#v", delivered)
	}
	if approval := f.approval(t, proposal.Approval.ID); approval.Status != ApprovalStatusApproved || approval.DecisionID != "accept" {
		t.Fatalf("approval = %#v", approval)
	}
}

// Declining a brief is a decision: the Run completes with a calm reply and
// no failure. Other declined actions still end the attempt as before.
func TestDeclinedRoutineBriefCompletesCalmly(t *testing.T) {
	f := newRoutineBriefFixture(t)
	run, proposal := briefRunWaiting(t, f, RunbookManagementSkillID, RunbookActionCreateTask, routineBriefArguments("Asia/Kolkata"))
	declined, err := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{}).Resolve(t.Context(), ResolveApprovalRequest{
		Scope: run.Scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "decline",
		Decision: ApprovalDecisionReject, Principal: ApprovalPrincipal{Type: "user", ID: "user"},
	})
	if err != nil || declined.Approval.Status != ApprovalStatusRejected || declined.Run.Status != AgentRunStatusQueued || requiresFinalFailureExplanation(declined.Run.Checkpoint) {
		t.Fatalf("decline = %#v, %v", declined, err)
	}
	runner := f.runner(t, func(TurnExecutionContext) *TurnOutcome {
		t.Fatal("a declined brief went back to the model")
		return nil
	})
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: declined.Run})
	if err != nil || outcome.NextRunStatus != AgentRunStatusCompleted || outcome.RunError != "" || len(outcome.ProposedActions) != 0 {
		t.Fatalf("decline outcome = %#v, %v", outcome, err)
	}
	reply, err := f.service.GetChannelMessage(t.Context(), f.conversation.Scope, f.conversation.ID, fmt.Sprint(outcome.RunOutput["messageId"]))
	if err != nil || reply.Content != DeclinedRoutineBriefReply {
		t.Fatalf("decline reply = %#v, %v", reply, err)
	}

	other := newRoutineBriefFixture(t)
	payRun, payment := briefRunWaiting(t, other, "shop", "pay", map[string]interface{}{})
	rejected, err := NewApprovalCoordinator(other.store, other.store, EligibleApprovalAuthorizer{}).Resolve(t.Context(), ResolveApprovalRequest{
		Scope: payRun.Scope, ApprovalID: payment.Approval.ID, ExpectedRevision: payment.Approval.Revision, DecisionID: "decline",
		Decision: ApprovalDecisionReject, Principal: ApprovalPrincipal{Type: "user", ID: "user"},
	})
	if err != nil || !requiresFinalFailureExplanation(rejected.Run.Checkpoint) {
		t.Fatalf("declined payment no longer ends the attempt: %#v, %v", rejected, err)
	}
}
