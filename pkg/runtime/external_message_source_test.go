package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExternalMessageSourceSurvivesUnavailableHistoryAndReachesPrompt(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := receiveContextTestEvent(t, store, endpoint, "identity-message", "trigger", "thread", now)
	host := &recordingContextHost{result: &ExternalConversationContextResult{Status: ExternalConversationContextUnavailable, Source: &ExternalMessageSource{ChannelID: item.Event.ExternalConversationID, ParticipantID: item.Event.ExternalParticipantID, ParticipantDisplayName: "Kev", ChannelName: "engineering", ChannelType: "private_channel"}}}
	worker, _ := newContextTestWorker(t, store, contextHistoryResolver{catalog}, host, false)
	worker.now = func() time.Time { return now }
	applied, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 20})
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	message := messages[0]
	if message.ExternalSource == nil || message.ExternalSource.ChannelName != "engineering" || message.SenderDisplayName != "Kev" || message.ExternalSource.ThreadID != "thread" {
		t.Fatalf("source lost: %#v", message)
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var restored ChannelMessage
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	goal, err := (&ConversationRunTurnRunner{}).agentConversationGoal(ctx, &Conversation{ID: applied.ConversationID, Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID}}, &restored, []*ChannelMessage{&restored}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"Kev", "engineering", "workspace/channel", "current-user", "thread", "private_channel", "not necessarily the agent's owner"} {
		if !strings.Contains(goal, value) {
			t.Errorf("prompt missing %q: %s", value, goal)
		}
	}
	message.ExternalSource.ChannelName = "mutated"
	again, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 20})
	if err != nil || again[0].ExternalSource.ChannelName != "engineering" {
		t.Fatal("source shared mutable storage")
	}
}

func TestExternalMessageSourceRejectsLabelsForDifferentSenderOrChannel(t *testing.T) {
	for _, extra := range []*ExternalMessageSource{{ChannelID: "other", ParticipantID: "U1", ChannelName: "wrong"}, {ChannelID: "C1", ParticipantID: "other", ChannelName: "wrong"}} {
		got := externalMessageSource(&ExternalConversationEndpoint{Provider: "slack"}, NormalizedExternalConversationEvent{ExternalConversationID: "C1", ExternalParticipantID: "U1", Source: extra})
		if got.ChannelName != "" || got.ChannelID != "C1" || got.ParticipantID != "U1" {
			t.Fatalf("unmatched source accepted: %#v", got)
		}
	}
}

func TestExternalMessageSourcePreservesRawProviderMessageIDOnlyForMatchingProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*ExternalMessageSource)
		wantRaw bool
	}{
		{"matching provenance", func(*ExternalMessageSource) {}, true},
		{"different provider", func(s *ExternalMessageSource) { s.Provider = "slack" }, false},
		{"different channel", func(s *ExternalMessageSource) { s.ChannelID = "other-chat" }, false},
		{"different sender", func(s *ExternalMessageSource) { s.ParticipantID = "other-sender" }, false},
		{"empty ID", func(s *ExternalMessageSource) { s.MessageID = "" }, false},
		{"invalid ID", func(s *ExternalMessageSource) { s.MessageID = "102\n" }, false},
		{"oversized ID", func(s *ExternalMessageSource) { s.MessageID = strings.Repeat("x", 1025) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			extra := &ExternalMessageSource{Provider: "telegram", ChannelID: "chat-A", ParticipantID: "Kev", MessageID: "102"}
			test.mutate(extra)
			event := NormalizedExternalConversationEvent{
				ExternalConversationID: "chat-A", ExternalMessageID: "chat-A:102", ExternalParticipantID: "Kev", Source: extra,
			}
			source := externalMessageSource(&ExternalConversationEndpoint{Provider: "telegram"}, event)
			want := event.ExternalMessageID
			if test.wantRaw {
				want = "102"
			}
			if source.MessageID != want || event.ExternalMessageID != "chat-A:102" {
				t.Fatalf("provenance changed canonical mapping identity: source=%#v, event=%#v", source, event)
			}
		})
	}
}
