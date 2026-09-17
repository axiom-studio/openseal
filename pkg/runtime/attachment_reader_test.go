package runtime

import (
	"context"
	"testing"
)

type attachmentReadFixture struct {
	conversation  *Conversation
	messages      []*ChannelMessage
	artifact      *Artifact
	artifactReads int
}

func (f *attachmentReadFixture) GetConversation(context.Context, Scope, string) (*Conversation, error) {
	return f.conversation, nil
}
func (f *attachmentReadFixture) ListChannelMessages(_ context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	if filter.BeforeSequence != 0 {
		return nil, nil
	}
	return f.messages, nil
}
func (f *attachmentReadFixture) GetArtifact(context.Context, Scope, string, int64) (*Artifact, error) {
	f.artifactReads++
	return f.artifact, nil
}

func TestReadAttachedFileAuthorization(t *testing.T) {
	for _, variant := range []string{"valid", "other owner", "other tenant", "other conversation", "other version", "missing reference", "tampered content", "artifact tenant mismatch"} {
		t.Run(variant, func(t *testing.T) {
			artifact := conversationTestArtifact()
			artifact.MediaType, artifact.SizeBytes, artifact.Digest = "text/plain", 5, conversationTestDigest("hello")
			scope := artifact.Scope
			owner := ObjectiveOwner{Type: "agent", ID: "agent-one"}
			conversation := &Conversation{ID: "chat", Scope: scope, Owner: owner}
			message := &ChannelMessage{Scope: scope, ConversationID: "chat", Sequence: 1, References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: 1}}}
			fixture := &attachmentReadFixture{conversation: conversation, messages: []*ChannelMessage{message}, artifact: artifact}
			content := &conversationTestContent{text: "hello"}
			switch variant {
			case "other owner":
				conversation.Owner.ID = "agent-two"
			case "other tenant":
				conversation.Scope.ID = "two"
			case "other conversation":
				message.ConversationID = "another-chat"
			case "other version":
				message.References[0].Version = 2
			case "missing reference":
				message.References = nil
			case "tampered content":
				content.text = "wrong"
			case "artifact tenant mismatch":
				artifact.Scope.ID = "two"
			}
			result, err := ReadAttachedFile(t.Context(), fixture, content, scope, owner, "chat", "file", 1)
			switch variant {
			case "valid":
				if err != nil || result["status"] != "supplied" || result["text"] != "hello" {
					t.Fatalf("valid attachment not read: %v %v", result, err)
				}
			case "tampered content":
				if err != nil || result["status"] != "unreadable" || result["text"] != nil {
					t.Fatalf("tampered data exposed: %v %v", result, err)
				}
			default:
				if err == nil || result != nil || content.opens != 0 {
					t.Fatalf("unauthorized file read: %v %v opens=%d", result, err, content.opens)
				}
				if variant != "artifact tenant mismatch" && fixture.artifactReads != 0 {
					t.Fatal("artifact looked up before conversation authority verified")
				}
			}
		})
	}
}
