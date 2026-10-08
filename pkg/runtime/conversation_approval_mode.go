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

// ConversationApprovalMode is the per-conversation review boundary the kernel
// applies to side-effecting actions proposed by Runs acting for that
// conversation. It is enforced by the action policy, never by a client.
type ConversationApprovalMode string

const (
	// ConversationApprovalManual requires approval for every side-effecting action.
	ConversationApprovalManual ConversationApprovalMode = "manual"
	// ConversationApprovalAuto allows read and write risk actions; external,
	// production, and destructive actions still require approval.
	ConversationApprovalAuto ConversationApprovalMode = "auto"
	// ConversationApprovalSkip allows every action without approval. It also
	// means the agent completes browser tasks end to end (signing in, checking
	// out, placing orders, accepting terms, sending) instead of handing off.
	// The action policy remains the authority for each action.
	ConversationApprovalSkip ConversationApprovalMode = "skip"

	DefaultConversationApprovalMode = ConversationApprovalAuto

	// ConversationApprovalModeChangedEvent is the activity event recorded when
	// a conversation's approval mode changes. Its payload is {from, to}.
	ConversationApprovalModeChangedEvent = "conversation.approval_mode_changed"
)

func (m ConversationApprovalMode) Valid() bool {
	switch m {
	case ConversationApprovalManual, ConversationApprovalAuto, ConversationApprovalSkip:
		return true
	}
	return false
}

// rank orders modes from strictest to loosest.
func (m ConversationApprovalMode) rank() int {
	switch m {
	case ConversationApprovalAuto:
		return 1
	case ConversationApprovalSkip:
		return 2
	default:
		return 0
	}
}

// ApprovalModeAllowsAction reports whether mode lets an action with this
// classification proceed without an approval checkpoint. An empty mode (a Run
// that acts for no conversation) behaves like manual.
func ApprovalModeAllowsAction(mode ConversationApprovalMode, risk skill.RiskLevel, sideEffect skill.SideEffect) bool {
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

// ApprovalModeExemptAction reports kernel actions that keep an explicit review
// in every approval mode. Agent behavior amendments change the durable worker
// for every conversation, so a conversation-scoped mode cannot waive them.
func ApprovalModeExemptAction(skillID, action string) bool {
	return skillID == AgentManagementSkillID && (action == AgentActionAmendBehavior || action == AgentActionConfigureChannel)
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
func RunConversationApprovalMode(ctx context.Context, runs agentRunReader, conversations conversationReader, run *AgentRun) (string, ConversationApprovalMode, error) {
	conversationID, err := RunConversationID(ctx, runs, run)
	if err != nil || conversationID == "" || conversations == nil {
		return conversationID, "", err
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

func conversationApprovalModeOrDefault(mode ConversationApprovalMode) ConversationApprovalMode {
	if mode == "" {
		return DefaultConversationApprovalMode
	}
	return mode
}

// applyConversationDefaults fills fields introduced after a conversation was
// first persisted, so every stored and returned conversation is explicit.
func applyConversationDefaults(conversation *Conversation) {
	if conversation != nil && conversation.ApprovalMode == "" {
		conversation.ApprovalMode = DefaultConversationApprovalMode
	}
}

// ConversationApprovalModeStore atomically commits a conversation revision
// together with the activity event that records it.
type ConversationApprovalModeStore interface {
	UpdateConversationWithEvent(ctx context.Context, conversation *Conversation, expectedRevision int64, event *ActivityEvent) (*Conversation, *ActivityEvent, error)
}

func validateConversationEvent(conversation *Conversation, event *ActivityEvent) error {
	if err := conversation.Validate(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if event.Scope != conversation.Scope || event.RunID != "" || event.ObjectiveID != "" || event.ProjectID != "" ||
		activityConversationSubject(event) != conversation.ID {
		return fmt.Errorf("%w: conversation event must be recorded against the updated conversation", ErrInvalidConversation)
	}
	return nil
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
	approvals     *ApprovalCoordinator
	now           func() time.Time
	newID         func() string
}

type ConversationApprovalModeKernelStore interface {
	ConversationStore
	ConversationApprovalModeStore
	PortfolioStore
	ActionStore
}

func NewConversationApprovalModeService(store ConversationApprovalModeKernelStore) *ConversationApprovalModeService {
	return &ConversationApprovalModeService{
		conversations: store, modes: store, actions: store,
		approvals: NewApprovalCoordinator(store, store, nil), now: time.Now, newID: uuid.NewString,
	}
}

const approvalModeSweepPageSize = 100

func (s *ConversationApprovalModeService) SetApprovalMode(ctx context.Context, req SetConversationApprovalModeRequest) (*SetConversationApprovalModeResult, error) {
	if s == nil || s.conversations == nil || s.modes == nil || s.actions == nil || s.approvals == nil {
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
	if req.Mode.rank() < previous.rank() {
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
		if call.Status != ActionCallStatusWaitingApproval || ApprovalModeExemptAction(call.SkillID, call.Action) ||
			!ApprovalModeAllowsAction(mode, call.Risk, call.SideEffect) {
			continue
		}
		current, err := s.conversations.GetConversation(ctx, scope, conversationID)
		if err != nil || current == nil {
			return approved, true
		}
		if !ApprovalModeAllowsAction(conversationApprovalModeOrDefault(current.ApprovalMode), call.Risk, call.SideEffect) {
			continue
		}
		result, err := s.approvals.ResolveByApprovalMode(ctx, scope, approval.ID, approval.Revision, mode, correlationID)
		switch {
		case err == nil:
			if result != nil && result.Resolved {
				approved = append(approved, approval.ID)
			}
		case errors.Is(err, ErrApprovalResolved), errors.Is(err, ErrRevisionConflict), errors.Is(err, ErrApprovalExpired),
			errors.Is(err, ErrInvalidRunTransition):
			// A concurrent decision, expiry, or paused Run owns this checkpoint.
		default:
			incomplete = true
		}
	}
	return approved, incomplete
}
