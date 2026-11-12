package runtime

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

var testSessionScope = models.Scope{Kind: "org", ID: "o1"}

func TestValidToolEntryTransitionTable(t *testing.T) {
	allowed := map[models.ToolEntryState]map[models.ToolEntryState]bool{
		models.ToolEntryPending: {models.ToolEntryRunning: true, models.ToolEntryAwaitingApproval: true, models.ToolEntryAwaitingChild: true,
			models.ToolEntryAwaitingEvent: true, models.ToolEntryCompleted: true, models.ToolEntryError: true, models.ToolEntryInterrupted: true},
		models.ToolEntryAwaitingApproval: {models.ToolEntryRunning: true, models.ToolEntryError: true, models.ToolEntryInterrupted: true},
		models.ToolEntryAwaitingChild:    {models.ToolEntryCompleted: true, models.ToolEntryError: true, models.ToolEntryInterrupted: true},
		models.ToolEntryAwaitingEvent:    {models.ToolEntryRunning: true, models.ToolEntryCompleted: true, models.ToolEntryError: true, models.ToolEntryInterrupted: true},
		models.ToolEntryRunning: {models.ToolEntryPending: true, models.ToolEntryAwaitingApproval: true, models.ToolEntryAwaitingChild: true,
			models.ToolEntryAwaitingEvent: true, models.ToolEntryCompleted: true, models.ToolEntryError: true, models.ToolEntryInterrupted: true},
	}
	states := append([]models.ToolEntryState{"", "bogus"}, ToolEntryStates...)
	for _, from := range states {
		for _, to := range states {
			want := allowed[from][to]
			if got := models.ValidToolEntryTransition(from, to); got != want {
				t.Errorf("ValidToolEntryTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, state := range ToolEntryStates {
		if state.Terminal() == state.Open() {
			t.Errorf("%s: Terminal and Open must differ", state)
		}
	}
	if models.ToolEntryState("bogus").Open() {
		t.Error("unknown state must not be open")
	}
}

func testPathEntry(entry *models.SessionEntry, sessionID string, seq int64, tokens int64) *models.SessionEntry {
	entry.SessionID, entry.Sequence, entry.Tokens, entry.Scope = sessionID, seq, tokens, testSessionScope
	return entry
}

func TestSessionEntryFilterNormalizeAndMatch(t *testing.T) {
	if _, err := models.NormalizeSessionEntryFilter(models.SessionEntryFilter{Scope: testSessionScope}); !errors.Is(err, models.ErrInvalidSessionEntry) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := models.NormalizeSessionEntryFilter(models.SessionEntryFilter{Scope: testSessionScope, RunID: "r", AfterSequence: 2}); !errors.Is(err, models.ErrInvalidSessionEntry) {
		t.Fatalf("afterSequence without session: %v", err)
	}
	filter, err := models.NormalizeSessionEntryFilter(models.SessionEntryFilter{Scope: testSessionScope, RunID: "r1", States: OpenToolEntryStates, Limit: 5000})
	if err != nil || filter.Limit != models.MaxSessionEntryListLimit {
		t.Fatalf("filter %+v err %v", filter, err)
	}
	open := testPathEntry(storetest.ToolSessionEntry("t", "s", "c", models.ToolEntryRunning), "sess", 2, 1)
	open.RunID = "r1"
	done := testPathEntry(storetest.ToolSessionEntry("t2", "s", "c2", models.ToolEntryCompleted), "sess", 3, 1)
	done.RunID = "r1"
	if !models.SessionEntryMatchesFilter(open, filter) || models.SessionEntryMatchesFilter(done, filter) {
		t.Fatal("open-state filter")
	}
	notes := models.SessionEntryFilter{Scope: testSessionScope, SessionID: "sess", NoteKinds: []models.NoteKind{models.NoteRepeat}}
	note := testPathEntry(&models.SessionEntry{ID: "n", Type: models.SessionEntryNote, Note: &models.NoteEntry{Kind: models.NoteRepeat, Text: "x"}}, "sess", 4, 1)
	if !models.SessionEntryMatchesFilter(note, notes) || models.SessionEntryMatchesFilter(open, notes) {
		t.Fatal("note kind filter")
	}
}
