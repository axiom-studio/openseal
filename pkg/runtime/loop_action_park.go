package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

// governedParker decides where a Run with open governed tool entries waits:
// the pending card a terminal call opened, the approval park at step end, and
// the re-plan after a failed park CAS. The governed executor embeds it.
type governedParker struct {
	actions     ActionStore
	setups      SkillSetupRequestStore
	credentials CredentialRequestStore
	approvals   *ApprovalCoordinator
	now         func() time.Time
}

func newGovernedParker(store KernelStore, approvals *ApprovalCoordinator, now func() time.Time) governedParker {
	if now == nil {
		now = time.Now
	}
	return governedParker{actions: store, setups: store, credentials: store, approvals: approvals, now: now}
}

// GovernedParkPlan is the step-end park of a Run's governed tool entries.
type GovernedParkPlan struct {
	// Park is nil when nothing governed waits or when Settled.
	Park *GovernedPark
	// Replaced are entries whose outcome changed while planning: held
	// approvals superseded by a newer message. The caller applies them to
	// their tool entries before parking.
	Replaced []GovernedToolOutcome
	// Settled reports that a governed wait target was decided or settled
	// after its entry went awaiting: the Run must not park now; the next
	// step resumes the entries.
	Settled bool
	// Run is the Run after superseding (each held decision bumps its
	// revision); nil when planning wrote nothing.
	Run *AgentRun
}

// governedMaintenanceWaitReason is the ToolEntryWait.Reason of a call that
// waits for its Skill runtime maintenance to end (awaiting_event + RetryAt).
const governedMaintenanceWaitReason = "skill_maintenance"

const governedMaintenanceWaitSummary = "Waiting for a tool that is being updated"

const governedHumanInterventionWakeType = "human_intervention"

// governedCardWait returns the pending card the terminal call opened, if
// any: a human intervention, a credential request, a connect app card, or a
// connect account card of a parked call. Every check reads the durable
// record, so a card settled before this point never parks the Run.
func (p *governedParker) governedCardWait(ctx context.Context, run *AgentRun, call *ActionCall) (*WakeCondition, string, error) {
	if call == nil {
		return nil, "", nil
	}
	switch call.Status {
	case ActionCallStatusSucceeded:
		if request := pendingHumanInterventionForCall(run, call); request != nil {
			return &WakeCondition{Type: governedHumanInterventionWakeType, Reference: request.ID}, request.Summary, nil
		}
		if interactionRequestRefused(call) {
			return nil, "", nil
		}
		if p.credentials != nil {
			if request := pendingCredentialRequestFromAction(ctx, p.credentials, call); request != nil {
				return &WakeCondition{Type: CredentialRequestWakeType, Reference: request.ID}, "Waiting for the user to save a requested credential", nil
			}
		}
		if request := pendingConnectAppRequestFromAction(ctx, p.setups, call); request != nil {
			return &WakeCondition{Type: ConnectAppWakeType, Reference: request.ID}, "Waiting for the user to connect " + request.SkillName, nil
		}
	case ActionCallStatusFailed:
		if request := pendingConnectAccountRequestFromAction(ctx, p.setups, call); request != nil {
			return &WakeCondition{Type: ConnectAccountWakeType, Reference: request.ID}, connectAccountWaitSummary(request), nil
		}
	}
	return nil, "", nil
}

func pendingHumanInterventionForCall(run *AgentRun, call *ActionCall) *HumanInterventionRequest {
	if run == nil || call == nil {
		return nil
	}
	if requiresHuman, _ := call.Output["requiresHuman"].(bool); !requiresHuman {
		return nil
	}
	for i := len(run.HumanInterventions) - 1; i >= 0; i-- {
		request := run.HumanInterventions[i]
		if request.ActionCallID == call.ID && request.Status == HumanInterventionStatusPending {
			return &request
		}
	}
	return nil
}

