package modelstep

import (
	"encoding/json"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

// ModelWindow is the model's limits as the host resolved them from the gateway
// catalog. Zero = unknown. The kernel never calls the gateway: every model step
// result carries the limits and the loop stores them on the assistant entry
// (AssistantEntry.Limits).
type ModelWindow struct {
	ContextTokens    int64 `json:"contextTokens,omitempty"`
	InputLimitTokens int64 `json:"inputLimitTokens,omitempty"`
}

const (
	compactionBufferPercent    int64 = 15
	compactionBufferMinTokens  int64 = 16000
	compactionKeepRecentTokens int64 = 15000
	compactionMinimumThreshold int64 = 8000 // below: no proactive compaction, overflow path only
)

// Known reports whether the host reported any limit.
func (w ModelWindow) Known() bool { return w.ContextTokens > 0 || w.InputLimitTokens > 0 }

func (w ModelWindow) size() int64 {
	if w.ContextTokens > 0 {
		return w.ContextTokens
	}
	if w.InputLimitTokens > 0 {
		return w.InputLimitTokens
	}
	return 0
}

// compactionThreshold is size − max(size×15%, 16000), capped at
// InputLimitTokens when that is set. 0 when the window is unknown or the
// result is below compactionMinimumThreshold.
func compactionThreshold(w ModelWindow) int64 {
	size := w.size()
	if size <= 0 {
		return 0
	}
	buffer := size * compactionBufferPercent / 100
	if buffer < compactionBufferMinTokens {
		buffer = compactionBufferMinTokens
	}
	threshold := size - buffer
	if w.InputLimitTokens > 0 && threshold > w.InputLimitTokens {
		threshold = w.InputLimitTokens
	}
	if threshold < compactionMinimumThreshold {
		return 0
	}
	return threshold
}

// compactionKeepTokens is min(15000, threshold/4); 0 when the threshold is 0.
func compactionKeepTokens(w ModelWindow) int64 {
	threshold := compactionThreshold(w)
	if threshold <= 0 {
		return 0
	}
	keep := threshold / 4
	if keep > compactionKeepRecentTokens {
		keep = compactionKeepRecentTokens
	}
	return keep
}

// pathModelWindow is the window of the newest assistant entry on the path
// whose model is model. Unknown (zero) when there is none: a new session or a
// model change relies on the host preflight and the overflow path.
func pathModelWindow(path *models.AgentSessionPath, model string) ModelWindow {
	if path == nil {
		return ModelWindow{}
	}
	for i := len(path.Entries) - 1; i >= 0; i-- {
		entry := path.Entries[i]
		if entry == nil || entry.Type != models.SessionEntryAssistant || entry.Assistant == nil {
			continue
		}
		if entry.Assistant.Model != model || entry.Assistant.Limits == nil {
			continue
		}
		window := ModelWindow{
			ContextTokens:    entry.Assistant.Limits.ContextTokens,
			InputLimitTokens: entry.Assistant.Limits.InputLimitTokens,
		}
		if window.Known() {
			return window
		}
	}
	return ModelWindow{}
}

// ---------------------------------------------------------------------------
// Per-entry estimate

const (
	estimatedBytesPerToken       = 4
	estimatedEntryFraming        = 8
	estimatedImageTokens   int64 = 1600
)

// EstimateEntryTokens is what the loop and the tool executor pass as
// SessionEntry.Tokens on append and as ToolEntryTransitionRequest.Tokens on a
// terminal transition. Unlike EstimateSessionEntryTokens it counts what the
// provider reads: text, tool calls, reasoning text and result blocks, never
// AssistantNative's raw provider JSON (which repeats the same content) or
// kernel-only fields.
func EstimateEntryTokens(entry *models.SessionEntry) int64 {
	if entry == nil {
		return 0
	}
	var bytes, images int64
	switch entry.Type {
	case models.SessionEntryUser:
		if entry.User == nil {
			return 0
		}
		bytes += int64(len(sessionAuthorLine(entry.User.Author)))
		b, n := contentPartsBytes(entry.User.Content)
		bytes, images = bytes+b, images+n
	case models.SessionEntryNote:
		if entry.Note == nil {
			return 0
		}
		bytes += int64(len(entry.Note.Text))
	case models.SessionEntryAssistant:
		if entry.Assistant == nil {
			return 0
		}
		bytes += int64(len(entry.Assistant.Text))
		for _, call := range entry.Assistant.ToolCalls {
			bytes += int64(len(call.Name) + len(call.Arguments))
		}
		bytes += assistantReasoningBytes(entry.Assistant.Native)
	case models.SessionEntryTool:
		if entry.Tool == nil || !entry.State.Terminal() || entry.Tool.Result == nil {
			return 0
		}
		b, n := contentPartsBytes(entry.Tool.Result.Content)
		bytes, images = bytes+b, images+n
	case models.SessionEntryCompaction:
		if entry.Compaction == nil {
			return 0
		}
		bytes += int64(len(CompactionSummaryText(entry.Compaction)))
	case models.SessionEntryModelChange:
	default:
		return 0
	}
	return (bytes+estimatedBytesPerToken-1)/estimatedBytesPerToken + estimatedEntryFraming + images*estimatedImageTokens
}

func sessionAuthorLine(author models.SessionAuthor) string {
	name := strings.TrimSpace(author.DisplayName)
	if name == "" {
		name = strings.TrimSpace(author.ID)
	}
	if name == "" {
		return ""
	}
	return name + ":\n"
}

// contentPartsBytes is the text byte count of parts plus the number of images.
func contentPartsBytes(parts []models.ContentPart) (int64, int64) {
	var bytes, images int64
	for _, part := range parts {
		switch part.Type {
		case models.ContentPartText:
			bytes += int64(len(part.Text))
		case models.ContentPartJSON:
			bytes += int64(len(part.Value))
		case models.ContentPartFile:
			bytes += int64(len(contentPartFileLine(part)))
		case models.ContentPartImage:
			images++
		}
	}
	return bytes, images
}

// contentPartFileLine is the one line a file block costs in the estimate.
func contentPartFileLine(part models.ContentPart) string {
	line := "[file " + part.Path
	if part.MimeType != "" {
		line += " " + part.MimeType
	}
	line += "]"
	if part.Note != "" {
		line += " " + part.Note
	}
	return line
}

// assistantReasoningBytes counts only the reasoning text inside the native
// content (Anthropic thinking, Responses reasoning summaries, chat
// reasoning), not the blocks that repeat the entry's text and tool calls.
func assistantReasoningBytes(native *models.AssistantNative) int64 {
	if native == nil {
		return 0
	}
	var bytes int64
	for _, raw := range native.Anthropic {
		var block struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		}
		if json.Unmarshal(raw, &block) == nil && block.Type == "thinking" {
			bytes += int64(len(block.Thinking))
		}
	}
	for _, raw := range native.Responses {
		var item struct {
			Type    string `json:"type"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "reasoning" {
			for _, summary := range item.Summary {
				bytes += int64(len(summary.Text))
			}
		}
	}
	bytes += int64(len(native.ChatReasoning))
	return bytes
}

// compactionSummaryPreamble opens the user-role text that replaces summarized
// history.
const compactionSummaryPreamble = "The conversation and work before this point were compacted. This summary replaces them; the messages after it are verbatim."

// CompactionSummaryText is the user-role text that replaces summarized
// history. Atlas and the daemon render this same text.
func CompactionSummaryText(c *models.CompactionEntry) string {
	if c == nil {
		return ""
	}
	text := compactionSummaryPreamble + "\n\n" + strings.TrimSpace(c.Summary)
	if len(c.ActionCallIDs) > 0 {
		text += "\n\nAction calls recorded before this point: " + strings.Join(c.ActionCallIDs, ", ")
	}
	return text
}

// ---------------------------------------------------------------------------
// Path estimate

// PathEstimate is the estimated provider input of the next step.
type PathEstimate struct {
	Tokens        int64
	AnchorEntryID string // "" = no anchor
}

// defaultPrefixTokens stands for system prompt + tools when no anchored step exists.
const defaultPrefixTokens int64 = 16384

// estimatePathTokens estimates the next step's provider input for path.
//
// The anchor is the last assistant entry after the path's latest compaction
// whose model is model, that carries the real PromptTokens of its request,
// and that no model_change follows. With an anchor the estimate is its
// PromptTokens (which already include system and tools) plus its own and every
// later entry's Tokens; without one it is defaultPrefixTokens + path.Tokens.
func estimatePathTokens(path *models.AgentSessionPath, model string) PathEstimate {
	if path == nil {
		return PathEstimate{Tokens: defaultPrefixTokens}
	}
	order := pathSessionOrder(path)
	var after int64
	for i := len(path.Entries) - 1; i >= 0; i-- {
		entry := path.Entries[i]
		if entry == nil {
			continue
		}
		if entry.Type == models.SessionEntryModelChange {
			break
		}
		if !pathEntryAfterCompaction(path, order, entry) {
			break
		}
		if entry.Type == models.SessionEntryAssistant && entry.Assistant != nil &&
			entry.Assistant.Model == model && entry.Assistant.Limits != nil && entry.Assistant.Limits.PromptTokens > 0 {
			return PathEstimate{
				Tokens:        entry.Assistant.Limits.PromptTokens + entry.Tokens + after,
				AnchorEntryID: entry.ID,
			}
		}
		after += entry.Tokens
	}
	return PathEstimate{Tokens: defaultPrefixTokens + path.Tokens}
}

// compactionDue is LoopCompactor.Due's pure part: the window has a threshold,
// the path estimate is over it, and at least one model step has run since the
// latest compaction (otherwise the kept tail alone is over the threshold and
// the overflow path takes over instead of compacting every step).
func compactionDue(path *models.AgentSessionPath, model string, window ModelWindow) bool {
	threshold := compactionThreshold(window)
	if threshold <= 0 || path == nil {
		return false
	}
	if estimatePathTokens(path, model).Tokens <= threshold {
		return false
	}
	if path.Compaction == nil {
		return true
	}
	order := pathSessionOrder(path)
	for i := len(path.Entries) - 1; i >= 0; i-- {
		entry := path.Entries[i]
		if entry != nil && entry.Type == models.SessionEntryAssistant && pathEntryAfterCompaction(path, order, entry) {
			return true
		}
	}
	return false
}

// pathSessionOrder maps each session of the path's chain to its render index.
func pathSessionOrder(path *models.AgentSessionPath) map[string]int {
	order := make(map[string]int, len(path.Chain))
	for i, session := range path.Chain {
		if session != nil {
			order[session.ID] = i
		}
	}
	return order
}

// pathEntryAfterCompaction reports whether entry renders after the path's
// latest compaction: a higher sequence in the same session, or a later session
// of the chain. Kept entries before the compaction row are not after it.
func pathEntryAfterCompaction(path *models.AgentSessionPath, order map[string]int, entry *models.SessionEntry) bool {
	compaction := path.Compaction
	if compaction == nil {
		return true
	}
	if entry.SessionID == compaction.SessionID {
		return entry.Sequence > compaction.Sequence
	}
	entryIndex, entryOK := order[entry.SessionID]
	compactionIndex, compactionOK := order[compaction.SessionID]
	if !entryOK || !compactionOK {
		return false
	}
	return entryIndex > compactionIndex
}
