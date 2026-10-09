package runtime

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const ConversationTaskForegroundTimeout = 15 * time.Second

const conversationTaskContinuationKey = "foreground-timeout"

// ConversationTaskPromotionRequest is a trusted kernel request. Identity and
// execution state are read again under the store's transaction or lock.
type ConversationTaskPromotionRequest struct {
	Scope Scope
	RunID string
	Now   time.Time
	// Zero uses the production policy; only the fixed timeout is accepted.
	// Tests control Now and CreatedAt instead of weakening durable proof.
	MinimumAge time.Duration
}

func (r ConversationTaskPromotionRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(r.RunID, 128) || r.Now.IsZero() || r.MinimumAge != 0 && r.MinimumAge != ConversationTaskForegroundTimeout {
		return ErrInvalidConversationTask
	}
	return nil
}

type ConversationTaskPromotionStore interface {
	PromoteConversationTask(context.Context, ConversationTaskPromotionRequest) (*ConversationTaskResult, error)
}

// ConversationTaskDueFilter pages only live, unpromoted conversation roots in
// ascending admission order. A keyset cursor makes busy scopes bounded.
type ConversationTaskDueFilter struct {
	Scope           Scope
	BeforeCreatedAt time.Time
	Limit           int
	AfterCreatedAt  *time.Time
	AfterID         string
}

func (f ConversationTaskDueFilter) Validate() error {
	if err := f.Scope.Validate(); err != nil {
		return err
	}
	if f.BeforeCreatedAt.IsZero() || f.Limit < 1 || f.Limit > 500 ||
		(f.AfterCreatedAt == nil) != (f.AfterID == "") ||
		f.AfterCreatedAt != nil && (f.AfterCreatedAt.IsZero() || !validOpaqueIdentifier(f.AfterID, 128)) {
		return ErrInvalidConversationTask
	}
	return nil
}

type ConversationTaskDueStore interface {
	ListDueConversationTaskRuns(context.Context, ConversationTaskDueFilter) ([]*AgentRun, error)
}

func ConversationTaskAcknowledgmentMessageID(task *ConversationTask) string {
	if task == nil {
		return ""
	}
	return stableConversationID(task.Scope, "conversation-task-handoff:"+task.ID, "message")
}

func buildConversationTaskAcknowledgment(task *ConversationTask, conversation *Conversation, source *ChannelMessage) (ChannelMessageCommitRecord, error) {
	sender := ConversationParticipant{Type: ConversationParticipantAgent, ID: task.TargetAgentID}
	if sender.ID == "" {
		sender = ConversationParticipant{Type: ConversationParticipantTeam, ID: task.Owner.ID}
	}
	message := &ChannelMessage{ID: ConversationTaskAcknowledgmentMessageID(task), Scope: task.Scope, ConversationID: task.ConversationID,
		Sequence: conversation.LastSequence + 1, Sender: sender, Intent: MessageIntentUpdate, Content: task.Acknowledgment,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: source.ID, ThreadRootID: task.ThreadRootID,
		BroadcastToChannel: source.ThreadRootID == "" || source.BroadcastToChannel, ResponseMode: source.ResponseMode,
		References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: task.WorkRunID}, {Kind: ConversationReferenceTask, ID: task.ID}},
		IdempotencyKey: "conversation-task-handoff:" + task.ID, CreatedAt: task.CreatedAt}
	updated := cloneConversation(conversation)
	updated.LastSequence = message.Sequence
	updated.Revision++
	updated.UpdatedAt = task.CreatedAt
	record := ChannelMessageCommitRecord{Conversation: updated, Message: message, ExpectedRevision: conversation.Revision}
	return record, validateChannelMessageCommitRecord(record)
}

func conversationTaskContinuationCandidate(run *AgentRun) bool {
	return run != nil && run.Kind == RunKindConversation && run.Source == RunSourceChat &&
		run.ParentRunID == "" && run.RootRunID == run.ID && !isTerminalAgentRunStatus(run.Status) &&
		run.Context[ConversationTaskContextKey] == nil
}

func conversationTaskFinalResponseKey(run *AgentRun) string {
	trigger, _ := run.Context[conversationRunContextTriggerID].(string)
	return conversationRunResponseKey(run, trigger)
}

// conversationRunResponseKey names one Run's answer to its trigger and the
// follow-ups it has received. A follow-up delivered after an answer was
// posted makes the continued Run's next answer a distinct message.
func conversationRunResponseKey(run *AgentRun, triggerID string) string {
	material := run.Scope.Kind + "\x00" + run.Scope.ID + "\x00" + run.ID + "\x00" + triggerID
	if followUps := conversationRunFollowUps(run); len(followUps) > 0 {
		material += "\x00" + followUps[len(followUps)-1].MessageID
	}
	return "agent-channel-response:" + hashString(material)
}

func conversationTaskFinalResponseKeys(run *AgentRun) []string {
	trigger, _ := run.Context[conversationRunContextTriggerID].(string)
	return []string{conversationTaskFinalResponseKey(run),
		"team-action-outcome:" + hashString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+run.ID+"\x00"+trigger)}
}

