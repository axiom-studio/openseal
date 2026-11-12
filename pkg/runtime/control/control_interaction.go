package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// Interaction control tools (section 04 §10): react (inline) and wait
// (settle, timer). ask_user lives in control_ask_user.go.

// ---------------------------------------------------------------------------
// react

type reactControlArgs struct {
	MessageID string `json:"messageId"`
	Emoji     string `json:"emoji"`
}

type reactControlHandler struct{}

func (reactControlHandler) Spec(agentrun.ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: runtimeconversation.ConversationReactInstruction,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"messageId", "emoji"},
			"properties": map[string]interface{}{
				"messageId": map[string]interface{}{"type": "string", "description": "A message id from this conversation."},
				"emoji":     map[string]interface{}{"type": "string", "description": "One emoji, e.g. 👀 picked up, ✅ done, 👍 agree."},
			},
		},
	}
}

func (reactControlHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return models.DecodeControlInto[reactControlArgs](raw)
}

func (reactControlHandler) Run(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*reactControlArgs)
	reaction := runtimeconversation.TurnReaction{MessageID: strings.TrimSpace(args.MessageID), Emoji: args.Emoji}
	if !models.IsValidOpaqueIdentifier(reaction.MessageID, 256) || models.ValidateReactionEmoji(reaction.Emoji) != nil {
		return x.refuse(ctx, cc, ErrorResult(models.ToolResultFailed, "reaction_invalid",
			"Use one emoji and a message id from this conversation.", nil))
	}
	done, err := completedRunReactionEntries(ctx, x.Sessions, cc.Run)
	if err != nil {
		return ControlResult{}, err
	}
	if len(done) >= runtimeconversation.MaximumTurnReactions {
		return x.refuse(ctx, cc, ErrorResult(models.ToolResultRefused, "reaction_limit",
			fmt.Sprintf("This reply already added %d reactions, the most one reply may add.", runtimeconversation.MaximumTurnReactions), nil))
	}
	if err := x.addRunReaction(ctx, cc.Run, reaction); err != nil {
		if errors.Is(err, models.ErrLeaseLost) {
			return ControlResult{}, err
		}
		return x.refuse(ctx, cc, ErrorResult(models.ToolResultFailed, "reaction_failed",
			"The reaction could not be added: "+err.Error()+".", nil))
	}
	return x.complete(ctx, cc, models.ControlJSONResult(models.ToolResultSucceeded, map[string]interface{}{
		"status": "added", "messageId": reaction.MessageID, "emoji": reaction.Emoji,
	}), nil)
}

// addRunReaction adds the reaction as the Agent answering in the Run's
// conversation, on a message that Agent can see.
func (x *ControlExecutor) addRunReaction(ctx context.Context, run *models.AgentRun, reaction runtimeconversation.TurnReaction) error {
	if x.Conversations == nil {
		return errors.New("conversations are not configured")
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("this Run has no conversation")
	}
	conversation, err := x.Conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	agentID := runtimeconversation.ConversationRunAgentID(conversation, run)
	if agentID == "" {
		return errors.New("this Run is not the answering Agent of its conversation")
	}
	_, err = x.Conversations.AddMessageReaction(ctx, runtimeconversation.MessageReactionRequest{
		Scope: run.Scope, ConversationID: conversation.ID, MessageID: reaction.MessageID, Emoji: reaction.Emoji,
		Actor: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: agentID},
	})
	return err
}

// completedRunReactionEntries lists the Run's completed react entries in
// session order.
func completedRunReactionEntries(ctx context.Context, sessions models.AgentSessionStore, run *models.AgentRun) ([]*models.SessionEntry, error) {
	if sessions == nil || run == nil {
		return nil, errors.New("reactions need the session store and the Run")
	}
	return sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, ToolNames: []string{string(models.ControlToolReact)},
		States: []models.ToolEntryState{models.ToolEntryCompleted}, Limit: models.MaxSessionEntryListLimit,
	})
}

// RunReactions returns the reactions this Run added through react, in call
// order. The conversation final-answer sink uses it for reactions-only
// replies (reactionEcho).
func RunReactions(ctx context.Context, sessions models.AgentSessionStore, run *models.AgentRun) ([]runtimeconversation.TurnReaction, error) {
	entries, err := completedRunReactionEntries(ctx, sessions, run)
	if err != nil {
		return nil, err
	}
	reactions := make([]runtimeconversation.TurnReaction, 0, len(entries))
	for _, entry := range entries {
		var args reactControlArgs
		if err := models.DecodeControlArguments(entry.Tool.Arguments, &args); err != nil {
			return nil, fmt.Errorf("decode react entry %s: %w", entry.ID, err)
		}
		reactions = append(reactions, runtimeconversation.TurnReaction{MessageID: strings.TrimSpace(args.MessageID), Emoji: args.Emoji})
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

func (waitControlHandler) Spec(agentrun.ControlAuthority) ControlToolSpec {
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

func (waitControlHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[waitControlArgs](raw)
	if err != nil {
		return nil, err
	}
	if seconds := value.(*waitControlArgs).Seconds; seconds < 1 || seconds > maximumWaitSeconds {
		return nil, fmt.Errorf("seconds must be between 1 and %d", maximumWaitSeconds)
	}
	return value, nil
}

func (waitControlHandler) Run(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResult, error) {
	args := cc.Arguments.(*waitControlArgs)
	wakeAt := x.now().Add(time.Duration(args.Seconds) * time.Second)
	entry, err := x.transitionControlEntry(ctx, cc, models.ToolEntryAwaitingEvent,
		&models.ToolEntryWait{Reason: "timer"}, nil, &ControlEntryMeta{WakeAt: &wakeAt})
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlParked, Entry: entry, Park: waitPark(entry.ID, wakeAt)}, nil
}

func (waitControlHandler) Resume(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResume, error) {
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
	result, err := x.complete(ctx, cc, models.ControlJSONResult(models.ToolResultSucceeded, value), nil)
	if err != nil {
		return ControlResume{}, err
	}
	return ControlResume{Resolved: true, Result: result}, nil
}

func waitPark(entryID string, wakeAt time.Time) *agentrun.LoopPark {
	at := wakeAt
	return &agentrun.LoopPark{
		Status:  models.AgentRunStatusSleeping,
		Wake:    &models.WakeCondition{Type: "timer", WakeAt: &at, Reference: entryID},
		Summary: "Waiting until " + at.Format(time.RFC3339),
	}
}
