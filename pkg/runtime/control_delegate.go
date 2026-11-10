package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

// agent_delegate and agent_delegate_reply (section 04 §8): settle-phase
// control tools that hand a piece of work to another Agent through a durable
// AgentRequest. The request create moves the tool entry to awaiting_child and
// parks the source Run in one transaction (CTL-6a), so the park is committed.
// The recipient's decision or the child's terminal state re-queues the source;
// resume turns the request outcome into the tool result. No failure is
// written into the source checkpoint: a failed delegation is an is_error tool
// result the model reads.

const (
	maximumDelegateGoalBytes   = 16384
	maximumDelegateAnswerBytes = 8192

	controlDelegationRejectedCode = "delegation_rejected"
	controlRequestNotAwaitingCode = "request_not_awaiting_answer"
	controlNewMessageCode         = "new_message"
)

type delegateArgs struct {
	AgentID string                 `json:"agentId"`
	Goal    string                 `json:"goal"`
	Context map[string]interface{} `json:"context,omitempty"`
	Mode    runbook.DelegateMode   `json:"mode,omitempty"`
}

type delegateReplyArgs struct {
	RequestID string `json:"requestId"`
	Answer    string `json:"answer"`
}

// DelegationSpec is one delegation, from a model call (built from the tool
// arguments) or from a deterministic producer (runbook engine, §12).
type DelegationSpec struct {
	StepID, AgentID, Goal string
	Context               map[string]interface{}
	Mode                  runbook.DelegateMode
	// Timeout is carried for kernel-origin delegations; the request itself has
	// no timeout (the old materialization did not apply one either).
	Timeout time.Duration
	// Budget is the kernel-origin child budget. Model calls never choose one:
	// a bounded source funds the minimum child budget (DI-8).
	Budget *BudgetPolicy
}

// delegateEntryMeta is the kernel metadata of a delegate or reply entry.
// RequestRevision (reply only) is the request revision the answer was given
// against, so a resume can tell whether the answer was recorded.
type delegateEntryMeta struct {
	ControlEntryMeta
	RequestRevision int64 `json:"requestRevision,omitempty"`
}

type delegateHandler struct{}

type delegateReplyHandler struct{}

func init() {
	registerControlHandler(ControlToolDelegate, delegateHandler{})
	registerControlHandler(ControlToolDelegateReply, delegateReplyHandler{})
}

// spec carries no Agent list: the eligible Agents are in the capabilities
// note (capability_note.go), so a new Agent never changes the tool, and the
// handler resolves agentId against the claim's targets.
func (delegateHandler) spec(a ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: "Ask another Agent to do a piece of work and wait for its answer. " +
			"The Agent sees only the goal and context you give it. The Agents you can ask are listed in the capabilities note.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"agentId", "goal"},
			"properties": map[string]interface{}{
				"agentId": map[string]interface{}{"type": "string", "minLength": 1, "description": "An agentId from the capabilities note."},
				"goal":    map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumDelegateGoalBytes},
				"context": map[string]interface{}{"type": "object", "description": "Non-secret data the Agent needs."},
				"mode": map[string]interface{}{
					"type": "string", "enum": []interface{}{string(runbook.DelegateReason), string(runbook.DelegateBehavior)},
					"description": "reason (default): the Agent reasons and answers; behavior: the Agent performs its configured behavior.",
				},
			},
		},
	}
}

func (delegateHandler) decode(raw json.RawMessage) (interface{}, error) {
	value, err := decodeControlInto[delegateArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*delegateArgs)
	args.AgentID = strings.TrimSpace(args.AgentID)
	args.Goal = strings.TrimSpace(args.Goal)
	if args.AgentID == "" {
		return nil, errors.New("agentId must not be empty")
	}
	if args.Goal == "" {
		return nil, errors.New("goal must not be empty")
	}
	if len(args.Goal) > maximumDelegateGoalBytes || !utf8.ValidString(args.Goal) {
		return nil, fmt.Errorf("goal must be valid text of at most %d bytes", maximumDelegateGoalBytes)
	}
	switch args.Mode {
	case "":
		args.Mode = runbook.DelegateReason
	case runbook.DelegateReason, runbook.DelegateBehavior:
	default:
		return nil, errors.New("mode must be reason or behavior")
	}
	return args, nil
}

