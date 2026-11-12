package modelstep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// The capabilities note (cache-stable capability loading). What an Agent can
// load and hand work to changes between Runs of a lane: a Skill is connected,
// an Agent joins, an operation is added. None of it is in the system prompt
// or a tool definition, where any change would invalidate the provider's
// prompt cache from that point on. Instead the step producer appends a
// capabilities note entry before a model step whenever the path does not
// already show the current listing:
//
//   - no full capabilities note on the path (a new session, or a compaction
//     summarized it, even if a later update note survived): the full listing;
//   - the catalog mode changed: the full listing again;
//   - the latest note's digest differs: only what changed, as "now available
//     or changed", "available again" and "no longer available" lines;
//   - the digest matches: nothing.
//
// Every note's Data holds the digest, catalog mode and per-item digests of
// the whole listing at that point (an update note too), so the delta needs no
// other state.

// Listing sections, in note order.
const (
	capabilityNoteCatalog     = "catalog"
	capabilityNoteAgents      = "agents"
	capabilityNoteOperations  = "operations"
	CapabilityNoteUnavailable = "unavailable"
	CapabilityNoteGuidance    = "guidance"

	// capabilityNoteApprovalKey is the conversation's approval mode, a
	// guidance paragraph: a changed mode shows as the new paragraph.
	CapabilityNoteApprovalKey = "guide:approval_mode"
)

var capabilityNoteSectionOrder = []string{
	capabilityNoteCatalog, capabilityNoteAgents, capabilityNoteOperations, CapabilityNoteUnavailable, CapabilityNoteGuidance,
}

// Catalog modes: every ID listed, a directory per Skill or server, or no
// catalog lines (no catalog, or a Run that offers no capabilities).
const (
	CapabilityNoteModeList      = "list"
	CapabilityNoteModeDirectory = "directory"
	CapabilityNoteModeNone      = "none"
)

const (
	capabilityNoteOpen         = "<capabilities>\n"
	capabilityNoteClose        = "\n</capabilities>"
	capabilityNoteUpdateOpen   = "<capabilities_update>\nChanged since the last capabilities note; everything else in it still holds.\n\n"
	capabilityNoteUpdateClose  = "\n</capabilities_update>"
	capabilityNoteRemovedHead  = "No longer available:"
	capabilityNoteRestoredHead = "Available again (calls work again):"
	capabilityNoteNoCatalog    = "Every capability you can use is declared as a tool; nothing needs loading."
)

// capabilityNoteItem is one line (or guidance paragraph) of the listing. Key
// is unique in a listing and names the item in a "no longer available" line.
type CapabilityNoteItem struct {
	Key     string
	Section string
	Text    string
}

// capabilityListing is the current listing of one step.
type CapabilityListing struct {
	Mode  string
	Items []CapabilityNoteItem
}

// capabilityNoteData is the note entry's Data.
type capabilityNoteData struct {
	Digest string            `json:"digest"`
	Mode   string            `json:"mode"`
	IDs    map[string]string `json:"ids"` // item key -> digest of its text
	// Full marks a note that lists everything (<capabilities>); an update
	// note (<capabilities_update>) only makes sense after a full one.
	Full bool `json:"full,omitempty"`
}

// capabilityListingSource builds the listing of a step. The catalog preparer
// implements it; a preparer without a catalog does not.
type capabilityListingSource interface {
	CapabilityListing(in agentrun.StepInput) CapabilityListing
}

func capabilityItemDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

func (l CapabilityListing) data() capabilityNoteData {
	data := capabilityNoteData{Mode: l.Mode, IDs: make(map[string]string, len(l.Items))}
	h := sha256.New()
	h.Write([]byte(l.Mode))
	for _, item := range l.Items {
		data.IDs[item.Key] = capabilityItemDigest(item.Section + "\x00" + item.Text)
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s", item.Key, item.Section, item.Text)
	}
	data.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return data
}

// capabilityNoteSectionHead is the heading line of a section, "" for
// guidance paragraphs.
func capabilityNoteSectionHead(section, mode string) string {
	switch section {
	case capabilityNoteCatalog:
		if mode == CapabilityNoteModeDirectory {
			return "Capability directory (one Skill or connected server per line; search_capabilities finds their exact IDs):"
		}
		return "Capabilities you can load (copy an ID verbatim into load_capabilities):"
	case capabilityNoteAgents:
		return "Agents you can hand work to (agentId for delegate and fork):"
	case capabilityNoteOperations:
		return "Operations you can start with run_operation (entrypoint):"
	case CapabilityNoteUnavailable:
		return "Tools no longer available (a call returns an error):"
	}
	return ""
}