// governedApprovalOutcome is the awaiting_approval outcome of a call waiting
// on approval.
func governedApprovalOutcome(toolCallID string, call *ActionCall, approval *ApprovalCheckpoint) GovernedToolOutcome {
	return GovernedToolOutcome{
		ToolCallID: toolCallID, State: ToolEntryAwaitingApproval, ActionCallID: call.ID,
		Wait: &ToolEntryWait{ApprovalID: approval.ID},
		Park: governedApprovalPark(approval),
	}
}

func governedApprovalPark(approval *ApprovalCheckpoint) *GovernedPark {
	return &GovernedPark{
		Status:  AgentRunStatusWaitingForApproval,
		Wake:    &WakeCondition{Type: "approval", Reference: approval.ID},
		Summary: "Waiting for approval: " + approval.Summary,
	}
}

// governedCardOutcome is the awaiting_event outcome of a call that opened a
// pending card.
func governedCardOutcome(toolCallID string, call *ActionCall, wake *WakeCondition, summary string) GovernedToolOutcome {
	return GovernedToolOutcome{
		ToolCallID: toolCallID, State: ToolEntryAwaitingEvent, ActionCallID: call.ID,
		Wait: &ToolEntryWait{EventWaitID: wake.Reference},
		Park: &GovernedPark{Status: AgentRunStatusWaitingForEvent, Wake: wake, Summary: summary},
	}
}

// governedMaintenanceOutcome is the awaiting_event outcome of a ready call
// whose Skill runtime is under maintenance: the Run sleeps until retryAt and
// the call executes on resume.
func governedMaintenanceOutcome(toolCallID string, call *ActionCall, retryAt time.Time) GovernedToolOutcome {
	at := retryAt.UTC()
	return GovernedToolOutcome{
		ToolCallID: toolCallID, State: ToolEntryAwaitingEvent, ActionCallID: call.ID,
		Wait: &ToolEntryWait{Reason: governedMaintenanceWaitReason, RetryAt: &at},
		Park: governedMaintenancePark(call.ID, at),
	}
}

func governedMaintenancePark(actionCallID string, retryAt time.Time) *GovernedPark {
	at := retryAt
	return &GovernedPark{
		Status:  AgentRunStatusSleeping,
		Wake:    &WakeCondition{Type: "timer", WakeAt: &at, Reference: actionCallID},
		Summary: governedMaintenanceWaitSummary,
	}
}

// newestSteeringMessage is the newest unconsumed conversation message
// delivered to run as steering; nil when none.
func newestSteeringMessage(run *AgentRun) *AgentRunIntervention {
	if run == nil {
		return nil
	}
	start := run.ConsumedInterventions
	if start < 0 {
		start = 0
	}
	for i := len(run.PendingInterventions) - 1; i >= start; i-- {
		intervention := run.PendingInterventions[i]
		if intervention.ConversationMessage != nil && intervention.ActionCallID == "" &&
			strings.TrimSpace(intervention.ConversationMessage.MessageID) != "" {
			return &intervention
		}
	}
	return nil
}

// PlanPark plans the step-end park after every governed tool entry of the
// step is terminal or awaiting. A newer conversation message supersedes the
// held approvals created before it instead of parking on them. Approvals win
// over cards; within a kind the first call in step order wins. The other open
// entries stay open and are settled or re-parked when the Run resumes.
func (p *governedParker) PlanPark(ctx context.Context, run *AgentRun, outcomes []GovernedToolOutcome) (GovernedParkPlan, error) {
	var plan GovernedParkPlan
	message := newestSteeringMessage(run)
	var approvals []GovernedToolOutcome
	var cards []GovernedToolOutcome
	for _, outcome := range outcomes {
		switch {
		case outcome.State == ToolEntryAwaitingApproval && outcome.Wait != nil && outcome.Wait.ApprovalID != "":
			approvals = append(approvals, outcome)
		case outcome.State == ToolEntryAwaitingEvent && outcome.Park != nil:
			cards = append(cards, outcome)
		}
	}
	remaining := approvals[:0:0]
	for _, outcome := range approvals {
		if message == nil {
			remaining = append(remaining, outcome)
			continue
		}
		replaced, decided, err := p.supersedeHeld(ctx, run, outcome.ToolCallID, outcome.Wait.ApprovalID, message, &plan)
		if err != nil {
			return GovernedParkPlan{}, err
		}
		switch {
		case replaced != nil:
			plan.Replaced = append(plan.Replaced, *replaced)
		case decided:
			plan.Settled = true
		default:
			remaining = append(remaining, outcome)
		}
	}
	if plan.Settled {
		return plan, nil
	}
	if len(remaining) > 0 {
		plan.Park = remaining[0].Park
		if plan.Park == nil {
			approval, err := p.actions.GetApproval(ctx, run.Scope, remaining[0].Wait.ApprovalID)
			if err != nil {
				return GovernedParkPlan{}, err
			}
			plan.Park = governedApprovalPark(approval)
		}
		return plan, nil
	}
	if len(cards) > 0 {
		plan.Park = cards[0].Park
	}
	return plan, nil
}

