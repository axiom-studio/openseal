package runtime

import (
	"errors"
	"fmt"
)

// compactionPlan is where a compaction cuts the rendered path: Summarized is
// replaced by the summary, Kept stays verbatim after it.
type compactionPlan struct {
	Summarized       []*SessionEntry // Entries[:cut] (render order)
	Kept             []*SessionEntry // Entries[cut:]
	FirstKeptEntryID string
}

var errCompactionImpossible = errors.New("nothing before the latest step can be compacted")

const (
	// compactionOverflowMinimumKeep is the floor of the overflow keep
	// (max(keep/2, 2048)).
	compactionOverflowMinimumKeep int64 = 2048
	// compactionMinimumSummarizedTokens is SPEC §4.6: a cut that leaves fewer
	// tokens than this to summarize is skipped; the length-rejection path
	// handles the rest.
	compactionMinimumSummarizedTokens int64 = 4000
	// compactionChunkMarginTokens is the slack kept below the input limit for
	// each overflow summary call.
	compactionChunkMarginTokens int64 = 2048
	// compactionMaxFolds caps the summary calls of one overflow compaction.
	compactionMaxFolds = 8
	// compactionActionCallsKept caps CompactionEntry.ActionCallIDs.
	compactionActionCallsKept = 500
)

// compactionKeepFor is the verbatim tail size the cut aims for:
// compactionKeepTokens(window) for a threshold compaction, half of it (at
// least 2048) for an overflow compaction.
func compactionKeepFor(trigger string, window ModelWindow) int64 {
	keep := compactionKeepTokens(window)
	if trigger == CompactionTriggerOverflow {
		keep /= 2
		if keep < compactionOverflowMinimumKeep {
			keep = compactionOverflowMinimumKeep
		}
	}
	return keep
}

// planCompaction picks the cut over path entries in render order (compaction
// entries are never among them).
//
//  1. Walk back from the newest entry summing Tokens. At the first index where
//     the sum reaches keep, move down to the nearest valid cut.
//  2. No valid cut there: cut at the largest valid index ≥ 1 (keep only the
//     latest step, less than keep).
//  3. No valid index ≥ 1 at all: errCompactionImpossible.
//
// A valid cut is validCompactionCutAt: the first kept entry is a user,
// assistant or note entry, and the cut is at or before the earliest assistant
// entry that has an open tool entry (SPEC §4.6), so no in-flight call is
// summarized away. A cut that leaves fewer than
// compactionMinimumSummarizedTokens to summarize is errCompactionImpossible
// too.
func planCompaction(entries []*SessionEntry, keep int64) (compactionPlan, error) {
	valid := func(cut int) bool {
		return cut >= 1 && cut < len(entries) && validCompactionCutAt(entries, cut)
	}
	cut := 0
	var acc int64
walk:
	for i := len(entries) - 1; i >= 1; i-- {
		acc += compactionEntryTokens(entries[i])
		if acc < keep {
			continue
		}
		for j := i; j >= 1; j-- {
			if valid(j) {
				cut = j
				break walk
			}
		}
		break
	}
	if cut == 0 {
		for j := len(entries) - 1; j >= 1; j-- {
			if valid(j) {
				cut = j
				break
			}
		}
	}
	if cut == 0 {
		return compactionPlan{}, errCompactionImpossible
	}
	if summarized := compactionSpanTokens(entries[:cut]); summarized < compactionMinimumSummarizedTokens {
		return compactionPlan{}, fmt.Errorf("%w: only %d tokens before the cut", errCompactionImpossible, summarized)
	}
	return compactionPlan{
		Summarized:       entries[:cut],
		Kept:             entries[cut:],
		FirstKeptEntryID: entries[cut].ID,
	}, nil
}

// compactionEntryTokens is entry.Tokens, or the kernel estimate when the entry
// was never stamped.
func compactionEntryTokens(entry *SessionEntry) int64 {
	if entry == nil {
		return 0
	}
	if entry.Tokens > 0 {
		return entry.Tokens
	}
	return estimateEntryTokens(entry)
}

func compactionSpanTokens(entries []*SessionEntry) int64 {
	var total int64
	for _, entry := range entries {
		total += compactionEntryTokens(entry)
	}
	return total
}

// compactionChunkBudget is the estimated input one overflow summary call may
// carry: the input limit minus the default prefix, the summary output and a
// margin. The limit is the window's input limit (or context size), else the
// limit the host reported with the context failure. With no limit at all it
// is half of the host's estimate of the rejected input. 0 means nothing fits.
func compactionChunkBudget(window ModelWindow, pressure *HostedContextPressure) int64 {
	limit := window.InputLimitTokens
	if limit <= 0 {
		limit = window.ContextTokens
	}
	if limit <= 0 && pressure != nil {
		limit = pressure.LimitTokens
	}
	if limit > 0 {
		budget := limit - defaultPrefixTokens - compactionSummaryMaxOutputTokens - compactionChunkMarginTokens
		if budget < 0 {
			return 0
		}
		return budget
	}
	if pressure != nil && pressure.EstimatedTokens > 0 {
		return pressure.EstimatedTokens / 2
	}
	return 0
}

