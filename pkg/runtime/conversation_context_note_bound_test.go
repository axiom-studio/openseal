package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestRunContextBoundedTo32KiB(t *testing.T) {
	session := &models.AgentSession{ID: "session", Scope: models.Scope{Kind: "tenant", ID: "bound"}}
	var active, objectives []string
	for index := 0; index < 150; index++ {
		active = append(active, fmt.Sprintf("- run run-%03d (running, chat) %q", index, strings.Repeat("a", 150)))
	}
	for index := 0; index < 50; index++ {
		objectives = append(objectives, fmt.Sprintf("- objective obj-%03d \"Goal\" (active, priority 1, revision 1): %s", index, strings.Repeat("o", 150)))
	}
	sections := []conversationContextSection{
		{Name: contextSectionRespondTo, Always: true, Text: "Reply to message m1 from Kev (user kev)."},
		{Name: contextSectionObjectives, Text: "Objectives:\n" + strings.Join(objectives, "\n")},
		{Name: contextSectionActiveRuns, Text: "Active work:\n" + strings.Join(active, "\n")},
		{Name: contextSectionPlaces, Text: "Places already found in this chat (reuse their ids):\n- Home (place p1, 1,2)"},
	}
	note := conversationContextNote(session, "run-1", sections, nil)
	text := note.Note.Text
	if len(text) > maximumRunContextBytes {
		t.Fatalf("note is %d bytes", len(text))
	}
	if !strings.Contains(text, "Reply to message m1 from Kev (user kev).") || !strings.Contains(text, "- Home (place p1, 1,2)") {
		t.Fatalf("small sections lost: %q", text[:200])
	}
	if !strings.Contains(text, "Active work:\n- run run-000") || !strings.Contains(text, " more)") || note.Validate() != nil {
		t.Fatalf("active work was not truncated with a count")
	}
	// Active work is cut first: the objectives that fit stay whole.
	if got := strings.Count(text, "- objective "); got != 50 || strings.Count(text, "- run run-") >= 150 {
		t.Fatalf("objectives %d, active %d", got, strings.Count(text, "- run run-"))
	}
	// The digest is of the whole section, so an unchanged truncated section is not sent again.
	again := conversationContextNote(session, "run-2", sections, latestContextDigests([]*models.SessionEntry{note}))
	if names := contextNoteSectionNames(t, again); len(names) != 1 || !names[contextSectionRespondTo] {
		t.Fatalf("re-sent sections = %v", names)
	}
	// Past what active work can give, objectives are cut too.
	for index := 50; index < 300; index++ {
		objectives = append(objectives, fmt.Sprintf("- objective obj-%03d \"Goal\" (active, priority 1, revision 1): %s", index, strings.Repeat("o", 150)))
	}
	sections[1].Text = "Objectives:\n" + strings.Join(objectives, "\n")
	large := conversationContextNote(session, "run-1", sections, nil).Note.Text
	if len(large) > maximumRunContextBytes || !strings.Contains(large, "Active work:\n… (150 more)") || !strings.Contains(large, "- objective obj-000") ||
		strings.Count(large, "- objective ") >= 300 {
		t.Fatalf("large note is %d bytes, %d objectives", len(large), strings.Count(large, "- objective "))
	}
}
