package runtime

import (
	"context"
	"encoding/json"
	"fmt"
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

// conversationTaskFinishedBeforePromotion is the probe on the Run's latest
// step row: a step whose model output was final (no tool calls, or a final
// answer) is posting the answer, whatever its status, so it is not promoted.
func conversationTaskFinishedBeforePromotion(step *AgentTurn) bool {
	return step != nil && step.Final
}

// latestConversationTaskStep is the step row with the highest sequence.
func latestConversationTaskStep(steps map[string]*AgentTurn) *AgentTurn {
	var latest *AgentTurn
	for _, step := range steps {
		if step != nil && (latest == nil || step.Sequence > latest.Sequence) {
			latest = step
		}
	}
	return latest
}

// conversationTaskPromotionFork is the session half of a promotion (SPEC
// §4.5): the child session the promoted Run moves to, and the task_promoted
// note on the chat session after the fork point. Note is nil when the chat
// session row does not exist yet (the Run was promoted before its first step
// opened it); the child is then a parentless run session seeded with the brief.
type conversationTaskPromotionFork struct {
	Child CreateRunAgentSessionTxRequest
	Note  *AppendAgentSessionEntriesRequest
}

// conversationTaskPromotionNoteWorker names the promotion transaction in the
// note append's placeholder lease; the fence is the transaction itself.
const conversationTaskPromotionNoteWorker = "conversation-task-promotion"

func conversationTaskPromotedText(messageID, taskID string) string {
	return "Your work on message " + messageID + " continues in background task " + taskID +
		"; its result will be posted in this chat when it finishes."
}

// buildConversationTaskPromotionFork forks the chat session at its leaf for
// the promoted Run. chat is the Run's lane session read in the transaction, or
// nil when its row does not exist yet.
func buildConversationTaskPromotionFork(task *ConversationTask, run *AgentRun, chat *AgentSession, now time.Time) (*conversationTaskPromotionFork, error) {
	if chat != nil && (chat.ID != run.SessionID || chat.Scope != run.Scope || chat.Kind != AgentSessionKindConversation) {
		return nil, fmt.Errorf("%w: promoted Run is not on its chat session", ErrInvalidConversationTask)
	}
	conversationID := task.ConversationID
	root, _ := run.Context["threadRootMessageId"].(string)
	fork := &conversationTaskPromotionFork{}
	if chat != nil && chat.NextSequence > 1 {
		child, err := newChildSession(ChildSessionRequest{Scope: run.Scope, ParentSessionID: chat.ID, ForkSequence: chat.NextSequence - 1,
			OwnerRunID: run.ID, AgentID: run.AssignedAgentID, ConversationID: conversationID, ThreadRootID: root})
		if err != nil {
			return nil, err
		}
		fork.Child = child
	} else {
		seed, err := childSessionSeed(run.Scope, run.ID, "task", conversationTaskBrief(task.Goal))
		if err != nil {
			return nil, err
		}
		fork.Child = CreateRunAgentSessionTxRequest{Scope: run.Scope, OwnerRunID: run.ID, AgentID: run.AssignedAgentID,
			ConversationID: conversationID, ThreadRootID: root, InitialEntries: []*SessionEntry{seed}}
		if chat != nil {
			fork.Child.ParentSessionID = chat.ID
		}
	}
	fork.Child.Now = now
	if err := validateCreateRunAgentSessionTxRequest(fork.Child); err != nil {
		return nil, err
	}
	if chat == nil {
		return fork, nil
	}
	data, err := json.Marshal(map[string]string{"taskId": task.ID, "runId": run.ID, "messageId": task.SourceMessageID})
	if err != nil {
		return nil, err
	}
	key := TaskPromotedSourceKey(run.ID)
	note := &SessionEntry{ID: SessionEntryID(chat.ID, key), Scope: run.Scope, SessionID: chat.ID, RunID: run.ID, Type: SessionEntryNote, SourceKey: key,
		Note: &NoteEntry{Kind: NoteTaskPromoted, Text: conversationTaskPromotedText(task.SourceMessageID, task.ID), Data: data}}
	fork.Note = &AppendAgentSessionEntriesRequest{Scope: run.Scope, SessionID: chat.ID, RunID: run.ID,
		Lease: &AgentRunLeaseGuard{WorkerID: conversationTaskPromotionNoteWorker, Now: now}, Entries: []*SessionEntry{note}, Now: now}
	if err := validateAppendAgentSessionEntries(*fork.Note); err != nil {
		return nil, err
	}
	return fork, nil
}

// buildConversationTaskContinuation changes the scheduling lane, the task
// marker and, for a Run on a chat session, the session binding: the promoted
// Run moves to a child session forked at the chat leaf and the lane keeps the
// chat session. Leases, status, checkpoints, funding and pending approvals stay
// byte-for-byte identical to the accepted Run. chat is the Run's lane session
// read in the transaction (nil when run.SessionID is "" or its row does not
// exist yet); the fork is nil when run.SessionID is "".
func buildConversationTaskContinuation(req ConversationTaskPromotionRequest, run *AgentRun, conversation *Conversation, message *ChannelMessage, chat *AgentSession) (*ConversationTask, *AgentRun, *ActivityEvent, *conversationTaskPromotionFork, error) {
	if !conversationTaskContinuationCandidate(run) || conversation == nil || message == nil {
		return nil, nil, nil, nil, nil
	}
	age := req.MinimumAge
	if age == 0 {
		age = ConversationTaskForegroundTimeout
	}
	deadline := run.CreatedAt.Add(age).UTC()
	if req.Now.Before(deadline) || conversation.Status != ConversationStatusActive || voiceCallStartedMessage(message) {
		return nil, nil, nil, nil, nil
	}
	if err := validateConversationRun(run); err != nil {
		return nil, nil, nil, nil, err
	}
	if run.Scope != req.Scope || run.ID != req.RunID || conversation.Scope != run.Scope || conversation.Owner != run.Owner ||
		run.Context[conversationRunContextConversationID] != conversation.ID || run.Context[conversationRunContextTriggerID] != message.ID ||
		message.Scope != run.Scope || message.ConversationID != conversation.ID {
		return nil, nil, nil, nil, ErrInvalidConversationTask
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil {
		return nil, nil, nil, nil, nil
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
		return nil, nil, nil, nil, err
	}
	if err = task.Validate(); err != nil {
		return nil, nil, nil, nil, err
	}
	updated := cloneAgentRun(run)
	if updated.Context == nil {
		updated.Context = map[string]interface{}{}
	}
	updated.Context[ConversationTaskContextKey] = id
	updated.ConcurrencyKey = "task:" + id
	var fork *conversationTaskPromotionFork
	if run.SessionID != "" {
		if fork, err = buildConversationTaskPromotionFork(task, run, chat, now); err != nil {
			return nil, nil, nil, nil, err
		}
		updated.SessionID = RunAgentSessionID(run.Scope, run.ID)
	}
	updated.Revision++
	updated.UpdatedAt = now
	if err = updated.Validate(); err != nil {
		return nil, nil, nil, nil, err
	}
	event := &ActivityEvent{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("conversation-task-continued:"+id)).String(), Scope: run.Scope, RunID: run.ID,
		AgentID: run.AssignedAgentID, EventType: "conversation.task_continued", Summary: "Continued conversation work in a background task",
		Actor: ActivityActor{Type: "service", ID: conversationRunSchedulerParticipant}, Visibility: ActivityVisibilityScope, CorrelationID: id,
		ConversationRefs: []string{conversation.ID}, Payload: map[string]interface{}{"taskId": id, "workRunId": run.ID, "foregroundDeadline": deadline}, CreatedAt: now}
	if err = event.Validate(); err != nil {
		return nil, nil, nil, nil, err
	}
	return task, updated, event, fork, nil
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
