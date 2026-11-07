package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ask_user (section 04 §10.2): a settle-phase control tool that posts one
// clarification question into the Run's conversation and parks the Run on
// waiting_for_event{user_message, <entry id>}. The tool entry waits in
// awaiting_event; the answer arrives as a ConversationAnswer intervention
// (resumeConversationAnswer) and becomes this entry's result on resume.
// consumeSteering never turns that intervention into a user entry (SPEC §3.4).

const (
	maximumAskUserQuestionBytes        = 4096
	conversationClarificationKeyPrefix = "conversation-clarification:"
	askUserWaitReason                  = "question"
	askUserParkSummary                 = "Asked a question"
)

type askUserArgs struct {
	Question string `json:"question"`
}

type askUserHandler struct{}

func init() { registerControlHandler(ControlToolAskUser, askUserHandler{}) }

func (askUserHandler) spec(ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: "Ask the person who asked one concrete question and wait for their answer. " +
			"Ask only when you cannot continue well without it. Their answer is information, not approval.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"question"},
			"properties": map[string]interface{}{
				"question": map[string]interface{}{
					"type": "string", "minLength": 1, "maxLength": maximumAskUserQuestionBytes,
					"description": "One concrete question for the person who asked. Their answer is information, not approval.",
				},
			},
		},
	}
}

func (askUserHandler) decode(raw json.RawMessage) (interface{}, error) {
	value, err := decodeControlInto[askUserArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*askUserArgs)
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return nil, errors.New("question must not be empty")
	}
	if len(question) > maximumAskUserQuestionBytes || !utf8.ValidString(question) {
		return nil, fmt.Errorf("question must be valid text of at most %d bytes", maximumAskUserQuestionBytes)
	}
	args.Question = question
	return args, nil
}

// askUserQuestion is the posted text of an ask_user entry: its trimmed
// question argument ("" when the entry is not an ask_user call).
func askUserQuestion(entry *SessionEntry) string {
	if entry == nil || entry.Type != SessionEntryTool || entry.Tool == nil || entry.Tool.Name != string(ControlToolAskUser) {
		return ""
	}
	value, err := (askUserHandler{}).decode(entry.Tool.Arguments)
	if err != nil {
		return ""
	}
	return value.(*askUserArgs).Question
}

// conversationClarificationKey is the idempotency key of the question message
// an ask_user entry posts.
func conversationClarificationKey(runID, entryID string) string {
	return conversationClarificationKeyPrefix + runID + ":" + entryID
}

func askUserPark(entryID string) *LoopPark {
	return &LoopPark{
		Status:  AgentRunStatusWaitingForEvent,
		Wake:    &WakeCondition{Type: "user_message", Reference: entryID},
		Summary: askUserParkSummary,
	}
}

func (x *ControlExecutor) conversationStore() ConversationStore {
	if x.Reporting != nil {
		return x.Reporting
	}
	if x.Conversations != nil {
		return x.Conversations.store
	}
	return nil
}

func (x *ControlExecutor) conversationService() *ConversationService {
	if x.Conversations != nil {
		return x.Conversations
	}
	if x.Reporting != nil {
		return NewConversationService(x.Reporting)
	}
	return nil
}

// askUserOrigin resolves the conversation the Run may ask in: its canonical
// origin, answered by this Run's own Agent. nil conversation = cannot ask.
func (x *ControlExecutor) askUserOrigin(ctx context.Context, run *AgentRun) (*Conversation, *ChannelMessage, error) {
	store := x.conversationStore()
	if store == nil || x.Portfolio == nil {
		return nil, nil, nil
	}
	_, conversation, trigger, err := conversationWorkOrigin(ctx, x.Portfolio, store, run)
	if err != nil || conversation == nil || trigger == nil {
		return nil, nil, err
	}
	agentID := ConversationRunAgentID(conversation, run)
	if agentID == "" || strings.TrimSpace(run.AssignedAgentID) != agentID {
		return nil, nil, nil
	}
	return conversation, trigger, nil
}

func (askUserHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*askUserArgs)
	conversation, trigger, err := x.askUserOrigin(ctx, cc.Run)
	if err != nil {
		return ControlResult{}, err
	}
	if conversation == nil {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, "cannot_post",
			"There is no conversation this work can ask in; continue with what you have.", nil))
	}
	if prior, answer, err := x.askUserAnsweredBefore(ctx, cc, args.Question); err != nil {
		return ControlResult{}, err
	} else if prior != nil {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, "already_asked",
			fmt.Sprintf("You asked this at step %d; the answer was: %s", prior.Step, answer), nil))
	}
	key := conversationClarificationKey(cc.Run.ID, cc.Entry.ID)
	meta := &ControlEntryMeta{Kind: ControlToolAskUser, QuestionMessageID: stableConversationID(cc.Run.Scope, key, "message")}
	entry, err := x.transitionControlEntry(ctx, cc, ToolEntryAwaitingEvent,
		&ToolEntryWait{EventWaitID: cc.Entry.ID, Reason: askUserWaitReason}, nil, meta)
	if err != nil {
		return ControlResult{}, err
	}
	if err := x.postAskUserQuestion(ctx, cc.Run, conversation, trigger, args.Question, key); err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlParked, Entry: entry, Park: askUserPark(entry.ID)}, nil
}