func conversationTaskParticipationRoundKey(run *AgentRun) string {
	conversation, _ := run.Context[conversationRunContextConversationID].(string)
	trigger, _ := run.Context[conversationRunContextTriggerID].(string)
	return "participation-round:" + hashString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+conversation+"\x00"+trigger)
}

func conversationTaskFinishedTeamRound(run *AgentRun, round *ParticipationRound) bool {
	return run.Owner.Type == OwnerTypeTeam && round != nil && round.Status == ParticipationRoundCommitted &&
		round.TriggerMessageID == run.Context[conversationRunContextTriggerID] && selectedParticipationAction(round) == nil
}

func conversationTaskFinishedBeforePromotion(turn *AgentTurn) bool {
	return turn != nil && terminalAgentTurnStatus(turn.Status) && isTerminalAgentRunStatus(turn.NextRunStatus)
}

// buildConversationTaskContinuation only changes the scheduling lane and the
// task marker. Leases, status, checkpoints, funding and pending approvals stay
// byte-for-byte identical to the accepted Run.
func buildConversationTaskContinuation(req ConversationTaskPromotionRequest, run *AgentRun, conversation *Conversation, message *ChannelMessage) (*ConversationTask, *AgentRun, *ActivityEvent, error) {
	if !conversationTaskContinuationCandidate(run) || conversation == nil || message == nil {
		return nil, nil, nil, nil
	}
	age := req.MinimumAge
	if age == 0 {
		age = ConversationTaskForegroundTimeout
	}
	deadline := run.CreatedAt.Add(age).UTC()
	if req.Now.Before(deadline) || conversation.Status != ConversationStatusActive || voiceCallStartedMessage(message) {
		return nil, nil, nil, nil
	}
	if err := validateConversationRun(run); err != nil {
		return nil, nil, nil, err
	}
	if run.Scope != req.Scope || run.ID != req.RunID || conversation.Scope != run.Scope || conversation.Owner != run.Owner ||
		run.Context[conversationRunContextConversationID] != conversation.ID || run.Context[conversationRunContextTriggerID] != message.ID ||
		message.Scope != run.Scope || message.ConversationID != conversation.ID {
		return nil, nil, nil, ErrInvalidConversationTask
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil {
		return nil, nil, nil, nil
	}
	if run.Owner.Type == OwnerTypeAgent && run.AssignedAgentID != run.Owner.ID {
		return nil, nil, nil, ErrInvalidConversationTask
	}
	thread := message.ThreadRootID
	if thread == "" {
		thread = message.ID
	}
	id := conversationTaskID(run.Scope, run.ID, conversationTaskContinuationKey)
	goal := strings.TrimSpace(message.Content)
	if goal == "" {
		goal = run.Goal
	}
	if len(goal) > 16384 {
		goal = goal[:16384]
		for len(goal) > 0 && !utf8.ValidString(goal) {
			goal = goal[:len(goal)-1]
		}
	}
	now := req.Now.UTC()
	task := &ConversationTask{ID: id, Mode: ConversationTaskModeContinuation, ForegroundDeadline: &deadline, Scope: run.Scope, Owner: run.Owner,
		ConversationID: conversation.ID, SourceMessageID: message.ID, ThreadRootID: thread, SourceRunID: run.ID, WorkRunID: run.ID,
		AuthenticatedActor: actor, TargetAgentID: run.AssignedAgentID, TaskKey: conversationTaskContinuationKey, Goal: goal,
		Acknowledgment: "I’m continuing this in a background task. You can keep chatting while I work.", Revision: 1, CreatedAt: now}
	var err error
	task.RequestDigest, err = conversationTaskDigest(task)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = task.Validate(); err != nil {
		return nil, nil, nil, err
	}
	updated := cloneAgentRun(run)
	if updated.Context == nil {
		updated.Context = map[string]interface{}{}
	}
	updated.Context[ConversationTaskContextKey] = id
	updated.ConcurrencyKey = "task:" + id
	updated.Revision++
	updated.UpdatedAt = now
	if err = updated.Validate(); err != nil {
		return nil, nil, nil, err
	}
	event := &ActivityEvent{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("conversation-task-continued:"+id)).String(), Scope: run.Scope, RunID: run.ID,
		AgentID: run.AssignedAgentID, EventType: "conversation.task_continued", Summary: "Continued conversation work in a background task",
		Actor: ActivityActor{Type: "service", ID: conversationRunSchedulerParticipant}, Visibility: ActivityVisibilityScope, CorrelationID: id,
		ConversationRefs: []string{conversation.ID}, Payload: map[string]interface{}{"taskId": id, "workRunId": run.ID, "foregroundDeadline": deadline}, CreatedAt: now}
	if err = event.Validate(); err != nil {
		return nil, nil, nil, err
	}
	return task, updated, event, nil
}

func replayConversationTaskContinuation(task *ConversationTask, run *AgentRun) (*ConversationTaskResult, error) {
	if task == nil {
		return nil, nil
	}
	if task.Mode != ConversationTaskModeContinuation || !ConversationTaskMatchesWorkRun(task, run) {
		return nil, ErrConversationTaskConflict
	}
	return &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(run), SourceRun: cloneAgentRun(run), Replayed: true,
		AcknowledgmentMessageID: ConversationTaskAcknowledgmentMessageID(task)}, nil
}