func (delegateHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*delegateArgs)
	spec := DelegationSpec{
		StepID: "delegate-" + cc.Entry.ID, AgentID: args.AgentID, Goal: args.Goal,
		Context: args.Context, Mode: args.Mode,
	}
	if cc.Origin != ControlOriginKernel {
		agentID, err := resolveHostedAgentTarget(args.AgentID, cc.Authority.DelegateTargets)
		if err != nil {
			return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, controlDelegationRejectedCode, err.Error(), nil))
		}
		spec.AgentID = agentID
	}
	return x.Delegate(ctx, cc, spec)
}

func (delegateHandler) resume(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResume, error) {
	return x.resumeDelegation(ctx, cc)
}

// Delegate creates the durable AgentRequest of spec and parks the source on
// its decision. It is idempotent by the request id derived from the Run and
// spec.StepID: a re-settle after a crash creates no second request; an
// existing request is resumed instead. cc.Entry may be nil for a kernel-origin
// call that keeps no tool entry; then no entry is transitioned.
func (x *ControlExecutor) Delegate(ctx context.Context, cc ControlContext, spec DelegationSpec) (ControlResult, error) {
	if x == nil || x.Collaboration == nil || x.Portfolio == nil {
		return ControlResult{}, errors.New("delegation needs the collaboration service")
	}
	run := cc.Run
	if run == nil {
		return ControlResult{}, ErrRunNotFound
	}
	spec.StepID, spec.AgentID, spec.Goal = strings.TrimSpace(spec.StepID), strings.TrimSpace(spec.AgentID), strings.TrimSpace(spec.Goal)
	if spec.StepID == "" || spec.AgentID == "" || spec.Goal == "" {
		return ControlResult{}, errors.New("delegation needs a step, an Agent and a goal")
	}
	if err := ValidateCredentialFreeContext(spec.Context); err != nil {
		return x.rejectDelegation(ctx, cc, err.Error())
	}
	if err := validateTaskDelegationSource(ctx, x.Portfolio, run); err != nil {
		if errors.Is(err, ErrInvalidConversationTask) {
			return x.rejectDelegation(ctx, cc, "Work inside a conversation task cannot be handed to another Agent; do it here.")
		}
		return ControlResult{}, err
	}
	sharedContext, err := delegationSharedContext(run, spec)
	if err != nil {
		return x.rejectDelegation(ctx, cc, err.Error())
	}
	key := delegationIdempotencyKey(run.ID, spec.StepID)
	requestID := stableCollaborationID(run.Scope, key, "request")
	existing, err := x.Collaboration.GetAgentRequest(ctx, run.Scope, requestID)
	if err != nil && !errors.Is(err, ErrAgentRequestNotFound) {
		return ControlResult{}, err
	}
	if existing != nil {
		return x.replayDelegation(ctx, cc, existing)
	}
	budget, err := x.delegationBudget(cc, spec)
	if err != nil {
		var insufficient *ControlBudgetInsufficientError
		if errors.As(err, &insufficient) {
			return x.refuseIfEntry(ctx, cc, controlErrorResult(ToolResultRefused, insufficient.Code(), insufficient.Message(), nil))
		}
		return ControlResult{}, err
	}
	acceptancePolicy := AgentRequestAcceptanceRecipientReview
	if delegatedRunbookOrigin(run.Context) != nil {
		// The kernel-authored Runbook step already selected the exact recipient
		// under an immutable governed definition; a second admission adds no
		// authority. Model-authored delegation keeps recipient review.
		acceptancePolicy = AgentRequestAcceptancePreauthorized
	}
	count := len(run.PendingInterventions)
	request := CreateAgentRequestRequest{
		ID: requestID, Scope: run.Scope, Kind: AgentRequestKindRequest,
		Requester:   CollaborationParty{Type: run.Owner.Type, ID: run.Owner.ID},
		Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: spec.AgentID},
		SourceRunID: run.ID, Goal: spec.Goal, SharedContext: sharedContext,
		AcceptancePolicy: acceptancePolicy, BudgetAllocation: budget, IdempotencyKey: key,
		RequireInterventionCount: &count,
	}
	if cc.Entry != nil {
		transition, err := x.delegateEntryTransition(cc, cc.Entry, requestID, 0)
		if err != nil {
			return ControlResult{}, err
		}
		request.ToolTransition = &transition
	}
	created, err := x.Collaboration.CreateAgentRequest(ctx, request)
	if err != nil {
		if errors.Is(err, ErrSteeringArrived) {
			return x.refuseIfEntry(ctx, cc, controlErrorResult(ToolResultRefused, controlNewMessageCode,
				"A new message arrived; read it first.", nil))
		}
		if delegationRejection(err) {
			return x.rejectDelegation(ctx, cc, err.Error())
		}
		return ControlResult{}, err
	}
	if created == nil || created.Request == nil {
		return ControlResult{}, errors.New("delegation returned no durable Agent request")
	}
	if created.Source == nil {
		// Idempotency replay: the request was created by an earlier attempt.
		return x.replayDelegation(ctx, cc, created.Request)
	}
	entry, err := x.reloadControlEntry(ctx, cc)
	if err != nil {
		return ControlResult{}, err
	}
	park, err := delegationCommittedPark(created.Source, spec.AgentID)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlParked, Run: created.Source, Entry: entry, Park: park}, nil
}

