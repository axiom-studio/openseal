package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// OutreachInvocationContextKey selects a trusted, persisted outreach draft.
// The Run carries only resource identity; target, body, evidence, and action
// arguments are always reloaded from the authoritative OutreachThread.
const OutreachInvocationContextKey = "outreachInvocation"

type OutreachTurnLifecycle interface {
	GetOutreachThread(context.Context, Scope, string) (*OutreachThread, error)
	ReconcileOutreachAction(context.Context, Scope, string, ReconcileOutreachActionRequest) (*ReconcileOutreachActionResult, error)
}

type OutreachTurnRunner struct {
	lifecycle OutreachTurnLifecycle
	actions   []capability.ModelAction
}

type OutreachActionProposalObserver struct {
	lifecycle OutreachTurnLifecycle
}

func NewOutreachActionProposalObserver(lifecycle OutreachTurnLifecycle) (*OutreachActionProposalObserver, error) {
	if lifecycle == nil {
		return nil, errors.New("outreach action projection requires a durable lifecycle")
	}
	return &OutreachActionProposalObserver{lifecycle: lifecycle}, nil
}

func (o *OutreachActionProposalObserver) ObserveActionProposal(ctx context.Context, run *AgentRun, _ *AgentTurn, proposal *ActionProposalResult) error {
	if o == nil || o.lifecycle == nil || run == nil || proposal == nil || proposal.Call == nil {
		return errors.New("outreach action projection requires a Run and committed ActionCall")
	}
	invocation, requested := run.Context[OutreachInvocationContextKey].(map[string]interface{})
	if !requested {
		return nil
	}
	threadID, _ := invocation["threadId"].(string)
	messageID, _ := invocation["messageId"].(string)
	if len(invocation) != 2 || !validOpaqueIdentifier(threadID, 128) || !validOpaqueIdentifier(messageID, 128) {
		return errors.New("outreach action projection has invalid resource identity")
	}
	_, err := o.lifecycle.ReconcileOutreachAction(ctx, run.Scope, threadID, ReconcileOutreachActionRequest{
		MessageID: messageID, ActionCallID: proposal.Call.ID, Actor: ActivityActor{Type: "worker", ID: "outreach-projector"},
	})
	return err
}

func NewOutreachTurnRunner(lifecycle OutreachTurnLifecycle, actions []capability.ModelAction) (*OutreachTurnRunner, error) {
	if lifecycle == nil || len(actions) == 0 {
		return nil, errors.New("outreach execution requires a durable lifecycle and authorized actions")
	}
	return &OutreachTurnRunner{lifecycle: lifecycle, actions: cloneHostedModelActions(actions)}, nil
}

