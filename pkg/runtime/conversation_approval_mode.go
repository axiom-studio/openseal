package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

const (

	// ConversationApprovalModeChangedEvent is the activity event recorded when
	// a conversation's approval mode changes. Its payload is {from, to}.
	ConversationApprovalModeChangedEvent = "conversation.approval_mode_changed"
)

// ApprovalModeAllowsAction reports whether mode lets an action with this
// classification proceed without an approval checkpoint. An empty mode (a Run
// that acts for no conversation) behaves like manual. An action whose manifest
// declares review: always keeps its checkpoint in every mode, including skip.
func ApprovalModeAllowsAction(mode ConversationApprovalMode, review skill.ActionReview, risk skill.RiskLevel, sideEffect skill.SideEffect) bool {
	if review == skill.ActionReviewAlways {
		return false
	}
	switch mode {
	case ConversationApprovalSkip:
		return true
	case ConversationApprovalAuto:
		return (risk == skill.RiskLevelRead || risk == skill.RiskLevelWrite) &&
			(sideEffect == skill.SideEffectNone || sideEffect == skill.SideEffectRead || sideEffect == skill.SideEffectWrite)
	default:
		return false
	}
}

type agentRunReader interface {
	GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error)
}

type conversationReader interface {
	GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error)
}

// RunConversationID returns the canonical conversation a Run acts for: the
// Run's own conversation context, else its root Run's. This is the same
// projection approval listings use to place a checkpoint in a conversation.
func RunConversationID(ctx context.Context, runs agentRunReader, run *AgentRun) (string, error) {
	if run == nil {
		return "", nil
	}
	if id, _ := run.Context[conversationRunContextConversationID].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id), nil
	}
	rootID := strings.TrimSpace(run.RootRunID)
	if rootID == "" || rootID == run.ID || runs == nil {
		return "", nil
	}
	root, err := runs.GetAgentRun(ctx, run.Scope, rootID)
	if errors.Is(err, ErrRunNotFound) || err == nil && root == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id, _ := root.Context[conversationRunContextConversationID].(string)
	return strings.TrimSpace(id), nil
}

// RunConversationApprovalMode resolves the approval mode that governs a Run.
// A Run that acts for no stored conversation has no mode (manual behavior).
// An event routine occurrence, and any Run under one, is always manual
// whatever the conversation's mode: outside events must not widen what runs
// without asking, so only the routine's grants run unasked.
func RunConversationApprovalMode(ctx context.Context, runs agentRunReader, conversations conversationReader, run *AgentRun) (string, ConversationApprovalMode, error) {
	conversationID, err := RunConversationID(ctx, runs, run)
	if err != nil {
		return conversationID, "", err
	}
	if routineEventOccurrenceRun(run) {
		return conversationID, ConversationApprovalManual, nil
	}
	if conversationID == "" || conversations == nil {
		return conversationID, "", nil
	}
	conversation, err := conversations.GetConversation(ctx, run.Scope, conversationID)
	if errors.Is(err, ErrConversationNotFound) || err == nil && conversation == nil {
		return conversationID, "", nil
	}
	if err != nil {
		return conversationID, "", err
	}
	return conversationID, conversationApprovalModeOrDefault(conversation.ApprovalMode), nil
}

// routineEventOccurrenceRun reports whether run is an event routine
// occurrence or runs under one. Child Runs inherit routineEventBatchId from
// their source (inheritRoutineEventBatch), so this reads only run.Context and
// never the store.
func routineEventOccurrenceRun(run *AgentRun) bool {
	return run != nil && routineEventBatchOf(run) != ""
}

// inheritRoutineEventBatch gives a child Run its source's routineEventBatchId,
// and drops any value the child's caller-supplied context tried to set, so
// only a real occurrence and the Runs under it carry the key.
func inheritRoutineEventBatch(source *AgentRun, context map[string]interface{}) map[string]interface{} {
	batch := ""
	if source != nil {
		batch = routineEventBatchOf(source)
	}
	if batch == "" {
		delete(context, RoutineEventBatchContextKey)
		return context
	}
	if context == nil {
		context = map[string]interface{}{}
	}
	context[RoutineEventBatchContextKey] = batch
	return context
}

