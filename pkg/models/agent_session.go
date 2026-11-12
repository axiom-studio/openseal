package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Agent session log (run-loop SPEC §2.1, section 01). A session is the
// append-only log one Agent's loop renders into provider messages. This file
// holds the shared types, validation, caps, deterministic ids, the transition
// table and the pure path builder; the SQLite and Postgres backends use
// these and never redeclare them.

type AgentSessionKind string

const (
	AgentSessionKindConversation AgentSessionKind = "conversation" // keyed by lane
	AgentSessionKindRun          AgentSessionKind = "run"          // owned by one Run (background/child/promoted/deterministic)
)

// MaxAgentSessionDepth bounds fork chains (chat -> task -> delegation -> review ...).
const MaxAgentSessionDepth = 8

// MaxAgentSessionPathEntries bounds one rendered path.
const MaxAgentSessionPathEntries = 5000

// Size caps. Encoded sizes are measured with json.Marshal.
const (
	// MaxToolResultContentBytes caps text + json bytes over a ToolResult's parts.
	// Image and file parts are references and do not count.
	MaxToolResultContentBytes = 64 << 10
	MaxToolResultParts        = 64
	MaxToolArgumentsBytes     = 256 << 10
	// MaxUserEntryContentBytes caps text + json bytes over a user entry's parts
	// (a 64 KiB channel message plus attribution/quote).
	MaxUserEntryContentBytes  = 128 << 10
	MaxUserEntryParts         = 64
	MaxUserEntryFileParts     = 32
	MaxSessionEntryImageParts = 8
	MaxNoteTextBytes          = 64 << 10
	MaxCompactionSummaryBytes = 64 << 10
	MaxSessionEntryBytes      = 2 << 20
	MaxSessionAppendBytes     = 8 << 20
	MaxSessionAppendEntries   = 128
)

var (
	ErrInvalidAgentSession        = errors.New("invalid agent session")
	ErrAgentSessionNotFound       = errors.New("agent session not found")
	ErrAgentSessionConflict       = errors.New("agent session conflict")
	ErrAgentSessionForkTooDeep    = errors.New("agent session fork chain is too deep")
	ErrInvalidSessionEntry        = errors.New("invalid agent session entry")
	ErrSessionEntryTooLarge       = errors.New("agent session entry is too large")
	ErrSessionEntryNotFound       = errors.New("agent session entry not found")
	ErrInvalidToolEntryTransition = errors.New("invalid tool entry transition")
	ErrToolEntryStateConflict     = errors.New("tool entry state changed")
	ErrToolEntryNotOwned          = errors.New("tool entry belongs to another run")
	ErrAgentSessionPathTooLarge   = errors.New("agent session path is too large")
	ErrSessionMoved               = errors.New("run is bound to another agent session")
	ErrChannelWatermarkMoved      = errors.New("agent session channel watermark moved")
)

