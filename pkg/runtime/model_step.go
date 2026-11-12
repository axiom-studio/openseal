package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
)

// ModelStep wire types: one request is one model call. The kernel renders the
// session path, the system spec and the final tool list; the step host returns
// one assistant entry ready to append. The persisted payload types
// (AssistantEntry, ContentPart, ModelToolCall, AssistantNative, ModelStepUsage,
// ModelStopReason, ModelStepProvider) live in pkg/models/agent_session.go.

// v2: tools surfaced by a load carry ModelStepTool.Surface and the surfacing
// tool entry lists them in SessionEntryView.Surfaces; ModelStepSystem no
// longer carries approval hints.
const ModelStepAPIVersion = "openseal.model-step/v2"

type ModelStepPurpose string

const (
	// ModelStepPurposeStep is a normal loop step.
	ModelStepPurposeStep ModelStepPurpose = "step"
	// ModelStepPurposeSummary is a compaction call: same system and tools,
	// tool_choice none, with SummaryInstruction appended as a user message.
	ModelStepPurposeSummary ModelStepPurpose = "summary"
	// ModelStepPurposeExplanation is a failure explanation call; Tools is empty.
	ModelStepPurposeExplanation ModelStepPurpose = "explanation"
	// ModelStepPurposeAcknowledgement is a progress acknowledgement: sessionless
	// (SessionID may be ""), Tools is empty and the entries are in-memory.
	ModelStepPurposeAcknowledgement ModelStepPurpose = "acknowledgement"
)

func (p ModelStepPurpose) Valid() bool {
	switch p {
	case ModelStepPurposeStep, ModelStepPurposeSummary, ModelStepPurposeExplanation, ModelStepPurposeAcknowledgement:
		return true
	}
	return false
}

// toolFree reports whether the purpose sends no tools.
func (p ModelStepPurpose) toolFree() bool {
	return p == ModelStepPurposeExplanation || p == ModelStepPurposeAcknowledgement
}

// ErrInvalidModelStepRequest wraps every ModelStepRequest.Validate failure.
var ErrInvalidModelStepRequest = errors.New("invalid model step request")

// ModelStepRequest is the whole input of one model call. Identity is
// StepID + Attempt; the model grant's TurnID and InvocationID are StepID.
type ModelStepRequest struct {
	APIVersion        string                          `json:"apiVersion"`
	Purpose           ModelStepPurpose                `json:"purpose"`
	Scope             models.Scope                    `json:"scope"`
	RunID             string                          `json:"runId"`
	StepID            string                          `json:"stepId"`
	Attempt           int                             `json:"attempt"` // 0-based; forced-compaction retry = 1; logs only
	SessionID         string                          `json:"sessionId"`
	AgentID           string                          `json:"agentId"`
	DefinitionID      string                          `json:"definitionId"`
	DefinitionVersion string                          `json:"definitionVersion"`
	ModelCredential   *capability.CredentialReference `json:"modelCredential,omitempty"`
	Model             string                          `json:"model,omitempty"` // requested model; the binding may resolve another
	System            ModelStepSystem                 `json:"system"`
	Tools             []models.ModelStepTool          `json:"tools"`
	Entries           []SessionEntryView              `json:"entries"`
	MaxOutputTokens   int64                           `json:"maxOutputTokens,omitempty"` // per-step reservation; 0 = no Run limit
	Stream            ModelStepStream                 `json:"stream"`
	// SummaryInstruction is the kernel's compaction instruction. It is
	// required when Purpose is summary and empty otherwise.
	SummaryInstruction string `json:"summaryInstruction,omitempty"`
}

type ModelStepSystem struct {
	// Kernel-authored, stable for the session: persona, channel rules,
	// routine-script rules, chat-files instruction, spoken style, Team rules.
	AgentInstructions []string            `json:"agentInstructions,omitempty"`
	SkillPrompts      []HostedSkillPrompt `json:"skillPrompts,omitempty"`
	// Kernel-authored named sections, rendered in the given order.
	Sections []ModelStepSystemSection `json:"sections,omitempty"`
}

type ModelStepSystemSection struct {
	ID    string `json:"id"`    // [a-z0-9_.-]{1,64}, unique
	Title string `json:"title"` // rendered as "## <Title>"
	Text  string `json:"text"`
}