// ReplanPark re-plans the park after the loop's park CAS failed (a decision,
// a settled card or a new message moved the Run). run is the reloaded Run;
// entries are its open tool entries in call order (entries without an action
// call are ignored). Held approvals created before a newer message are
// superseded; any approval decided or card settled since gives Settled (do
// not park).
func (p *governedParker) ReplanPark(ctx context.Context, run *AgentRun, entries []*SessionEntry) (GovernedParkPlan, error) {
	var plan GovernedParkPlan
	message := newestSteeringMessage(run)
	var approval *GovernedPark
	var event *GovernedPark
	for _, entry := range entries {
		if entry == nil || entry.Type != SessionEntryTool || entry.ActionCallID == "" || entry.Tool == nil || entry.Tool.Wait == nil {
			continue
		}
		wait := entry.Tool.Wait
		switch entry.State {
		case ToolEntryAwaitingApproval:
			if wait.ApprovalID == "" {
				continue
			}
			if message != nil {
				replaced, decided, err := p.supersedeHeld(ctx, run, entry.ToolCallID, wait.ApprovalID, message, &plan)
				if err != nil {
					return GovernedParkPlan{}, err
				}
				if replaced != nil {
					plan.Replaced = append(plan.Replaced, *replaced)
					continue
				}
				if decided {
					plan.Settled = true
					continue
				}
			}
			current, err := p.actions.GetApproval(ctx, run.Scope, wait.ApprovalID)
			if err != nil {
				return GovernedParkPlan{}, err
			}
			if current.Status != ApprovalStatusPending {
				plan.Settled = true
				continue
			}
			if approval == nil {
				approval = governedApprovalPark(current)
			}
		case ToolEntryAwaitingEvent:
			park, err := p.currentEventPark(ctx, run, entry)
			if err != nil {
				return GovernedParkPlan{}, err
			}
			if park == nil {
				plan.Settled = true
				continue
			}
			if event == nil {
				event = park
			}
		}
	}
	if plan.Settled {
		return plan, nil
	}
	if approval != nil {
		plan.Park = approval
	} else {
		plan.Park = event
	}
	return plan, nil
}

// currentEventPark is the park an awaiting_event governed entry still needs,
// nil when its target settled.
func (p *governedParker) currentEventPark(ctx context.Context, run *AgentRun, entry *SessionEntry) (*GovernedPark, error) {
	wait := entry.Tool.Wait
	if wait.Reason == governedMaintenanceWaitReason {
		if wait.RetryAt == nil || !p.now().Before(*wait.RetryAt) {
			return nil, nil
		}
		return governedMaintenancePark(entry.ActionCallID, wait.RetryAt.UTC()), nil
	}
	call, err := p.actions.GetActionCall(ctx, run.Scope, entry.ActionCallID)
	if err != nil {
		return nil, err
	}
	wake, summary, err := p.governedCardWait(ctx, run, call)
	if err != nil || wake == nil {
		return nil, err
	}
	return &GovernedPark{Status: AgentRunStatusWaitingForEvent, Wake: wake, Summary: summary}, nil
}

