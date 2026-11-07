package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Interaction control tools (section 04 §10): react (inline) and wait
// (settle, timer). ask_user lives in control_ask_user.go.

func init() {
	registerControlHandler(ControlToolReact, reactControlHandler{})
	registerControlHandler(ControlToolWait, waitControlHandler{})
}

// ---------------------------------------------------------------------------
// react

type reactControlArgs struct {
	MessageID string `json:"messageId"`
	Emoji     string `json:"emoji"`
}

type reactControlHandler struct{}

func (reactControlHandler) spec(ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: ConversationReactInstruction,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"messageId", "emoji"},
			"properties": map[string]interface{}{
				"messageId": map[string]interface{}{"type": "string", "description": "A message id from this conversation."},
				"emoji":     map[string]interface{}{"type": "string", "description": "One emoji, e.g. 👀 picked up, ✅ done, 👍 agree."},
			},
		},
	}
}

func (reactControlHandler) decode(raw json.RawMessage) (interface{}, error) {
	return decodeControlInto[reactControlArgs](raw)
}

func (reactControlHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*reactControlArgs)
	reaction := TurnReaction{MessageID: strings.TrimSpace(args.MessageID), Emoji: args.Emoji}
	if !validOpaqueIdentifier(reaction.MessageID, 256) || ValidateReactionEmoji(reaction.Emoji) != nil {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultFailed, "reaction_invalid",
			"Use one emoji and a message id from this conversation.", nil))
	}
	done, err := completedRunReactionEntries(ctx, x.Sessions, cc.Run)
	if err != nil {
		return ControlResult{}, err
	}
	if len(done) >= MaximumTurnReactions {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, "reaction_limit",
			fmt.Sprintf("This reply already added %d reactions, the most one reply may add.", MaximumTurnReactions), nil))
	}
	if err := x.addRunReaction(ctx, cc.Run, reaction); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return ControlResult{}, err
		}
		return x.refuse(ctx, cc, controlErrorResult(ToolResultFailed, "reaction_failed",
			"The reaction could not be added: "+err.Error()+".", nil))
	}
	return x.complete(ctx, cc, controlJSONResult(ToolResultSucceeded, map[string]interface{}{
		"status": "added", "messageId": reaction.MessageID, "emoji": reaction.Emoji,
	}), nil)
}

// addRunReaction adds the reaction as the Agent answering in the Run's
// conversation, on a message that Agent can see.
func (x *ControlExecutor) addRunReaction(ctx context.Context, run *AgentRun, reaction TurnReaction) error {
	if x.Conversations == nil {
		return errors.New("conversations are not configured")
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("this Run has no conversation")
	}
	conversation, err := x.Conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	agentID := ConversationRunAgentID(conversation, run)
	if agentID == "" {
		return errors.New("this Run is not the answering Agent of its conversation")
	}
	_, err = x.Conversations.AddMessageReaction(ctx, MessageReactionRequest{
		Scope: run.Scope, ConversationID: conversation.ID, MessageID: reaction.MessageID, Emoji: reaction.Emoji,
		Actor: ConversationParticipant{Type: ConversationParticipantAgent, ID: agentID},
	})
	return err
}

// completedRunReactionEntries lists the Run's completed react entries in
// session order.
func completedRunReactionEntries(ctx context.Context, sessions AgentSessionStore, run *AgentRun) ([]*SessionEntry, error) {
	if sessions == nil || run == nil {
		return nil, errors.New("reactions need the session store and the Run")
	}
	return sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, ToolNames: []string{string(ControlToolReact)},
		States: []ToolEntryState{ToolEntryCompleted}, Limit: maxSessionEntryListLimit,
	})
}