func (r *OutreachTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || input.Run == nil || input.Turn == nil {
		return nil, errors.New("outreach execution requires a durable Run and Turn")
	}
	invocation, ok := input.Run.Context[OutreachInvocationContextKey].(map[string]interface{})
	if !ok || len(invocation) != 2 {
		return nil, errors.New("Run requires an exact outreach thread and message invocation")
	}
	threadID, _ := invocation["threadId"].(string)
	messageID, _ := invocation["messageId"].(string)
	threadID, messageID = strings.TrimSpace(threadID), strings.TrimSpace(messageID)
	if !validOpaqueIdentifier(threadID, 128) || !validOpaqueIdentifier(messageID, 128) {
		return nil, errors.New("outreach invocation identifiers are invalid")
	}
	thread, err := r.lifecycle.GetOutreachThread(ctx, input.Run.Scope, threadID)
	if err != nil {
		return nil, err
	}
	message := findOutreachMessage(thread, messageID)
	if message == nil || message.Direction != OutreachMessageOutbound || message.Capability == nil {
		return nil, errors.New("outreach invocation does not identify an outbound message")
	}
	if thread.Status != OutreachThreadOpen || thread.AssignedAgentID != input.Run.AssignedAgentID || thread.Owner != input.Run.Owner {
		return nil, errors.New("outreach thread is not open or does not belong to the assigned Run owner and Agent")
	}
	selected, err := selectOutreachAction(r.actions, message.Capability)
	if err != nil {
		return nil, err
	}
	checkpoint := cloneMap(input.Run.Checkpoint)
	if checkpoint == nil {
		checkpoint = map[string]interface{}{}
	}
	if last, resumed := checkpoint["lastAction"].(map[string]interface{}); resumed {
		return r.consumeAction(ctx, input.Run, thread, message, selected, checkpoint, last)
	}
	if message.Status != OutreachMessageDraft {
		return r.reconcileLinkedAction(ctx, input.Run, thread, message, checkpoint)
	}
	checkpoint["outreachActionInputs"] = map[string]interface{}{"reviewed": cloneMap(message.Capability.Arguments)}
	return &TurnOutcome{
		Decisions: []TurnDecision{{Summary: "Selected the reviewed outreach action and immutable source evidence", EvidenceRefs: []string{thread.SourceObservationID}}},
		ProposedActions: []TurnAction{{
			Type: "skill_action", Capability: selected.Name, Summary: "Deliver reviewed outreach message",
			BindingID: selected.BindingID, BindingRevision: selected.BindingRevision,
			IdempotencyKey: "outreach:" + thread.ID + ":" + message.ID, InputRef: "/outreachActionInputs/reviewed",
			EvidenceRefs: []string{thread.SourceObservationID},
		}},
		OutputSummary: "Requested governed outreach delivery", ContinuationCheckpoint: checkpoint,
		NextRunStatus: AgentRunStatusRunning,
	}, nil
}

// A denied or canceled ActionCall never executes, so there is intentionally no
// lastAction result in the Run checkpoint. Approval resolution still wakes the
// Run. Reconcile the already-linked durable ActionCall directly so its terminal
// disposition is projected into the OutreachThread instead of treating the
// pending message as a new delivery attempt.
func (r *OutreachTurnRunner) reconcileLinkedAction(ctx context.Context, run *AgentRun, thread *OutreachThread, message *OutreachMessage, checkpoint map[string]interface{}) (*TurnOutcome, error) {
	if strings.TrimSpace(message.ActionCallID) == "" {
		return nil, fmt.Errorf("outreach message cannot resume from %s without a linked action", message.Status)
	}
	reconciled, err := r.lifecycle.ReconcileOutreachAction(ctx, run.Scope, thread.ID, ReconcileOutreachActionRequest{
		MessageID: message.ID, ActionCallID: message.ActionCallID, Actor: ActivityActor{Type: "worker", ID: "outreach-reconciler"},
	})
	if err != nil {
		return nil, err
	}
	if reconciled == nil || reconciled.Thread == nil {
		return nil, errors.New("outreach action reconciliation returned no thread")
	}
	message = findOutreachMessage(reconciled.Thread, message.ID)
	if message == nil {
		return nil, errors.New("outreach action reconciliation lost the selected message")
	}
	switch message.Status {
	case OutreachMessageDeclined:
		delete(checkpoint, "outreachActionInputs")
		return &TurnOutcome{OutputSummary: "Governed outreach delivery declined", RunError: "governed outreach delivery declined", ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusFailed}, nil
	case OutreachMessageFailed:
		delete(checkpoint, "outreachActionInputs")
		return &TurnOutcome{OutputSummary: "Governed outreach delivery failed", RunError: "governed outreach delivery failed", ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusFailed}, nil
	case OutreachMessageCanceled:
		delete(checkpoint, "outreachActionInputs")
		return &TurnOutcome{OutputSummary: "Governed outreach delivery canceled", RunError: "governed outreach delivery canceled", ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusFailed}, nil
	default:
		return nil, fmt.Errorf("linked outreach action remains in non-terminal message state %s", message.Status)
	}
}

