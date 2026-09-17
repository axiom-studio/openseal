package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"strings"
	"unicode/utf8"
)

const conversationAttachmentBudget = 64 * 1024

type conversationAttachment struct {
	MessageID string `json:"messageId"`
	ID        string `json:"id"`
	Version   int64  `json:"version"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Status    string `json:"status"`
	Text      string `json:"text,omitempty"`
	media     *HostedTurnMedia
}

// Only persisted, scoped message references are eligible. Prioritize the current
// request, then recent context, without following URLs or paths from file data.
func (r *ConversationRunTurnRunner) conversationAttachments(ctx context.Context, conversation *Conversation, trigger *ChannelMessage, recent []*ChannelMessage) []conversationAttachment {
	if r == nil || conversation == nil || conversation.Scope.Validate() != nil {
		return nil
	}
	messages := []*ChannelMessage{trigger}
	for i := len(recent) - 1; i >= 0; i-- {
		messages = append(messages, recent[i])
	}
	seen := make(map[string]bool)
	remaining := conversationAttachmentBudget
	imageRemaining := maximumHostedTurnMediaBytes
	var result []conversationAttachment
	for _, message := range messages {
		if message == nil || message.Scope != conversation.Scope || message.ConversationID != conversation.ID {
			continue
		}
		for _, ref := range message.References {
			if ref.Kind != ConversationReferenceArtifact {
				continue
			}
			key := fmt.Sprintf("%s:%d", ref.ID, ref.Version)
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(result) == 8 {
				return result
			}
			attachment := conversationAttachment{MessageID: message.ID, ID: ref.ID, Version: ref.Version, Status: "unavailable"}
			if r.artifacts != nil && r.config.AttachmentContent != nil && ref.Version > 0 {
				artifact, err := r.artifacts.GetArtifact(ctx, conversation.Scope, ref.ID, ref.Version)
				if err == nil && artifact != nil && artifact.Scope == conversation.Scope && artifact.ID == ref.ID && artifact.Version == ref.Version {
					attachment.Name, attachment.MediaType = artifact.Name, artifact.MediaType
					mediaType, _, _ := mime.ParseMediaType(artifact.MediaType)
					if mediaType == "image/png" || mediaType == "image/jpeg" {
						var consumed int
						attachment.Status, attachment.media, consumed = readConversationImage(ctx, r.config.AttachmentContent, artifact, message.ID, imageRemaining)
						imageRemaining -= consumed
					} else {
						attachment.Status, attachment.Text = readConversationAttachment(ctx, r.config.AttachmentContent, artifact, remaining)
					}
					remaining -= len(attachment.Text)
				}
			}
			result = append(result, attachment)
		}
	}
	return result
}

// Image bytes remain outside the textual prompt. Verify the persisted digest,
// format and dimensions before handing them to a provider-capability gate.
func readConversationImage(ctx context.Context, store ArtifactContentStore, artifact *Artifact, messageID string, remaining int) (string, *HostedTurnMedia, int) {
	if remaining <= 0 || artifact.SizeBytes <= 0 || artifact.SizeBytes > int64(remaining) {
		return "size_limit", nil, 0
	}
	reader, err := store.Open(ctx, artifact.Scope, artifact.ContentRef)
	if err != nil {
		return "unavailable", nil, 0
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
	if err != nil || ctx.Err() != nil {
		return "unavailable", nil, 0
	}
	if len(content) > remaining {
		return "size_limit", nil, 0
	}
	if int64(len(content)) != artifact.SizeBytes || !strings.EqualFold(fmt.Sprintf("sha256:%x", sha256.Sum256(content)), artifact.Digest) {
		return "integrity_failed", nil, 0
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(content))
	mediaType, _, _ := mime.ParseMediaType(artifact.MediaType)
	if err != nil || (format != "png" && format != "jpeg") || mediaType != "image/"+format || config.Width <= 0 || config.Height <= 0 {
		return "unsupported_encoding", nil, 0
	}
	if config.Width > 16384 || config.Height > 16384 || int64(config.Width)*int64(config.Height) > 16000000 {
		return "size_limit", nil, 0
	}
	return "image_context", &HostedTurnMedia{MediaType: mediaType, DataBase64: base64.StdEncoding.EncodeToString(content), Detail: "auto", SourceArtifactID: artifact.ID, SourceArtifactVersion: artifact.Version, SourceMessageID: messageID}, len(content)
}

func readConversationAttachment(ctx context.Context, store ArtifactContentStore, artifact *Artifact, remaining int) (string, string) {
	mediaType, _, err := mime.ParseMediaType(artifact.MediaType)
	if err != nil {
		return "unsupported_format", ""
	}
	switch mediaType {
	case "text/plain", "text/csv", "text/tab-separated-values", "text/markdown", "application/json":
	case spreadsheetMediaType:
	default:
		return "unsupported_format", ""
	}
	byteLimit := remaining
	if mediaType == spreadsheetMediaType {
		byteLimit = 4 << 20
	}
	if artifact.SizeBytes < 0 || artifact.SizeBytes > int64(byteLimit) || remaining <= 0 {
		return "size_limit", ""
	}
	reader, err := store.Open(ctx, artifact.Scope, artifact.ContentRef)
	if err != nil {
		return "unavailable", ""
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, int64(byteLimit)+1))
	if err != nil {
		return "unavailable", ""
	}
	if len(content) > byteLimit {
		return "size_limit", ""
	}
	if int64(len(content)) != artifact.SizeBytes || !strings.EqualFold(fmt.Sprintf("sha256:%x", sha256.Sum256(content)), artifact.Digest) {
		return "integrity_failed", ""
	}
	if mediaType == spreadsheetMediaType {
		return spreadsheetAttachmentText(ctx, content, remaining)
	}
	if !utf8.Valid(content) || strings.ContainsRune(string(content), '\x00') {
		return "unsupported_encoding", ""
	}
	return "supplied", string(content)
}
