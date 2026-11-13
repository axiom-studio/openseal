package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func InvalidSessionEntryf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidSessionEntry, fmt.Sprintf(format, args...))
}

func entryTooLarge(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrSessionEntryTooLarge, fmt.Sprintf(format, args...))
}

func (e *SessionEntry) payloadCount() int {
	n := 0
	for _, set := range []bool{e.User != nil, e.Assistant != nil, e.Tool != nil, e.Compaction != nil, e.Note != nil, e.ModelChange != nil} {
		if set {
			n++
		}
	}
	return n
}

// Validate checks shape and caps of a persisted entry. It does not check
// fences, sequences or the batch rules (ValidateAppendAgentSessionEntries).
func (e *SessionEntry) Validate() error {
	if e == nil {
		return InvalidSessionEntryf("entry is required")
	}
	if strings.TrimSpace(e.ID) == "" {
		return InvalidSessionEntryf("id is required")
	}
	if !e.Type.Valid() {
		return InvalidSessionEntryf("unknown type %q", e.Type)
	}
	if e.payloadCount() != 1 {
		return InvalidSessionEntryf("entry %s needs exactly one payload", e.ID)
	}
	if e.Sequence < 0 || e.Step < 0 || e.Tokens < 0 || e.Revision < 0 {
		return InvalidSessionEntryf("entry %s has negative counters", e.ID)
	}
	if e.Type != SessionEntryTool && (e.State != "" || e.ToolCallID != "") {
		return InvalidSessionEntryf("entry %s: state and toolCallId are for tool entries", e.ID)
	}
	var err error
	switch e.Type {
	case SessionEntryUser:
		if e.User == nil {
			return InvalidSessionEntryf("user entry %s has no user payload", e.ID)
		}
		err = e.User.validate()
	case SessionEntryAssistant:
		if e.Assistant == nil {
			return InvalidSessionEntryf("assistant entry %s has no assistant payload", e.ID)
		}
		if strings.TrimSpace(e.StepID) == "" {
			return InvalidSessionEntryf("assistant entry %s needs stepId", e.ID)
		}
		err = e.Assistant.Validate()
	case SessionEntryTool:
		if e.Tool == nil {
			return InvalidSessionEntryf("tool entry %s has no tool payload", e.ID)
		}
		err = e.validateTool()
	case SessionEntryCompaction:
		if e.Compaction == nil {
			return InvalidSessionEntryf("compaction entry %s has no compaction payload", e.ID)
		}
		err = e.Compaction.validate(e.ID)
	case SessionEntryNote:
		if e.Note == nil {
			return InvalidSessionEntryf("note entry %s has no note payload", e.ID)
		}
		err = e.Note.validate()
	case SessionEntryModelChange:
		if e.ModelChange == nil {
			return InvalidSessionEntryf("model_change entry %s has no model_change payload", e.ID)
		}
		if strings.TrimSpace(e.ModelChange.Provider) == "" || strings.TrimSpace(e.ModelChange.Model) == "" {
			err = InvalidSessionEntryf("model_change needs provider and model")
		}
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return InvalidSessionEntryf("entry %s does not encode: %v", e.ID, err)
	}
	if len(encoded) > MaxSessionEntryBytes {
		return entryTooLarge("entry %s is %d bytes (max %d)", e.ID, len(encoded), MaxSessionEntryBytes)
	}
	return nil
}

type ContentPartLimits struct {
	Field          string
	MaxParts       int
	MaxContent     int // text + json bytes
	MaxFileParts   int // 0 = no extra file cap
	AllowDataBytes bool
}

