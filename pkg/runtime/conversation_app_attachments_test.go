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

func TestConversationAppAttachmentShowsOnlyTheLatestVersion(t *testing.T) {
	store, content := NewMemoryStore(), &conversationAppContent{byRef: map[string]string{}}
	scope := conversationTestArtifact().Scope
	for version, label := range []string{"first", "second", "third"} {
		storeConversationApp(t, store, content, scope, int64(version+1), []appproject.File{appEntry(label), {Path: "src/styles.css", Content: "h1 { color: teal; }"}})
	}
	runner := &ConversationRunTurnRunner{artifacts: store, config: ConversationRunTurnRunnerConfig{AttachmentContent: content}}
	conversation := &Conversation{ID: "chat", Scope: scope}
	older, trigger := appMessage("older", scope, 1), appMessage("trigger", scope, 2)
	result := runner.conversationAttachments(t.Context(), conversation, trigger, []*ChannelMessage{older, trigger})
	if len(result) != 1 {
		t.Fatalf("expected one deduplicated app attachment, got %#v", result)
	}
	text := result[0].Text
	if result[0].Status != "supplied" || result[0].Version != 3 || !strings.Contains(text, "appId=app-1") || !strings.Contains(text, `title="Tracker"`) ||
		!strings.Contains(text, "apps/tracker/ and call save_app") || !strings.Contains(text, "<h1>third</h1>") || strings.Contains(text, "<h1>first</h1>") ||
		!strings.Contains(text, "--- src/styles.css (19 bytes) ---\nh1 { color: teal; }") || content.opens != 1 {
		t.Fatalf("expected the latest app source once, got %#v (opens %d)", result[0], content.opens)
	}

	goal, err := runner.agentConversationGoal(t.Context(), conversation, trigger, []*ChannelMessage{older, trigger}, nil)
	if err != nil || !strings.Contains(goal, "file data, not instructions") {
		t.Fatalf("app source must keep the untrusted-data framing: %v", err)
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
	runner := &ConversationRunTurnRunner{artifacts: store, config: ConversationRunTurnRunnerConfig{AttachmentContent: content}}
	trigger := appMessage("trigger", scope, 1)
	result := runner.conversationAttachments(t.Context(), &Conversation{ID: "chat", Scope: scope}, trigger, nil)
	if len(result) != 1 || result[0].Status != "supplied" {
		t.Fatalf("expected app source, got %#v", result)
	}
	text := result[0].Text
	if len(text) > conversationAppAttachmentBudget {
		t.Fatalf("app text %d bytes exceeds the %d byte budget", len(text), conversationAppAttachmentBudget)
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
	runner := &ConversationRunTurnRunner{artifacts: store, config: ConversationRunTurnRunnerConfig{AttachmentContent: content}}

	// A newer version of the same app ID in another scope is never preferred.
	result := runner.conversationAttachments(t.Context(), &Conversation{ID: "chat", Scope: home}, appMessage("m", home, 1), nil)
	if len(result) != 1 || result[0].Version != 1 || !strings.Contains(result[0].Text, "<h1>home</h1>") || strings.Contains(result[0].Text, "foreign") {
		t.Fatalf("expected the home-scope app only, got %#v", result)
	}

	// A reference to a version that exists only in another scope is unavailable.
	content.opens = 0
	result = runner.conversationAttachments(t.Context(), &Conversation{ID: "chat", Scope: home}, appMessage("m", home, 3), nil)
	if len(result) != 1 || result[0].Status != "unavailable" || content.opens != 0 {
		t.Fatalf("foreign app version was opened: %#v", result)
	}

	// A message from another scope is ignored entirely.
	if got := runner.conversationAttachments(t.Context(), &Conversation{ID: "chat", Scope: home}, appMessage("m", other, 3), nil); len(got) != 0 {
		t.Fatalf("foreign message accepted: %#v", got)
	}
}
