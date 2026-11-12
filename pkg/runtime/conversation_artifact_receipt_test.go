package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// runActionArtifactReferences reads the Run's tool entries from the session
// log; this lister returns fixed entries.
type fixedEntryLister []*models.SessionEntry

func (l fixedEntryLister) ListAgentSessionEntries(_ context.Context, filter models.SessionEntryFilter) ([]*models.SessionEntry, error) {
	var out []*models.SessionEntry
	for _, entry := range l {
		if entry.RunID != filter.RunID {
			continue
		}
		if len(filter.States) > 0 && entry.State != filter.States[0] {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func TestRunActionArtifactReferencesFromSucceededCalls(t *testing.T) {
	scope := models.Scope{Kind: "tenant", ID: "one"}
	run := &models.AgentRun{ID: "run", Scope: scope}
	refs := func(values ...interface{}) map[string]interface{} {
		list := make([]interface{}, 0, len(values)/2)
		for i := 0; i+1 < len(values); i += 2 {
			list = append(list, map[string]interface{}{"id": values[i], "version": values[i+1]})
		}
		return map[string]interface{}{"artifactRefs": list}
	}
	actions := &receiptActionCalls{calls: map[string]*models.ActionCall{
		"a1": {ID: "a1", RunID: "run", Status: models.ActionCallStatusSucceeded, Output: refs("one", 1, "two", 3)},
		"a2": {ID: "a2", RunID: "run", Status: models.ActionCallStatusSucceeded, Output: refs("one", 1, strings.Repeat("x", 257), 1, "zero", 0)},
		"a3": {ID: "a3", RunID: "run", Status: models.ActionCallStatusFailed, Output: refs("failed", 1)},
	}}
	entries := fixedEntryLister{
		{ID: "e1", RunID: "run", Type: models.SessionEntryTool, State: models.ToolEntryCompleted, ActionCallID: "a1"},
		{ID: "e2", RunID: "run", Type: models.SessionEntryTool, State: models.ToolEntryCompleted, ActionCallID: "a2"},
		{ID: "e3", RunID: "run", Type: models.SessionEntryTool, State: models.ToolEntryCompleted, ActionCallID: "a3"},
		{ID: "e4", RunID: "run", Type: models.SessionEntryTool, State: models.ToolEntryCompleted, ActionCallID: "missing"},
		{ID: "e5", RunID: "run", Type: models.SessionEntryTool, State: models.ToolEntryCompleted},
	}
	got, err := runActionArtifactReferences(context.Background(), entries, actions, run)
	if err != nil {
		t.Fatal(err)
	}
	want := []models.ConversationReference{
		{Kind: models.ConversationReferenceArtifact, ID: "one", Version: 1},
		{Kind: models.ConversationReferenceArtifact, ID: "two", Version: 3},
	}
	if len(got) != len(want) || got[0].Ref != want[0] || got[1].Ref != want[1] {
		t.Fatalf("references = %#v", got)
	}
}

// Only the files the final answer hands over go out with it, named by id or
// by the path they were presented from; the Run's other files stay in the
// Library.
func TestHandedOverArtifactsKeepsOnlyTheAnswersFiles(t *testing.T) {
	saved := []models.SavedArtifact{
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "panel-1", Version: 1}, Path: "images/panel-1.png"},
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "panel-2", Version: 1}, Path: "images/panel-2.png"},
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "comic-pdf", Version: 2}, Path: "docs/comic.pdf"},
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "chart", Version: 1}},
	}
	for _, files := range [][]interface{}{{"comic-pdf"}, {"docs/comic.pdf"}, {"/docs/comic.pdf"}, {"./docs/comic.pdf", "comic-pdf"}} {
		got, unknown := handedOverArtifacts(saved, map[string]interface{}{finalOutputFilesKey: files})
		if len(got) != 1 || got[0].ID != "comic-pdf" || got[0].Version != 2 || len(unknown) != 0 {
			t.Fatalf("files %v: handed over = %+v, unknown %v; want only the comic PDF", files, got, unknown)
		}
	}
	// A file the run never presented goes back to the model rather than
	// silently attaching nothing.
	got, unknown := handedOverArtifacts(saved, map[string]interface{}{finalOutputFilesKey: []interface{}{"chart", "docs/other.pdf"}})
	if len(got) != 1 || got[0].ID != "chart" || len(unknown) != 1 || unknown[0] != "docs/other.pdf" {
		t.Fatalf("handed over = %+v, unknown = %v", got, unknown)
	}
	if text := unknownHandedOverFilesText(unknown, saved); !strings.Contains(text, "docs/other.pdf") || !strings.Contains(text, "docs/comic.pdf") || !strings.Contains(text, "chart") {
		t.Fatalf("rejection = %q; want the unknown file and the presented ones", text)
	}
	if got, unknown := handedOverArtifacts(saved, map[string]interface{}{}); len(got) != 0 || len(unknown) != 0 {
		t.Fatalf("an answer that hands over nothing attached %+v (unknown %v)", got, unknown)
	}
	if ids := handedOverFileIDs([]string{" a ", "a", "", "b"}); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("file ids = %v", ids)
	}
}

// An app the Agent saved or a file it presented was shown on purpose: it goes
// out with the answer even when the answer's files do not name it, as its
// newest version only. Prod runs 30763c84, bd4516b7 and ee3cf649 built an
// app three times and presented a page, listed nothing, and told the person
// "the game is above" with nothing above.
func TestShownAppsAndPresentedFilesGoOutWithoutBeingNamed(t *testing.T) {
	saved := []models.SavedArtifact{
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "app-1", Version: 1}, Shown: true},
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "panel", Version: 1}, Path: "images/panel.png"},
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "app-1", Version: 2}, Shown: true},
		{Ref: models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: "page", Version: 3}, Path: "pages/game.html", Shown: true},
	}
	got, unknown := handedOverArtifacts(saved, map[string]interface{}{})
	if len(unknown) != 0 || len(got) != 2 || got[0].ID != "app-1" || got[0].Version != 2 || got[1].ID != "page" {
		t.Fatalf("handed over = %+v, unknown %v; want the app's newest version and the page", got, unknown)
	}
	// Naming a shown file as well attaches it once.
	got, _ = handedOverArtifacts(saved, map[string]interface{}{finalOutputFilesKey: []interface{}{"pages/game.html", "images/panel.png"}})
	if len(got) != 3 || got[0].ID != "panel" || got[1].ID != "app-1" || got[2].ID != "page" {
		t.Fatalf("handed over = %+v; want the panel, the app and the page once each", got)
	}
	for call, want := range map[*models.ActionCall]bool{
		{SkillID: "openseal.builtin", Action: "save_app"}:       true,
		{SkillID: "openseal.builtin", Action: "present"}:        true,
		{SkillID: "openseal.computer", Action: "present"}:       true,
		{SkillID: "openseal.builtin", Action: "generate_image"}: false,
		{SkillID: "skill-other", Action: "present"}:             false,
	} {
		if actionShowsInChat(call) != want {
			t.Fatalf("%s %s shows in chat = %v, want %v", call.SkillID, call.Action, !want, want)
		}
	}
}