type AgentSession struct {
	ID               string           `json:"id"`
	Scope            Scope            `json:"scope"`
	Kind             AgentSessionKind `json:"kind"`
	LaneKey          string           `json:"laneKey,omitempty"`        // ConversationRunLane(conv, threadRoot, agent); Kind conversation only
	ConversationID   string           `json:"conversationId,omitempty"` // conversation sessions and their forks
	ThreadRootID     string           `json:"threadRootId,omitempty"`
	AgentID          string           `json:"agentId"`              // the Agent whose context this is (Team member, not the Team)
	OwnerRunID       string           `json:"ownerRunId,omitempty"` // Kind run only
	ParentSessionID  string           `json:"parentSessionId,omitempty"`
	ForkEntryID      string           `json:"forkEntryId,omitempty"`  // last parent entry on the child's path ("" when ForkSequence is 0)
	ForkSequence     int64            `json:"forkSequence,omitempty"` // sequence of ForkEntryID in the parent
	RootSessionID    string           `json:"rootSessionId"`
	Depth            int              `json:"depth"`
	NextSequence     int64            `json:"nextSequence"`     // next entry sequence; leaf = NextSequence-1
	ChannelWatermark int64            `json:"channelWatermark"` // highest channel message sequence ingested
	ModelProvider    string           `json:"modelProvider,omitempty"`
	Model            string           `json:"model,omitempty"`
	Revision         int64            `json:"revision"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

func (s *AgentSession) Validate() error {
	if s == nil {
		return fmt.Errorf("%w: session is required", ErrInvalidAgentSession)
	}
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidAgentSession)
	}
	if err := s.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAgentSession, err)
	}
	if strings.TrimSpace(s.AgentID) == "" {
		return fmt.Errorf("%w: agentId is required", ErrInvalidAgentSession)
	}
	switch s.Kind {
	case AgentSessionKindConversation:
		if strings.TrimSpace(s.LaneKey) == "" || strings.TrimSpace(s.ConversationID) == "" {
			return fmt.Errorf("%w: conversation session needs laneKey and conversationId", ErrInvalidAgentSession)
		}
		if s.OwnerRunID != "" || s.ParentSessionID != "" {
			return fmt.Errorf("%w: conversation session has no owner run or parent", ErrInvalidAgentSession)
		}
	case AgentSessionKindRun:
		if strings.TrimSpace(s.OwnerRunID) == "" {
			return fmt.Errorf("%w: run session needs ownerRunId", ErrInvalidAgentSession)
		}
		if s.LaneKey != "" {
			return fmt.Errorf("%w: run session has no laneKey", ErrInvalidAgentSession)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidAgentSession, s.Kind)
	}
	if s.Depth < 0 {
		return fmt.Errorf("%w: negative depth", ErrInvalidAgentSession)
	}
	if s.Depth > MaxAgentSessionDepth {
		return ErrAgentSessionForkTooDeep
	}
	if strings.TrimSpace(s.RootSessionID) == "" {
		return fmt.Errorf("%w: rootSessionId is required", ErrInvalidAgentSession)
	}
	if s.ParentSessionID == "" {
		if s.Depth != 0 || s.RootSessionID != s.ID || s.ForkEntryID != "" || s.ForkSequence != 0 {
			return fmt.Errorf("%w: a parentless session is its own root at depth 0", ErrInvalidAgentSession)
		}
	} else {
		if s.ParentSessionID == s.ID || s.Depth < 1 || s.ForkSequence < 0 {
			return fmt.Errorf("%w: invalid fork", ErrInvalidAgentSession)
		}
		if (s.ForkSequence == 0) != (s.ForkEntryID == "") {
			return fmt.Errorf("%w: forkEntryId and forkSequence disagree", ErrInvalidAgentSession)
		}
	}
	if s.NextSequence < 1 || s.ChannelWatermark < 0 || s.Revision < 0 {
		return fmt.Errorf("%w: invalid counters", ErrInvalidAgentSession)
	}
	return nil
}

// ConversationAgentSessionID is the stable id of the session of a conversation lane.
func ConversationAgentSessionID(scope Scope, laneKey string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent-session:conversation:"+scope.Key()+":"+laneKey)).String()
}

// RunAgentSessionID is the stable id of the session owned by one Run.
func RunAgentSessionID(scope Scope, ownerRunID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent-session:run:"+scope.Key()+":"+ownerRunID)).String()
}

var agentSessionEntryNS = uuid.NewSHA1(uuid.NameSpaceOID, []byte("openseal.agent-session-entry"))

// SessionEntryID derives a deterministic entry id from the session and a
// logical key (SPEC §2.1 table). The same logical key in another session gives
// another id, so a forked or promoted session never collides with its parent.
func SessionEntryID(sessionID, logicalKey string) string {
	return uuid.NewSHA1(agentSessionEntryNS, []byte(sessionID+"\x00"+logicalKey)).String()
}

// Logical keys of the SPEC §2.1 table. The ones that are dedupe keys go in
// SessionEntry.SourceKey too.
func AssistantEntryKey(stepID string) string             { return "assistant:" + stepID }
func ToolEntryKey(stepID, toolCallID string) string      { return "tool:" + stepID + ":" + toolCallID }
func ChannelMessageSourceKey(messageID string) string    { return "message:" + messageID }
func InterventionSourceKey(interventionID string) string { return "intervention:" + interventionID }
func RunContextSourceKey(runID string) string            { return "run-context:" + runID }
func GoalSourceKey(ownerRunID string) string             { return "goal:" + ownerRunID }
func FailureNoteSourceKey(runID, stepID string) string   { return "failure-note:" + runID + ":" + stepID }
func FinalRejectionEntryKey(stepID string) string        { return "final-rejection:" + stepID }
func TaskPromotedSourceKey(runID string) string          { return "task-promoted:" + runID }
func RoutineEventEntrySourceKey(runID string) string     { return "routine-event:" + runID }

type SessionEntryType string

const (
	SessionEntryUser        SessionEntryType = "user"
	SessionEntryAssistant   SessionEntryType = "assistant"
	SessionEntryTool        SessionEntryType = "tool"
	SessionEntryCompaction  SessionEntryType = "compaction"
	SessionEntryNote        SessionEntryType = "note"
	SessionEntryModelChange SessionEntryType = "model_change"
)

func (t SessionEntryType) Valid() bool {
	switch t {
	case SessionEntryUser, SessionEntryAssistant, SessionEntryTool, SessionEntryCompaction, SessionEntryNote, SessionEntryModelChange:
		return true
	}
	return false
}

type ToolEntryState string

const (
	ToolEntryPending          ToolEntryState = "pending"
	ToolEntryAwaitingApproval ToolEntryState = "awaiting_approval"
	ToolEntryAwaitingChild    ToolEntryState = "awaiting_child"
	ToolEntryAwaitingEvent    ToolEntryState = "awaiting_event"
	ToolEntryRunning          ToolEntryState = "running"
	ToolEntryCompleted        ToolEntryState = "completed"
	ToolEntryError            ToolEntryState = "error"
	ToolEntryInterrupted      ToolEntryState = "interrupted"
)

// toolEntryStates lists every state, open ones first.
var toolEntryStates = []ToolEntryState{
	ToolEntryPending, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent,
	ToolEntryRunning, ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted,
}

// ToolEntryStateValues returns a fresh copy of every state, open ones first.
func ToolEntryStateValues() []ToolEntryState {
	return slices.Clone(toolEntryStates)
}

func (s ToolEntryState) Valid() bool {
	return slices.Contains(toolEntryStates, s)
}

// Terminal is completed, error or interrupted.
func (s ToolEntryState) Terminal() bool {
	return s == ToolEntryCompleted || s == ToolEntryError || s == ToolEntryInterrupted
}

// Open is a known, non-terminal state.
func (s ToolEntryState) Open() bool { return s.Valid() && !s.Terminal() }

func (s ToolEntryState) Awaiting() bool {
	return s == ToolEntryAwaitingApproval || s == ToolEntryAwaitingChild || s == ToolEntryAwaitingEvent
}

var toolEntryEdges = map[ToolEntryState][]ToolEntryState{
	ToolEntryPending: {ToolEntryRunning, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent,
		ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted},
	ToolEntryAwaitingApproval: {ToolEntryRunning, ToolEntryError, ToolEntryInterrupted},
	ToolEntryAwaitingChild:    {ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted},
	ToolEntryAwaitingEvent:    {ToolEntryRunning, ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted},
	ToolEntryRunning: {ToolEntryPending, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent,
		ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted},
}

// ValidToolEntryTransition is the tool entry edge table (section 01 §4.4 plus
// running→awaiting_approval, running→pending and awaiting_event→running).
func ValidToolEntryTransition(from, to ToolEntryState) bool {
	for _, next := range toolEntryEdges[from] {
		if next == to {
			return true
		}
	}
	return false
}

// SessionEntry is one row of agent_session_entries. Exactly one payload is set,
// matching Type.
type SessionEntry struct {
	ID           string           `json:"id"` // kernel-generated; the store never invents ids
	Scope        Scope            `json:"scope"`
	SessionID    string           `json:"sessionId"`
	Sequence     int64            `json:"sequence"` // assigned by the store
	RunID        string           `json:"runId"`
	StepID       string           `json:"stepId,omitempty"` // agent_turns.id; required for assistant and tool (unless a directive)
	Step         int64            `json:"step,omitempty"`   // agent_turns.sequence
	Type         SessionEntryType `json:"type"`
	State        ToolEntryState   `json:"state,omitempty"` // tool only
	ToolCallID   string           `json:"toolCallId,omitempty"`
	ActionCallID string           `json:"actionCallId,omitempty"`
	SourceKey    string           `json:"sourceKey,omitempty"` // idempotency key within the session
	Tokens       int64            `json:"tokens"`              // estimate; the store fills it when 0
	Revision     int64            `json:"revision"`            // 1 on append, +1 per transition
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`

	User        *UserEntry        `json:"user,omitempty"`
	Assistant   *AssistantEntry   `json:"assistant,omitempty"`
	Tool        *ToolEntry        `json:"tool,omitempty"`
	Compaction  *CompactionEntry  `json:"compaction,omitempty"`
	Note        *NoteEntry        `json:"note,omitempty"`
	ModelChange *ModelChangeEntry `json:"modelChange,omitempty"`
}

