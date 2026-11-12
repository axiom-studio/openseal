package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/appproject"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestMessageHeaderExactFormat(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 42, 13, 0, time.UTC)
	session := &models.AgentSession{ID: "session", AgentID: "a_1"}
	user := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "u_7"}
	for _, tc := range []struct {
		name      string
		message   models.ChannelMessage
		teamOwned bool
		want      string
	}{
		{"user reply", models.ChannelMessage{ID: "m_41", Sender: user, SenderDisplayName: "Maya", CreatedAt: at, ReplyToMessageID: "m_40"},
			false, "[Maya (user u_7) · m_41 · 2026-11-07 10:42 UTC · replying to m_40]"},
		{"teammate", models.ChannelMessage{ID: "m_42", Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "a_2"}, SenderDisplayName: "Rio", CreatedAt: at},
			true, "[Rio (agent, teammate a_2) · m_42 · 2026-11-07 10:42 UTC]"},
		{"other agent", models.ChannelMessage{ID: "m_43", Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "a_3"}, SenderDisplayName: "Ada", CreatedAt: at},
			false, "[Ada (agent a_3) · m_43 · 2026-11-07 10:42 UTC]"},
		{"you elsewhere", models.ChannelMessage{ID: "m_44", Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "a_1"}, SenderDisplayName: "Finn", CreatedAt: at},
			true, "[Finn (you, elsewhere a_1) · m_44 · 2026-11-07 10:42 UTC]"},
		{"service without a display name", models.ChannelMessage{ID: "m_45", Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "openseal"}, CreatedAt: at},
			false, "[openseal (service openseal) · m_45 · 2026-11-07 10:42 UTC]"},
		{"edit", models.ChannelMessage{ID: "m_46", Sender: user, SenderDisplayName: "Maya", CreatedAt: at, SupersedesMessageID: "m_41"},
			false, "[Maya (user u_7) · m_46 · 2026-11-07 10:42 UTC · edits m_41]"},
		{"historical slack", models.ChannelMessage{ID: "m_47", Sender: user, SenderDisplayName: "Maya", CreatedAt: at.Add(time.Hour), Historical: true,
			ExternalSource: &models.ExternalMessageSource{Provider: "slack", ChannelID: "C1", ChannelName: "launch", ParticipantID: "U9", OccurredAt: at.Add(-3 * time.Second)}},
			false, "[Maya (user u_7) · m_47 · 2026-11-07 11:42 UTC · historical, occurred 2026-11-07 10:42 UTC · via slack launch U9]"},
		{"live slack without a channel name", models.ChannelMessage{ID: "m_48", Sender: user, SenderDisplayName: "Maya", CreatedAt: at,
			ExternalSource: &models.ExternalMessageSource{Provider: "slack", ChannelID: "C1", ParticipantID: "U9"}},
			false, "[Maya (user u_7) · m_48 · 2026-11-07 10:42 UTC · via slack C1 U9]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attribution := conversationMessageAttribution(&tc.message, session, tc.teamOwned)
			if got := renderConversationMessageHeader(&tc.message, attribution); got != tc.want {
				t.Fatalf("header\n got %s\nwant %s", got, tc.want)
			}
		})
	}

	t.Run("body with quote, no text and attachments", func(t *testing.T) {
		message := &models.ChannelMessage{ID: "m_41", Sender: user, SenderDisplayName: "Maya", CreatedAt: at, ReplyToMessageID: "m_40",
			Quote: &models.ChannelMessageQuote{MessageID: "m_40", SenderDisplayName: "Rio", Excerpt: "the press list…"}}
		attached := []conversationAttachedArtifact{{ID: "a_1", Version: 2, Name: "guests.csv"}, {ID: "a_2", Version: 1}}
		want := "[Maya (user u_7) · m_41 · 2026-11-07 10:42 UTC · replying to m_40]\n" +
			"> Quoting Rio: \"the press list…\"\n" +
			"(no text)\n" +
			"Attached: guests.csv (artifact a_1 v2), a_2 (artifact a_2 v1)"
		if got := renderConversationMessageBody(message, conversationAttributionUser, attached); got != want {
			t.Fatalf("body\n got %q\nwant %q", got, want)
		}
		message.Content = "Can you add Kim?"
		message.Quote = &models.ChannelMessageQuote{MessageID: "m_40", SenderDisplayName: ""}
		if got := renderConversationMessageBody(message, conversationAttributionUser, nil); !strings.HasSuffix(got, "\n> Quoting someone: \"\"\nCan you add Kim?") {
			t.Fatalf("anonymous quote: %q", got)
		}
		message.Quote = &models.ChannelMessageQuote{MessageID: "m_40", Unavailable: true}
		if got := renderConversationMessageBody(message, conversationAttributionUser, nil); strings.Contains(got, "Quoting") {
			t.Fatalf("unavailable quote rendered: %q", got)
		}
	})
}

