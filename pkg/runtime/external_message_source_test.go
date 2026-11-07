package runtime

import (
	"strings"
	"testing"
)

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