func routineEventBatchOf(run *AgentRun) string {
	batch, _ := run.Context[RoutineEventBatchContextKey].(string)
	return strings.TrimSpace(batch)
}

func conversationApprovalModeOrDefault(mode ConversationApprovalMode) ConversationApprovalMode {
	if mode == "" {
		return DefaultConversationApprovalMode
	}
	return mode
}

type SetConversationApprovalModeRequest struct {
	Scope            Scope
	ConversationID   string
	ExpectedRevision int64
	Mode             ConversationApprovalMode
	Actor            ActivityActor
	CorrelationID    string
}

type SetConversationApprovalModeResult struct {
	Conversation *Conversation `json:"conversation"`
	// Changed is false when the conversation already had the requested mode.
	Changed bool           `json:"changed"`
	Event   *ActivityEvent `json:"event,omitempty"`
	// ApprovedApprovalIDs lists pending approvals in the conversation that the
	// new mode allows and that this request approved.
	ApprovedApprovalIDs []string `json:"approvedApprovalIds"`
	// ApprovalSweepIncomplete reports that some eligible approvals could not be
	// resolved; setting the same mode again retries the sweep.
	ApprovalSweepIncomplete bool `json:"approvalSweepIncomplete,omitempty"`
}

// ConversationApprovalModeService is the governed update of a conversation's
// approval mode. Hosts authorize the caller (a principal allowed to execute
// the conversation's Agent) before calling it.
type ConversationApprovalModeService struct {
	conversations ConversationStore
	modes         ConversationApprovalModeStore
	actions       ActionStore
	runs          agentRunReader
	approvals     *ApprovalCoordinator
	now           func() time.Time
	newID         func() string
}

func NewConversationApprovalModeService(store ConversationApprovalModeKernelStore) *ConversationApprovalModeService {
	return &ConversationApprovalModeService{
		conversations: store, modes: store, actions: store, runs: store,
		// Approval modes never approve an agent-builder card (D11): those
		// actions are Review always, and the coordinator refuses the approve.
		approvals: NewApprovalCoordinator(store, store, nil), now: time.Now, newID: uuid.NewString,
	}
}

const approvalModeSweepPageSize = 100

func (s *ConversationApprovalModeService) SetApprovalMode(ctx context.Context, req SetConversationApprovalModeRequest) (*SetConversationApprovalModeResult, error) {
	if s == nil || s.conversations == nil || s.modes == nil || s.actions == nil || s.runs == nil || s.approvals == nil {
		return nil, errors.New("conversation approval mode service is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !req.Mode.Valid() {
		return nil, fmt.Errorf("%w: approval mode must be manual, auto, or skip", ErrInvalidConversation)
	}
	if req.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: expected revision must be positive", ErrInvalidConversation)
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" {
		return nil, fmt.Errorf("%w: approval mode actor is required", ErrInvalidConversation)
	}
	conversation, err := s.conversations.GetConversation(ctx, req.Scope, strings.TrimSpace(req.ConversationID))
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrConversationNotFound
	}
	if conversation.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	previous := conversationApprovalModeOrDefault(conversation.ApprovalMode)
	result := &SetConversationApprovalModeResult{Conversation: cloneConversation(conversation), ApprovedApprovalIDs: []string{}}
	if previous != req.Mode {
		if conversation.Status != ConversationStatusActive {
			return nil, fmt.Errorf("%w: approval mode can only change on an active conversation", ErrInvalidConversation)
		}
		now := s.now().UTC()
		updated := cloneConversation(conversation)
		updated.ApprovalMode = req.Mode
		updated.Revision++
		updated.UpdatedAt = now
		if err := updated.Validate(); err != nil {
			return nil, err
		}
		event := &ActivityEvent{
			ID: s.newID(), Scope: req.Scope, EventType: ConversationApprovalModeChangedEvent, Severity: ActivitySeverityInfo,
			ConversationRefs: []string{updated.ID}, Actor: req.Actor, Visibility: ActivityVisibilityScope,
			Summary:       fmt.Sprintf("Approval mode changed from %s to %s", previous, req.Mode),
			Payload:       map[string]interface{}{"conversationId": updated.ID, "from": string(previous), "to": string(req.Mode)},
			CorrelationID: req.CorrelationID, CreatedAt: now,
		}
		switch updated.Owner.Type {
		case OwnerTypeAgent:
			event.AgentID = updated.Owner.ID
		case OwnerTypeTeam:
			event.TeamID = updated.Owner.ID
		}
		committed, persisted, err := s.modes.UpdateConversationWithEvent(ctx, updated, conversation.Revision, event)
		if err != nil {
			return nil, err
		}
		result.Conversation, result.Changed, result.Event = committed, true, persisted
	}
	// Tightening never touches pending approvals: they already require review.
	if req.Mode.Rank() < previous.Rank() {
		return result, nil
	}
	approved, incomplete := s.sweepPendingApprovals(ctx, req.Scope, result.Conversation.ID, req.Mode, req.CorrelationID)
	result.ApprovedApprovalIDs, result.ApprovalSweepIncomplete = approved, incomplete
	return result, nil
}