// supersedeHeld supersedes the pending approval approvalID when it was
// created before message. It returns the superseded outcome, or decided when
// the approval is no longer pending (or another decision raced this one), or
// neither when the approval stays pending. plan.Run tracks the Run after each
// held supersede.
func (p *governedParker) supersedeHeld(ctx context.Context, run *AgentRun, toolCallID, approvalID string, message *AgentRunIntervention, plan *GovernedParkPlan) (*GovernedToolOutcome, bool, error) {
	approval, err := p.actions.GetApproval(ctx, run.Scope, approvalID)
	if err != nil {
		return nil, false, err
	}
	if approval.Status != ApprovalStatusPending {
		return nil, true, nil
	}
	if !approval.CreatedAt.Before(message.CreatedAt) {
		return nil, false, nil
	}
	if p.approvals == nil {
		return nil, false, errors.New("governed parker has no approval coordinator")
	}
	result, err := p.approvals.ResolveSupersededByMessage(ctx, run.Scope, approval.ID, approval.Revision, message.ConversationMessage.MessageID)
	if errors.Is(err, ErrApprovalResolved) || errors.Is(err, ErrApprovalExpired) || errors.Is(err, ErrRevisionConflict) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if result.Run != nil {
		plan.Run = result.Run
	}
	state, rendered := renderGovernedResult(result.Call, result.Approval, "")
	if rendered == nil {
		return nil, true, nil
	}
	return &GovernedToolOutcome{ToolCallID: toolCallID, State: state, Result: rendered, ActionCallID: result.Call.ID}, false, nil
}

// settledText is the settled card or human intervention text for call: the
// newest settled intervention carrying its ActionCallID, else (settled while
// the Run still held the call) the resolution derived from the durable
// record. "" when the call opened no settled card.
func (p *governedParker) settledText(ctx context.Context, run *AgentRun, call *ActionCall, settled []AgentRunIntervention) string {
	if call == nil {
		return ""
	}
	for i := len(settled) - 1; i >= 0; i-- {
		if settled[i].ActionCallID == call.ID {
			if text := strings.TrimSpace(settled[i].Instruction); text != "" {
				return text
			}
		}
	}
	switch call.Status {
	case ActionCallStatusSucceeded:
		if requiresHuman, _ := call.Output["requiresHuman"].(bool); requiresHuman && run != nil {
			for i := len(run.HumanInterventions) - 1; i >= 0; i-- {
				request := run.HumanInterventions[i]
				if request.ActionCallID == call.ID && request.Status != HumanInterventionStatusPending {
					return strings.TrimSpace(request.Resolution)
				}
			}
			return ""
		}
		if interactionRequestRefused(call) || call.SkillID != SkillManagementSkillID {
			return ""
		}
		switch call.Action {
		case SkillActionRequestCredential:
			id := interactionRequestID(call, "credentialRequest")
			if id == "" || p.credentials == nil {
				return ""
			}
			request, err := p.credentials.GetCredentialRequest(ctx, call.Scope, id)
			if err != nil || request == nil || request.RunID != call.RunID || request.Status == CredentialRequestStatusPending {
				return ""
			}
			return CredentialRequestResolutionInstruction(request)
		case SkillActionRequestConnection:
			id := interactionRequestID(call, "setupRequest")
			if id == "" || p.setups == nil {
				return ""
			}
			request, err := p.setups.GetSkillSetupRequest(ctx, call.Scope, id)
			if err != nil || request == nil || request.Kind != SkillSetupKindConnectApp || request.RunID != call.RunID || request.Status == "pending" {
				return ""
			}
			return ConnectAppResolutionInstruction(request, nil)
		}
	case ActionCallStatusFailed:
		if !isParkedConnectAccountCall(call) || p.setups == nil {
			return ""
		}
		request, err := p.setups.GetSkillSetupRequest(ctx, call.Scope, connectAccountRequestPrefix+call.ID)
		if err != nil || request == nil || request.Kind != SkillSetupKindConnectAccount || request.RunID != call.RunID || request.Status == "pending" {
			return ""
		}
		return ConnectAccountResolutionInstruction(request)
	}
	return ""
}
