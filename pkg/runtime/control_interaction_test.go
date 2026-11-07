package runtime

import (
	"encoding/json"
	"testing"
	"time"
)

// interactionFixture is a control fixture whose Run answers in a
// conversation that holds one user message.
type interactionFixture struct {
	*controlFixture
	conversation *Conversation
	message      *ChannelMessage
	clock        time.Time
}

func newInteractionFixture(t *testing.T, calls []LoopToolCall) *interactionFixture {
	t.Helper()
	f := newControlFixture(t, calls)
	conversations := NewConversationService(f.store)
	conversation, _, err := conversations.CreateConversation(f.ctx, CreateConversationRequest{
		Scope: f.run.Scope, Owner: f.run.Owner, Title: "Chat", IdempotencyKey: "chat",
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	posted, err := conversations.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.run.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "maya"},
		ID:     reactTestMessageID, Intent: MessageIntentQuestion, Content: "Ship it?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: "ship",
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	f.run.Context = map[string]interface{}{conversationRunContextConversationID: conversation.ID}
	fx := &interactionFixture{controlFixture: f, conversation: conversation, message: posted.Message, clock: f.now}
	f.exec.Conversations = conversations
	f.exec.handlers = nil // the real handlers
	f.exec.Now = func() time.Time { return fx.clock }
	return fx
}

func reactCall(id, messageID, emoji string) LoopToolCall {
	raw, _ := json.Marshal(map[string]string{"messageId": messageID, "emoji": emoji})
	return LoopToolCall{ID: id, Name: string(ControlToolReact), Arguments: raw}
}

func controlResultValue(t *testing.T, entry *SessionEntry) map[string]interface{} {
	t.Helper()
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil || len(entry.Tool.Result.Content) != 1 {
		t.Fatalf("entry has no single-part result: %#v", entry)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(entry.Tool.Result.Content[0].Value, &value); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return value
}

// reactTestMessageID is the id of the fixture's user message; the calls are
// appended before the message is posted.
const reactTestMessageID = "msg-ship"

var reactAuthority = ControlAuthority{CanReact: true}

func TestReactAddsReactionAsAnsweringAgent(t *testing.T) {
	f := newInteractionFixture(t, []LoopToolCall{reactCall("r1", reactTestMessageID, "👀")})
	result, err := f.exec.Inline(f.ctx, f.context("r1", reactAuthority))
	if err != nil || result.Kind != ControlContinue {
		t.Fatalf("inline = %#v, %v", result, err)
	}
	stored := f.stored("r1")
	if stored.State != ToolEntryCompleted {
		t.Fatalf("state = %s", stored.State)
	}
	value := controlResultValue(t, stored)
	if value["status"] != "added" || value["messageId"] != f.message.ID || value["emoji"] != "👀" {
		t.Fatalf("result = %v", value)
	}
	read, err := f.exec.Conversations.GetChannelMessage(f.ctx, f.run.Scope, f.conversation.ID, f.message.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := ConversationParticipant{Type: ConversationParticipantAgent, ID: f.run.AssignedAgentID}
	if len(read.Reactions) != 1 || read.Reactions[0].Emoji != "👀" || len(read.Reactions[0].Actors) != 1 || read.Reactions[0].Actors[0] != want {
		t.Fatalf("reactions = %+v", read.Reactions)
	}
}

func TestReactInvalidOrUnknownMessageIsError(t *testing.T) {
	f := newInteractionFixture(t, []LoopToolCall{
		reactCall("bad-emoji", reactTestMessageID, "ok"),
		reactCall("bad-id", "", "👍"),
		reactCall("unknown", "no-such-message", "👍"),
	})
	for callID, code := range map[string]string{"bad-emoji": "reaction_invalid", "bad-id": "reaction_invalid", "unknown": "reaction_failed"} {
		if _, err := f.exec.Inline(f.ctx, f.context(callID, reactAuthority)); err != nil {
			t.Fatalf("%s: %v", callID, err)
		}
		stored := f.stored(callID)
		if stored.State != ToolEntryError || !stored.Tool.Result.IsError || controlResultValue(t, stored)["error"] != code {
			t.Fatalf("%s: state %s result %v", callID, stored.State, controlResultValue(t, stored))
		}
	}
}

func TestReactFourthInRunRefused(t *testing.T) {
	emojis := []string{"👀", "👍", "✅", "🎉"}
	var calls []LoopToolCall
	for i, emoji := range emojis {
		calls = append(calls, reactCall("r"+string(rune('1'+i)), reactTestMessageID, emoji))
	}
	f := newInteractionFixture(t, calls)
	for i := range emojis {
		callID := "r" + string(rune('1'+i))
		if _, err := f.exec.Inline(f.ctx, f.context(callID, reactAuthority)); err != nil {
			t.Fatalf("%s: %v", callID, err)
		}
	}
	for i := 0; i < 3; i++ {
		if state := f.stored("r" + string(rune('1'+i))).State; state != ToolEntryCompleted {
			t.Fatalf("reaction %d state = %s", i+1, state)
		}
	}
	fourth := f.stored("r4")
	if fourth.State != ToolEntryError || controlResultValue(t, fourth)["error"] != "reaction_limit" {
		t.Fatalf("fourth = %s %v", fourth.State, controlResultValue(t, fourth))
	}
	read, err := f.exec.Conversations.GetChannelMessage(f.ctx, f.run.Scope, f.conversation.ID, f.message.ID)
	if err != nil || len(read.Reactions) != 3 {
		t.Fatalf("reactions = %+v, %v", read, err)
	}
}

func TestReactOnlyReplyPostsNoMessage(t *testing.T) {
	f := newInteractionFixture(t, []LoopToolCall{reactCall("r1", reactTestMessageID, "👍")})
	if _, err := f.exec.Inline(f.ctx, f.context("r1", reactAuthority)); err != nil {
		t.Fatal(err)
	}
	messages, err := f.exec.Conversations.ListChannelMessages(f.ctx, ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID})
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages = %d, %v", len(messages), err)
	}
	reactions, err := RunReactions(f.ctx, f.store, f.run)
	if err != nil || len(reactions) != 1 || reactions[0] != (TurnReaction{MessageID: f.message.ID, Emoji: "👍"}) {
		t.Fatalf("run reactions = %+v, %v", reactions, err)
	}
	// An empty (or emoji-only) final answer after the reaction says nothing
	// more, so the sink completes without a reply.
	if !reactionEcho("", reactions) || !reactionEcho("👍", reactions) || reactionEcho("Done", reactions) {
		t.Fatal("reaction echo")
	}
}

func TestReactNotOfferedOutsideConversation(t *testing.T) {
	f := newInteractionFixture(t, []LoopToolCall{reactCall("r1", reactTestMessageID, "👍")})
	if _, err := f.exec.Inline(f.ctx, f.context("r1", ControlAuthority{})); err != nil {
		t.Fatal(err)
	}
	if value := controlResultValue(t, f.stored("r1")); value["error"] != "tool_not_available" {
		t.Fatalf("result = %v", value)
	}
}

func waitCall(id string, seconds int) LoopToolCall {
	raw, _ := json.Marshal(map[string]int{"seconds": seconds})
	return LoopToolCall{ID: id, Name: string(ControlToolWait), Arguments: raw}
}

var waitAuthority = ControlAuthority{CanWait: true}

func (f *interactionFixture) settleWait(t *testing.T, callID string) ControlResult {
	t.Helper()
	result, err := f.exec.Settle(f.ctx, f.context(callID, waitAuthority))
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	f.entries[callID] = f.stored(callID)
	return result
}

func TestWaitSleepsAndResumesElapsed(t *testing.T) {
	f := newInteractionFixture(t, []LoopToolCall{waitCall("w", 90)})
	result := f.settleWait(t, "w")
	wakeAt := f.clock.Add(90 * time.Second)
	if result.Kind != ControlParked || result.Park == nil || result.Park.Committed ||
		result.Park.Status != AgentRunStatusSleeping || result.Park.Wake == nil || result.Park.Wake.Type != "timer" ||
		result.Park.Wake.WakeAt == nil || !result.Park.Wake.WakeAt.Equal(wakeAt) || result.Park.Wake.Reference != f.entries["w"].ID {
		t.Fatalf("park = %#v", result.Park)
	}
	stored := f.entries["w"]
	meta, err := DecodeControlEntryMeta(stored)
	if err != nil || stored.State != ToolEntryAwaitingEvent || meta == nil || meta.Kind != ControlToolWait || meta.WakeAt == nil || !meta.WakeAt.Equal(wakeAt) {
		t.Fatalf("entry = %s %+v, %v", stored.State, meta, err)
	}

	// Before the timer with nothing new: still waiting.
	f.clock = f.clock.Add(30 * time.Second)
	resumed, err := f.exec.Resume(f.ctx, f.context("w", waitAuthority))
	if err != nil || resumed.Resolved || resumed.Park == nil || !resumed.Park.Wake.WakeAt.Equal(wakeAt) {
		t.Fatalf("early resume = %#v, %v", resumed, err)
	}

	f.clock = wakeAt.Add(2 * time.Second)
	resumed, err = f.exec.Resume(f.ctx, f.context("w", waitAuthority))
	if err != nil || !resumed.Resolved || resumed.Result.Kind != ControlContinue {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	done := f.stored("w")
	value := controlResultValue(t, done)
	if done.State != ToolEntryCompleted || value["status"] != "elapsed" || value["waitedSeconds"] != float64(92) {
		t.Fatalf("result = %s %v", done.State, value)
	}
}

func TestWaitWokenEarlyBySteering(t *testing.T) {
	f := newInteractionFixture(t, []LoopToolCall{waitCall("w", 600)})
	f.settleWait(t, "w")
	f.clock = f.clock.Add(100 * time.Second)
	f.run.PendingInterventions = append(f.run.PendingInterventions, AgentRunIntervention{
		ID: "steer-1", Instruction: "Actually, check now.", CreatedAt: f.clock,
	})
	resumed, err := f.exec.Resume(f.ctx, f.context("w", waitAuthority))
	if err != nil || !resumed.Resolved {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	done := f.stored("w")
	value := controlResultValue(t, done)
	if done.State != ToolEntryCompleted || value["status"] != "woken_early" || value["reason"] != "new_message" || value["remainingSeconds"] != float64(500) {
		t.Fatalf("result = %s %v", done.State, value)
	}
}

func TestWaitSecondsOutOfRangeIsArgumentsError(t *testing.T) {
	f := newInteractionFixture(t, []LoopToolCall{waitCall("zero", 0), waitCall("long", maximumWaitSeconds+1)})
	for _, callID := range []string{"zero", "long"} {
		result, err := f.exec.Settle(f.ctx, f.context(callID, waitAuthority))
		if err != nil || result.Kind != ControlContinue {
			t.Fatalf("%s: %#v, %v", callID, result, err)
		}
		if value := controlResultValue(t, f.stored(callID)); value["error"] != controlArgumentsInvalidCode {
			t.Fatalf("%s: %v", callID, value)
		}
	}
}