func delegationIdempotencyKey(runID, stepID string) string {
	return strings.Join([]string{"delegation", runID, stepID}, ":")
}

// delegationSharedContext is the request's shared context: the caller's
// context, the delegation mode, an explicitly requested entrypoint, the
// Runbook origin and the forwarded Runbook invocation.
func delegationSharedContext(run *AgentRun, spec DelegationSpec) (map[string]interface{}, error) {
	shared := cloneMap(spec.Context)
	if shared == nil {
		shared = make(map[string]interface{})
	}
	if spec.Mode != "" {
		shared[DelegationModeContextKey] = string(spec.Mode)
	}
	requestedEntrypoint, err := requestedAcceptedChildEntrypoint(shared)
	if err != nil {
		return nil, err
	}
	if requestedEntrypoint != "" {
		shared["runbookEntrypoint"] = requestedEntrypoint
	}
	if origin := delegatedRunbookOrigin(run.Context); origin != nil {
		if entrypoint := strings.TrimSpace(run.Entrypoint); entrypoint != "" {
			origin["runbookEntrypoint"] = entrypoint
		}
		shared["triggerInput"] = origin
	}
	if invocation, ok := run.Context[RunbookInvocationContextKey].(map[string]interface{}); ok {
		// Explicit arguments stay shareable; the original chat message goes
		// along only when the same Agent continues its own operation. Only this
		// delegation's explicit target method can select an entrypoint.
		forwarded := forwardRunbookInvocation(invocation, spec.AgentID == run.AssignedAgentID)
		delete(forwarded, "entrypoint")
		if requestedEntrypoint != "" {
			forwarded["entrypoint"] = requestedEntrypoint
		}
		shared[RunbookInvocationContextKey] = forwarded
	}
	return shared, nil
}