// sweepPendingApprovals approves the conversation's pending checkpoints that
// mode allows. Each resolution is revision-checked; a checkpoint a human (or
// another sweep) resolved first is skipped. The conversation mode is re-read
// before every resolution so a concurrent tightening stops the sweep.
func (s *ConversationApprovalModeService) sweepPendingApprovals(ctx context.Context, scope Scope, conversationID string, mode ConversationApprovalMode, correlationID string) ([]string, bool) {
	pending := make([]*ApprovalCheckpoint, 0)
	for offset := 0; ; offset += approvalModeSweepPageSize {
		page, err := s.actions.ListApprovals(ctx, ApprovalFilter{
			Scope: scope, ConversationID: conversationID, Status: []ApprovalStatus{ApprovalStatusPending},
			Limit: approvalModeSweepPageSize, Offset: offset,
		})
		if err != nil {
			return []string{}, true
		}
		pending = append(pending, page...)
		if len(page) < approvalModeSweepPageSize {
			break
		}
	}
	approved := make([]string, 0)
	incomplete := false
	for _, approval := range pending {
		call, err := s.actions.GetActionCall(ctx, scope, approval.ActionCallID)
		if err != nil || call == nil {
			incomplete = true
			continue
		}
		if call.Status != ActionCallStatusWaitingApproval || !ApprovalModeAllowsAction(mode, call.Review, call.Risk, call.SideEffect) {
			continue
		}
		// An event routine occurrence stays manual whatever the chat's mode,
		// so loosening the mode never approves its checkpoints.
		run, err := s.runs.GetAgentRun(ctx, scope, call.RunID)
		if errors.Is(err, ErrRunNotFound) || err == nil && run == nil {
			continue
		}
		if err != nil {
			incomplete = true
			continue
		}
		if routineEventOccurrenceRun(run) {
			continue
		}
		current, err := s.conversations.GetConversation(ctx, scope, conversationID)
		if err != nil || current == nil {
			return approved, true
		}
		if !ApprovalModeAllowsAction(conversationApprovalModeOrDefault(current.ApprovalMode), call.Review, call.Risk, call.SideEffect) {
			continue
		}
		result, err := s.approvals.ResolveByApprovalMode(ctx, scope, approval.ID, approval.Revision, mode, correlationID)
		switch {
		case err == nil:
			if result != nil && result.Resolved {
				approved = append(approved, approval.ID)
			}
		case errors.Is(err, ErrApprovalResolved), errors.Is(err, ErrRevisionConflict), errors.Is(err, ErrApprovalExpired),
			errors.Is(err, ErrInvalidRunTransition), errors.Is(err, ErrAgentBuilderApprovalForbidden):
			// A concurrent decision, expiry, or paused Run owns this checkpoint,
			// or it is an agent-builder card only a person approves (D11).
		default:
			incomplete = true
		}
	}
	return approved, incomplete
}
