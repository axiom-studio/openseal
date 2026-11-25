package conversation

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
	actions := &fakeActionCalls{calls: map[string]*models.ActionCall{
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
	got, err := RunActionArtifactReferences(context.Background(), entries, actions, run)
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
