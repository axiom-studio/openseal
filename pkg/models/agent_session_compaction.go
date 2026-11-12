package models

// Compaction cut (SPEC §4.6)

// compactionOpenToolBound is the highest allowed cut index over render-order
// path entries: the index of the earliest assistant entry (of any Run) that
// has an open tool entry on the path, or len(entries) when none is open. An
// open tool entry whose assistant is not on the path bounds the cut at itself.
func compactionOpenToolBound(entries []*SessionEntry) int {
	bound := len(entries)
	assistants := make(map[string]int)
	for i, entry := range entries {
		switch {
		case entry == nil:
		case entry.Type == SessionEntryAssistant && entry.Assistant != nil:
			if _, ok := assistants[entry.StepID]; !ok {
				assistants[entry.StepID] = i
			}
		case entry.Type == SessionEntryTool && entry.State.Open():
			at := i
			if j, ok := assistants[entry.StepID]; ok && entry.StepID != "" {
				at = j
			}
			if at < bound {
				bound = at
			}
		}
	}
	return bound
}

// IsValidCompactionCut reports whether e may be the first kept entry of a
// compaction (SPEC 08 G2): a user, assistant or note entry. Never a tool entry,
// whose result must follow its call, nor a model_change.
func IsValidCompactionCut(e *SessionEntry) bool {
	if e == nil {
		return false
	}
	switch e.Type {
	case SessionEntryUser, SessionEntryAssistant, SessionEntryNote:
		return true
	}
	return false
}

// IsValidCompactionCutAt reports whether entries[cut:] may be kept verbatim after
// a summary of entries[:cut] (FirstKeptEntryID = entries[cut].ID; cut ==
// len(entries) keeps nothing). entries[cut] passes IsValidCompactionCut, and the
// cut is at or before the earliest assistant entry with an open tool entry
// (compactionOpenToolBound), so no in-flight call is summarized away.
func IsValidCompactionCutAt(entries []*SessionEntry, cut int) bool {
	if cut < 0 || cut > len(entries) {
		return false
	}
	if cut > compactionOpenToolBound(entries) {
		return false
	}
	if cut == len(entries) {
		return true
	}
	return IsValidCompactionCut(entries[cut])
}

// ValidateCompactionAgainstPath is the in-transaction append check of a
// compaction entry against the session's current path.
func ValidateCompactionAgainstPath(path *AgentSessionPath, entry *SessionEntry) error {
	if entry == nil || entry.Compaction == nil {
		return InvalidSessionEntryf("not a compaction entry")
	}
	if path == nil {
		return InvalidSessionEntryf("compaction needs the current path")
	}
	cut := len(path.Entries)
	if kept := entry.Compaction.FirstKeptEntryID; kept != "" {
		cut = -1
		for i, candidate := range path.Entries {
			if candidate.ID == kept {
				cut = i
				break
			}
		}
		if cut < 0 {
			return InvalidSessionEntryf("compaction keeps %s, which is not on the current path", kept)
		}
	}
	if !IsValidCompactionCutAt(path.Entries, cut) {
		return InvalidSessionEntryf("compaction cut at %d is not allowed", cut)
	}
	return nil
}
