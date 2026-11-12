package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
)

func conversationTestPNG(t *testing.T) string {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return encoded.String()
}

func TestConversationImageBecomesReferencePart(t *testing.T) {
	content := conversationTestPNG(t)
	artifact := conversationTestArtifact()
	artifact.Name, artifact.MediaType, artifact.SizeBytes, artifact.Digest = "chart.png", "image/png", int64(len(content)), conversationTestDigest(content)
	store := kerneltest.NewStore(t)
	if err := store.CreateArtifactVersion(t.Context(), artifact, 0); err != nil {
		t.Fatal(err)
	}
	bytesStore := &conversationTestContent{text: content}
	reader := &AttachmentReader{artifacts: store, content: bytesStore}
	conversation := &models.Conversation{ID: "chat", Scope: artifact.Scope}
	message := &models.ChannelMessage{ID: "message", Scope: artifact.Scope, ConversationID: "chat", References: []models.ConversationReference{{Kind: models.ConversationReferenceArtifact, ID: artifact.ID, Version: 1}}}
	got := reader.parts(t.Context(), conversation, message, models.MaxUserEntryContentBytes)
	digest := sha256.Sum256([]byte(content))
	want := models.ContentPart{Type: models.ContentPartImage, MediaType: "image/png", Detail: "auto", SHA256: hex.EncodeToString(digest[:]), ArtifactID: artifact.ID, ArtifactVersion: "1"}
	if len(got.Parts) != 1 || !reflect.DeepEqual(got.Parts[0], want) || bytesStore.opens != 1 {
		t.Fatalf("image not projected once as a reference: %#v (opens %d)", got.Parts, bytesStore.opens)
	}
	if len(got.Attached) != 1 || got.Attached[0] != (conversationAttachedArtifact{ID: artifact.ID, Version: 1, Name: "chart.png"}) {
		t.Fatalf("attached = %#v", got.Attached)
	}
	encoded, err := json.Marshal(got.Parts)
	if err != nil || strings.Contains(string(encoded), base64.StdEncoding.EncodeToString([]byte(content))) || strings.Contains(string(encoded), "dataBase64") {
		t.Fatalf("image bytes leaked into the part: %s", encoded)
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
			status, verified := verifyConversationImage(t.Context(), &conversationTestContent{text: tc.content}, &copy, tc.limit)
			if status != tc.want || verified != nil {
				t.Fatalf("invalid image exposed: %s %d bytes", status, len(verified))
			}
		})
	}
	t.Run("failed image is a self-closed text part", func(t *testing.T) {
		bytesStore.text = "bad"
		got := reader.parts(t.Context(), conversation, message, models.MaxUserEntryContentBytes)
		if len(got.Parts) != 1 || got.Parts[0].Type != models.ContentPartText ||
			got.Parts[0].Text != `<attachment id="file" version="1" name="chart.png" status="integrity_failed"/>` {
			t.Fatalf("parts = %#v", got.Parts)
		}
	})
}

// conversationTestContent, conversationTestDigest and
// conversationTestArtifact copy conversationtest's TextContent, Digest and
// CSVArtifact for the in-package tests, which cannot import
// conversationtest. Change the copies together.
type conversationTestContent struct {
	text  string
	opens int
	scope models.Scope
}

func (s *conversationTestContent) Put(context.Context, models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	return models.ArtifactStoredContent{}, errors.New("not supported")
}
func (s *conversationTestContent) Open(_ context.Context, scope models.Scope, _ string) (io.ReadCloser, error) {
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
		{"page", "text/html", `<html><body>Original source</body></html>`, "supplied", 100, false},
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
	store := kerneltest.NewStore(t)
	artifact := conversationTestArtifact()
	content := "company,total\nAcme,42"
	artifact.MediaType, artifact.SizeBytes, artifact.Digest = "text/csv", int64(len(content)), conversationTestDigest(content)
	if err := store.CreateArtifactVersion(ctx, artifact, 0); err != nil {
		t.Fatal(err)
	}
	bytesStore := &conversationTestContent{text: content}
	reader := &AttachmentReader{artifacts: store, content: bytesStore}
	conversation := &models.Conversation{ID: "chat", Scope: artifact.Scope}
	message := &models.ChannelMessage{ID: "message", Scope: artifact.Scope, ConversationID: "chat", References: []models.ConversationReference{
		{Kind: models.ConversationReferenceArtifact, ID: "file", Version: 1},
		{Kind: models.ConversationReferenceArtifact, ID: "file", Version: 1},
		{Kind: models.ConversationReferenceRun, ID: "run-1"},
	}}
	got := reader.parts(ctx, conversation, message, models.MaxUserEntryContentBytes)
	want := "<attachment id=\"file\" version=\"1\" name=\"file.csv\" status=\"supplied\">\ncompany,total\nAcme,42\n</attachment>"
	if len(got.Parts) != 1 || !reflect.DeepEqual(got.Parts[0], models.ContentPart{Type: models.ContentPartText, Text: want}) || bytesStore.opens != 1 {
		t.Fatalf("scoped content not supplied exactly once: %#v (opens %d)", got.Parts, bytesStore.opens)
	}

	conversation.Scope.ID = "other"
	message.Scope = conversation.Scope
	got = reader.parts(ctx, conversation, message, models.MaxUserEntryContentBytes)
	if len(got.Parts) != 1 || got.Parts[0].Text != `<attachment id="file" version="1" status="unavailable"/>` || bytesStore.opens != 1 {
		t.Fatalf("foreign tenant content was opened: %#v", got.Parts)
	}
	message.Scope = artifact.Scope
	if got := reader.parts(ctx, conversation, message, models.MaxUserEntryContentBytes); len(got.Parts) != 0 || len(got.Attached) != 0 {
		t.Fatal("foreign message accepted")
	}
	conversation.Scope = artifact.Scope
	message.ConversationID = "another-chat"
	if got := reader.parts(ctx, conversation, message, models.MaxUserEntryContentBytes); len(got.Parts) != 0 {
		t.Fatal("message of another conversation accepted")
	}
}

func conversationTestDigest(content string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))
}

func conversationTestArtifact() *models.Artifact {
	return &models.Artifact{ID: "file", Version: 1, Scope: models.Scope{Kind: "tenant", ID: "one"},
		Name: "file.csv", ContentRef: "object-store:file", Classification: models.ArtifactClassificationInternal,
		Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "user", ID: "owner"}}}
}
