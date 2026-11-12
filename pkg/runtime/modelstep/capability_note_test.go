package modelstep

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestCapabilityNoteRelistsWhenOnlyAnUpdateSurvives(t *testing.T) {
	listing := CapabilityListing{Mode: CapabilityNoteModeList, Items: []CapabilityNoteItem{
		{Key: "action:a", Section: capabilityNoteCatalog, Text: "- action:a"},
	}}
	fullText, fullData := capabilityNoteText(listing, nil)
	if !fullData.Full || !strings.HasPrefix(fullText, capabilityNoteOpen) {
		t.Fatalf("first note = %q full=%v", fullText, fullData.Full)
	}
	listing.Items = append(listing.Items, CapabilityNoteItem{Key: "action:b", Section: capabilityNoteCatalog, Text: "- action:b"})
	updateText, updateData := capabilityNoteText(listing, &fullData)
	if updateData.Full || !strings.HasPrefix(updateText, "<capabilities_update>") {
		t.Fatalf("second note = %q full=%v", updateText, updateData.Full)
	}
	note := func(id, text string, data capabilityNoteData) *models.SessionEntry {
		encoded, _ := json.Marshal(data)
		return &models.SessionEntry{ID: id, Type: models.SessionEntryNote, Note: &models.NoteEntry{Kind: models.NoteCapabilities, Text: text, Data: encoded}}
	}
	both := &models.AgentSessionPath{Entries: []*models.SessionEntry{note("n1", fullText, fullData), note("n2", updateText, updateData)}}
	if latest := latestCapabilityNote(both); latest == nil || latest.Digest != updateData.Digest {
		t.Fatalf("latest with both notes = %+v", latest)
	}
	// A compaction summarized the full note but kept the update: the
	// listing is gone, so the next step lists everything again.
	onlyUpdate := &models.AgentSessionPath{Entries: []*models.SessionEntry{note("n2", updateText, updateData)}}
	if latest := latestCapabilityNote(onlyUpdate); latest != nil {
		t.Fatalf("an update without its full note counts as the listing: %+v", latest)
	}
	text, data := capabilityNoteText(listing, latestCapabilityNote(onlyUpdate))
	if !data.Full || !strings.Contains(text, "- action:a") || !strings.Contains(text, "- action:b") {
		t.Fatalf("relisting = %q", text)
	}
}

func TestCapabilityNoteModeChangeToNoCatalog(t *testing.T) {
	deferred := CapabilityListing{Mode: CapabilityNoteModeList, Items: []CapabilityNoteItem{
		{Key: "action:a", Section: capabilityNoteCatalog, Text: "- action:a"},
		{Key: "action:b", Section: capabilityNoteCatalog, Text: "- action:b"},
	}}
	_, previous := capabilityNoteText(deferred, nil)
	text, data := capabilityNoteText(CapabilityListing{Mode: CapabilityNoteModeNone}, &previous)
	if !data.Full || strings.Contains(text, capabilityNoteRemovedHead) || !strings.Contains(text, capabilityNoteNoCatalog) {
		t.Fatalf("mode change note = %q", text)
	}
}

func TestCapabilityNoteRestoredTool(t *testing.T) {
	catalog := CapabilityNoteItem{Key: "action:a", Section: capabilityNoteCatalog, Text: "- action:a"}
	revoked := CapabilityListing{Mode: CapabilityNoteModeList, Items: []CapabilityNoteItem{
		catalog, {Key: "unavailable:action_gmail_send", Section: CapabilityNoteUnavailable, Text: "- action_gmail_send"},
	}}
	_, previous := capabilityNoteText(revoked, nil)
	restored := CapabilityListing{Mode: CapabilityNoteModeList, Items: []CapabilityNoteItem{
		catalog, {Key: "action:gmail.send", Section: capabilityNoteCatalog, Text: "- action:gmail.send"},
	}}
	text, _ := capabilityNoteText(restored, &previous)
	if !strings.Contains(text, capabilityNoteRestoredHead+"\n- action_gmail_send") || strings.Contains(text, capabilityNoteRemovedHead) {
		t.Fatalf("restored note = %q", text)
	}
}
