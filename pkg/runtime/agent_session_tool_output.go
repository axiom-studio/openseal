package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/toolfile"
)

// ErrToolOutputNoChat means the Run is not part of a chat, so output cannot be kept as a file.
var ErrToolOutputNoChat = errors.New("this work is not part of a chat, so it has no chat files")

// ToolOutputKeeper keeps payloads as chat files under tool-output/<call>/ and
// returns each one's path ("" when a file was too large to keep).
type ToolOutputKeeper interface {
	KeepToolOutput(ctx context.Context, invocation ToolInvocation, files []toolfile.Payload) ([]string, error)
}

const toolOutputPreviewBytes = 16 << 10

// toolOutputCompactLevels is the depth / entries / text ladder used to shorten
// json output that has nowhere to be kept.
var toolOutputCompactLevels = []struct{ depth, entries, text int }{
	{8, 64, 2048}, {7, 32, 1024}, {6, 16, 512}, {5, 8, 256}, {4, 4, 128},
}

// BoundToolResult returns result unchanged when it fits MaxToolResultContentBytes.
// Otherwise it keeps the full text/json content as one chat file and replaces it
// with a preview and a file part. With no chat (keeper nil or ErrToolOutputNoChat),
// or when the file was too large to keep, it compacts the content to fit and marks
// it truncated. File and image parts are always kept as they are.
func BoundToolResult(ctx context.Context, keeper ToolOutputKeeper, invocation ToolInvocation, result ToolResult) (ToolResult, error) {
	var content, others []ContentPart
	total := 0
	for _, part := range result.Content {
		switch part.Type {
		case ContentPartText:
			content = append(content, part)
			total += len(part.Text)
		case ContentPartJSON:
			content = append(content, part)
			total += len(part.Value)
		default:
			others = append(others, part)
		}
	}
	if total <= MaxToolResultContentBytes {
		return result, nil
	}

	reason := "this work has no chat to keep the full output in"
	if keeper != nil {
		if strings.TrimSpace(invocation.ActionCallID) == "" {
			return ToolResult{}, errors.New("bound tool result: the invocation has no action call id to keep its output under")
		}
		payload := toolOutputPayload(content)
		paths, err := keeper.KeepToolOutput(ctx, invocation, []toolfile.Payload{payload})
		switch {
		case errors.Is(err, ErrToolOutputNoChat):
		case err != nil:
			return ToolResult{}, fmt.Errorf("keep tool output: %w", err)
		case len(paths) > 0 && paths[0] != "":
			preview := cutUTF8(toolOutputPreviewText(content), toolOutputPreviewBytes)
			kept := []ContentPart{
				{Type: ContentPartText, Text: preview + fmt.Sprintf("\n… (%s in total; the full output is in %s)", formatToolOutputSize(len(payload.Data)), paths[0])},
				{Type: ContentPartFile, Path: paths[0], MimeType: payload.MimeType, SizeBytes: int64(len(payload.Data))},
			}
			return withToolResultContent(result, append(kept, others...)), nil
		default:
			reason = "the full output was too large to keep as a file"
		}
	}
	return withToolResultContent(result, append(compactToolResultContent(content, total, reason), others...)), nil
}

func withToolResultContent(result ToolResult, content []ContentPart) ToolResult {
	result.Content = content
	return result
}

// toolOutputPayload is the file that holds the full text/json content: plain
// text for exactly one text part, otherwise the parts as json.
func toolOutputPayload(content []ContentPart) toolfile.Payload {
	if len(content) == 1 && content[0].Type == ContentPartText {
		return toolfile.Payload{Name: "output.txt", MimeType: "text/plain", Data: []byte(content[0].Text)}
	}
	data, _ := json.Marshal(content)
	return toolfile.Payload{Name: "output.json", MimeType: "application/json", Data: data}
}

func toolOutputPreviewText(content []ContentPart) string {
	pieces := make([]string, 0, len(content))
	for _, part := range content {
		if part.Type == ContentPartText {
			pieces = append(pieces, part.Text)
		} else {
			pieces = append(pieces, string(part.Value))
		}
	}
	return strings.Join(pieces, "\n")
}

