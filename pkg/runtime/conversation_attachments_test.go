package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
)

func TestConversationImageProjection(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	content := encoded.String()
	artifact := conversationTestArtifact()
	artifact.MediaType, artifact.SizeBytes, artifact.Digest = "image/png", int64(len(content)), conversationTestDigest(content)
	store := NewMemoryStore()
	if err := store.CreateArtifactVersion(t.Context(), artifact, 0); err != nil {
		t.Fatal(err)
	}
	reader := &conversationTestContent{text: content}
	runner := &ConversationRunTurnRunner{artifacts: store, config: ConversationRunTurnRunnerConfig{AttachmentContent: reader}}
	conversation := &Conversation{ID: "chat", Scope: artifact.Scope}
	message := &ChannelMessage{ID: "message", Scope: artifact.Scope, ConversationID: "chat", References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: 1}}}
	attachments := runner.conversationAttachments(t.Context(), conversation, message, []*ChannelMessage{message})
	if len(attachments) != 1 || attachments[0].Status != "image_context" || attachments[0].media == nil || reader.opens != 1 {
		t.Fatalf("image not projected once: %#v", attachments)
	}
	media := *attachments[0].media
	if media.SourceArtifactID != artifact.ID || media.SourceArtifactVersion != 1 || media.SourceMessageID != message.ID || media.SourceActionCallID != "" {
		t.Fatal("incorrect image provenance")
	}
	if err := validateHostedTurnMedia(media); err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(attachments)
	if err != nil || strings.Contains(string(text), media.DataBase64) {
		t.Fatal("binary image leaked into textual context")
	}
	for _, tc := range []struct {
		name, content, mime, digest, want string
		limit                             int
	}{
		{"oversize", content, "image/png", artifact.Digest, "size_limit", 1},
		{"tampered", content, "image/png", conversationTestDigest("wrong"), "integrity_failed", 1024},
		{"wrong format", content, "image/jpeg", artifact.Digest, "unsupported_encoding", 1024},
		{"not image", "bad", "image/png", conversationTestDigest("bad"), "unsupported_encoding", 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *artifact
			copy.MediaType, copy.Digest, copy.SizeBytes = tc.mime, tc.digest, int64(len(tc.content))
			status, media, consumed := readConversationImage(t.Context(), &conversationTestContent{text: tc.content}, &copy, message.ID, tc.limit)
			if status != tc.want || media != nil || consumed != 0 {
				t.Fatalf("invalid image exposed: %s %v %d", status, media, consumed)
			}
		})
	}
}

type conversationTestContent struct {
	text  string
	opens int
	scope Scope
}

func (s *conversationTestContent) Put(context.Context, ArtifactContentWrite) (ArtifactStoredContent, error) {
	return ArtifactStoredContent{}, errors.New("not supported")
}
func (s *conversationTestContent) Open(_ context.Context, scope Scope, _ string) (io.ReadCloser, error) {
	s.opens++
	s.scope = scope
	return io.NopCloser(strings.NewReader(s.text)), nil
}

func TestConversationAttachmentContent(t *testing.T) {
	for _, tc := range []struct {
		name, media, text, status string
		limit                     int
		tamper                    bool
	}{
		{"csv", "text/csv", "name,total\nAcme,42", "supplied", 100, false},
		{"json", "application/json; charset=utf-8", `{"total":42}`, "supplied", 100, false},
		{"image", "image/png", "binary", "unsupported_format", 100, false},
		{"large", "text/plain", "oversized", "size_limit", 2, false},
		{"tampered", "text/plain", "secret", "integrity_failed", 100, true},
		{"binary", "text/plain", "\x00", "unsupported_encoding", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := conversationTestArtifact()
			artifact.MediaType, artifact.SizeBytes, artifact.Digest = tc.media, int64(len(tc.text)), conversationTestDigest(tc.text)
			if tc.tamper {
				artifact.Digest = conversationTestDigest("different")
			}
			store := &conversationTestContent{text: tc.text}
			status, text := readConversationAttachment(t.Context(), store, artifact, tc.limit)
			if status != tc.status {
				t.Fatalf("got %s, want %s", status, tc.status)
			}
			if status == "supplied" && text != tc.text {
				t.Fatal("missing actual content")
			}
			if status != "supplied" && text != "" {
				t.Fatal("failed content was exposed")
			}
		})
	}
}

func TestConversationAttachmentScopeAndProjection(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	artifact := conversationTestArtifact()
	content := "company,total\nAcme,42"
	artifact.MediaType, artifact.SizeBytes, artifact.Digest = "text/csv", int64(len(content)), conversationTestDigest(content)
	if err := store.CreateArtifactVersion(ctx, artifact, 0); err != nil {
		t.Fatal(err)
	}
	bytes := &conversationTestContent{text: content}
	runner := &ConversationRunTurnRunner{artifacts: store, config: ConversationRunTurnRunnerConfig{AttachmentContent: bytes}}
	conversation := &Conversation{ID: "chat", Scope: artifact.Scope}
	message := &ChannelMessage{ID: "message", Scope: artifact.Scope, ConversationID: "chat", References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "file", Version: 1}}}
	goal, err := runner.agentConversationGoal(ctx, conversation, message, []*ChannelMessage{message}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(goal, `"status":"supplied"`) || !strings.Contains(goal, `Acme,42`) || bytes.opens != 1 {
		t.Fatal("scoped content not supplied exactly once")
	}
	conversation.Scope.ID = "other"
	message.Scope = conversation.Scope
	result := runner.conversationAttachments(ctx, conversation, message, nil)
	if len(result) != 1 || result[0].Status != "unavailable" || bytes.opens != 1 {
		t.Fatal("foreign tenant content was opened")
	}
	message.Scope = artifact.Scope
	if got := runner.conversationAttachments(ctx, conversation, message, nil); len(got) != 0 {
		t.Fatal("foreign message accepted")
	}
}

func conversationTestDigest(content string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))
}

func conversationTestArtifact() *Artifact {
	return &Artifact{ID: "file", Version: 1, Scope: Scope{Kind: "tenant", ID: "one"},
		Name: "file.csv", ContentRef: "object-store:file", Classification: ArtifactClassificationInternal,
		Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "user", ID: "owner"}}}
}