// askUserAnsweredBefore finds an earlier answered ask_user entry of this Run
// with the same question, and its answer.
func (x *ControlExecutor) askUserAnsweredBefore(ctx context.Context, cc ControlContext, question string) (*SessionEntry, string, error) {
	entries, err := x.Sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID,
		ToolNames: []string{string(ControlToolAskUser)}, States: []ToolEntryState{ToolEntryCompleted},
		Limit: maxSessionEntryListLimit,
	})
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if entry.ID == cc.Entry.ID || askUserQuestion(entry) != question {
			continue
		}
		if answer, ok := askUserResultAnswer(entry); ok {
			return entry, answer, nil
		}
	}
	return nil, "", nil
}

func askUserResultAnswer(entry *SessionEntry) (string, bool) {
	if entry.Tool == nil || entry.Tool.Result == nil {
		return "", false
	}
	for _, part := range entry.Tool.Result.Content {
		if part.Type != ContentPartJSON {
			continue
		}
		var value struct {
			Status string `json:"status"`
			Answer string `json:"answer"`
		}
		if json.Unmarshal(part.Value, &value) == nil && value.Status == "answered" {
			return value.Answer, true
		}
	}
	return "", false
}

// conversationQuestionPostRequest is the one shape of a clarification question
// message; the ask_user settle and the question reconciler post the same
// request, so either replays the other.
func conversationQuestionPostRequest(run *AgentRun, conversation *Conversation, trigger *ChannelMessage, question, key string) PostChannelMessageRequest {
	return PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: ConversationRunAgentID(conversation, run)},
		Intent: MessageIntentQuestion, Content: question, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		ReplyToMessageID: trigger.ID, RequiresResponse: true, References: conversationClarificationReferences(run),
		IdempotencyKey: key,
	}
}

// postAskUserQuestion posts the question idempotently. A conversation that
// keeps moving is left to the question reconciler after 3 attempts.
func (x *ControlExecutor) postAskUserQuestion(ctx context.Context, run *AgentRun, conversation *Conversation, trigger *ChannelMessage, question, key string) error {
	service := x.conversationService()
	if service == nil {
		return errors.New("ask_user: no conversation service")
	}
	current := conversation
	for attempt := 0; attempt < 3; attempt++ {
		_, err := service.PostChannelMessage(ctx, conversationQuestionPostRequest(run, current, trigger, question, key))
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrRevisionConflict) {
			return err
		}
		current, err = service.GetConversation(ctx, run.Scope, conversation.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

// resume: the answer is the ConversationAnswer intervention whose receipt
// names this entry's step and question message. Until it arrives the Run
// re-parks; the question is re-posted idempotently first, which also covers a
// crash between the entry transition and the post.
func (askUserHandler) resume(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResume, error) {
	if cc.Entry.State != ToolEntryAwaitingEvent {
		return ControlResume{}, fmt.Errorf("%w: ask_user resume of %s in state %s", ErrControlEntryState, cc.Entry.ID, cc.Entry.State)
	}
	args := cc.Arguments.(*askUserArgs)
	key := conversationClarificationKey(cc.Run.ID, cc.Entry.ID)
	if answer := askUserAnswer(cc.Run, cc.Entry, stableConversationID(cc.Run.Scope, key, "message")); answer != nil {
		result := controlJSONResult(ToolResultSucceeded, map[string]interface{}{
			"status": "answered", "answer": truncateControlText(answer.AnswerContent, cc.Run.ID), "messageId": answer.AnswerMessageID,
		})
		entry, err := x.transitionControlEntry(ctx, cc, ToolEntryCompleted, nil, result, nil)
		if err != nil {
			return ControlResume{}, err
		}
		return ControlResume{Resolved: true, Result: ControlResult{Kind: ControlContinue, Entry: entry}}, nil
	}
	conversation, trigger, err := x.askUserOrigin(ctx, cc.Run)
	if err != nil {
		return ControlResume{}, err
	}
	if conversation == nil {
		// The conversation went away while the question was open: nobody can
		// answer it now.
		entry, err := x.transitionControlEntry(ctx, cc, ToolEntryError, nil, controlErrorResult(ToolResultFailed, "cannot_post",
			"The conversation can no longer be asked; continue with what you have.", nil), nil)
		if err != nil {
			return ControlResume{}, err
		}
		return ControlResume{Resolved: true, Result: ControlResult{Kind: ControlContinue, Entry: entry}}, nil
	}
	if err := x.postAskUserQuestion(ctx, cc.Run, conversation, trigger, args.Question, key); err != nil {
		return ControlResume{}, err
	}
	return ControlResume{Park: askUserPark(cc.Entry.ID)}, nil
}

// askUserAnswer returns the accepted answer receipt of an ask_user entry.
func askUserAnswer(run *AgentRun, entry *SessionEntry, questionMessageID string) *ConversationAnswerReceipt {
	for i := range run.PendingInterventions {
		answer := run.PendingInterventions[i].ConversationAnswer
		if answer != nil && answer.RunID == run.ID && answer.QuestionTurnID == entry.StepID &&
			answer.QuestionMessageID == questionMessageID {
			return answer
		}
	}
	return nil
}