// attachmentFixture stores artifacts whose content is served by ref.
type attachmentFixture struct {
	t       *testing.T
	store   *sqlite.Store
	content *conversationAppContent
	scope   models.Scope
}

func newAttachmentFixture(t *testing.T) *attachmentFixture {
	return &attachmentFixture{t: t, store: newConversationTestStore(t), content: &conversationAppContent{byRef: map[string]string{}}, scope: conversationTestArtifact().Scope}
}

func (f *attachmentFixture) put(id, name, mediaType, data string) models.ConversationReference {
	f.t.Helper()
	artifact := conversationTestArtifact()
	artifact.ID, artifact.Name, artifact.MediaType, artifact.Scope = id, name, mediaType, f.scope
	artifact.ContentRef = "object-store:" + id
	artifact.SizeBytes, artifact.Digest = int64(len(data)), conversationTestDigest(data)
	f.content.byRef[artifact.ContentRef] = data
	if err := f.store.CreateArtifactVersion(f.t.Context(), artifact, 0); err != nil {
		f.t.Fatal(err)
	}
	return models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: id, Version: 1}
}

func (f *attachmentFixture) putApp(id string, files []appproject.File) models.ConversationReference {
	f.t.Helper()
	project, err := appproject.New("Tracker", files)
	if err != nil {
		f.t.Fatal(err)
	}
	data, err := project.Marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	ref := f.put(id, "Tracker", appproject.MediaType, string(data))
	return ref
}

func TestAttachmentPartsBudgetsAndImageRefs(t *testing.T) {
	f := newAttachmentFixture(t)
	png := conversationTestPNG(t)
	big := strings.Repeat("a", 60<<10)
	appFiles := []appproject.File{appEntry("big")}
	for _, name := range []string{"a", "b", "c"} {
		appFiles = append(appFiles, appproject.File{Path: "src/" + name + ".ts", Content: "export const " + name + " = `" + strings.Repeat(name, 20<<10) + "`;\n"})
	}
	refs := []models.ConversationReference{
		f.put("notes", "notes.txt", "text/plain", big),                        // 60 KiB of the 64 KiB file budget
		f.put("extra", "extra.txt", "text/plain", strings.Repeat("b", 8<<10)), // over what is left
		f.putApp("app", appFiles),                                             // 48 KiB app budget
		f.put("chart", "chart.png", "image/png", png),
		f.put("tiny", "tiny.csv", "text/csv", "k,v\n1,2"),
	}
	for i := 0; i < 5; i++ {
		refs = append(refs, f.put(fmt.Sprintf("more-%d", i), "more.csv", "text/csv", "x"))
	}
	message := &models.ChannelMessage{ID: "m_41", Scope: f.scope, ConversationID: "chat", Sequence: 41, ThreadRootID: "m_1", ReplyToMessageID: "m_40",
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "u_7"}, SenderDisplayName: "Maya",
		Content: "See attached", CreatedAt: time.Date(2026, 11, 7, 10, 42, 0, 0, time.UTC), References: refs}
	conversation := &models.Conversation{ID: "chat", Scope: f.scope}
	session := &models.AgentSession{ID: "session-1", Scope: f.scope, AgentID: "a_1"}
	reader := &conversationAttachmentReader{artifacts: f.store, content: f.content}

	entry := readConversationMessageEntry(t.Context(), reader, conversation, session, "run-1", message, false)
	if err := entry.Validate(); err != nil {
		t.Fatal(err)
	}
	if entry.ID != models.SessionEntryID("session-1", models.ChannelMessageSourceKey("m_41")) || entry.SourceKey != "message:m_41" || entry.SessionID != "session-1" ||
		entry.RunID != "run-1" || entry.Type != models.SessionEntryUser || entry.Tokens <= 0 {
		t.Fatalf("entry identity = %#v", entry)
	}
	user := entry.User
	if user.Origin != models.UserEntryChannelMessage || user.Author != (models.SessionAuthor{Type: "user", ID: "u_7", DisplayName: "Maya"}) ||
		user.MessageID != "m_41" || user.MessageSequence != 41 || user.ThreadRootID != "m_1" || user.ReplyToMessageID != "m_40" {
		t.Fatalf("user payload = %#v", user)
	}
	// One text part for the message, then one part per attachment, capped at 8.
	if len(user.Content) != 1+conversationAttachmentsPerMessage {
		t.Fatalf("got %d parts, want %d", len(user.Content), 1+conversationAttachmentsPerMessage)
	}
	body := user.Content[0].Text
	if !strings.HasPrefix(body, "[Maya (user u_7) · m_41 · 2026-11-07 10:42 UTC · replying to m_40]\nSee attached\nAttached: notes.txt (artifact notes v1), extra.txt (artifact extra v1), Tracker (artifact app v1), chart.png (artifact chart v1), tiny.csv (artifact tiny v1), more.csv (artifact more-0 v1)") ||
		strings.Contains(body, "more-3") {
		t.Fatalf("body = %q", body)
	}
	texts := map[string]string{}
	for _, part := range user.Content[1:] {
		if part.Type == models.ContentPartText {
			id := strings.SplitN(strings.TrimPrefix(part.Text, `<attachment id="`), `"`, 2)[0]
			texts[id] = part.Text
		}
	}
	if !strings.Contains(texts["notes"], `status="supplied">`) || len(texts["notes"]) > conversationAttachmentBudget+200 {
		t.Fatalf("notes part: %d bytes", len(texts["notes"]))
	}
	if texts["extra"] != `<attachment id="extra" version="1" name="extra.txt" status="size_limit"/>` {
		t.Fatalf("file budget not shared across the message: %q", texts["extra"][:min(len(texts["extra"]), 200)])
	}
	if !strings.Contains(texts["app"], "Omitted to fit the context budget") || len(texts["app"]) > conversationAppAttachmentBudget+200 {
		t.Fatalf("app part over its budget: %d bytes", len(texts["app"]))
	}
	if !strings.Contains(texts["tiny"], "k,v\n1,2") {
		t.Fatalf("tiny part = %q", texts["tiny"])
	}
	var images []models.ContentPart
	for _, part := range user.Content {
		if part.Type == models.ContentPartImage {
			images = append(images, part)
		}
	}
	if len(images) != 1 || images[0].ArtifactID != "chart" || images[0].ArtifactVersion != "1" || images[0].MediaType != "image/png" ||
		len(images[0].SHA256) != 64 || images[0].DataBase64 != "" {
		t.Fatalf("image refs = %#v", images)
	}
	encoded, err := json.Marshal(entry)
	if err != nil || strings.Contains(string(encoded), "dataBase64") || strings.Contains(string(encoded), "iVBOR") {
		t.Fatalf("image bytes stored in the entry")
	}
}