// ContentPart is the ONE content shape for user entries, tool results,
// host-tool results and step views. Persisted parts never carry DataBase64.
type ContentPart struct {
	Type  string          `json:"type"` // text | json | file | image
	Text  string          `json:"text,omitempty"`
	Value json.RawMessage `json:"value,omitempty"` // json
	// file (same JSON names as toolfile.Ref)
	Path      string `json:"path,omitempty"`
	MimeType  string `json:"mimeType,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	URI       string `json:"uri,omitempty"`
	Note      string `json:"note,omitempty"`
	// image: a reference; bytes are resolved by the kernel renderer
	MediaType       string `json:"mediaType,omitempty"` // image/png|jpeg|webp
	Detail          string `json:"detail,omitempty"`    // auto|low|high
	SHA256          string `json:"sha256,omitempty"`
	ArtifactID      string `json:"artifactId,omitempty"`
	ArtifactVersion string `json:"artifactVersion,omitempty"`
	ActionCallID    string `json:"actionCallId,omitempty"` // governed screenshot (output.modelMedia)
	DataBase64      string `json:"dataBase64,omitempty"`   // VIEW ONLY; rejected on persisted entries
}

const (
	ContentPartText  = "text"
	ContentPartJSON  = "json"
	ContentPartFile  = "file"
	ContentPartImage = "image"
)

type SessionAuthor struct {
	Type        string `json:"type"` // user | agent | service
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

type UserEntryOrigin string

const (
	UserEntryChannelMessage UserEntryOrigin = "channel_message" // catch-up / seed
	UserEntryIntervention   UserEntryOrigin = "intervention"    // steering
	UserEntryGoal           UserEntryOrigin = "goal"            // child / background session seed
)

type UserEntry struct {
	Origin           UserEntryOrigin `json:"origin"`
	GoalKind         string          `json:"goalKind,omitempty"` // goal only: task|fork_branch|decision|review|delegation|routine|routine_event
	Author           SessionAuthor   `json:"author"`
	Content          []ContentPart   `json:"content"` // first part is text (the formatted message)
	MessageID        string          `json:"messageId,omitempty"`
	ThreadRootID     string          `json:"threadRootId,omitempty"`
	ReplyToMessageID string          `json:"replyToMessageId,omitempty"`
	MessageSequence  int64           `json:"messageSequence,omitempty"`
	InterventionID   string          `json:"interventionId,omitempty"`
}

var userEntryGoalKinds = map[string]bool{
	"task": true, "fork_branch": true, "decision": true, "review": true, "delegation": true, "routine": true,
	"routine_event": true,
}

type ModelStepProvider string

const (
	ModelStepProviderAnthropic ModelStepProvider = "anthropic"        // Messages API
	ModelStepProviderResponses ModelStepProvider = "openai-responses" // Responses API
	ModelStepProviderChat      ModelStepProvider = "openai-chat"      // Chat Completions
)

func (p ModelStepProvider) Valid() bool {
	return p == ModelStepProviderAnthropic || p == ModelStepProviderResponses || p == ModelStepProviderChat
}

type ModelStopReason string

const (
	ModelStopToolCalls ModelStopReason = "tool_calls"
	ModelStopEnd       ModelStopReason = "end"
	ModelStopRefusal   ModelStopReason = "refusal"
	ModelStopMaxTokens ModelStopReason = "max_tokens"
)

func (r ModelStopReason) Valid() bool {
	return r == ModelStopToolCalls || r == ModelStopEnd || r == ModelStopRefusal || r == ModelStopMaxTokens
}

type ModelToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // always a JSON object
	// ArgumentsError is set when the provider's arguments were not a JSON
	// object after the in-step retry; Arguments is then {}.
	ArgumentsError string `json:"argumentsError,omitempty"`
}

// AssistantNative is the provider-native content, verbatim. It is never
// projected out of the kernel (D2).
type AssistantNative struct {
	Anthropic          []json.RawMessage `json:"anthropic,omitempty"`
	Responses          []json.RawMessage `json:"responses,omitempty"`
	ChatReasoningField string            `json:"chatReasoningField,omitempty"`
	ChatReasoning      string            `json:"chatReasoning,omitempty"`
}

type ModelStepUsage struct {
	InputTokens        int64   `json:"inputTokens"`
	OutputTokens       int64   `json:"outputTokens"`
	ReasoningTokens    int64   `json:"reasoningTokens,omitempty"`
	CacheReadTokens    int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens   int64   `json:"cacheWriteTokens,omitempty"`
	Cost               float64 `json:"cost,omitempty"`
	DurationMS         int64   `json:"durationMs"`
	ProviderDurationMS int64   `json:"providerDurationMs"`
	FirstOutputMS      int64   `json:"firstOutputMs,omitempty"`
	Attempts           int     `json:"attempts"`
}

type AssistantModelLimits struct {
	ContextTokens    int64 `json:"contextTokens,omitempty"`
	InputLimitTokens int64 `json:"inputLimitTokens,omitempty"`
	PromptTokens     int64 `json:"promptTokens,omitempty"`
}

// AssistantEntry is the assistant entry payload and the ModelStepResult.Assistant type.
type AssistantEntry struct {
	Provider     ModelStepProvider     `json:"provider"`
	Model        string                `json:"model"`
	Gateway      bool                  `json:"gateway,omitempty"`
	SystemDigest string                `json:"systemDigest"`
	ToolsDigest  string                `json:"toolsDigest"`
	Text         string                `json:"text,omitempty"`
	ToolCalls    []ModelToolCall       `json:"toolCalls,omitempty"`
	Native       *AssistantNative      `json:"native,omitempty"`
	StopReason   ModelStopReason       `json:"stopReason"`
	Usage        ModelStepUsage        `json:"usage"`
	Limits       *AssistantModelLimits `json:"limits,omitempty"` // kernel-filled; stripped from views
}

type ToolSideEffect string

const (
	ToolSideEffectNone  ToolSideEffect = "none"
	ToolSideEffectRead  ToolSideEffect = "read"
	ToolSideEffectWrite ToolSideEffect = "write"
)

func (s ToolSideEffect) Valid() bool {
	return s == ToolSideEffectNone || s == ToolSideEffectRead || s == ToolSideEffectWrite
}

type ToolEntryWait struct {
	ApprovalID     string     `json:"approvalId,omitempty"`
	GroupID        string     `json:"groupId,omitempty"`
	AgentRequestID string     `json:"agentRequestId,omitempty"`
	EventWaitID    string     `json:"eventWaitId,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	ChildRunIDs    []string   `json:"childRunIds,omitempty"`
	RetryAt        *time.Time `json:"retryAt,omitempty"` // skill maintenance park
}