type ModelStepStream struct {
	AnswerPreview        bool   `json:"answerPreview,omitempty"`
	ExternalChannelReply bool   `json:"externalChannelReply,omitempty"`
	Spoken               bool   `json:"spoken,omitempty"`
	FinalAnswerTool      string `json:"finalAnswerTool,omitempty"` // "" = no tool preview
}

// ---------------------------------------------------------------------------
// Entry views (the rendered path)

// SessionEntryView is one rendered session entry. The kernel sends entries in
// render order: the latest compaction (if any), its kept tail by sequence,
// then every later entry by sequence.
type SessionEntryView struct {
	ID          string                   `json:"id"`
	Sequence    int64                    `json:"sequence"`
	Type        models.SessionEntryType  `json:"type"`
	CreatedAt   time.Time                `json:"createdAt"`
	User        *UserEntryView           `json:"user,omitempty"`
	Assistant   *models.AssistantEntry   `json:"assistant,omitempty"` // Limits is nil in a view
	Tool        *ToolEntryView           `json:"tool,omitempty"`
	Compaction  *CompactionEntryView     `json:"compaction,omitempty"`
	Note        *NoteEntryView           `json:"note,omitempty"`
	ModelChange *models.ModelChangeEntry `json:"modelChange,omitempty"`
	// Surfaces names, in order, the tools that became callable after this
	// entry (each listed tool has Surface == this entry's id). Only a
	// completed tool entry carries it.
	Surfaces []string `json:"surfaces,omitempty"`
}

const (
	EntryAuthorPerson = "person"
	EntryAuthorAgent  = "agent"
)

type EntryAuthor struct {
	Kind  string `json:"kind"` // person | agent
	ID    string `json:"id,omitempty"`
	Label string `json:"label"` // display name used for attribution
}

type UserEntryView struct {
	Author  *EntryAuthor         `json:"author,omitempty"` // nil = the Run's requester
	Content []models.ContentPart `json:"content"`
}

type ToolEntryView struct {
	CallID       string                `json:"callId"`
	Name         string                `json:"name"`
	Arguments    json.RawMessage       `json:"arguments"`
	State        models.ToolEntryState `json:"state"` // always terminal
	Result       ToolResultView        `json:"result"`
	ActionCallID string                `json:"actionCallId,omitempty"`
}

type ToolResultView struct {
	Content []models.ContentPart `json:"content"`
	IsError bool                 `json:"isError,omitempty"`
}