// delegationBudget is the child budget: unbounded under an unbounded source;
// under a bounded source the minimum child budget (model) or the producer's
// explicit allocation (kernel), completed to the remaining capacity.
func (x *ControlExecutor) delegationBudget(cc ControlContext, spec DelegationSpec) (*BudgetPolicy, error) {
	if cc.Run.Budget == nil {
		return nil, nil
	}
	allocation := spec.Budget
	if cc.Origin != ControlOriginKernel || allocation == nil {
		allocations, err := controlBranchBudgets(cc.Run, stepIDOf(cc.Step), 1)
		if err != nil {
			return nil, err
		}
		allocation = allocations[0]
	}
	return completeChildBudgetAllocation(cc.Run, allocation)
}

// delegationRejection reports errors that refuse this delegation (policy,
// authority, validation) rather than abort the claim.
func delegationRejection(err error) bool {
	for _, target := range []error{
		ErrAgentRequestUnauthorized, ErrAgentRequestAssignment, ErrInvalidAgentRequestState,
		ErrInvalidConversationTask, ErrInvalidArtifact,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (x *ControlExecutor) rejectDelegation(ctx context.Context, cc ControlContext, message string) (ControlResult, error) {
	return x.refuseIfEntry(ctx, cc, controlErrorResult(ToolResultRefused, controlDelegationRejectedCode, message, nil))
}

// refuseIfEntry is refuse for calls that may have no entry (kernel origin):
// without an entry the refusal is returned as a Go error.
func (x *ControlExecutor) refuseIfEntry(ctx context.Context, cc ControlContext, result *ToolResult) (ControlResult, error) {
	if cc.Entry == nil {
		return ControlResult{}, fmt.Errorf("%w: %s", ErrInvalidAgentRequestState, controlResultMessage(result))
	}
	return x.refuse(ctx, cc, result)
}

func controlResultMessage(result *ToolResult) string {
	for _, part := range result.Content {
		var value struct{ Error, Message string }
		if part.Type == ContentPartJSON && json.Unmarshal(part.Value, &value) == nil {
			return value.Error + ": " + value.Message
		}
	}
	return "refused"
}

// delegateEntryTransition moves entry to awaiting_child on requestID. It is
// sent inside the request create (delegate) or applied directly (reply,
// replay).
func (x *ControlExecutor) delegateEntryTransition(cc ControlContext, entry *SessionEntry, requestID string, requestRevision int64) (ToolEntryTransitionRequest, error) {
	meta := delegateEntryMeta{
		ControlEntryMeta: ControlEntryMeta{Kind: ControlToolName(entry.Tool.Name), RequestID: requestID},
		RequestRevision:  requestRevision,
	}
	encoded, err := json.Marshal(&meta)
	if err != nil {
		return ToolEntryTransitionRequest{}, fmt.Errorf("encode delegation metadata: %w", err)
	}
	now := x.now()
	return ToolEntryTransitionRequest{
		Scope: cc.Run.Scope, EntryID: entry.ID, RunID: cc.Run.ID, Lease: cc.lease(now),
		From: entry.State, ExpectedRevision: entry.Revision, To: ToolEntryAwaitingChild,
		Wait:   &ToolEntryWait{AgentRequestID: requestID},
		Kernel: &ToolEntryKernelPatch{Control: encoded}, Now: now,
	}, nil
}

func (x *ControlExecutor) awaitDelegation(ctx context.Context, cc ControlContext, requestID string, requestRevision int64) (*SessionEntry, error) {
	transition, err := x.delegateEntryTransition(cc, cc.Entry, requestID, requestRevision)
	if err != nil {
		return nil, err
	}
	transitioned, err := x.Sessions.TransitionToolEntry(ctx, transition)
	if err != nil {
		return nil, err
	}
	return transitioned.Entry, nil
}

func (x *ControlExecutor) reloadControlEntry(ctx context.Context, cc ControlContext) (*SessionEntry, error) {
	if cc.Entry == nil {
		return nil, nil
	}
	return x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, cc.Entry.ID)
}

// delegationCommittedPark is the park the request transaction already
// applied to source.
func delegationCommittedPark(source *AgentRun, agentID string) (*LoopPark, error) {
	if source == nil || source.WakeCondition == nil {
		return nil, errors.New("delegation did not park its source Run")
	}
	return &LoopPark{
		Status: source.Status, Wake: cloneWakeCondition(source.WakeCondition),
		Summary: "Delegated work to Agent " + agentID, Committed: true,
	}, nil
}

// replayDelegation handles a request that already exists under the stable
// key while the source is still running: the request resolved (or the entry
// was left behind) before a crash. The entry is moved onto the request and
// resumed.
func (x *ControlExecutor) replayDelegation(ctx context.Context, cc ControlContext, request *AgentRequest) (ControlResult, error) {
	if cc.Entry == nil {
		resumed, err := x.delegationOutcome(ctx, cc, request)
		if err != nil {
			return ControlResult{}, err
		}
		if !resumed.Resolved {
			return ControlResult{Kind: ControlParked, Park: resumed.Park}, nil
		}
		return ControlResult{Kind: ControlContinue, Run: cc.Run}, nil
	}
	entry, err := x.reloadControlEntry(ctx, cc)
	if err != nil {
		return ControlResult{}, err
	}
	if entry.State == ToolEntryPending || entry.State == ToolEntryRunning {
		cc.Entry = entry
		if entry, err = x.awaitDelegation(ctx, cc, request.ID, 0); err != nil {
			return ControlResult{}, err
		}
	}
	cc.Entry = entry
	if entry.State != ToolEntryAwaitingChild {
		return ControlResult{}, fmt.Errorf("%w: delegation entry %s is %s", ErrControlEntryState, entry.ID, entry.State)
	}
	resumed, err := x.delegationOutcome(ctx, cc, request)
	if err != nil {
		return ControlResult{}, err
	}
	if !resumed.Resolved {
		return ControlResult{Kind: ControlParked, Entry: entry, Park: resumed.Park}, nil
	}
	return resumed.Result, nil
}

// ---------------------------------------------------------------------------
// agent_delegate_reply

func (delegateReplyHandler) spec(ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: "Answer the clarification question an Agent asked about work you delegated with agent_delegate, " +
			"and wait for that Agent again.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"requestId", "answer"},
			"properties": map[string]interface{}{
				"requestId": map[string]interface{}{"type": "string"},
				"answer":    map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumDelegateAnswerBytes},
			},
		},
	}
}

