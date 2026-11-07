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

// outreachToolCallID is the id of the one governed delivery call an outreach
// Run makes.
const outreachToolCallID = "outreach-delivery"

// OutreachStepProducer delivers one reviewed outreach draft through one
// governed tool call. Target, body and action arguments are always reloaded
// from the authoritative OutreachThread; the provider receipt is read from the
// ActionCall and reconciled into the thread before the Run completes.
type OutreachStepProducer struct {
	lifecycle OutreachTurnLifecycle
	actions   []capability.ModelAction
	calls     ActionCallReader
}

func NewOutreachStepProducer(lifecycle OutreachTurnLifecycle, actions []capability.ModelAction, calls ActionCallReader) (*OutreachStepProducer, error) {
	if lifecycle == nil || len(actions) == 0 {
		return nil, errors.New("outreach execution requires a durable lifecycle and authorized actions")
	}
	if calls == nil {
		return nil, errors.New("outreach execution requires the action-call store")
	}
	return &OutreachStepProducer{lifecycle: lifecycle, actions: append([]capability.ModelAction(nil), actions...), calls: calls}, nil
}

func (p *OutreachStepProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (p *OutreachStepProducer) Produce(ctx context.Context, in StepInput) (StepOutput, error) {
	if p == nil || in.Run == nil || in.Step == nil {
		return StepOutput{}, errors.New("outreach execution requires a durable Run and step")
	}
	run := in.Run
	invocation, ok := run.Context[OutreachInvocationContextKey].(map[string]interface{})
	if !ok || len(invocation) != 2 {
		return StepOutput{}, errors.New("Run requires an exact outreach thread and message invocation")
	}
	threadID, _ := invocation["threadId"].(string)
	messageID, _ := invocation["messageId"].(string)
	threadID, messageID = strings.TrimSpace(threadID), strings.TrimSpace(messageID)
	if !validOpaqueIdentifier(threadID, 128) || !validOpaqueIdentifier(messageID, 128) {
		return StepOutput{}, errors.New("outreach invocation identifiers are invalid")
	}
	thread, err := p.lifecycle.GetOutreachThread(ctx, run.Scope, threadID)
	if err != nil {
		return StepOutput{}, err
	}
	message := findOutreachMessage(thread, messageID)
	if message == nil || message.Direction != OutreachMessageOutbound || message.Capability == nil {
		return StepOutput{}, errors.New("outreach invocation does not identify an outbound message")
	}
	if thread.Status != OutreachThreadOpen || thread.AssignedAgentID != run.AssignedAgentID || thread.Owner != run.Owner {
		return StepOutput{}, errors.New("outreach thread is not open or does not belong to the assigned Run owner and Agent")
	}
	selected, err := selectOutreachAction(p.actions, message.Capability)
	if err != nil {
		return StepOutput{}, err
	}
	result, call, err := producerToolResult(ctx, p.calls, run, in.Path, outreachToolCallID)
	if err != nil {
		return StepOutput{}, err
	}
	if result == nil {
		if message.Status != OutreachMessageDraft {
			return p.reconcileLinkedAction(ctx, run, thread, message)
		}
		toolCall, err := producerGovernedToolCall(outreachToolCallID, selected, cloneMap(message.Capability.Arguments), &ToolCallDirective{
			IdempotencyKey: "outreach:" + thread.ID + ":" + message.ID,
			Summary:        "Deliver reviewed outreach message",
		})
		if err != nil {
			return StepOutput{}, err
		}
		return StepOutput{ToolCalls: []LoopToolCall{toolCall}}, nil
	}
	if call == nil {
		return outreachFailure(OutreachMessageFailed), nil
	}
	if !producerCallMatches(call, selected) {
		return StepOutput{}, errors.New("durable action result does not match the reviewed outreach capability")
	}
	if call.Status != ActionCallStatusSucceeded {
		reconciled, err := p.reconcile(ctx, run, thread, message, call.ID, nil)
		if err != nil {
			return StepOutput{}, err
		}
		return outreachFailure(reconciled.Status), nil
	}
	receipt, err := normalizedOutreachReceipt(result.Output)
	if err != nil {
		return StepOutput{}, err
	}
	if _, err := p.reconcile(ctx, run, thread, message, call.ID, receipt); err != nil {
		return StepOutput{}, err
	}
	return outreachDelivered(thread.ID, message.ID, receipt), nil
}

// A message that already left draft without a tool entry of this Run was
// linked to an ActionCall elsewhere (an earlier attempt). Reconcile that
// durable call directly so its disposition is projected into the thread
// instead of treating the message as a new delivery attempt.
func (p *OutreachStepProducer) reconcileLinkedAction(ctx context.Context, run *AgentRun, thread *OutreachThread, message *OutreachMessage) (StepOutput, error) {
	if message.Status == OutreachMessageDelivered && message.Receipt != nil {
		return outreachDelivered(thread.ID, message.ID, cloneOutreachReceipt(message.Receipt)), nil
	}
	if strings.TrimSpace(message.ActionCallID) == "" {
		return StepOutput{}, fmt.Errorf("outreach message cannot resume from %s without a linked action", message.Status)
	}
	reconciled, err := p.reconcile(ctx, run, thread, message, message.ActionCallID, nil)
	if err != nil {
		return StepOutput{}, err
	}
	switch reconciled.Status {
	case OutreachMessageDeclined, OutreachMessageFailed, OutreachMessageCanceled:
		return outreachFailure(reconciled.Status), nil
	case OutreachMessageDelivered:
		if reconciled.Receipt != nil {
			return outreachDelivered(thread.ID, message.ID, cloneOutreachReceipt(reconciled.Receipt)), nil
		}
	}
	return StepOutput{}, fmt.Errorf("linked outreach action remains in non-terminal message state %s", reconciled.Status)
}

func (p *OutreachStepProducer) reconcile(ctx context.Context, run *AgentRun, thread *OutreachThread, message *OutreachMessage, actionCallID string, receipt *OutreachReceipt) (*OutreachMessage, error) {
	reconciled, err := p.lifecycle.ReconcileOutreachAction(ctx, run.Scope, thread.ID, ReconcileOutreachActionRequest{
		MessageID: message.ID, ActionCallID: actionCallID, Receipt: receipt, Actor: ActivityActor{Type: "worker", ID: "outreach-reconciler"},
	})
	if err != nil {
		return nil, err
	}
	if reconciled == nil || reconciled.Thread == nil {
		return nil, errors.New("outreach action reconciliation returned no thread")
	}
	updated := findOutreachMessage(reconciled.Thread, message.ID)
	if updated == nil {
		return nil, errors.New("outreach action reconciliation lost the selected message")
	}
	return updated, nil
}

func outreachDelivered(threadID, messageID string, receipt *OutreachReceipt) StepOutput {
	return StepOutput{Final: &LoopFinal{
		Answer: "Delivered the reviewed outreach message.",
		Output: map[string]interface{}{"outreachThreadId": threadID, "outreachMessageId": messageID, "receipt": receipt},
		Status: AgentRunStatusCompleted,
	}}
}

func outreachFailure(status OutreachMessageStatus) StepOutput {
	code, message := "outreach_delivery_failed", "The reviewed outreach message was not delivered."
	switch status {
	case OutreachMessageDeclined:
		code, message = "outreach_delivery_declined", "The reviewed outreach message was declined, so it was not delivered."
	case OutreachMessageCanceled:
		code, message = "outreach_delivery_canceled", "The reviewed outreach message was canceled before delivery."
	}
	return StepOutput{Failure: &LoopFailure{Kind: "execution", Code: code, Message: message}}
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