func TestMessageEntryStaysWithinUserEntryCap(t *testing.T) {
	f := newAttachmentFixture(t)
	appFiles := []appproject.File{appEntry("big")}
	for _, name := range []string{"a", "b"} {
		appFiles = append(appFiles, appproject.File{Path: "src/" + name + ".ts", Content: strings.Repeat(name, 20<<10)})
	}
	refs := []models.ConversationReference{
		f.put("notes", "notes.txt", "text/plain", strings.Repeat("n", 60<<10)),
		f.putApp("app", appFiles),
	}
	message := &models.ChannelMessage{ID: "m_1", Scope: f.scope, ConversationID: "chat", Sequence: 1,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "u"}, Content: strings.Repeat("c", 65536), CreatedAt: time.Now(), References: refs}
	reader := &conversationAttachmentReader{artifacts: f.store, content: f.content}
	entry := readConversationMessageEntry(t.Context(), reader, &models.Conversation{ID: "chat", Scope: f.scope}, &models.AgentSession{ID: "s", Scope: f.scope}, "run", message, false)
	if err := entry.Validate(); err != nil {
		t.Fatalf("a full message with full attachments must still be a valid entry: %v", err)
	}
	total := 0
	for _, part := range entry.User.Content {
		total += len(part.Text)
	}
	if total > models.MaxUserEntryContentBytes {
		t.Fatalf("content %d bytes over the cap", total)
	}
}

func TestMessageEntryWithoutReaderHasOnlyTheText(t *testing.T) {
	message := &models.ChannelMessage{ID: "m_2", Scope: conversationTestArtifact().Scope, ConversationID: "chat", Sequence: 2,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "a_9"}, SenderDisplayName: "Rio", Content: "Done.",
		CreatedAt: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)}
	entry := readConversationMessageEntry(t.Context(), nil, nil, &models.AgentSession{ID: "s", AgentID: "a_1"}, "run", message, true)
	if err := entry.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(entry.User.Content) != 1 || entry.User.Content[0].Text != "[Rio (agent, teammate a_9) · m_2 · 2026-11-07 09:00 UTC]\nDone." ||
		entry.User.Author.Type != "agent" {
		t.Fatalf("entry = %#v", entry.User)
	}
}