func (delegateReplyHandler) decode(raw json.RawMessage) (interface{}, error) {
	value, err := decodeControlInto[delegateReplyArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*delegateReplyArgs)
	args.RequestID = strings.TrimSpace(args.RequestID)
	args.Answer = strings.TrimSpace(args.Answer)
	if args.RequestID == "" {
		return nil, errors.New("requestId must not be empty")
	}
	if args.Answer == "" {
		return nil, errors.New("answer must not be empty")
	}
	if len(args.Answer) > maximumDelegateAnswerBytes || !utf8.ValidString(args.Answer) {
		return nil, fmt.Errorf("answer must be valid text of at most %d bytes", maximumDelegateAnswerBytes)
	}
	return args, nil
}

func (delegateReplyHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	if x.Collaboration == nil {
		return ControlResult{}, errors.New("delegation needs the collaboration service")
	}
	args := cc.Arguments.(*delegateReplyArgs)
	request, err := x.Collaboration.GetAgentRequest(ctx, cc.Run.Scope, args.RequestID)
	if err != nil && !errors.Is(err, ErrAgentRequestNotFound) {
		return ControlResult{}, err
	}
	if request == nil || request.SourceRunID != cc.Run.ID {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, controlRequestNotAwaitingCode,
			fmt.Sprintf("Request %s is not one of your delegations.", args.RequestID), nil))
	}
	if request.Status != AgentRequestStatusClarificationRequested {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, controlRequestNotAwaitingCode,
			fmt.Sprintf("Request %s is %s.", request.ID, request.Status), nil))
	}
	entry, err := x.awaitDelegation(ctx, cc, request.ID, request.Revision)
	if err != nil {
		return ControlResult{}, err
	}
	cc.Entry = entry
	return x.answerClarification(ctx, cc, request, args.Answer)
}