// ToolGuardOutcome Kind: repeat_note | repeat_refused | duplicate_suppressed | error_streak.
type ToolGuardOutcome struct {
	Kind         string `json:"kind"`
	Count        int    `json:"count,omitempty"`
	PriorEntryID string `json:"priorEntryId,omitempty"`
}

type ToolEntry struct {
	Name               string            `json:"name"`
	Arguments          json.RawMessage   `json:"arguments"`
	ArgumentsError     string            `json:"argumentsError,omitempty"`
	SideEffect         ToolSideEffect    `json:"sideEffect"`
	ArgumentsDigest    string            `json:"argumentsDigest,omitempty"`
	RepeatEffect       string            `json:"repeatEffect,omitempty"`
	ResultDigest       string            `json:"resultDigest,omitempty"`
	Parked             bool              `json:"parked,omitempty"`
	Guard              *ToolGuardOutcome `json:"guard,omitempty"`
	DuplicateOfEntryID string            `json:"duplicateOfEntryId,omitempty"`
	Wait               *ToolEntryWait    `json:"wait,omitempty"`   // set only in awaiting_* states
	Result             *ToolResult       `json:"result,omitempty"` // set only in terminal states
	Control            json.RawMessage   `json:"control,omitempty"`
	// SurfacedTools (load_capabilities only) are the definitions of the tools
	// the load made callable, frozen as the model first saw them: later
	// steps of the session declare exactly these bytes, whatever the catalog
	// says now.
	SurfacedTools []SurfacedTool `json:"surfacedTools,omitempty"`
	// LoadedInstructions (load_capabilities only) are the canonical IDs of
	// the instructions and runbooks whose text the load result carried.
	LoadedInstructions []string        `json:"loadedInstructions,omitempty"`
	OriginRef          string          `json:"originRef,omitempty"` // "command:<entryId>"
	PinnedRun          json.RawMessage `json:"pinnedRun,omitempty"`
	Directive          json.RawMessage `json:"directive,omitempty"` // ToolCallDirective (deterministic producers)
	StartedAt          *time.Time      `json:"startedAt,omitempty"`
	CompletedAt        *time.Time      `json:"completedAt,omitempty"`
}

