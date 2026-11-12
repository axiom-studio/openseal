package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
)

// A provider message whose thread history was unavailable keeps its source
// labels in the store, and the chat step that answers it renders them in the
// message header and the run_context note, under the external source rules.
func TestExternalMessageSourceSurvivesUnavailableHistoryAndReachesStep(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := ReceiveContextEvent(t, store, endpoint, "identity-message", "trigger", "thread", now)
	host := &RecordingContextHost{Result: &externalconversation.ExternalConversationContextResult{Status: conversation.ExternalConversationContextUnavailable, Source: &ExternalMessageSource{
		ChannelID: item.Event.ExternalConversationID, ParticipantID: item.Event.ExternalParticipantID, ParticipantDisplayName: "Kev",
		ChannelName: "engineering", ChannelType: "private_channel",
	}}}
	worker, _ := NewContextWorker(t, store, ContextHistoryResolver{catalog}, host, false, models.WithClock(func() time.Time { return now }))
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
	if err := json.Unmarshal(encoded, &restored); err != nil || restored.ExternalSource == nil || *restored.ExternalSource != *message.ExternalSource {
		t.Fatalf("source does not round-trip: %#v %v", restored.ExternalSource, err)
	}
	message.ExternalSource.ChannelName = "mutated"
	again, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 20})
	if err != nil || again[0].ExternalSource.ChannelName != "engineering" {
		t.Fatal("source shared mutable storage")
	}

	// The inbox scheduled the Run that answers the message; the loop runs it.
	f := newChatLoopFixture(t, store, chatLoopOptions{})
	if f.conversation, err = f.service.GetConversation(ctx, endpoint.Scope, applied.ConversationID); err != nil {
		t.Fatal(err)
	}
	// The scheduler stamped the Run on the wall clock; the fixture clock is
	// truncated to the millisecond, so step past the Run's AvailableAt.
	f.now = f.now.Add(time.Second)
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: endpoint.Scope, WorkerID: chatLoopWorker, Now: f.now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.Context[conversationRunContextTriggerID] != again[0].ID {
		t.Fatalf("claim = %#v, %v", run, err)
	}
	producer := &chatLoopProducer{answers: []string{"I can pick up the first two."}}
	binding := bindChatAgent(f, run, producer, endpoint.Owner.ID)
	if exit := f.drive(run, binding); exit.Run.Status != AgentRunStatusCompleted || len(producer.paths) != 1 {
		t.Fatalf("exit %s, run %s, steps %d", exit.Reason, exit.Run.Status, len(producer.paths))
	}
	var trigger, runContext *SessionEntry
	for _, entry := range producer.paths[0] {
		switch {
		case entry.User != nil && entry.User.MessageID == again[0].ID:
			trigger = entry
		case entry.Note != nil && entry.Note.Kind == NoteRunContext:
			runContext = entry
		}
	}
	if trigger == nil || runContext == nil {
		t.Fatalf("step path = %v", chatLoopShape(producer.paths[0]))
	}
	header, _, _ := strings.Cut(chatLoopEntryText(trigger), "\n")
	for _, want := range []string{"Kev", "via slack engineering current-user"} {
		if !strings.Contains(header, want) {
			t.Errorf("message header missing %q: %s", want, header)
		}
	}
	note := chatLoopEntryText(runContext)
	for _, want := range []string{"slack channel engineering (private_channel)", "Provider thread history: unavailable"} {
		if !strings.Contains(note, want) {
			t.Errorf("run_context missing %q: %s", want, note)
		}
	}
	if !strings.Contains(chatStepText(binding, nil), "A provider sender is not necessarily your owner.") {
		t.Error("external source rules missing from the system prompt")
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