// answerClarification records the answer on the request, which parks the
// source on the recipient's next decision (a committed park). The entry is
// already awaiting_child.
func (x *ControlExecutor) answerClarification(ctx context.Context, cc ControlContext, request *AgentRequest, answer string) (ControlResult, error) {
	answered, err := x.Collaboration.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: cc.Run.Scope, RequestID: request.ID, ExpectedRevision: request.Revision,
		Decision: AgentRequestDecisionProvideClarification, Principal: request.Requester, Message: answer,
	})
	if err != nil {
		if delegationRejection(err) {
			entry, transitionErr := x.transitionControlEntry(ctx, cc, ToolEntryError, nil,
				controlErrorResult(ToolResultRefused, controlDelegationRejectedCode, err.Error(), nil), nil)
			if transitionErr != nil {
				return ControlResult{}, transitionErr
			}
			return ControlResult{Kind: ControlContinue, Entry: entry}, nil
		}
		return ControlResult{}, err
	}
	if answered == nil || answered.Source == nil {
		return ControlResult{}, errors.New("delegation answer did not return the waiting source Run")
	}
	park, err := delegationCommittedPark(answered.Source, request.Recipient.ID)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlParked, Run: answered.Source, Entry: cc.Entry, Park: park}, nil
}

func (delegateReplyHandler) resume(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResume, error) {
	return x.resumeDelegation(ctx, cc)
}

// ---------------------------------------------------------------------------
// Resume (§8.4)

// resumeDelegation turns the request of an awaiting_child delegate or reply
// entry into its tool result, or re-parks while the request is open. A reply
// whose answer was never recorded (crash between the entry transition and the
// answer) records it now.
func (x *ControlExecutor) resumeDelegation(ctx context.Context, cc ControlContext) (ControlResume, error) {
	if x.Collaboration == nil {
		return ControlResume{}, errors.New("delegation needs the collaboration service")
	}
	if cc.Entry.State != ToolEntryAwaitingChild {
		return ControlResume{}, fmt.Errorf("%w: delegation resume of %s in state %s", ErrControlEntryState, cc.Entry.ID, cc.Entry.State)
	}
	meta, err := decodeDelegateEntryMeta(cc.Entry)
	if err != nil {
		return ControlResume{}, err
	}
	requestID := meta.RequestID
	if requestID == "" && cc.Entry.Tool.Wait != nil {
		requestID = cc.Entry.Tool.Wait.AgentRequestID
	}
	if requestID == "" {
		return ControlResume{}, fmt.Errorf("delegation entry %s names no request", cc.Entry.ID)
	}
	request, err := x.Collaboration.GetAgentRequest(ctx, cc.Run.Scope, requestID)
	if err != nil {
		return ControlResume{}, err
	}
	if cc.toolName() == ControlToolDelegateReply && request.Status == AgentRequestStatusClarificationRequested &&
		request.Revision == meta.RequestRevision {
		args := cc.Arguments.(*delegateReplyArgs)
		result, err := x.answerClarification(ctx, cc, request, args.Answer)
		if err != nil {
			return ControlResume{}, err
		}
		if result.Kind == ControlParked {
			return ControlResume{Park: result.Park}, nil
		}
		return ControlResume{Resolved: true, Result: result}, nil
	}
	return x.delegationOutcome(ctx, cc, request)
}

func decodeDelegateEntryMeta(entry *SessionEntry) (delegateEntryMeta, error) {
	var meta delegateEntryMeta
	if entry == nil || entry.Tool == nil || len(entry.Tool.Control) == 0 {
		return meta, nil
	}
	if err := json.Unmarshal(entry.Tool.Control, &meta); err != nil {
		return meta, fmt.Errorf("decode control metadata of tool entry %s: %w", entry.ID, err)
	}
	return meta, nil
}