// RunReactions returns the reactions this Run added through react, in call
// order. The conversation final-answer sink uses it for reactions-only
// replies (reactionEcho).
func RunReactions(ctx context.Context, sessions AgentSessionStore, run *AgentRun) ([]TurnReaction, error) {
	entries, err := completedRunReactionEntries(ctx, sessions, run)
	if err != nil {
		return nil, err
	}
	reactions := make([]TurnReaction, 0, len(entries))
	for _, entry := range entries {
		var args reactControlArgs
		if err := decodeControlArguments(entry.Tool.Arguments, &args); err != nil {
			return nil, fmt.Errorf("decode react entry %s: %w", entry.ID, err)
		}
		reactions = append(reactions, TurnReaction{MessageID: strings.TrimSpace(args.MessageID), Emoji: args.Emoji})
	}
	return reactions, nil
}

// ---------------------------------------------------------------------------
// wait

// maximumWaitSeconds is the longest timer wait (30 days).
const maximumWaitSeconds = 30 * 24 * 60 * 60

const waitControlDescription = "Pause this background work for a number of seconds, then continue. A new message for this work wakes it early."

type waitControlArgs struct {
	Seconds int64 `json:"seconds"`
}

type waitControlHandler struct{}

func (waitControlHandler) spec(ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: waitControlDescription,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"seconds"},
			"properties": map[string]interface{}{
				"seconds": map[string]interface{}{
					"type": "integer", "minimum": 1, "maximum": maximumWaitSeconds,
					"description": "How long to pause this background work before continuing.",
				},
			},
		},
	}
}

func (waitControlHandler) decode(raw json.RawMessage) (interface{}, error) {
	value, err := decodeControlInto[waitControlArgs](raw)
	if err != nil {
		return nil, err
	}
	if seconds := value.(*waitControlArgs).Seconds; seconds < 1 || seconds > maximumWaitSeconds {
		return nil, fmt.Errorf("seconds must be between 1 and %d", maximumWaitSeconds)
	}
	return value, nil
}

func (waitControlHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*waitControlArgs)
	wakeAt := x.now().Add(time.Duration(args.Seconds) * time.Second)
	entry, err := x.transitionControlEntry(ctx, cc, ToolEntryAwaitingEvent,
		&ToolEntryWait{Reason: "timer"}, nil, &ControlEntryMeta{WakeAt: &wakeAt})
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlParked, Entry: entry, Park: waitPark(entry.ID, wakeAt)}, nil
}

func (waitControlHandler) resume(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResume, error) {
	args := cc.Arguments.(*waitControlArgs)
	meta, err := DecodeControlEntryMeta(cc.Entry)
	if err != nil {
		return ControlResume{}, err
	}
	if meta == nil || meta.WakeAt == nil {
		return ControlResume{}, fmt.Errorf("wait entry %s has no wake time", cc.Entry.ID)
	}
	wakeAt := meta.WakeAt.UTC()
	now := x.now()
	var value map[string]interface{}
	switch {
	case !now.Before(wakeAt):
		started := wakeAt.Add(-time.Duration(args.Seconds) * time.Second)
		value = map[string]interface{}{"status": "elapsed", "waitedSeconds": int64(now.Sub(started) / time.Second)}
	case len(cc.Run.PendingInterventions) > cc.Run.ConsumedInterventions:
		// A steering message queued the sleeping Run before its timer.
		value = map[string]interface{}{
			"status": "woken_early", "reason": "new_message",
			"remainingSeconds": int64(math.Ceil(wakeAt.Sub(now).Seconds())),
		}
	default:
		// Nothing woke it (a crash between the entry park and the Run park):
		// keep sleeping until the timer.
		return ControlResume{Resolved: false, Park: waitPark(cc.Entry.ID, wakeAt)}, nil
	}
	result, err := x.complete(ctx, cc, controlJSONResult(ToolResultSucceeded, value), nil)
	if err != nil {
		return ControlResume{}, err
	}
	return ControlResume{Resolved: true, Result: result}, nil
}

func waitPark(entryID string, wakeAt time.Time) *LoopPark {
	at := wakeAt
	return &LoopPark{
		Status:  AgentRunStatusSleeping,
		Wake:    &WakeCondition{Type: "timer", WakeAt: &at, Reference: entryID},
		Summary: "Waiting until " + at.Format(time.RFC3339),
	}
}
