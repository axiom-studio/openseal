package conversation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Channel messages reach a chat session as user entries (run-loop section 05
// §4.4). The entry's first content part is the formatted message: a header
// line with the sender, attribution, message id and time, the quoted message,
// the text and the attached files. Attachment parts follow (§4.6). The
// renderer sends only the content parts; the other UserEntry fields are
// metadata for UIs, tests and dedupe.

const (
	conversationMessageTimeLayout = "2006-01-02 15:04 UTC"
	conversationMessageNoText     = "(no text)"
	// conversationAttachedNameBytes caps one file name on the "Attached:" line.
	conversationAttachedNameBytes = 200
)

// Message attributions, as the header shows them.
const (
	conversationAttributionUser      = "user"
	conversationAttributionAgent     = "agent"
	conversationAttributionTeammate  = "agent, teammate"
	conversationAttributionElsewhere = "you, elsewhere"
	conversationAttributionService   = "service"
)

// conversationMessageAttribution says who a message is from, seen by the
// session's Agent. A message of the session's own Agent reaches the session
// only when it was produced by another session (catch-up skips its own), so it
// is "you, elsewhere".
func conversationMessageAttribution(message *models.ChannelMessage, session *models.AgentSession, teamOwned bool) string {
	if message == nil {
		return conversationAttributionService
	}
	switch message.Sender.Type {
	case models.ConversationParticipantUser:
		return conversationAttributionUser
	case models.ConversationParticipantAgent:
		if session != nil && session.AgentID != "" && message.Sender.ID == session.AgentID {
			return conversationAttributionElsewhere
		}
		if teamOwned {
			return conversationAttributionTeammate
		}
		return conversationAttributionAgent
	case models.ConversationParticipantTeam:
		return conversationAttributionAgent
	default:
		return conversationAttributionService
	}
}

// conversationMessageAuthor is the entry author for a sender. Team senders
// speak as agents; anything that is not a person or an agent is the service.
func conversationMessageAuthor(message *models.ChannelMessage) models.SessionAuthor {
	author := models.SessionAuthor{ID: message.Sender.ID, DisplayName: message.SenderDisplayName}
	switch message.Sender.Type {
	case models.ConversationParticipantUser:
		author.Type = "user"
	case models.ConversationParticipantAgent, models.ConversationParticipantTeam:
		author.Type = "agent"
	default:
		author.Type = "service"
	}
	return author
}

// renderConversationMessageHeader is the bracketed first line of a message,
// with each optional piece only when its field is set:
//
//	[<name> (<attribution> <senderId>) · <messageId> · <createdAt>
//	 · replying to <id> · edits <id> · historical, occurred <time>
//	 · via <provider> <channelName|channelId> <participantId>]
func renderConversationMessageHeader(message *models.ChannelMessage, attribution string) string {
	name := message.SenderDisplayName
	if strings.TrimSpace(name) == "" {
		name = message.Sender.ID
	}
	sender := attribution
	if message.Sender.ID != "" {
		sender += " " + message.Sender.ID
	}
	pieces := []string{fmt.Sprintf("%s (%s)", name, sender), message.ID}
	if !message.CreatedAt.IsZero() {
		pieces = append(pieces, ConversationMessageTime(message.CreatedAt))
	}
	if message.ReplyToMessageID != "" {
		pieces = append(pieces, "replying to "+message.ReplyToMessageID)
	}
	if message.SupersedesMessageID != "" {
		pieces = append(pieces, "edits "+message.SupersedesMessageID)
	}
	if message.Historical {
		historical := "historical"
		if source := message.ExternalSource; source != nil && !source.OccurredAt.IsZero() {
			historical += ", occurred " + ConversationMessageTime(source.OccurredAt)
		}
		pieces = append(pieces, historical)
	}
	if source := message.ExternalSource; source != nil {
		via := []string{"via", source.Provider}
		if source.ChannelName != "" {
			via = append(via, source.ChannelName)
		} else if source.ChannelID != "" {
			via = append(via, source.ChannelID)
		}
		if source.ParticipantID != "" {
			via = append(via, source.ParticipantID)
		}
		pieces = append(pieces, strings.Join(nonEmptyStrings(via), " "))
	}
	return "[" + strings.Join(pieces, " · ") + "]"
}

func ConversationMessageTime(at time.Time) string {
	return at.UTC().Format(conversationMessageTimeLayout)
}

// renderConversationMessageBody is the header, the quote line, the content,
// the builder parameters lines and, when files are attached, the "Attached:"
// line.
func renderConversationMessageBody(message *models.ChannelMessage, attribution string, attached []conversationAttachedArtifact) string {
	lines := []string{renderConversationMessageHeader(message, attribution)}
	if quote := message.Quote; quote != nil && !quote.Unavailable {
		who := quote.SenderDisplayName
		if strings.TrimSpace(who) == "" {
			who = "someone"
		}
		lines = append(lines, fmt.Sprintf("> Quoting %s: \"%s\"", who, quote.Excerpt))
	}
	content := message.Content
	if strings.TrimSpace(content) != "" {
		lines = append(lines, content)
	} else if message.BuilderParameters == nil {
		lines = append(lines, conversationMessageNoText)
	}
	lines = append(lines, renderAgentBuilderParameters(message.BuilderParameters)...)
	if len(attached) > 0 {
		names := make([]string, 0, len(attached))
		for _, file := range attached {
			name := models.TruncateUTF8(file.Name, conversationAttachedNameBytes)
			if name == "" {
				name = file.ID
			}
			names = append(names, fmt.Sprintf("%s (artifact %s v%d)", name, file.ID, file.Version))
		}
		lines = append(lines, "Attached: "+strings.Join(names, ", "))
	}
	return strings.Join(lines, "\n")
}

// conversationMessageEntry is the user entry of one channel message in a
// session. Its id and source key derive from the message id, so ingesting the
// same message twice into one session is a no-op append.
func conversationMessageEntry(sessionID, runID string, message *models.ChannelMessage, attribution string, attachments conversationMessageAttachments) *models.SessionEntry {
	content := make([]models.ContentPart, 0, 1+len(attachments.Parts))
	content = append(content, models.ContentPart{Type: models.ContentPartText, Text: renderConversationMessageBody(message, attribution, attachments.Attached)})
	content = append(content, attachments.Parts...)
	sourceKey := models.ChannelMessageSourceKey(message.ID)
	entry := &models.SessionEntry{
		ID: models.SessionEntryID(sessionID, sourceKey), Scope: message.Scope, SessionID: sessionID, RunID: runID,
		Type: models.SessionEntryUser, SourceKey: sourceKey,
		User: &models.UserEntry{
			Origin: models.UserEntryChannelMessage, Author: conversationMessageAuthor(message), Content: content,
			MessageID: message.ID, ThreadRootID: message.ThreadRootID, ReplyToMessageID: message.ReplyToMessageID,
			MessageSequence: message.Sequence,
		},
	}
	entry.Tokens = modelstep.EstimateEntryTokens(entry)
	return entry
}

// readConversationMessageEntry reads a message's attachments and builds its
// user entry. The attachments get what is left of MaxUserEntryContentBytes
// after the message text and the largest "Attached:" line its references can
// produce.
func readConversationMessageEntry(ctx context.Context, reader *ConversationAttachmentReader, conversation *models.Conversation, session *models.AgentSession, runID string, message *models.ChannelMessage, teamOwned bool) *models.SessionEntry {
	attribution := conversationMessageAttribution(message, session, teamOwned)
	budget := models.MaxUserEntryContentBytes - len(renderConversationMessageBody(message, attribution, nil)) - conversationAttachedLineReserve(message)
	var attachments conversationMessageAttachments
	if reader != nil {
		attachments = reader.parts(ctx, conversation, message, budget)
	}
	return conversationMessageEntry(session.ID, runID, message, attribution, attachments)
}

// conversationAttachedLineReserve bounds the "Attached:" line the message's
// artifact references can add.
func conversationAttachedLineReserve(message *models.ChannelMessage) int {
	reserve, count := len("\nAttached: "), 0
	for _, ref := range message.References {
		if ref.Kind != models.ConversationReferenceArtifact || count == conversationAttachmentsPerMessage {
			continue
		}
		count++
		reserve += 2*len(ref.ID) + conversationAttachedNameBytes + len(" (artifact  v)") + 20 + len(", ")
	}
	return reserve
}

func nonEmptyStrings(values []string) []string {
	out := values[:0:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

// renderAgentBuilderParameters is the model's view of a builder message's form
// fields (D7): "Builder parameters (set in the form): …" and, for a migrated
// draft, "Started draft: …". People never see these lines.
func renderAgentBuilderParameters(parameters *models.AgentBuilderParameters) []string {
	if parameters == nil {
		return nil
	}
	fields := []string{"kind " + parameters.Kind}
	if parameters.Name != "" {
		fields = append(fields, "name \u201c"+parameters.Name+"\u201d")
	}
	if parameters.Model != "" {
		fields = append(fields, "model "+parameters.Model)
	}
	lines := []string{"Builder parameters (set in the form): " + strings.Join(fields, "; ")}
	if draft := parameters.Draft; draft != nil && !draft.IsEmpty() {
		var parts []string
		if draft.Purpose != "" {
			parts = append(parts, "purpose "+draft.Purpose)
		}
		if draft.SystemPrompt != "" {
			parts = append(parts, "behaviour "+draft.SystemPrompt)
		}
		if draft.Personality != "" {
			parts = append(parts, "personality "+draft.Personality)
		}
		if len(draft.OperatingPrinciples) > 0 {
			parts = append(parts, "principles: "+strings.Join(draft.OperatingPrinciples, "; "))
		}
		lines = append(lines, "Started draft: "+strings.Join(parts, "; "))
	}
	return lines
}