// ValidateContentParts checks part shapes and caps. Image and file parts are
// references and do not count toward maxContent.
func ValidateContentParts(parts []ContentPart, limits ContentPartLimits) (images int, err error) {
	if limits.MaxParts > 0 && len(parts) > limits.MaxParts {
		return 0, entryTooLarge("%s has %d parts (max %d)", limits.Field, len(parts), limits.MaxParts)
	}
	contentBytes, files := 0, 0
	for i, part := range parts {
		if part.DataBase64 != "" && !limits.AllowDataBytes {
			return 0, InvalidSessionEntryf("%s part %d carries dataBase64; persisted parts are references", limits.Field, i)
		}
		switch part.Type {
		case ContentPartText:
			contentBytes += len(part.Text)
		case ContentPartJSON:
			if len(part.Value) == 0 || !json.Valid(part.Value) {
				return 0, InvalidSessionEntryf("%s part %d: json part needs a valid value", limits.Field, i)
			}
			contentBytes += JSONPartBytes(part.Value)
		case ContentPartFile:
			if part.Path == "" && part.URI == "" && part.Note == "" {
				return 0, InvalidSessionEntryf("%s part %d: file part needs path, uri or note", limits.Field, i)
			}
			if part.SizeBytes < 0 {
				return 0, InvalidSessionEntryf("%s part %d: negative size", limits.Field, i)
			}
			files++
		case ContentPartImage:
			switch part.MediaType {
			case "image/png", "image/jpeg", "image/webp":
			default:
				return 0, InvalidSessionEntryf("%s part %d: unsupported image media type %q", limits.Field, i, part.MediaType)
			}
			switch part.Detail {
			case "", "auto", "low", "high":
			default:
				return 0, InvalidSessionEntryf("%s part %d: unknown image detail %q", limits.Field, i, part.Detail)
			}
			sources := 0
			for _, source := range []string{part.ArtifactID, part.ActionCallID, part.Path} {
				if source != "" {
					sources++
				}
			}
			if sources == 0 && !(limits.AllowDataBytes && part.DataBase64 != "") {
				return 0, InvalidSessionEntryf("%s part %d: image part needs artifactId, actionCallId or path", limits.Field, i)
			}
			if sources > 1 {
				return 0, InvalidSessionEntryf("%s part %d: image part names more than one of artifactId, actionCallId and path; it needs exactly one source", limits.Field, i)
			}
			if part.SizeBytes < 0 {
				return 0, InvalidSessionEntryf("%s part %d: negative size", limits.Field, i)
			}
			images++
		default:
			return 0, InvalidSessionEntryf("%s part %d: unknown part type %q", limits.Field, i, part.Type)
		}
	}
	if limits.MaxContent > 0 && contentBytes > limits.MaxContent {
		return 0, entryTooLarge("%s text and json are %d bytes (max %d)", limits.Field, contentBytes, limits.MaxContent)
	}
	if limits.MaxFileParts > 0 && files > limits.MaxFileParts {
		return 0, entryTooLarge("%s has %d file parts (max %d)", limits.Field, files, limits.MaxFileParts)
	}
	if images > MaxSessionEntryImageParts {
		return 0, entryTooLarge("%s has %d image parts (max %d)", limits.Field, images, MaxSessionEntryImageParts)
	}
	return images, nil
}

func (u *UserEntry) validate() error {
	switch u.Origin {
	case UserEntryChannelMessage:
		if strings.TrimSpace(u.MessageID) == "" {
			return InvalidSessionEntryf("channel message entry needs messageId")
		}
	case UserEntryIntervention:
		if strings.TrimSpace(u.InterventionID) == "" && strings.TrimSpace(u.MessageID) == "" {
			return InvalidSessionEntryf("intervention entry needs interventionId or messageId")
		}
	case UserEntryGoal:
	default:
		return InvalidSessionEntryf("unknown user entry origin %q", u.Origin)
	}
	if u.GoalKind != "" && (u.Origin != UserEntryGoal || !userEntryGoalKinds[u.GoalKind]) {
		return InvalidSessionEntryf("invalid goal kind %q", u.GoalKind)
	}
	switch u.Author.Type {
	case "user", "agent", "service":
	default:
		return InvalidSessionEntryf("unknown author type %q", u.Author.Type)
	}
	if len(u.Content) == 0 || u.Content[0].Type != ContentPartText {
		return InvalidSessionEntryf("user entry content starts with a text part")
	}
	if u.MessageSequence < 0 {
		return InvalidSessionEntryf("negative message sequence")
	}
	_, err := ValidateContentParts(u.Content, ContentPartLimits{
		Field: "user content", MaxParts: MaxUserEntryParts, MaxContent: MaxUserEntryContentBytes, MaxFileParts: MaxUserEntryFileParts,
	})
	return err
}

func ValidateToolArguments(field string, args json.RawMessage) error {
	if len(args) > MaxToolArgumentsBytes {
		return entryTooLarge("%s are %d bytes (max %d)", field, len(args), MaxToolArgumentsBytes)
	}
	if len(args) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return InvalidSessionEntryf("%s must be a JSON object", field)
	}
	return nil
}

func (a *AssistantEntry) Validate() error {
	if !a.Provider.Valid() {
		return InvalidSessionEntryf("unknown assistant provider %q", a.Provider)
	}
	if strings.TrimSpace(a.Model) == "" {
		return InvalidSessionEntryf("assistant entry needs model")
	}
	if !a.StopReason.Valid() {
		return InvalidSessionEntryf("unknown stop reason %q", a.StopReason)
	}
	seen := make(map[string]bool, len(a.ToolCalls))
	for i, call := range a.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return InvalidSessionEntryf("tool call %d needs id and name", i)
		}
		if seen[call.ID] {
			return InvalidSessionEntryf("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = true
		if err := ValidateToolArguments("tool call "+call.ID+" arguments", call.Arguments); err != nil {
			return err
		}
	}
	if a.Native != nil {
		for i, raw := range a.Native.Anthropic {
			if !json.Valid(raw) {
				return InvalidSessionEntryf("native anthropic block %d is not JSON", i)
			}
		}
		for i, raw := range a.Native.Responses {
			if !json.Valid(raw) {
				return InvalidSessionEntryf("native responses item %d is not JSON", i)
			}
		}
	}
	return nil
}

