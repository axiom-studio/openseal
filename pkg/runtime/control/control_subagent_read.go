package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// read_subagent (inline phase) reads a subagent's own session: what it was
// asked, what it said, the tools it called with their arguments, and their
// results. It starts at the subagent's brief, never the parent path it
// inherited, and works while the subagent runs. Reasoning never leaves the
// kernel (D2): only an assistant entry's text is read. A subagent that runs
// as another Agent shares only its status and answer (DI-5).

const (
	subagentReadEntryBytes   = 8 << 10  // one entry's text or tool result
	subagentReadResultBytes  = 32 << 10 // one read
	subagentReadDefaultLimit = 50
	subagentReadMaximumLimit = 200
	subagentReadPage         = 200
	subagentReadMaximumPages = 10
)

type readSubagentArgs struct {
	ID    string `json:"id"`
	After int64  `json:"after,omitempty"`
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type readSubagentHandler struct{}

func (readSubagentHandler) Spec(agentrun.ControlAuthority) ToolSpec {
	return ToolSpec{
		Description: "Read a subagent's work so far: its messages, tool calls and results, also while it runs.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"id"},
			"properties": map[string]interface{}{
				"id":    map[string]interface{}{"type": "string", "description": "The subagent id (s1, ...)."},
				"after": map[string]interface{}{"type": "integer", "minimum": 0, "description": "Continue after this entry (the next of a previous read)."},
				"query": map[string]interface{}{"type": "string", "description": "Only entries containing this text (any case)."},
				"limit": map[string]interface{}{
					"type": "integer", "minimum": 1, "maximum": subagentReadMaximumLimit,
					"description": fmt.Sprintf("At most this many entries (default %d).", subagentReadDefaultLimit),
				},
			},
		},
	}
}

func (readSubagentHandler) Decode(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[readSubagentArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*readSubagentArgs)
	args.ID, args.Query = strings.TrimSpace(args.ID), strings.TrimSpace(args.Query)
	switch {
	case args.ID == "":
		return nil, errors.New("id is required")
	case args.After < 0:
		return nil, errors.New("after cannot be negative")
	case args.Limit == 0:
		args.Limit = subagentReadDefaultLimit
	case args.Limit < 0 || args.Limit > subagentReadMaximumLimit:
		return nil, fmt.Errorf("limit must be 1 to %d", subagentReadMaximumLimit)
	}
	return args, nil
}

func (readSubagentHandler) Run(ctx context.Context, x *Executor, cc agentrun.ControlContext) (Result, error) {
	args := cc.Arguments.(*readSubagentArgs)
	subagents, err := LoadSubagents(ctx, x.Sessions, x.Portfolio, cc.Run, cc.Entry.ID)
	if err != nil {
		return Result{}, err
	}
	ref, child, ok := subagents.lookup(args.ID)
	if !ok {
		return x.refuse(ctx, cc, subagents.unknown(args.ID))
	}
	value := map[string]interface{}{"id": ref.ID, "status": subagentStatus(child)}
	if child.AssignedAgentID != cc.Run.AssignedAgentID {
		value["agentId"] = child.AssignedAgentID
		value["message"] = "This subagent works as another Agent: only its status and answer are shared."
		if answer := agentbuilder.ChildRunAnswer(child); answer != "" && models.IsTerminalAgentRunStatus(child.Status) {
			value["answer"] = truncateControlText(answer, ref.ID)
		}
		return x.complete(ctx, cc, models.ControlJSONResult(models.ToolResultSucceeded, value), nil)
	}
	entries, next, err := x.readSubagentLog(ctx, child, args)
	if err != nil {
		return Result{}, err
	}
	value["entries"] = entries
	if next > 0 {
		value["next"] = next
	}
	return x.complete(ctx, cc, models.ControlJSONResult(models.ToolResultSucceeded, value), nil)
}