// compactToolResultContent shortens text/json parts so that together they fit
// MaxToolResultContentBytes. Json parts are compacted down the level ladder and
// wrapped as {"truncated":true,...}; text parts share what is left. When nothing
// fits, the content is one text part saying it could not be shortened.
func compactToolResultContent(content []ContentPart, total int, reason string) []ContentPart {
	decoded := make([]interface{}, len(content))
	decodedOK := make([]bool, len(content))
	for i, part := range content {
		if part.Type == ContentPartJSON {
			decodedOK[i] = json.Unmarshal(part.Value, &decoded[i]) == nil
		}
	}
	for _, level := range toolOutputCompactLevels {
		out := make([]ContentPart, len(content))
		used := 0
		var texts []int
		ok := true
		for i, part := range content {
			if part.Type == ContentPartText {
				texts = append(texts, i)
				continue
			}
			value, fits := compactToolResultJSONPart(part.Value, decoded[i], decodedOK[i], level.depth, level.entries, level.text)
			if !fits {
				ok = false
				break
			}
			out[i] = ContentPart{Type: ContentPartJSON, Value: value}
			used += len(value)
		}
		if !ok || used > MaxToolResultContentBytes {
			continue
		}
		if !fitToolResultTexts(content, texts, out, MaxToolResultContentBytes-used, reason) {
			continue
		}
		return out
	}
	return []ContentPart{{Type: ContentPartText, Text: fmt.Sprintf("The tool output (%s) could not be shortened to fit the tool result limit, and %s.", formatToolOutputSize(total), reason)}}
}

// compactToolResultJSONPart returns the part's value unchanged when compacting
// would not change it, otherwise the wrapped compacted value.
func compactToolResultJSONPart(raw json.RawMessage, value interface{}, decoded bool, depth, entries, text int) (json.RawMessage, bool) {
	if !decoded {
		encoded, err := json.Marshal(map[string]interface{}{
			"truncated": true, "originalSizeBytes": len(raw), "reason": "the output was not valid json",
		})
		return encoded, err == nil
	}
	compacted := compactToolResultValue(value, depth, entries, text)
	if compactedFull, err := json.Marshal(compacted); err == nil {
		if original, err := json.Marshal(value); err == nil && string(original) == string(compactedFull) {
			return raw, true
		}
	}
	encoded, err := json.Marshal(map[string]interface{}{
		"truncated": true, "originalSizeBytes": len(raw), "value": compacted,
	})
	return encoded, err == nil
}

// fitToolResultTexts gives text parts their share of budget: a part that fits
// its share is kept whole and its unused share goes to the others; the rest are
// cut with a marker saying so.
func fitToolResultTexts(content []ContentPart, texts []int, out []ContentPart, budget int, reason string) bool {
	if budget < 0 {
		return false
	}
	pending := append([]int(nil), texts...)
	for len(pending) > 0 {
		share := budget / len(pending)
		var rest []int
		for _, i := range pending {
			if len(content[i].Text) <= share {
				out[i] = ContentPart{Type: ContentPartText, Text: content[i].Text}
				budget -= len(content[i].Text)
			} else {
				rest = append(rest, i)
			}
		}
		if len(rest) == len(pending) {
			for _, i := range rest {
				marker := fmt.Sprintf("\n… (truncated: %d bytes in total; %s)", len(content[i].Text), reason)
				keep := share - len(marker)
				if keep < 0 {
					return false
				}
				out[i] = ContentPart{Type: ContentPartText, Text: cutUTF8(content[i].Text, keep) + marker}
			}
			return true
		}
		pending = rest
	}
	return true
}

// compactToolResultValue shortens deep values, long lists and long strings.
func compactToolResultValue(value interface{}, depth, entries, text int) interface{} {
	if depth <= 0 {
		return "(more, not shown)"
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		compacted := make(map[string]interface{}, min(len(keys), entries)+1)
		for _, key := range keys[:min(len(keys), entries)] {
			compacted[key] = compactToolResultValue(typed[key], depth-1, entries, text)
		}
		if len(keys) > entries {
			compacted["_omittedFields"] = len(keys) - entries
		}
		return compacted
	case []interface{}:
		compacted := make([]interface{}, 0, min(len(typed), entries)+1)
		for _, item := range typed[:min(len(typed), entries)] {
			compacted = append(compacted, compactToolResultValue(item, depth-1, entries, text))
		}
		if len(typed) > entries {
			compacted = append(compacted, map[string]interface{}{"_omittedItems": len(typed) - entries})
		}
		return compacted
	case string:
		if len(typed) <= text {
			return typed
		}
		return cutUTF8(typed, text) + "… (shortened)"
	default:
		return typed
	}
}

// cutUTF8 returns at most limit bytes of text without splitting a rune.
func cutUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit <= 0 {
		return ""
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func formatToolOutputSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