// delegationOutcome applies the §8.4 table to request. With no entry
// (kernel origin) nothing is transitioned.
func (x *ControlExecutor) delegationOutcome(ctx context.Context, cc ControlContext, request *AgentRequest) (ControlResume, error) {
	agentID := request.AssignedAgentID
	if agentID == "" {
		agentID = request.Recipient.ID
	}
	base := map[string]interface{}{"requestId": request.ID, "agentId": agentID}
	with := func(fields map[string]interface{}) map[string]interface{} {
		value := map[string]interface{}{}
		for key, v := range base {
			value[key] = v
		}
		for key, v := range fields {
			value[key] = v
		}
		return value
	}
	var result *ToolResult
	switch request.Status {
	case AgentRequestStatusPending:
		return ControlResume{Park: &LoopPark{
			Status: AgentRunStatusWaitingForAgent, Wake: &WakeCondition{Type: "agent_request_decision", Reference: request.ID},
			Summary: "Waiting for Agent " + agentID,
		}}, nil
	case AgentRequestStatusAccepted, AgentRequestStatusCompletionReview:
		return ControlResume{Park: &LoopPark{
			Status: AgentRunStatusWaitingForDependency, Wake: &WakeCondition{Type: "agent_request", Reference: request.ID},
			Summary: "Waiting for Agent " + agentID,
		}}, nil
	case AgentRequestStatusCompleted:
		artifacts := cloneArtifactReferences(request.Artifacts)
		if artifacts == nil {
			artifacts = []ArtifactReference{}
		}
		runID := request.ChildRunID
		if runID == "" {
			runID = cc.Run.ID
		}
		result = controlJSONResult(ToolResultSucceeded, with(map[string]interface{}{
			"status": "completed", "answer": truncateControlText(delegationAnswer(cc.Run, request), runID), "artifacts": artifacts,
		}))
	case AgentRequestStatusClarificationRequested:
		result = controlJSONResult(ToolResultSucceeded, with(map[string]interface{}{
			"status": "clarification_requested", "question": truncateControlText(request.Clarification, cc.Run.ID),
			"message": "Answer with agent_delegate_reply, or continue without this Agent.",
		}))
	case AgentRequestStatusRejected:
		result = controlJSONResult(ToolResultFailed, with(map[string]interface{}{
			"status": "rejected", "reason": truncateControlText(request.Response, cc.Run.ID),
		}))
	case AgentRequestStatusFailed, AgentRequestStatusCanceled:
		result = controlJSONResult(ToolResultFailed, with(map[string]interface{}{
			"status": string(request.Status), "reason": truncateControlText(request.ResolutionReason, cc.Run.ID),
		}))
	default:
		return ControlResume{}, fmt.Errorf("delegation request %s has unknown status %q", request.ID, request.Status)
	}
	if cc.Entry == nil {
		return ControlResume{Resolved: true, Result: ControlResult{Kind: ControlContinue, Run: cc.Run}}, nil
	}
	to := ToolEntryCompleted
	if result.IsError {
		to = ToolEntryError
	}
	entry, err := x.transitionControlEntry(ctx, cc, to, nil, result, nil)
	if err != nil {
		return ControlResume{}, err
	}
	return ControlResume{Resolved: true, Result: ControlResult{Kind: ControlContinue, Entry: entry}}, nil
}

// delegationAnswer is the delegated Agent's answer: the child's report or
// summary as recorded on the source Run, else the request's completion
// summary.
func delegationAnswer(run *AgentRun, request *AgentRequest) string {
	if run != nil {
		results, _ := run.Output["collaborationResults"].(map[string]interface{})
		recorded, _ := results[request.ID].(map[string]interface{})
		output, _ := recorded["output"].(map[string]interface{})
		for _, key := range []string{"report", "summary"} {
			if text, ok := output[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return strings.TrimSpace(request.CompletionSummary)
}
