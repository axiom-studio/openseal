package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	"github.com/axiom-studio/openseal/pkg/attachments"
)

// AttachmentReadStore deliberately exposes no unscoped lookup.
type AttachmentReadStore interface {
	GetConversation(context.Context, Scope, string) (*Conversation, error)
	ListChannelMessages(context.Context, ChannelMessageFilter) ([]*ChannelMessage, error)
	GetArtifact(context.Context, Scope, string, int64) (*Artifact, error)
}

// ReadAttachedFile is the read-only skill implementation. Scope, owner, and
// conversationID must come from the persisted executing run, never tool arguments.
func ReadAttachedFile(ctx context.Context, store AttachmentReadStore, content ArtifactContentStore, scope Scope, owner ObjectiveOwner, conversationID, artifactID string, version int64) (map[string]interface{}, error) {
	denied := errors.New("attached file is not available in this conversation")
	if store == nil || content == nil || scope.Validate() != nil || owner.Validate() != nil || conversationID == "" || artifactID == "" || version < 1 {
		return nil, denied
	}
	conversation, err := store.GetConversation(ctx, scope, conversationID)
	if err != nil || conversation == nil || conversation.Scope != scope || conversation.ID != conversationID || conversation.Owner != owner {
		return nil, denied
	}
	found := false
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantType(owner.Type), ID: owner.ID}}
	before := int64(0)
	for !found {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversationID, BeforeSequence: before, Descending: true, Limit: 100, Viewer: &viewer})
		if err != nil {
			return nil, denied
		}
		if len(messages) == 0 {
			break
		}
		next := before
		for _, message := range messages {
			if message == nil || message.Scope != scope || message.ConversationID != conversationID {
				continue
			}
			if message.Sequence > 0 && (next == 0 || message.Sequence < next) {
				next = message.Sequence
			}
			for _, ref := range message.References {
				if ref.Kind == ConversationReferenceArtifact && ref.ID == artifactID && ref.Version == version {
					found = true
					break
				}
			}
		}
		if found || next == before || next <= 1 {
			break
		}
		before = next
	}
	if !found {
		return nil, denied
	}
	artifact, err := store.GetArtifact(ctx, scope, artifactID, version)
	if err != nil || artifact == nil || artifact.Scope != scope || artifact.ID != artifactID || artifact.Version != version {
		return nil, denied
	}
	result := map[string]interface{}{"artifactId": artifactID, "version": version}
	status, text := "unreadable", ""
	mediaType, _, _ := mime.ParseMediaType(artifact.MediaType)
	if mediaType == "application/pdf" {
		if artifact.SizeBytes <= 0 || artifact.SizeBytes > attachments.MaximumFileBytes {
			status = "size_limit"
		} else if reader, err := content.Open(ctx, scope, artifact.ContentRef); err == nil {
			data, readErr := io.ReadAll(io.LimitReader(reader, attachments.MaximumFileBytes+1))
			reader.Close()
			if readErr == nil && int64(len(data)) == artifact.SizeBytes && strings.EqualFold(fmt.Sprintf("sha256:%x", sha256.Sum256(data)), artifact.Digest) {
				status, text = attachments.PDFText(ctx, data)
			}
		}
	} else {
		status, text = readConversationAttachment(ctx, content, artifact, attachments.MaximumTextBytes)
		if status != "supplied" && status != "size_limit" && status != "unsupported_format" {
			status, text = "unreadable", ""
		}
	}
	result["status"] = status
	if status == "supplied" {
		result["text"] = text
	}
	return result, nil
}
