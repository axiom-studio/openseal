package runtime

import (
	"context"
	"testing"
)

type invocationMessageFixture struct {
	conversation *Conversation
	message      *ChannelMessage
}

func (f invocationMessageFixture) GetConversation(_ context.Context, _ Scope, id string) (*Conversation, error) {
	if f.conversation == nil || f.conversation.ID != id {
		return nil, ErrConversationNotFound
	}
	return f.conversation, nil
}
func (f invocationMessageFixture) GetChannelMessage(context.Context, Scope, string, string) (*ChannelMessage, error) {
	return f.message, nil
}

func TestRunbookSourceMessagePreservesExactScopedInvocation(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "7"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	run := &AgentRun{Kind: RunKindConversation, Scope: scope, Owner: owner, ConcurrencyKey: "chat", Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "message"}}
	f := invocationMessageFixture{&Conversation{ID: "chat", Scope: scope, Owner: owner}, &ChannelMessage{ID: "message", Scope: scope, ConversationID: "chat", Content: "Read https://example.org/report and summarize it", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "23"}}}
	got, err := runbookSourceMessage(t.Context(), f, run)
	if err != nil || got["content"] != f.message.Content || got["messageId"] != "message" {
		t.Fatalf("source=%#v err=%v", got, err)
	}
	for _, change := range []func(*invocationMessageFixture){
		func(f *invocationMessageFixture) { f.conversation.Owner.ID = "other" },
		func(f *invocationMessageFixture) { f.conversation.Scope.ID = "other" },
		func(f *invocationMessageFixture) { f.message.Scope.ID = "other" },
		func(f *invocationMessageFixture) { f.message.ConversationID = "other" },
		func(f *invocationMessageFixture) { f.message.ID = "other" },
	} {
		conversation, message := *f.conversation, *f.message
		invalid := invocationMessageFixture{&conversation, &message}
		change(&invalid)
		if _, err := runbookSourceMessage(t.Context(), invalid, run); err == nil {
			t.Fatal("accepted foreign invocation")
		}
	}
	// A threaded reply runs under "<conversation>:thread:<root>"; the source
	// message still resolves through the Run's conversation.
	threaded := *run
	threaded.ConcurrencyKey = "chat:thread:root"
	if got, err := runbookSourceMessage(t.Context(), f, &threaded); err != nil || got["conversationId"] != "chat" {
		t.Fatalf("threaded source=%#v err=%v", got, err)
	}
	invocation := map[string]interface{}{"sourceMessage": got, "summary": "Read report", "arguments": map[string]interface{}{"format": "brief"}}
	if forwardRunbookInvocation(invocation, true)["sourceMessage"] == nil {
		t.Fatal("same-agent invocation lost")
	}
	shared := forwardRunbookInvocation(invocation, false)
	if shared["sourceMessage"] != nil || shared["summary"] != "Read report" || invocation["sourceMessage"] == nil {
		t.Fatal("cross-agent privacy or source mutation")
	}
}