func (r *OutreachTurnRunner) consumeAction(ctx context.Context, run *AgentRun, thread *OutreachThread, message *OutreachMessage, selected *capability.ModelAction, checkpoint map[string]interface{}, last map[string]interface{}) (*TurnOutcome, error) {
	actionID, _ := last["actionCallId"].(string)
	if strings.TrimSpace(actionID) == "" || (selected.BindingID != "" && (last["bindingId"] != selected.BindingID || fmt.Sprint(last["bindingRevision"]) != fmt.Sprint(selected.BindingRevision))) ||
		last["skillId"] != selected.SkillID || last["skillVersion"] != selected.Version || last["action"] != selected.Action {
		return nil, errors.New("durable action result does not match the reviewed outreach capability")
	}
	status, _ := last["status"].(string)
	if status != string(ActionCallStatusSucceeded) {
		_, reconcileErr := r.lifecycle.ReconcileOutreachAction(ctx, run.Scope, thread.ID, ReconcileOutreachActionRequest{
			MessageID: message.ID, ActionCallID: actionID, Actor: ActivityActor{Type: "worker", ID: "outreach-reconciler"},
		})
		if reconcileErr != nil {
			return nil, reconcileErr
		}
		return &TurnOutcome{OutputSummary: "Governed outreach delivery failed", RunError: "governed outreach delivery failed", ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusFailed}, nil
	}
	receipt, err := normalizedOutreachReceipt(last["result"])
	if err != nil {
		return nil, err
	}
	reconciled, err := r.lifecycle.ReconcileOutreachAction(ctx, run.Scope, thread.ID, ReconcileOutreachActionRequest{
		MessageID: message.ID, ActionCallID: actionID, Receipt: receipt, Actor: ActivityActor{Type: "worker", ID: "outreach-reconciler"},
	})
	if err != nil {
		return nil, err
	}
	delete(checkpoint, "lastAction")
	delete(checkpoint, "outreachActionInputs")
	return &TurnOutcome{
		Decisions:     []TurnDecision{{Summary: "Verified the governed provider receipt against the reviewed outreach message", EvidenceRefs: []string{thread.SourceObservationID}}},
		OutputSummary: "Governed outreach message delivered", ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusCompleted,
		RunOutput: map[string]interface{}{"outreachThreadId": reconciled.Thread.ID, "outreachMessageId": message.ID, "receipt": receipt},
	}, nil
}

func selectOutreachAction(actions []capability.ModelAction, requested *OutreachCapability) (*capability.ModelAction, error) {
	var selected *capability.ModelAction
	for index := range actions {
		candidate := &actions[index]
		if candidate.SkillID == requested.SkillID && candidate.Version == requested.SkillVersion && candidate.Action == requested.Action &&
			(requested.BindingID == "" || (candidate.BindingID == requested.BindingID && candidate.BindingRevision == requested.BindingRevision)) {
			if selected != nil {
				return nil, errors.New("outreach capability matches multiple authorized actions")
			}
			selected = candidate
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("outreach capability %s@%s/%s is not authorized", requested.SkillID, requested.SkillVersion, requested.Action)
	}
	return selected, nil
}

// Provider-facing Skills normalize their result to an outreachReceipt object.
// The receipt is public delivery evidence; credentials and raw provider
// payloads are never admitted into this durable contract.
func normalizedOutreachReceipt(result interface{}) (*OutreachReceipt, error) {
	output, ok := result.(map[string]interface{})
	if !ok {
		return nil, errors.New("outreach action returned no normalized result")
	}
	value, ok := output["outreachReceipt"].(map[string]interface{})
	if !ok {
		return nil, errors.New("outreach action returned no normalized provider receipt")
	}
	provider, _ := value["provider"].(string)
	externalID, _ := value["externalId"].(string)
	externalURI, _ := value["externalUri"].(string)
	digest, _ := value["digest"].(string)
	deliveredRaw, _ := value["deliveredAt"].(string)
	deliveredAt, err := time.Parse(time.RFC3339Nano, deliveredRaw)
	if err != nil {
		return nil, errors.New("outreach receipt deliveredAt must be RFC3339")
	}
	receipt := &OutreachReceipt{Provider: provider, ExternalID: externalID, ExternalURI: externalURI, Digest: digest, DeliveredAt: deliveredAt.UTC()}
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	return receipt, nil
}