// writeCapabilitySections renders items section by section.
func writeCapabilitySections(items []CapabilityNoteItem, mode string) []string {
	var blocks []string
	for _, section := range capabilityNoteSectionOrder {
		var lines []string
		for _, item := range items {
			if item.Section == section {
				lines = append(lines, item.Text)
			}
		}
		if len(lines) == 0 {
			continue
		}
		if head := capabilityNoteSectionHead(section, mode); head != "" {
			blocks = append(blocks, head+"\n"+strings.Join(lines, "\n"))
		} else {
			blocks = append(blocks, strings.Join(lines, "\n\n"))
		}
	}
	return blocks
}

// latestCapabilityNote is the Data of the newest capabilities note on the
// path (the path starts at the latest compaction's kept tail), nil when no
// full note is on the path: an update note whose full note was summarized
// away describes changes to a listing the model can no longer see.
func latestCapabilityNote(path *models.AgentSessionPath) *capabilityNoteData {
	if path == nil {
		return nil
	}
	var latest *capabilityNoteData
	for i := len(path.Entries) - 1; i >= 0; i-- {
		entry := path.Entries[i]
		if entry == nil || entry.Type != models.SessionEntryNote || entry.Note == nil || entry.Note.Kind != models.NoteCapabilities {
			continue
		}
		var data capabilityNoteData
		if json.Unmarshal(entry.Note.Data, &data) != nil || data.Digest == "" {
			continue
		}
		if latest == nil {
			latest = &data
		}
		if data.Full {
			return latest
		}
	}
	return nil
}

// capabilityNoteText is the note to append for listing over a path whose
// latest capabilities note is previous: the full listing, the delta, or ""
// when the path already shows the listing.
func capabilityNoteText(listing CapabilityListing, previous *capabilityNoteData) (string, capabilityNoteData) {
	data := listing.data()
	full := func() (string, capabilityNoteData) {
		data.Full = true
		blocks := writeCapabilitySections(listing.Items, listing.Mode)
		if listing.Mode == CapabilityNoteModeNone && len(listing.Items) == 0 {
			blocks = []string{capabilityNoteNoCatalog}
		}
		return capabilityNoteOpen + strings.Join(blocks, "\n\n") + capabilityNoteClose, data
	}
	switch {
	case previous == nil:
		if len(listing.Items) == 0 {
			return "", data
		}
		return full()
	case previous.Digest == data.Digest:
		return "", data
	case previous.Mode != data.Mode:
		// The catalog switched between a full list, a directory and no
		// catalog at all: the old lines say nothing about the new ones,
		// and a former catalog ID may now be a declared tool.
		return full()
	}
	var changed []CapabilityNoteItem
	for _, item := range listing.Items {
		if previous.IDs[item.Key] != data.IDs[item.Key] {
			changed = append(changed, item)
		}
	}
	blocks := writeCapabilitySections(changed, listing.Mode)
	var removed, restored []string
	for _, key := range capabilitySortedKeysOf(previous.IDs) {
		if _, still := data.IDs[key]; still || strings.HasPrefix(key, "guide:") {
			continue
		}
		if name, ok := strings.CutPrefix(key, "unavailable:"); ok {
			// A surfaced tool stays declared, so its unavailable line
			// going away means its capability is back.
			restored = append(restored, "- "+name)
			continue
		}
		removed = append(removed, "- "+key)
	}
	if len(restored) > 0 {
		blocks = append(blocks, capabilityNoteRestoredHead+"\n"+strings.Join(restored, "\n"))
	}
	if len(removed) > 0 {
		blocks = append(blocks, capabilityNoteRemovedHead+"\n"+strings.Join(removed, "\n"))
	}
	if len(blocks) == 0 {
		// Only guidance went away: record the new digest without telling
		// the model anything new.
		blocks = []string{"No new capabilities."}
	}
	return capabilityNoteUpdateOpen + strings.Join(blocks, "\n\n") + capabilityNoteUpdateClose, data
}