// readSubagentLog reads child's own session entries after args.After that
// match args.Query, within the entry and byte limits. next is the sequence
// to continue after, 0 when the log was read to its end.
func (x *Executor) readSubagentLog(ctx context.Context, child *models.AgentRun, args *readSubagentArgs) ([]interface{}, int64, error) {
	entries := []interface{}{}
	if child.SessionID == "" {
		return entries, 0, nil
	}
	query := strings.ToLower(args.Query)
	after, used := args.After, 0
	for page := 0; page < subagentReadMaximumPages; page++ {
		batch, err := x.Sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
			Scope: child.Scope, SessionID: child.SessionID, AfterSequence: after, Limit: subagentReadPage,
		})
		if err != nil {
			return nil, 0, err
		}
		for _, entry := range batch {
			item := subagentLogItem(entry)
			if item != nil && (query == "" || strings.Contains(strings.ToLower(item.text()), query)) {
				encoded, err := json.Marshal(item)
				if err != nil {
					return nil, 0, err
				}
				if len(entries) == args.Limit || used+len(encoded) > subagentReadResultBytes {
					return entries, after, nil
				}
				used += len(encoded)
				entries = append(entries, item)
			}
			after = entry.Sequence
		}
		if len(batch) < subagentReadPage {
			return entries, 0, nil
		}
	}
	return entries, after, nil
}

// subagentLogEntry is one entry of a read. Text fields are cut to 8 KiB;
// ResultBytes is the full size of a cut tool result.
type subagentLogEntry struct {
	Sequence    int64           `json:"seq"`
	Type        string          `json:"type"` // brief | message | assistant | tool | summary
	Text        string          `json:"text,omitempty"`
	Tool        string          `json:"tool,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	State       string          `json:"state,omitempty"`
	Result      string          `json:"result,omitempty"`
	ResultBytes int             `json:"resultBytes,omitempty"`
}

func (e *subagentLogEntry) text() string {
	return e.Text + "\n" + e.Tool + "\n" + string(e.Arguments) + "\n" + e.Result
}

// subagentLogItem is what a read shows of entry; nil for entries it skips
// (notes, model changes, a tool's pending state).
func subagentLogItem(entry *models.SessionEntry) *subagentLogEntry {
	item := &subagentLogEntry{Sequence: entry.Sequence}
	switch {
	case entry.Type == models.SessionEntryUser && entry.User != nil:
		item.Type = "message"
		if entry.User.Origin == models.UserEntryGoal {
			item.Type = "brief"
		}
		item.Text = cutSubagentText(subagentPartsText(entry.User.Content))
	case entry.Type == models.SessionEntryAssistant && entry.Assistant != nil:
		if strings.TrimSpace(entry.Assistant.Text) == "" {
			return nil
		}
		item.Type, item.Text = "assistant", cutSubagentText(entry.Assistant.Text)
	case entry.Type == models.SessionEntryTool && entry.Tool != nil:
		item.Type, item.Tool, item.State = "tool", entry.Tool.Name, string(entry.State)
		if len(entry.Tool.Arguments) <= subagentReadEntryBytes {
			item.Arguments = entry.Tool.Arguments
		}
		if entry.Tool.Result != nil {
			result := subagentPartsText(entry.Tool.Result.Content)
			item.Result = cutSubagentText(result)
			if len(result) > len(item.Result) {
				item.ResultBytes = len(result)
			}
		}
	case entry.Type == models.SessionEntryCompaction && entry.Compaction != nil:
		item.Type, item.Text = "summary", cutSubagentText(entry.Compaction.Summary)
	default:
		return nil
	}
	return item
}

func cutSubagentText(text string) string {
	if len(text) <= subagentReadEntryBytes {
		return text
	}
	return models.TruncateUTF8(text, subagentReadEntryBytes) + "…"
}

// subagentPartsText is the readable text of content parts: text and JSON
// verbatim, files and images by reference.
func subagentPartsText(parts []models.ContentPart) string {
	var b strings.Builder
	for _, part := range parts {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		switch part.Type {
		case models.ContentPartText:
			b.WriteString(part.Text)
		case models.ContentPartJSON:
			b.Write(part.Value)
		case models.ContentPartFile:
			b.WriteString("[file " + part.Path + "]")
		case models.ContentPartImage:
			b.WriteString("[image]")
		}
	}
	return b.String()
}