type CompactionEntryView struct {
	Summary          string `json:"summary"` // kernel-rendered (CompactionSummaryText)
	FirstKeptEntryID string `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int64  `json:"tokensBefore"`
}

type NoteEntryView struct {
	Kind string `json:"kind"` // kernel tag, not rendered
	Text string `json:"text"`
}

// ---------------------------------------------------------------------------
// Result

type ModelStepResult struct {
	APIVersion string                `json:"apiVersion"`
	StepID     string                `json:"stepId"`
	Assistant  models.AssistantEntry `json:"assistant"` // summary: Text is the summary, no tool calls
	// Narration is the text before the first tool call; "" when there are none.
	Narration string              `json:"narration,omitempty"`
	Model     ModelStepModelInfo  `json:"model"`
	Render    ModelStepRenderInfo `json:"render"`
}

type ModelStepModelInfo struct {
	Provider         models.ModelStepProvider `json:"provider"`
	Model            string                   `json:"model"`
	ContextTokens    int64                    `json:"contextTokens,omitempty"`    // 0 = unknown
	InputLimitTokens int64                    `json:"inputLimitTokens,omitempty"` // 0 = unknown
	MaxOutputTokens  int64                    `json:"maxOutputTokens,omitempty"`  // effective per-call cap used
}

type ModelStepRenderInfo struct {
	EstimatedInputTokens int64 `json:"estimatedInputTokens"`
	// ReasoningCutoffSequence is the highest entry sequence whose reasoning was
	// not replayed; 0 = none dropped.
	ReasoningCutoffSequence int64 `json:"reasoningCutoffSequence,omitempty"`
	ImagesOmitted           bool  `json:"imagesOmitted,omitempty"`
}

// ---------------------------------------------------------------------------
// Validation

// maxModelStepMediaBytes is the decoded cap of one view image (1 MiB).
const maxModelStepMediaBytes = 1 << 20

var (
	modelStepToolNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	modelStepSectionIDPattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
)

// validModelStepToolName reports whether name is a tool name every model
// provider accepts.
func validModelStepToolName(name string) bool {
	return modelStepToolNamePattern.MatchString(name)
}

func invalidModelStep(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidModelStepRequest, fmt.Sprintf(format, args...))
}

// Validate checks a request before the kernel sends it and when the host
// receives it. It returns the first error, in the order: identity, tools,
// system sections, entries, tool pairing, content parts, purpose rules.
func (r ModelStepRequest) Validate() error {
	if err := r.validateIdentity(); err != nil {
		return err
	}
	if err := r.validateTools(); err != nil {
		return err
	}
	if err := r.validateSections(); err != nil {
		return err
	}
	compacted, err := r.validateEntries()
	if err != nil {
		return err
	}
	if err := validateModelStepToolPairing(r.Entries, compacted); err != nil {
		return err
	}
	if err := r.validateSurfaces(); err != nil {
		return err
	}
	if err := r.validateContent(); err != nil {
		return err
	}
	if r.Purpose == ModelStepPurposeSummary && len(r.Entries) < 2 {
		return invalidModelStep("a summary step needs at least two entries")
	}
	return nil
}

func (r ModelStepRequest) validateIdentity() error {
	if r.APIVersion != ModelStepAPIVersion {
		return invalidModelStep("apiVersion %q is not %q", r.APIVersion, ModelStepAPIVersion)
	}
	if !r.Purpose.Valid() {
		return invalidModelStep("unknown purpose %q", r.Purpose)
	}
	if err := r.Scope.Validate(); err != nil {
		return invalidModelStep("scope: %v", err)
	}
	for _, field := range []struct{ name, value string }{
		{"runId", r.RunID}, {"stepId", r.StepID}, {"agentId", r.AgentID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return invalidModelStep("%s is required", field.name)
		}
	}
	if strings.TrimSpace(r.SessionID) == "" && r.Purpose != ModelStepPurposeAcknowledgement {
		return invalidModelStep("sessionId is required unless the purpose is acknowledgement")
	}
	if r.Attempt < 0 {
		return invalidModelStep("attempt is negative")
	}
	if r.MaxOutputTokens < 0 {
		return invalidModelStep("maxOutputTokens is negative")
	}
	if r.Purpose == ModelStepPurposeSummary {
		if strings.TrimSpace(r.SummaryInstruction) == "" {
			return invalidModelStep("a summary step needs summaryInstruction")
		}
	} else if r.SummaryInstruction != "" {
		return invalidModelStep("summaryInstruction is only for summary steps")
	}
	return nil
}

func (r ModelStepRequest) validateTools() error {
	if r.Purpose.toolFree() && len(r.Tools) > 0 {
		return invalidModelStep("a %s step sends no tools", r.Purpose)
	}
	names := make(map[string]bool, len(r.Tools))
	lastRank := 0
	for i, tool := range r.Tools {
		if !validModelStepToolName(tool.Name) {
			return invalidModelStep("tool %d has invalid name %q", i, tool.Name)
		}
		if names[tool.Name] {
			return invalidModelStep("duplicate tool name %q", tool.Name)
		}
		names[tool.Name] = true
		rank := tool.Group.Rank()
		if rank == 0 {
			return invalidModelStep("tool %q has unknown group %q", tool.Name, tool.Group)
		}
		if rank < lastRank {
			return invalidModelStep("tool %q (group %s) is out of order; groups run harness, core, loaded", tool.Name, tool.Group)
		}
		lastRank = rank
		if schemaType, _ := tool.InputSchema["type"].(string); schemaType != "object" {
			return invalidModelStep("tool %q input schema must have type object", tool.Name)
		}
	}
	if r.Stream.FinalAnswerTool != "" && !names[r.Stream.FinalAnswerTool] {
		return invalidModelStep("stream finalAnswerTool %q is not a listed tool", r.Stream.FinalAnswerTool)
	}
	return nil
}

// validateSurfaces checks the surfaced tools against the entries: only a
// loaded tool has a Surface, and it names a completed tool entry that lists
// it; every name an entry lists is a tool with that Surface, listed once; and
// within the loaded group the tools callable from the start come first, then
// the surfaced tools in (entry position, index in Surfaces) order.
func (r ModelStepRequest) validateSurfaces() error {
	tools := make(map[string]models.ModelStepTool, len(r.Tools))
	var surfaced []string
	for _, tool := range r.Tools {
		tools[tool.Name] = tool
		if tool.Surface == "" {
			if len(surfaced) > 0 && tool.Group == models.ModelStepToolLoaded {
				return invalidModelStep("tool %q is callable from the start but follows surfaced tools", tool.Name)
			}
			continue
		}
		if tool.Group != models.ModelStepToolLoaded {
			return invalidModelStep("tool %q (group %s) has a surface; only loaded tools do", tool.Name, tool.Group)
		}
		surfaced = append(surfaced, tool.Name)
	}
	listed := make(map[string]bool, len(surfaced))
	var order []string
	for i := range r.Entries {
		entry := &r.Entries[i]
		if len(entry.Surfaces) == 0 {
			continue
		}
		if entry.Type != models.SessionEntryTool || entry.Tool.State != models.ToolEntryCompleted {
			return invalidModelStep("entry %s surfaces tools but is not a completed tool entry", entry.ID)
		}
		for _, name := range entry.Surfaces {
			tool, ok := tools[name]
			if !ok || tool.Surface != entry.ID {
				return invalidModelStep("entry %s surfaces %q, which is not a tool with that surface", entry.ID, name)
			}
			if listed[name] {
				return invalidModelStep("tool %q is surfaced twice", name)
			}
			listed[name] = true
			order = append(order, name)
		}
	}
	if len(order) != len(surfaced) {
		for _, name := range surfaced {
			if !listed[name] {
				return invalidModelStep("tool %q surfaces at %q, which lists no such tool", name, tools[name].Surface)
			}
		}
	}
	for i := range order {
		if order[i] != surfaced[i] {
			return invalidModelStep("surfaced tool %q is out of order; surfaced tools follow their entries", surfaced[i])
		}
	}
	return nil
}

func (r ModelStepRequest) validateSections() error {
	seen := make(map[string]bool, len(r.System.Sections))
	for i, section := range r.System.Sections {
		if !modelStepSectionIDPattern.MatchString(section.ID) {
			return invalidModelStep("system section %d has invalid id %q", i, section.ID)
		}
		if seen[section.ID] {
			return invalidModelStep("duplicate system section id %q", section.ID)
		}
		seen[section.ID] = true
	}
	return nil
}

func (v *SessionEntryView) payloadCount() int {
	n := 0
	for _, set := range []bool{v.User != nil, v.Assistant != nil, v.Tool != nil, v.Compaction != nil, v.Note != nil, v.ModelChange != nil} {
		if set {
			n++
		}
	}
	return n
}

func (v *SessionEntryView) payloadMatchesType() bool {
	switch v.Type {
	case models.SessionEntryUser:
		return v.User != nil
	case models.SessionEntryAssistant:
		return v.Assistant != nil
	case models.SessionEntryTool:
		return v.Tool != nil
	case models.SessionEntryCompaction:
		return v.Compaction != nil
	case models.SessionEntryNote:
		return v.Note != nil
	case models.SessionEntryModelChange:
		return v.ModelChange != nil
	}
	return false
}

// validateEntries checks ids, payloads and render order. It reports whether
// the path starts with a compaction entry.
func (r ModelStepRequest) validateEntries() (bool, error) {
	if len(r.Entries) == 0 {
		return false, invalidModelStep("entries are required")
	}
	ids := make(map[string]bool, len(r.Entries))
	compacted := false
	for i := range r.Entries {
		entry := &r.Entries[i]
		if strings.TrimSpace(entry.ID) == "" {
			return false, invalidModelStep("entry %d needs an id", i)
		}
		if ids[entry.ID] {
			return false, invalidModelStep("duplicate entry id %q", entry.ID)
		}
		ids[entry.ID] = true
		if entry.Sequence <= 0 {
			return false, invalidModelStep("entry %s needs a positive sequence", entry.ID)
		}
		if !entry.Type.Valid() {
			return false, invalidModelStep("entry %s has unknown type %q", entry.ID, entry.Type)
		}
		if entry.payloadCount() != 1 || !entry.payloadMatchesType() {
			return false, invalidModelStep("entry %s needs exactly the %s payload", entry.ID, entry.Type)
		}
		if entry.Type == models.SessionEntryCompaction {
			if i != 0 {
				return false, invalidModelStep("compaction entry %s must be the first entry", entry.ID)
			}
			compacted = true
		}
		if err := entry.validatePayload(); err != nil {
			return false, err
		}
	}

	rest := r.Entries
	if compacted {
		rest = r.Entries[1:]
	}
	if len(rest) > 0 && rest[0].Type == models.SessionEntryTool {
		return false, invalidModelStep("entry %s: the path cannot start with a tool entry", rest[0].ID)
	}
	if compacted {
		compactionSequence := r.Entries[0].Sequence
		pastCompaction := false
		var previous int64
		for i := range rest {
			entry := &rest[i]
			if entry.Sequence == compactionSequence {
				return false, invalidModelStep("entry %s repeats the compaction sequence", entry.ID)
			}
			after := entry.Sequence > compactionSequence
			if pastCompaction && !after {
				return false, invalidModelStep("entry %s: the kept tail must come before entries after the compaction", entry.ID)
			}
			if after && !pastCompaction {
				pastCompaction = true
			} else if i > 0 && entry.Sequence <= previous {
				return false, invalidModelStep("entry %s is out of sequence order", entry.ID)
			}
			previous = entry.Sequence
		}
		if keep := r.Entries[0].Compaction.FirstKeptEntryID; keep != "" {
			if len(rest) == 0 || rest[0].ID != keep || rest[0].Sequence > compactionSequence {
				return false, invalidModelStep("compaction keeps %s but the kept tail does not start there", keep)
			}
		}
		return true, nil
	}
	for i := 1; i < len(rest); i++ {
		if rest[i].Sequence <= rest[i-1].Sequence {
			return false, invalidModelStep("entry %s is out of sequence order", rest[i].ID)
		}
	}
	return false, nil
}

// validatePayload checks the shape of the one payload; content parts are
// checked later (validateContent).
func (v *SessionEntryView) validatePayload() error {
	switch v.Type {
	case models.SessionEntryUser:
		if author := v.User.Author; author != nil {
			if author.Kind != EntryAuthorPerson && author.Kind != EntryAuthorAgent {
				return invalidModelStep("entry %s: unknown author kind %q", v.ID, author.Kind)
			}
			if strings.TrimSpace(author.Label) == "" {
				return invalidModelStep("entry %s: author needs a label", v.ID)
			}
		}
		if len(v.User.Content) == 0 {
			return invalidModelStep("user entry %s has no content", v.ID)
		}
	case models.SessionEntryAssistant:
		if v.Assistant.Limits != nil {
			return invalidModelStep("assistant entry %s: limits are kernel-only and not sent", v.ID)
		}
		if err := v.Assistant.Validate(); err != nil {
			return invalidModelStep("assistant entry %s: %v", v.ID, err)
		}
	case models.SessionEntryTool:
		tool := v.Tool
		if strings.TrimSpace(tool.CallID) == "" || strings.TrimSpace(tool.Name) == "" {
			return invalidModelStep("tool entry %s needs callId and name", v.ID)
		}
		if err := models.ValidateToolArguments("tool entry "+v.ID+" arguments", tool.Arguments); err != nil {
			return invalidModelStep("%v", err)
		}
		if !tool.State.Terminal() {
			return invalidModelStep("tool entry %s is %q; only finished tool entries are sent", v.ID, tool.State)
		}
		if tool.Result.IsError == (tool.State == models.ToolEntryCompleted) {
			return invalidModelStep("tool entry %s: isError disagrees with state %s", v.ID, tool.State)
		}
	case models.SessionEntryCompaction:
		if strings.TrimSpace(v.Compaction.Summary) == "" {
			return invalidModelStep("compaction entry %s needs a summary", v.ID)
		}
		if v.Compaction.TokensBefore < 0 {
			return invalidModelStep("compaction entry %s has negative tokensBefore", v.ID)
		}
	case models.SessionEntryNote:
		if strings.TrimSpace(v.Note.Text) == "" {
			return invalidModelStep("note entry %s needs text", v.ID)
		}
	case models.SessionEntryModelChange:
		if strings.TrimSpace(v.ModelChange.Provider) == "" || strings.TrimSpace(v.ModelChange.Model) == "" {
			return invalidModelStep("model_change entry %s needs provider and model", v.ID)
		}
	}
	return nil
}

// validateModelStepToolPairing: every assistant tool call has exactly one
// later tool entry before the next assistant entry, and every tool entry
// answers a call of the latest preceding assistant entry.
func validateModelStepToolPairing(entries []SessionEntryView, compacted bool) error {
	if compacted {
		entries = entries[1:]
	}
	var assistant *SessionEntryView
	answered := map[string]bool{}
	closeAssistant := func() error {
		if assistant == nil {
			return nil
		}
		for _, call := range assistant.Assistant.ToolCalls {
			if !answered[call.ID] {
				return invalidModelStep("tool call %s of assistant entry %s has no tool entry", call.ID, assistant.ID)
			}
		}
		return nil
	}
	for i := range entries {
		entry := &entries[i]
		switch entry.Type {
		case models.SessionEntryAssistant:
			if err := closeAssistant(); err != nil {
				return err
			}
			assistant = entry
			answered = make(map[string]bool, len(entry.Assistant.ToolCalls))
		case models.SessionEntryTool:
			callID := entry.Tool.CallID
			if assistant == nil || !assistantHasCall(assistant.Assistant, callID) {
				return invalidModelStep("tool entry %s answers no call of the preceding assistant entry", entry.ID)
			}
			if answered[callID] {
				return invalidModelStep("tool entry %s answers call %s a second time", entry.ID, callID)
			}
			answered[callID] = true
		}
	}
	return closeAssistant()
}

func assistantHasCall(a *models.AssistantEntry, callID string) bool {
	for _, call := range a.ToolCalls {
		if call.ID == callID {
			return true
		}
	}
	return false
}

func (r ModelStepRequest) validateContent() error {
	for i := range r.Entries {
		entry := &r.Entries[i]
		var (
			parts  []models.ContentPart
			limits models.ContentPartLimits
		)
		switch entry.Type {
		case models.SessionEntryUser:
			parts = entry.User.Content
			limits = models.ContentPartLimits{Field: "user entry " + entry.ID + " content", MaxParts: models.MaxUserEntryParts, MaxContent: models.MaxUserEntryContentBytes, MaxFileParts: models.MaxUserEntryFileParts, AllowDataBytes: true}
		case models.SessionEntryTool:
			parts = entry.Tool.Result.Content
			limits = models.ContentPartLimits{Field: "tool entry " + entry.ID + " result", MaxParts: models.MaxToolResultParts, MaxContent: models.MaxToolResultContentBytes, AllowDataBytes: true}
		default:
			continue
		}
		if _, err := models.ValidateContentParts(parts, limits); err != nil {
			return invalidModelStep("%v", err)
		}
		for j, part := range parts {
			if err := validateModelStepMedia(part); err != nil {
				return invalidModelStep("%s part %d: %v", limits.Field, j, err)
			}
		}
	}
	return nil
}

// validateModelStepMedia applies the host media rules to a view part: only
// image parts carry bytes, and every view image carries 1 byte .. 1 MiB.
func validateModelStepMedia(part models.ContentPart) error {
	if part.Type != models.ContentPartImage {
		if part.DataBase64 != "" {
			return errors.New("only image parts carry dataBase64")
		}
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(part.DataBase64)
	if err != nil || len(decoded) == 0 {
		return errors.New("image needs base64 data")
	}
	if len(decoded) > maxModelStepMediaBytes {
		return fmt.Errorf("image is %d bytes (max %d)", len(decoded), maxModelStepMediaBytes)
	}
	return nil
}