type ToolResultStatus string

const (
	ToolResultSucceeded           ToolResultStatus = "succeeded"
	ToolResultFailed              ToolResultStatus = "failed"
	ToolResultDenied              ToolResultStatus = "denied"
	ToolResultChangesRequested    ToolResultStatus = "changes_requested"
	ToolResultSuperseded          ToolResultStatus = "superseded"
	ToolResultExpired             ToolResultStatus = "expired"
	ToolResultRefused             ToolResultStatus = "refused"
	ToolResultDuplicateSuppressed ToolResultStatus = "duplicate_suppressed"
	ToolResultInterrupted         ToolResultStatus = "interrupted"
	ToolResultCanceled            ToolResultStatus = "canceled"
)

func (s ToolResultStatus) Valid() bool {
	switch s {
	case ToolResultSucceeded, ToolResultFailed, ToolResultDenied, ToolResultChangesRequested, ToolResultSuperseded,
		ToolResultExpired, ToolResultRefused, ToolResultDuplicateSuppressed, ToolResultInterrupted, ToolResultCanceled:
		return true
	}
	return false
}

type ToolResult struct {
	Status  ToolResultStatus `json:"status"`
	IsError bool             `json:"isError"` // must equal Status != succeeded
	Content []ContentPart    `json:"content"`
}