// compactionFoldReserveTokens is what a folded summary (the synthetic
// compaction view carrying summary k−1 into chunk k) is assumed to cost: the
// rendered preamble plus a full-length summary output.
var compactionFoldReserveTokens = (int64(len(compactionSummaryPreamble))+estimatedBytesPerToken-1)/estimatedBytesPerToken +
	estimatedEntryFraming + compactionSummaryMaxOutputTokens

// compactionChunks splits the summarized span of an overflow compaction into
// consecutive chunks, each estimated at most budget, for folding: chunk 0 is
// sent after the previous compaction (previousTokens, 0 when none), chunk k
// after a synthetic view of summary k−1. Every chunk boundary is a valid cut
// (the chunk starts at a user, assistant or note entry), so no chunk ends in
// an assistant tool call without its results. One chunk means no folding is
// needed. More than compactionMaxFolds chunks, or one indivisible step over
// the budget, is errCompactionImpossible.
func compactionChunks(summarized []*SessionEntry, previousTokens, budget int64) ([][]*SessionEntry, error) {
	if len(summarized) == 0 {
		return nil, errCompactionImpossible
	}
	if budget <= 0 {
		return nil, fmt.Errorf("%w: no input budget for a summary call", errCompactionImpossible)
	}
	foldReserve := compactionFoldReserveTokens
	if previousTokens > foldReserve {
		foldReserve = previousTokens
	}
	var chunks [][]*SessionEntry
	start, used, reserve := 0, previousTokens, previousTokens
	for _, segment := range compactionSegments(summarized) {
		tokens := compactionSpanTokens(summarized[segment[0]:segment[1]])
		if used+tokens > budget && segment[0] > start {
			chunks = append(chunks, summarized[start:segment[0]])
			start, reserve = segment[0], foldReserve
			used = reserve
		}
		if reserve+tokens > budget {
			return nil, fmt.Errorf("%w: one step is %d tokens, over the %d-token summary budget", errCompactionImpossible, tokens, budget-reserve)
		}
		used += tokens
	}
	chunks = append(chunks, summarized[start:])
	if len(chunks) > compactionMaxFolds {
		return nil, fmt.Errorf("%w: %d summary calls needed, at most %d allowed", errCompactionImpossible, len(chunks), compactionMaxFolds)
	}
	return chunks, nil
}

// compactionSegments splits entries into [start, end) runs that each begin at
// a valid cut (index 0 always begins one): a user or note entry, or an
// assistant entry with the tool entries that follow it.
func compactionSegments(entries []*SessionEntry) [][2]int {
	var segments [][2]int
	start := 0
	for i := 1; i < len(entries); i++ {
		if validCompactionCut(entries[i]) {
			segments = append(segments, [2]int{start, i})
			start = i
		}
	}
	if len(entries) > 0 {
		segments = append(segments, [2]int{start, len(entries)})
	}
	return segments
}

// splitCompactionChunk halves a chunk the host rejected as too long, at the
// valid cut closest to half its tokens. ok is false when the chunk is one
// indivisible step.
func splitCompactionChunk(chunk []*SessionEntry) (first, second []*SessionEntry, ok bool) {
	total := compactionSpanTokens(chunk)
	best, bestDistance := 0, int64(-1)
	var acc int64
	for i := 1; i < len(chunk); i++ {
		acc += compactionEntryTokens(chunk[i-1])
		if !validCompactionCut(chunk[i]) {
			continue
		}
		distance := 2*acc - total
		if distance < 0 {
			distance = -distance
		}
		if bestDistance < 0 || distance < bestDistance {
			best, bestDistance = i, distance
		}
	}
	if best == 0 {
		return nil, nil, false
	}
	return chunk[:best], chunk[best:], true
}

// compactionActionCallIDs is CompactionEntry.ActionCallIDs, derived by the
// kernel and never parsed from the summary: the previous compaction's ids,
// then the ActionCallID of every tool entry in the summarized span in step
// order, deduped, keeping the last compactionActionCallsKept.
func compactionActionCallIDs(previous *CompactionEntry, summarized []*SessionEntry) []string {
	seen := make(map[string]bool)
	var ids []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if previous != nil {
		for _, id := range previous.ActionCallIDs {
			add(id)
		}
	}
	for _, entry := range summarized {
		if entry != nil && entry.Type == SessionEntryTool {
			add(entry.ActionCallID)
		}
	}
	if len(ids) > compactionActionCallsKept {
		ids = ids[len(ids)-compactionActionCallsKept:]
	}
	return ids
}
