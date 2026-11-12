package runtime

import (
	"strings"
)

func externalMessageSource(endpoint *ExternalConversationEndpoint, event NormalizedExternalConversationEvent) *ExternalMessageSource {
	source := &ExternalMessageSource{Provider: endpoint.Provider, WorkspaceID: endpoint.InstallationID,
		ChannelID: event.ExternalConversationID, ThreadID: event.ExternalThreadID, MessageID: event.ExternalMessageID,
		ParticipantID: event.ExternalParticipantID, ParticipantDisplayName: sourceLabel(event.ParticipantDisplayName, 160), OccurredAt: event.OccurredAt}
	if team, ok := event.Attributes["teamId"].(string); ok && source.WorkspaceID == "" {
		source.WorkspaceID = team
	}
	if kind, ok := event.Attributes["channelType"].(string); ok {
		source.ChannelType = sourceLabel(kind, 40)
	}
	if event.Direct {
		source.ChannelType = "im"
	}
	if extra := event.Source; extra != nil && extra.ChannelID == source.ChannelID && extra.ParticipantID == source.ParticipantID {
		if extra.Provider == source.Provider && validExternalConversationReference(extra.MessageID, 1024) {
			// Provider-native IDs aid provenance and reply addressing. Canonical
			// external mapping identity remains event.ExternalMessageID.
			source.MessageID = extra.MessageID
		}
		source.ChannelName = sourceLabel(extra.ChannelName, 160)
		if extra.ChannelType != "" {
			source.ChannelType = sourceLabel(extra.ChannelType, 40)
		}
	}
	return source
}

func sourceLabel(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}