func capabilitySortedKeysOf(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CapabilityNoteSourceKey keys a step's capabilities note. The latest
// compaction is part of it: a forced compaction retry of the same step may
// need the note again after the earlier one was summarized.
func CapabilityNoteSourceKey(stepID, compactionID, digest string) string {
	return "capabilities:" + stepID + ":" + compactionID + ":" + strings.TrimPrefix(digest, "sha256:")[:16]
}

type capabilityNoteAppender interface {
	AppendAgentSessionEntries(ctx context.Context, req models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error)
}

// ensureCapabilityNote appends the capabilities note the step's path needs
// (none when it already shows the listing). It runs right before the step's
// model request is rendered, after the step's context and any compaction.
func ensureCapabilityNote(ctx context.Context, store capabilityNoteAppender, in agentrun.StepInput, path *models.AgentSessionPath, listing CapabilityListing, now time.Time) (bool, error) {
	text, data := capabilityNoteText(listing, latestCapabilityNote(path))
	if text == "" {
		return false, nil
	}
	compactionID := ""
	if path != nil && path.Compaction != nil {
		compactionID = path.Compaction.ID
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return false, err
	}
	key := CapabilityNoteSourceKey(in.Step.ID, compactionID, data.Digest)
	entry := &models.SessionEntry{
		ID: models.SessionEntryID(in.Session.ID, key), Scope: in.Run.Scope, SessionID: in.Session.ID, RunID: in.Run.ID,
		Step: in.Step.Sequence, Type: models.SessionEntryNote, SourceKey: key, CreatedAt: now,
		Note: &models.NoteEntry{Kind: models.NoteCapabilities, Text: text, Data: encoded},
	}
	entry.Tokens = EstimateEntryTokens(entry)
	_, err = store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: in.Run.Scope, SessionID: in.Session.ID, RunID: in.Run.ID,
		Lease:   &models.AgentRunLeaseGuard{WorkerID: in.WorkerID, Now: now},
		Entries: []*models.SessionEntry{entry}, Now: now,
	})
	if err != nil {
		return false, fmt.Errorf("append capabilities note: %w", err)
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// The catalog preparer's listing

// controlTargetItems are the Agents delegate and fork accept and the
// operations run_operation starts. Their tools carry no list or enum, so a
// new Agent or operation never changes a tool definition; each handler
// validates the target it is given.
func ControlTargetItems(a agentrun.ControlAuthority) []CapabilityNoteItem {
	var items []CapabilityNoteItem
	seen := map[string]bool{}
	addAgents := func(targets []agentrun.HostedAgentTarget) {
		for _, target := range targets {
			if target.ID == "" || seen[target.ID] {
				continue
			}
			seen[target.ID] = true
			line := "- " + target.ID + ": " + strings.TrimSpace(target.DisplayName)
			if purpose := strings.TrimSpace(target.Purpose); purpose != "" {
				line += " — " + purpose
			}
			items = append(items, CapabilityNoteItem{Key: "agent:" + target.ID, Section: capabilityNoteAgents, Text: line})
		}
	}
	if ControlToolOffered(models.ControlToolDelegate, a) {
		addAgents(a.DelegateTargets)
	}
	if ControlToolOffered(models.ControlToolFork, a) {
		addAgents(a.ForkTargets)
	}
	if ControlToolOffered(models.ControlToolRunOperation, a) {
		for _, op := range a.Operations {
			if op.Entrypoint == "" {
				continue
			}
			items = append(items, CapabilityNoteItem{Key: "operation:" + op.Entrypoint, Section: capabilityNoteOperations, Text: operationDescriptionLine(op)})
		}
	}
	return items
}

// operationDescriptionLine is one operation's line of the capabilities note.
func operationDescriptionLine(op agentrun.HostedRunbookOperation) string {
	schema := []byte(`{"type":"object"}`)
	if len(op.InputSchema) > 0 {
		if encoded, err := json.Marshal(op.InputSchema); err == nil {
			schema = encoded
		}
	}
	name := strings.TrimSpace(op.Name)
	if name == "" {
		name = op.Entrypoint
	}
	text := strings.TrimRight(strings.TrimSpace(op.Description), ".")
	line := "- " + op.Entrypoint + " — " + name
	if text != "" {
		line += ": " + text
	}
	return line + ". Input schema: " + string(schema)
}
