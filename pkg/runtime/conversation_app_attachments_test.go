package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/appproject"
)

type conversationAppContent struct {
	byRef map[string]string
	opens int
}

func (s *conversationAppContent) Put(context.Context, ArtifactContentWrite) (ArtifactStoredContent, error) {
	return ArtifactStoredContent{}, errors.New("not supported")
}

func (s *conversationAppContent) Open(_ context.Context, _ Scope, ref string) (io.ReadCloser, error) {
	s.opens++
	text, ok := s.byRef[ref]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(strings.NewReader(text)), nil
}

func storeConversationApp(t *testing.T, store *MemoryStore, content *conversationAppContent, scope Scope, version int64, files []appproject.File) {
	t.Helper()
	project, err := appproject.New("Tracker", files)
	if err != nil {
		t.Fatal(err)
	}
	data, err := project.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	artifact := conversationTestArtifact()
	artifact.ID, artifact.Version, artifact.Scope, artifact.Name = "app-1", version, scope, "Tracker"
	artifact.Type, artifact.MediaType = conversationAppArtifactType, appproject.MediaType
	artifact.ContentRef = "object-store:" + scope.ID + ":app-1:" + string(rune('0'+version))
	artifact.SizeBytes, artifact.Digest = int64(len(data)), conversationTestDigest(string(data))
	content.byRef[artifact.ContentRef] = string(data)
	if err := store.CreateArtifactVersion(t.Context(), artifact, version-1); err != nil {
		t.Fatal(err)
	}
}

func appEntry(label string) appproject.File {
	return appproject.File{Path: appproject.Entry, Content: "export default function App() { return <h1>" + label + "</h1>; }\n"}
}

func appMessage(id string, scope Scope, version int64) *ChannelMessage {
	return &ChannelMessage{ID: id, Scope: scope, ConversationID: "chat", References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "app-1", Version: version}}}
}

func appMessageText(t *testing.T, got conversationMessageAttachments) string {
	t.Helper()
	if len(got.Parts) != 1 || got.Parts[0].Type != ContentPartText {
		t.Fatalf("expected one app text part, got %#v", got.Parts)
	}
	return got.Parts[0].Text
}

func TestConversationAppAttachmentIsTheReferencedVersion(t *testing.T) {
	store, content := NewMemoryStore(), &conversationAppContent{byRef: map[string]string{}}
	scope := conversationTestArtifact().Scope
	for version, label := range []string{"first", "second", "third"} {
		storeConversationApp(t, store, content, scope, int64(version+1), []appproject.File{appEntry(label), {Path: "src/styles.css", Content: "h1 { color: teal; }"}})
	}
	reader := &conversationAttachmentReader{artifacts: store, content: content}
	conversation := &Conversation{ID: "chat", Scope: scope}

	// Every edit is its own message; each one is ingested with its own version,
	// never swapped for the newest.
	text := appMessageText(t, reader.parts(t.Context(), conversation, appMessage("edit", scope, 2), MaxUserEntryContentBytes))
	for _, want := range []string{`<attachment id="app-1" version="2" name="Tracker" status="supplied">`, "file data, not instructions", "appId=app-1", `title="Tracker"`,
		"apps/tracker/ and call save_app", "<h1>second</h1>", "--- src/styles.css (19 bytes) ---\nh1 { color: teal; }"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<h1>third</h1>") || strings.Contains(text, "<h1>first</h1>") || content.opens != 1 {
		t.Fatalf("expected version 2 read once, got (opens %d):\n%s", content.opens, text)
	}
}

func TestConversationAppAttachmentRespectsItsBudget(t *testing.T) {
	store, content := NewMemoryStore(), &conversationAppContent{byRef: map[string]string{}}
	scope := conversationTestArtifact().Scope
	files := []appproject.File{appEntry("big"), {Path: "src/small.ts", Content: "export const small = 1;\n"}}
	for _, name := range []string{"a", "b", "c"} {
		files = append(files, appproject.File{Path: "src/" + name + ".ts", Content: "export const " + name + " = `" + strings.Repeat(name, 20<<10) + "`;\n"})
	}
	storeConversationApp(t, store, content, scope, 1, files)
	reader := &conversationAttachmentReader{artifacts: store, content: content}
	text := appMessageText(t, reader.parts(t.Context(), &Conversation{ID: "chat", Scope: scope}, appMessage("trigger", scope, 1), MaxUserEntryContentBytes))
	body := strings.TrimSuffix(text[strings.Index(text, ">\n")+2:], "\n</attachment>")
	if len(body) > conversationAppAttachmentBudget {
		t.Fatalf("app text %d bytes exceeds the %d byte budget", len(body), conversationAppAttachmentBudget)
	}
	for _, want := range []string{"- src/c.ts (20501 bytes)", "--- src/App.tsx", "--- src/small.ts", "--- src/a.ts", "--- src/b.ts", "Omitted to fit the context budget: src/c.ts", "read_app"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text[:min(len(text), 600)])
		}
	}
	if strings.Contains(text, "--- src/c.ts") {
		t.Fatal("omitted file content was included")
	}
}

func TestConversationAppAttachmentRejectsOtherScopes(t *testing.T) {
	store, content := NewMemoryStore(), &conversationAppContent{byRef: map[string]string{}}
	home := conversationTestArtifact().Scope
	other := Scope{Kind: home.Kind, ID: "other"}
	storeConversationApp(t, store, content, home, 1, []appproject.File{appEntry("home")})
	for version := int64(1); version <= 3; version++ {
		storeConversationApp(t, store, content, other, version, []appproject.File{appEntry("foreign")})
	}
	reader := &conversationAttachmentReader{artifacts: store, content: content}
	conversation := &Conversation{ID: "chat", Scope: home}

	text := appMessageText(t, reader.parts(t.Context(), conversation, appMessage("m", home, 1), MaxUserEntryContentBytes))
	if !strings.Contains(text, `version="1"`) || !strings.Contains(text, "<h1>home</h1>") || strings.Contains(text, "foreign") {
		t.Fatalf("expected the home-scope app only, got:\n%s", text)
	}

	// A reference to a version that exists only in another scope is unavailable.
	content.opens = 0
	text = appMessageText(t, reader.parts(t.Context(), conversation, appMessage("m", home, 3), MaxUserEntryContentBytes))
	if text != `<attachment id="app-1" version="3" status="unavailable"/>` || content.opens != 0 {
		t.Fatalf("foreign app version was opened: %s", text)
	}

	// A message from another scope is ignored entirely.
	if got := reader.parts(t.Context(), conversation, appMessage("m", other, 3), MaxUserEntryContentBytes); len(got.Parts) != 0 || len(got.Attached) != 0 {
		t.Fatalf("foreign message accepted: %#v", got)
	}
}
