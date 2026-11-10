package runtime

import (
	"context"
	"strings"
	"testing"
)

// runActionArtifactReferences reads the Run's tool entries from the session
// log; this lister returns fixed entries.
type fixedEntryLister []*SessionEntry

func (l fixedEntryLister) ListAgentSessionEntries(_ context.Context, filter SessionEntryFilter) ([]*SessionEntry, error) {
	var out []*SessionEntry
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
	scope := Scope{Kind: "tenant", ID: "one"}
	run := &AgentRun{ID: "run", Scope: scope}
	refs := func(values ...interface{}) map[string]interface{} {
		list := make([]interface{}, 0, len(values)/2)
		for i := 0; i+1 < len(values); i += 2 {
			list = append(list, map[string]interface{}{"id": values[i], "version": values[i+1]})
		}
		return map[string]interface{}{"artifactRefs": list}
	}
	actions := &fakeActionCalls{calls: map[string]*ActionCall{
		"a1": {ID: "a1", RunID: "run", Status: ActionCallStatusSucceeded, Output: refs("one", 1, "two", 3)},
		"a2": {ID: "a2", RunID: "run", Status: ActionCallStatusSucceeded, Output: refs("one", 1, strings.Repeat("x", 257), 1, "zero", 0)},
		"a3": {ID: "a3", RunID: "run", Status: ActionCallStatusFailed, Output: refs("failed", 1)},
	}}
	entries := fixedEntryLister{
		{ID: "e1", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "a1"},
		{ID: "e2", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "a2"},
		{ID: "e3", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "a3"},
		{ID: "e4", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "missing"},
		{ID: "e5", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted},
	}
	got, err := runActionArtifactReferences(context.Background(), entries, actions, run)
	if err != nil {
		t.Fatal(err)
	}
	want := []ConversationReference{
		{Kind: ConversationReferenceArtifact, ID: "one", Version: 1},
		{Kind: ConversationReferenceArtifact, ID: "two", Version: 3},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("references = %#v", got)
	}
}

// Only the files the final answer hands over go out with it; the Run's other
// files stay in the Library.
func TestHandedOverArtifactsKeepsOnlyTheAnswersFiles(t *testing.T) {
	saved := []ConversationReference{
		{Kind: ConversationReferenceArtifact, ID: "panel-1", Version: 1},
		{Kind: ConversationReferenceArtifact, ID: "panel-2", Version: 1},
		{Kind: ConversationReferenceArtifact, ID: "comic-pdf", Version: 2},
	}
	got, unknown := handedOverArtifacts(saved, map[string]interface{}{finalOutputFilesKey: []interface{}{"comic-pdf", "docs/comic.pdf"}})
	if len(got) != 1 || got[0].ID != "comic-pdf" || got[0].Version != 2 {
		t.Fatalf("handed over = %+v; want only the comic PDF", got)
	}
	// A path instead of an id names no saved file: it goes back to the model
	// rather than silently attaching nothing.
	if len(unknown) != 1 || unknown[0] != "docs/comic.pdf" {
		t.Fatalf("unknown = %v; want the path", unknown)
	}
	if text := unknownHandedOverFilesText(unknown, saved); !strings.Contains(text, "docs/comic.pdf") || !strings.Contains(text, "comic-pdf") {
		t.Fatalf("rejection = %q; want the path and the saved ids", text)
	}
	if got, unknown := handedOverArtifacts(saved, map[string]interface{}{}); len(got) != 0 || len(unknown) != 0 {
		t.Fatalf("an answer that hands over nothing attached %+v (unknown %v)", got, unknown)
	}
	if ids := handedOverFileIDs([]string{" a ", "a", "", "b"}); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("file ids = %v", ids)
	}
}
