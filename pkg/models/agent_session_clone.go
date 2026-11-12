package models

import (
	"bytes"
	"encoding/json"
	"time"
)

// EstimateSessionEntryTokens is ceil(len(json.Marshal(payload))/4) of the one
// set payload. Plain byte heuristic; compaction may pass its own Tokens.
func EstimateSessionEntryTokens(e *SessionEntry) int64 {
	if e == nil {
		return 0
	}
	var payload interface{}
	switch {
	case e.User != nil:
		payload = e.User
	case e.Assistant != nil:
		payload = e.Assistant
	case e.Tool != nil:
		payload = e.Tool
	case e.Compaction != nil:
		payload = e.Compaction
	case e.Note != nil:
		payload = e.Note
	case e.ModelChange != nil:
		payload = e.ModelChange
	default:
		return 0
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return int64((len(encoded) + 3) / 4)
}

// JSONPartBytes is what a json part counts toward a content cap: the size of
// its compact encoding. A store can hand a value back re-encoded (Postgres
// jsonb puts a space after every ':' and ','), so the length of a value as
// read is not the size that was written and checked; counting the compact
// form keeps a result that fit when it was written fitting when it is read.
func JSONPartBytes(raw json.RawMessage) int {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return len(raw)
	}
	return out.Len()
}

// CompactJSONValue is raw in its compact encoding, or a copy of raw when it is
// not valid json.
func CompactJSONValue(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return CloneRawMessage(raw)
	}
	return json.RawMessage(out.Bytes())
}

func CloneRawMessage(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	return append(json.RawMessage(nil), in...)
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

func CloneContentParts(in []ContentPart) []ContentPart {
	if in == nil {
		return nil
	}
	out := make([]ContentPart, len(in))
	for i, part := range in {
		part.Value = CloneRawMessage(part.Value)
		out[i] = part
	}
	return out
}

func CloneToolResult(in *ToolResult) *ToolResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Content = CloneContentParts(in.Content)
	return &out
}

func cloneToolEntryWait(in *ToolEntryWait) *ToolEntryWait {
	if in == nil {
		return nil
	}
	out := *in
	out.ChildRunIDs = cloneStrings(in.ChildRunIDs)
	out.RetryAt = cloneTimePtr(in.RetryAt)
	return &out
}

func cloneToolGuardOutcome(in *ToolGuardOutcome) *ToolGuardOutcome {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func CloneAgentSession(in *AgentSession) *AgentSession {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func CloneSessionEntry(in *SessionEntry) *SessionEntry {
	if in == nil {
		return nil
	}
	out := *in
	if in.User != nil {
		user := *in.User
		user.Content = CloneContentParts(in.User.Content)
		out.User = &user
	}
	if in.Assistant != nil {
		assistant := *in.Assistant
		if in.Assistant.ToolCalls != nil {
			assistant.ToolCalls = make([]ModelToolCall, len(in.Assistant.ToolCalls))
			for i, call := range in.Assistant.ToolCalls {
				call.Arguments = CloneRawMessage(call.Arguments)
				assistant.ToolCalls[i] = call
			}
		}
		if in.Assistant.Native != nil {
			native := *in.Assistant.Native
			native.Anthropic = cloneRawList(in.Assistant.Native.Anthropic)
			native.Responses = cloneRawList(in.Assistant.Native.Responses)
			assistant.Native = &native
		}
		if in.Assistant.Limits != nil {
			limits := *in.Assistant.Limits
			assistant.Limits = &limits
		}
		out.Assistant = &assistant
	}
	if in.Tool != nil {
		tool := *in.Tool
		tool.Arguments = CloneRawMessage(in.Tool.Arguments)
		tool.Guard = cloneToolGuardOutcome(in.Tool.Guard)
		tool.Wait = cloneToolEntryWait(in.Tool.Wait)
		tool.Result = CloneToolResult(in.Tool.Result)
		tool.Control = CloneRawMessage(in.Tool.Control)
		tool.SurfacedTools = CloneSurfacedTools(in.Tool.SurfacedTools)
		tool.LoadedInstructions = cloneStrings(in.Tool.LoadedInstructions)
		tool.PinnedRun = CloneRawMessage(in.Tool.PinnedRun)
		tool.Directive = CloneRawMessage(in.Tool.Directive)
		tool.StartedAt = cloneTimePtr(in.Tool.StartedAt)
		tool.CompletedAt = cloneTimePtr(in.Tool.CompletedAt)
		out.Tool = &tool
	}
	if in.Compaction != nil {
		compaction := *in.Compaction
		compaction.ActionCallIDs = cloneStrings(in.Compaction.ActionCallIDs)
		compaction.CarriedTools = CloneSurfacedTools(in.Compaction.CarriedTools)
		out.Compaction = &compaction
	}
	if in.Note != nil {
		note := *in.Note
		note.Data = CloneRawMessage(in.Note.Data)
		out.Note = &note
	}
	if in.ModelChange != nil {
		change := *in.ModelChange
		out.ModelChange = &change
	}
	return &out
}

func cloneRawList(in []json.RawMessage) []json.RawMessage {
	if in == nil {
		return nil
	}
	out := make([]json.RawMessage, len(in))
	for i, raw := range in {
		out[i] = CloneRawMessage(raw)
	}
	return out
}
