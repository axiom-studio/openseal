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
	if len(got) != len(want) || got[0].Ref != want[0] || got[1].Ref != want[1] {
		t.Fatalf("references = %#v", got)
	}
}

// Only the files the final answer hands over go out with it, named by id or
// by the path they were presented from; the Run's other files stay in the
// Library.
func TestHandedOverArtifactsKeepsOnlyTheAnswersFiles(t *testing.T) {
	saved := []savedArtifact{
		{Ref: ConversationReference{Kind: ConversationReferenceArtifact, ID: "panel-1", Version: 1}, Path: "images/panel-1.png"},
		{Ref: ConversationReference{Kind: ConversationReferenceArtifact, ID: "panel-2", Version: 1}, Path: "images/panel-2.png"},
		{Ref: ConversationReference{Kind: ConversationReferenceArtifact, ID: "comic-pdf", Version: 2}, Path: "docs/comic.pdf"},
		{Ref: ConversationReference{Kind: ConversationReferenceArtifact, ID: "chart", Version: 1}},
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