type CompactionEntry struct {
	Summary          string   `json:"summary"`
	FirstKeptEntryID string   `json:"firstKeptEntryId,omitempty"` // "" = keep nothing
	TokensBefore     int64    `json:"tokensBefore"`
	ActionCallIDs    []string `json:"actionCallIds,omitempty"`
	// CarriedTools are the surfaced tools whose load entries this compaction
	// summarized away (with the previous compaction's carried tools first),
	// in their original order. Later steps declare them as loaded tools
	// callable from the start, so a compaction never withdraws a tool.
	CarriedTools []SurfacedTool `json:"carriedTools,omitempty"`
	Provider     string         `json:"provider"`
	Model        string         `json:"model"`
	Trigger      string         `json:"trigger"` // threshold | overflow
	Usage        ModelStepUsage `json:"usage"`
}

const (
	CompactionTriggerThreshold = "threshold"
	CompactionTriggerOverflow  = "overflow"
)

type NoteKind string

const (
	NoteRunContext         NoteKind = "run_context"
	NoteRepeat             NoteKind = "repeat"
	NoteHost               NoteKind = "host"
	NoteFailureExplanation NoteKind = "failure_explanation"
	NoteRunFailed          NoteKind = "run_failed"
	NoteTaskPromoted       NoteKind = "task_promoted"
	NoteFinalRejection     NoteKind = "final_rejection"
	NoteSetupPending       NoteKind = "setup_pending"
	NoteHistoryOmitted     NoteKind = "history_omitted"
	NoteRateLimitedSource  NoteKind = "rate_limited_source"
	NoteBrowserWaitingCode NoteKind = "browser_waiting_code"
	// NoteCapabilities lists what the Agent can load and use: the catalog
	// (or its directory), delegation targets, operations and the guidance
	// that depends on them. Appended in full once per compaction window, then
	// as deltas when the catalog changes (pkg/runtime/capability_note.go).
	NoteCapabilities NoteKind = "capabilities"
)

func (k NoteKind) Valid() bool {
	switch k {
	case NoteRunContext, NoteRepeat, NoteHost, NoteFailureExplanation, NoteRunFailed, NoteTaskPromoted,
		NoteFinalRejection, NoteSetupPending, NoteHistoryOmitted, NoteRateLimitedSource,
		NoteBrowserWaitingCode, NoteCapabilities:
		return true
	}
	return false
}

type NoteEntry struct {
	Kind NoteKind        `json:"kind"`
	Text string          `json:"text"`
	Data json.RawMessage `json:"data,omitempty"`
}

type ModelChangeEntry struct {
	FromProvider string `json:"fromProvider,omitempty"`
	FromModel    string `json:"fromModel,omitempty"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
}
