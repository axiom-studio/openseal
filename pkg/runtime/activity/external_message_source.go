package activity

import "github.com/axiom-studio/openseal/pkg/models"

func NewExternalMessageSource(endpoint *models.ExternalConversationEndpoint, event models.NormalizedExternalConversationEvent) *models.ExternalMessageSource {
	source := &models.ExternalMessageSource{Provider: endpoint.Provider, WorkspaceID: endpoint.InstallationID,
		ChannelID: event.ExternalConversationID, ThreadID: event.ExternalThreadID, MessageID: event.ExternalMessageID,
		ParticipantID: event.ExternalParticipantID, ParticipantDisplayName: models.ExternalSourceLabel(event.ParticipantDisplayName, 160), OccurredAt: event.OccurredAt}
	if team, ok := event.Attributes["teamId"].(string); ok && source.WorkspaceID == "" {
		source.WorkspaceID = team
	}
	if kind, ok := event.Attributes["channelType"].(string); ok {
		source.ChannelType = models.ExternalSourceLabel(kind, 40)
	}
	if event.Direct {
		source.ChannelType = "im"
	}
	if extra := event.Source; extra != nil && extra.ChannelID == source.ChannelID && extra.ParticipantID == source.ParticipantID {
		if extra.Provider == source.Provider && models.IsValidExternalConversationReference(extra.MessageID, 1024) {
			// Provider-native IDs aid provenance and reply addressing. Canonical
			// external mapping identity remains event.ExternalMessageID.
			source.MessageID = extra.MessageID
		}
		source.ChannelName = models.ExternalSourceLabel(extra.ChannelName, 160)
		if extra.ChannelType != "" {
			source.ChannelType = models.ExternalSourceLabel(extra.ChannelType, 40)
		}
	}
	return source
}