func (e *SessionEntry) validateTool() error {
	t := e.Tool
	if !e.State.Valid() {
		return InvalidSessionEntryf("tool entry %s has unknown state %q", e.ID, e.State)
	}
	if strings.TrimSpace(e.ToolCallID) == "" {
		return InvalidSessionEntryf("tool entry %s needs toolCallId", e.ID)
	}
	if strings.TrimSpace(e.StepID) == "" {
		return InvalidSessionEntryf("tool entry %s needs stepId", e.ID)
	}
	if strings.TrimSpace(t.Name) == "" {
		return InvalidSessionEntryf("tool entry %s needs name", e.ID)
	}
	if !t.SideEffect.Valid() {
		return InvalidSessionEntryf("tool entry %s has unknown side effect %q", e.ID, t.SideEffect)
	}
	if t.ArgumentsError == "" {
		if err := ValidateToolArguments("tool entry arguments", t.Arguments); err != nil {
			return err
		}
	} else if len(t.Arguments) > MaxToolArgumentsBytes {
		return entryTooLarge("tool entry arguments are %d bytes (max %d)", len(t.Arguments), MaxToolArgumentsBytes)
	}
	for field, raw := range map[string]json.RawMessage{"control": t.Control, "pinnedRun": t.PinnedRun, "directive": t.Directive} {
		if len(raw) > 0 && !json.Valid(raw) {
			return InvalidSessionEntryf("tool entry %s %s is not JSON", e.ID, field)
		}
	}
	if (t.Wait != nil) != e.State.Awaiting() {
		return InvalidSessionEntryf("tool entry %s: wait is set exactly in awaiting states", e.ID)
	}
	if (t.Result != nil) != e.State.Terminal() {
		return InvalidSessionEntryf("tool entry %s: result is set exactly in terminal states", e.ID)
	}
	if t.Result != nil {
		if err := validateToolResultForState(e.State, t.Result, ErrInvalidSessionEntry); err != nil {
			return err
		}
	}
	return nil
}

// validateToolResultForState applies the result rules of a terminal state:
// completed ⇔ succeeded, interrupted ⇔ interrupted|canceled, error = any
// other status; IsError == (Status != succeeded); content caps.
func validateToolResultForState(state ToolEntryState, result *ToolResult, kind error) error {
	if !result.Status.Valid() {
		return fmt.Errorf("%w: unknown result status %q", kind, result.Status)
	}
	if result.IsError != (result.Status != ToolResultSucceeded) {
		return fmt.Errorf("%w: isError disagrees with status %q", kind, result.Status)
	}
	switch state {
	case ToolEntryCompleted:
		if result.Status != ToolResultSucceeded {
			return fmt.Errorf("%w: completed needs status succeeded", kind)
		}
	case ToolEntryInterrupted:
		if result.Status != ToolResultInterrupted && result.Status != ToolResultCanceled {
			return fmt.Errorf("%w: interrupted needs status interrupted or canceled", kind)
		}
	case ToolEntryError:
		if result.Status == ToolResultSucceeded {
			return fmt.Errorf("%w: error cannot have status succeeded", kind)
		}
	default:
		return fmt.Errorf("%w: %s is not terminal", kind, state)
	}
	_, err := ValidateContentParts(result.Content, ContentPartLimits{
		Field: "tool result", MaxParts: MaxToolResultParts, MaxContent: MaxToolResultContentBytes,
	})
	return err
}

func (c *CompactionEntry) validate(entryID string) error {
	if strings.TrimSpace(c.Summary) == "" {
		return InvalidSessionEntryf("compaction needs a summary")
	}
	if len(c.Summary) > MaxCompactionSummaryBytes {
		return entryTooLarge("compaction summary is %d bytes (max %d)", len(c.Summary), MaxCompactionSummaryBytes)
	}
	if c.FirstKeptEntryID == entryID {
		return InvalidSessionEntryf("compaction cannot keep itself")
	}
	if c.Trigger != CompactionTriggerThreshold && c.Trigger != CompactionTriggerOverflow {
		return InvalidSessionEntryf("unknown compaction trigger %q", c.Trigger)
	}
	if c.TokensBefore < 0 {
		return InvalidSessionEntryf("negative tokensBefore")
	}
	return nil
}

func (n *NoteEntry) validate() error {
	if !n.Kind.Valid() {
		return InvalidSessionEntryf("unknown note kind %q", n.Kind)
	}
	if strings.TrimSpace(n.Text) == "" {
		return InvalidSessionEntryf("note needs text")
	}
	if len(n.Text) > MaxNoteTextBytes {
		return entryTooLarge("note text is %d bytes (max %d)", len(n.Text), MaxNoteTextBytes)
	}
	if len(n.Data) > 0 && !json.Valid(n.Data) {
		return InvalidSessionEntryf("note data is not JSON")
	}
	return nil
}
