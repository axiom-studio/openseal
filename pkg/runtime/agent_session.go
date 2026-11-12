package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent-session:conversation:"+scope.key()+":"+laneKey)).String()
}

// RunAgentSessionID is the stable id of the session owned by one Run.
func RunAgentSessionID(scope Scope, ownerRunID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent-session:run:"+scope.key()+":"+ownerRunID)).String()
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

func (t SessionEntryType) valid() bool {
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

// ToolEntryStates lists every state, open ones first.
var ToolEntryStates = []ToolEntryState{
	ToolEntryPending, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent,
	ToolEntryRunning, ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted,
}

// OpenToolEntryStates lists the non-terminal states (D14 resume filter).
var OpenToolEntryStates = []ToolEntryState{
	ToolEntryPending, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent, ToolEntryRunning,
}

func (s ToolEntryState) Valid() bool {
	for _, known := range ToolEntryStates {
		if s == known {
			return true
		}
	}
	return false
}

// Terminal is completed, error or interrupted.
func (s ToolEntryState) Terminal() bool {
	return s == ToolEntryCompleted || s == ToolEntryError || s == ToolEntryInterrupted
}

// Open is a known, non-terminal state.
func (s ToolEntryState) Open() bool { return s.Valid() && !s.Terminal() }

func (s ToolEntryState) awaiting() bool {
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
	// as deltas when the catalog changes (capability_note.go).
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

// ---------------------------------------------------------------------------
// Validation

func invalidEntry(format string, args ...interface{}) error {
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
// fences, sequences or the batch rules (validateAppendAgentSessionEntries).
func (e *SessionEntry) Validate() error {
	if e == nil {
		return invalidEntry("entry is required")
	}
	if strings.TrimSpace(e.ID) == "" {
		return invalidEntry("id is required")
	}
	if !e.Type.valid() {
		return invalidEntry("unknown type %q", e.Type)
	}
	if e.payloadCount() != 1 {
		return invalidEntry("entry %s needs exactly one payload", e.ID)
	}
	if e.Sequence < 0 || e.Step < 0 || e.Tokens < 0 || e.Revision < 0 {
		return invalidEntry("entry %s has negative counters", e.ID)
	}
	if e.Type != SessionEntryTool && (e.State != "" || e.ToolCallID != "") {
		return invalidEntry("entry %s: state and toolCallId are for tool entries", e.ID)
	}
	var err error
	switch e.Type {
	case SessionEntryUser:
		if e.User == nil {
			return invalidEntry("user entry %s has no user payload", e.ID)
		}
		err = e.User.validate()
	case SessionEntryAssistant:
		if e.Assistant == nil {
			return invalidEntry("assistant entry %s has no assistant payload", e.ID)
		}
		if strings.TrimSpace(e.StepID) == "" {
			return invalidEntry("assistant entry %s needs stepId", e.ID)
		}
		err = e.Assistant.validate()
	case SessionEntryTool:
		if e.Tool == nil {
			return invalidEntry("tool entry %s has no tool payload", e.ID)
		}
		err = e.validateTool()
	case SessionEntryCompaction:
		if e.Compaction == nil {
			return invalidEntry("compaction entry %s has no compaction payload", e.ID)
		}
		err = e.Compaction.validate(e.ID)
	case SessionEntryNote:
		if e.Note == nil {
			return invalidEntry("note entry %s has no note payload", e.ID)
		}
		err = e.Note.validate()
	case SessionEntryModelChange:
		if e.ModelChange == nil {
			return invalidEntry("model_change entry %s has no model_change payload", e.ID)
		}
		if strings.TrimSpace(e.ModelChange.Provider) == "" || strings.TrimSpace(e.ModelChange.Model) == "" {
			err = invalidEntry("model_change needs provider and model")
		}
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return invalidEntry("entry %s does not encode: %v", e.ID, err)
	}
	if len(encoded) > MaxSessionEntryBytes {
		return entryTooLarge("entry %s is %d bytes (max %d)", e.ID, len(encoded), MaxSessionEntryBytes)
	}
	return nil
}

type contentPartLimits struct {
	field          string
	maxParts       int
	maxContent     int // text + json bytes
	maxFileParts   int // 0 = no extra file cap
	allowDataBytes bool
}

// validateContentParts checks part shapes and caps. Image and file parts are
// references and do not count toward maxContent.
func validateContentParts(parts []ContentPart, limits contentPartLimits) (images int, err error) {
	if limits.maxParts > 0 && len(parts) > limits.maxParts {
		return 0, entryTooLarge("%s has %d parts (max %d)", limits.field, len(parts), limits.maxParts)
	}
	contentBytes, files := 0, 0
	for i, part := range parts {
		if part.DataBase64 != "" && !limits.allowDataBytes {
			return 0, invalidEntry("%s part %d carries dataBase64; persisted parts are references", limits.field, i)
		}
		switch part.Type {
		case ContentPartText:
			contentBytes += len(part.Text)
		case ContentPartJSON:
			if len(part.Value) == 0 || !json.Valid(part.Value) {
				return 0, invalidEntry("%s part %d: json part needs a valid value", limits.field, i)
			}
			contentBytes += jsonPartBytes(part.Value)
		case ContentPartFile:
			if part.Path == "" && part.URI == "" && part.Note == "" {
				return 0, invalidEntry("%s part %d: file part needs path, uri or note", limits.field, i)
			}
			if part.SizeBytes < 0 {
				return 0, invalidEntry("%s part %d: negative size", limits.field, i)
			}
			files++
		case ContentPartImage:
			switch part.MediaType {
			case "image/png", "image/jpeg", "image/webp":
			default:
				return 0, invalidEntry("%s part %d: unsupported image media type %q", limits.field, i, part.MediaType)
			}
			switch part.Detail {
			case "", "auto", "low", "high":
			default:
				return 0, invalidEntry("%s part %d: unknown image detail %q", limits.field, i, part.Detail)
			}
			if part.ArtifactID == "" && part.ActionCallID == "" && !(limits.allowDataBytes && part.DataBase64 != "") {
				return 0, invalidEntry("%s part %d: image part needs artifactId or actionCallId", limits.field, i)
			}
			if part.ArtifactID != "" && part.ActionCallID != "" {
				return 0, invalidEntry("%s part %d: image part names both an artifact and an action call; it needs exactly one source", limits.field, i)
			}
			images++
		default:
			return 0, invalidEntry("%s part %d: unknown part type %q", limits.field, i, part.Type)
		}
	}
	if limits.maxContent > 0 && contentBytes > limits.maxContent {
		return 0, entryTooLarge("%s text and json are %d bytes (max %d)", limits.field, contentBytes, limits.maxContent)
	}
	if limits.maxFileParts > 0 && files > limits.maxFileParts {
		return 0, entryTooLarge("%s has %d file parts (max %d)", limits.field, files, limits.maxFileParts)
	}
	if images > MaxSessionEntryImageParts {
		return 0, entryTooLarge("%s has %d image parts (max %d)", limits.field, images, MaxSessionEntryImageParts)
	}
	return images, nil
}

func (u *UserEntry) validate() error {
	switch u.Origin {
	case UserEntryChannelMessage:
		if strings.TrimSpace(u.MessageID) == "" {
			return invalidEntry("channel message entry needs messageId")
		}
	case UserEntryIntervention:
		if strings.TrimSpace(u.InterventionID) == "" && strings.TrimSpace(u.MessageID) == "" {
			return invalidEntry("intervention entry needs interventionId or messageId")
		}
	case UserEntryGoal:
	default:
		return invalidEntry("unknown user entry origin %q", u.Origin)
	}
	if u.GoalKind != "" && (u.Origin != UserEntryGoal || !userEntryGoalKinds[u.GoalKind]) {
		return invalidEntry("invalid goal kind %q", u.GoalKind)
	}
	switch u.Author.Type {
	case "user", "agent", "service":
	default:
		return invalidEntry("unknown author type %q", u.Author.Type)
	}
	if len(u.Content) == 0 || u.Content[0].Type != ContentPartText {
		return invalidEntry("user entry content starts with a text part")
	}
	if u.MessageSequence < 0 {
		return invalidEntry("negative message sequence")
	}
	_, err := validateContentParts(u.Content, contentPartLimits{
		field: "user content", maxParts: MaxUserEntryParts, maxContent: MaxUserEntryContentBytes, maxFileParts: MaxUserEntryFileParts,
	})
	return err
}

func validateToolArguments(field string, args json.RawMessage) error {
	if len(args) > MaxToolArgumentsBytes {
		return entryTooLarge("%s are %d bytes (max %d)", field, len(args), MaxToolArgumentsBytes)
	}
	if len(args) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return invalidEntry("%s must be a JSON object", field)
	}
	return nil
}

func (a *AssistantEntry) validate() error {
	if !a.Provider.Valid() {
		return invalidEntry("unknown assistant provider %q", a.Provider)
	}
	if strings.TrimSpace(a.Model) == "" {
		return invalidEntry("assistant entry needs model")
	}
	if !a.StopReason.Valid() {
		return invalidEntry("unknown stop reason %q", a.StopReason)
	}
	seen := make(map[string]bool, len(a.ToolCalls))
	for i, call := range a.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return invalidEntry("tool call %d needs id and name", i)
		}
		if seen[call.ID] {
			return invalidEntry("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = true
		if err := validateToolArguments("tool call "+call.ID+" arguments", call.Arguments); err != nil {
			return err
		}
	}
	if a.Native != nil {
		for i, raw := range a.Native.Anthropic {
			if !json.Valid(raw) {
				return invalidEntry("native anthropic block %d is not JSON", i)
			}
		}
		for i, raw := range a.Native.Responses {
			if !json.Valid(raw) {
				return invalidEntry("native responses item %d is not JSON", i)
			}
		}
	}
	return nil
}

func (e *SessionEntry) validateTool() error {
	t := e.Tool
	if !e.State.Valid() {
		return invalidEntry("tool entry %s has unknown state %q", e.ID, e.State)
	}
	if strings.TrimSpace(e.ToolCallID) == "" {
		return invalidEntry("tool entry %s needs toolCallId", e.ID)
	}
	if strings.TrimSpace(e.StepID) == "" {
		return invalidEntry("tool entry %s needs stepId", e.ID)
	}
	if strings.TrimSpace(t.Name) == "" {
		return invalidEntry("tool entry %s needs name", e.ID)
	}
	if !t.SideEffect.Valid() {
		return invalidEntry("tool entry %s has unknown side effect %q", e.ID, t.SideEffect)
	}
	if t.ArgumentsError == "" {
		if err := validateToolArguments("tool entry arguments", t.Arguments); err != nil {
			return err
		}
	} else if len(t.Arguments) > MaxToolArgumentsBytes {
		return entryTooLarge("tool entry arguments are %d bytes (max %d)", len(t.Arguments), MaxToolArgumentsBytes)
	}
	for field, raw := range map[string]json.RawMessage{"control": t.Control, "pinnedRun": t.PinnedRun, "directive": t.Directive} {
		if len(raw) > 0 && !json.Valid(raw) {
			return invalidEntry("tool entry %s %s is not JSON", e.ID, field)
		}
	}
	if (t.Wait != nil) != e.State.awaiting() {
		return invalidEntry("tool entry %s: wait is set exactly in awaiting states", e.ID)
	}
	if (t.Result != nil) != e.State.Terminal() {
		return invalidEntry("tool entry %s: result is set exactly in terminal states", e.ID)
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
	_, err := validateContentParts(result.Content, contentPartLimits{
		field: "tool result", maxParts: MaxToolResultParts, maxContent: MaxToolResultContentBytes,
	})
	return err
}

func (c *CompactionEntry) validate(entryID string) error {
	if strings.TrimSpace(c.Summary) == "" {
		return invalidEntry("compaction needs a summary")
	}
	if len(c.Summary) > MaxCompactionSummaryBytes {
		return entryTooLarge("compaction summary is %d bytes (max %d)", len(c.Summary), MaxCompactionSummaryBytes)
	}
	if c.FirstKeptEntryID == entryID {
		return invalidEntry("compaction cannot keep itself")
	}
	if c.Trigger != CompactionTriggerThreshold && c.Trigger != CompactionTriggerOverflow {
		return invalidEntry("unknown compaction trigger %q", c.Trigger)
	}
	if c.TokensBefore < 0 {
		return invalidEntry("negative tokensBefore")
	}
	return nil
}

func (n *NoteEntry) validate() error {
	if !n.Kind.Valid() {
		return invalidEntry("unknown note kind %q", n.Kind)
	}
	if strings.TrimSpace(n.Text) == "" {
		return invalidEntry("note needs text")
	}
	if len(n.Text) > MaxNoteTextBytes {
		return entryTooLarge("note text is %d bytes (max %d)", len(n.Text), MaxNoteTextBytes)
	}
	if len(n.Data) > 0 && !json.Valid(n.Data) {
		return invalidEntry("note data is not JSON")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tokens and clones

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

// jsonPartBytes is what a json part counts toward a content cap: the size of
// its compact encoding. A store can hand a value back re-encoded (Postgres
// jsonb puts a space after every ':' and ','), so the length of a value as
// read is not the size that was written and checked; counting the compact
// form keeps a result that fit when it was written fitting when it is read.
func jsonPartBytes(raw json.RawMessage) int {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return len(raw)
	}
	return out.Len()
}

// compactJSONValue is raw in its compact encoding, or a copy of raw when it is
// not valid json.
func compactJSONValue(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return cloneRaw(raw)
	}
	return json.RawMessage(out.Bytes())
}

func cloneRaw(in json.RawMessage) json.RawMessage {
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

func cloneContentParts(in []ContentPart) []ContentPart {
	if in == nil {
		return nil
	}
	out := make([]ContentPart, len(in))
	for i, part := range in {
		part.Value = cloneRaw(part.Value)
		out[i] = part
	}
	return out
}

func cloneToolResult(in *ToolResult) *ToolResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Content = cloneContentParts(in.Content)
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

func cloneAgentSession(in *AgentSession) *AgentSession {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneSessionEntry(in *SessionEntry) *SessionEntry {
	if in == nil {
		return nil
	}
	out := *in
	if in.User != nil {
		user := *in.User
		user.Content = cloneContentParts(in.User.Content)
		out.User = &user
	}
	if in.Assistant != nil {
		assistant := *in.Assistant
		if in.Assistant.ToolCalls != nil {
			assistant.ToolCalls = make([]ModelToolCall, len(in.Assistant.ToolCalls))
			for i, call := range in.Assistant.ToolCalls {
				call.Arguments = cloneRaw(call.Arguments)
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
		tool.Arguments = cloneRaw(in.Tool.Arguments)
		tool.Guard = cloneToolGuardOutcome(in.Tool.Guard)
		tool.Wait = cloneToolEntryWait(in.Tool.Wait)
		tool.Result = cloneToolResult(in.Tool.Result)
		tool.Control = cloneRaw(in.Tool.Control)
		tool.SurfacedTools = cloneSurfacedTools(in.Tool.SurfacedTools)
		tool.LoadedInstructions = cloneStrings(in.Tool.LoadedInstructions)
		tool.PinnedRun = cloneRaw(in.Tool.PinnedRun)
		tool.Directive = cloneRaw(in.Tool.Directive)
		tool.StartedAt = cloneTimePtr(in.Tool.StartedAt)
		tool.CompletedAt = cloneTimePtr(in.Tool.CompletedAt)
		out.Tool = &tool
	}
	if in.Compaction != nil {
		compaction := *in.Compaction
		compaction.ActionCallIDs = cloneStrings(in.Compaction.ActionCallIDs)
		compaction.CarriedTools = cloneSurfacedTools(in.Compaction.CarriedTools)
		out.Compaction = &compaction
	}
	if in.Note != nil {
		note := *in.Note
		note.Data = cloneRaw(in.Note.Data)
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
		out[i] = cloneRaw(raw)
	}
	return out
}

// ---------------------------------------------------------------------------
// Store contract

type AgentSessionStore interface {
	// OpenConversationAgentSession gets or creates the lane session; it checks
	// run.SessionID == the derived id (ErrSessionMoved otherwise).
	OpenConversationAgentSession(ctx context.Context, req OpenConversationAgentSessionRequest) (*OpenAgentSessionResult, error)
	// CreateRunAgentSession creates a parentless run session at first claim.
	CreateRunAgentSession(ctx context.Context, req CreateRunAgentSessionRequest) (*OpenAgentSessionResult, error)
	GetAgentSession(ctx context.Context, scope Scope, sessionID string) (*AgentSession, error)
	GetSessionEntry(ctx context.Context, scope Scope, entryID string) (*SessionEntry, error)
	AppendAgentSessionEntries(ctx context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error)
	TransitionToolEntry(ctx context.Context, req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error)
	LoadAgentSessionPath(ctx context.Context, scope Scope, sessionID string) (*AgentSessionPath, error)
	ListAgentSessionEntries(ctx context.Context, filter SessionEntryFilter) ([]*SessionEntry, error)
}

type OpenConversationAgentSessionRequest struct {
	Scope          Scope
	ConversationID string
	ThreadRootID   string // "" unless the lane is a thread lane
	AgentID        string // assigned Agent (Team member)
	RunID          string // must be bound to this session (run.SessionID)
	Lease          AgentRunLeaseGuard
	Now            time.Time
}

type OpenAgentSessionResult struct {
	Session *AgentSession
	Created bool // the session is new; the caller seeds it
}

type CreateRunAgentSessionRequest struct {
	Scope          Scope
	OwnerRunID     string // fenced by its lease; its SessionID is set to the new session
	AgentID        string
	ConversationID string // copied for attribution (optional)
	ThreadRootID   string
	Lease          AgentRunLeaseGuard
	// InitialEntries (e.g. the goal as a user entry with Origin goal). RunID of
	// each must be OwnerRunID. Capped as an append.
	InitialEntries []*SessionEntry
	Now            time.Time
}

// CreateRunAgentSessionTxRequest is the input of the backends' in-transaction
// createRunAgentSessionTx helpers (dependency-group, AgentRequest and
// ConversationTask create, promotion). ForkSequence <= parent.NextSequence-1.
type CreateRunAgentSessionTxRequest struct {
	Scope           Scope
	OwnerRunID      string
	AgentID         string
	ConversationID  string
	ThreadRootID    string
	ParentSessionID string
	ForkSequence    int64
	InitialEntries  []*SessionEntry
	Now             time.Time
}

type AppendAgentSessionEntriesRequest struct {
	Scope     Scope
	SessionID string
	RunID     string
	// Lease is nil only when the Run is terminal and every entry is a note (run_failed).
	Lease *AgentRunLeaseGuard
	// Entries: 0..128; empty only with ConsumeInterventionsThrough or a watermark.
	// RunID/Scope/SessionID of an entry may be empty (filled) or must match.
	Entries                     []*SessionEntry
	ExpectedChannelWatermark    *int64 // CAS; mismatch -> ErrChannelWatermarkMoved
	ChannelWatermark            int64  // new value when ExpectedChannelWatermark != nil
	ConsumeInterventionsThrough *int   // sets AgentRun.ConsumedInterventions in the same tx (>= current, <= len)
	Events                      []*ActivityEvent
	Now                         time.Time
}

type AppendAgentSessionEntriesResult struct {
	Session  *AgentSession   // after the append
	Entries  []*SessionEntry // persisted entries in request order, skipped ones omitted
	Skipped  []string        // ids of request entries skipped because their SourceKey already exists
	Replayed bool            // the whole batch was already stored (same ids); nothing written
	Events   []*ActivityEvent
}

type ToolEntryTransitionRequest struct {
	Scope   Scope
	EntryID string
	RunID   string // must equal entry.RunID
	// Lease fences the transition by the Run lease. A zero Lease (no
	// WorkerID) is the terminal close (loop-16): allowed only to error or
	// interrupted, and only while the Run is terminal (ErrLeaseLost otherwise).
	Lease            AgentRunLeaseGuard
	From             ToolEntryState
	ExpectedRevision int64 // 0 = CAS on state only
	To               ToolEntryState
	ActionCallID     string         // set once; must match an existing one
	Wait             *ToolEntryWait // required for awaiting_*; nil otherwise
	Result           *ToolResult    // required for terminal states; nil otherwise
	Tokens           int64          // 0 = re-estimate when Result is set
	Kernel           *ToolEntryKernelPatch
	Events           []*ActivityEvent
	Now              time.Time
}

// ToolEntryKernelPatch sets kernel fields of a tool entry on a transition.
// Empty strings and nil values leave a field unchanged.
type ToolEntryKernelPatch struct {
	ResultDigest       string
	OriginRef          string
	DuplicateOfEntryID string
	Parked             *bool
	Guard              *ToolGuardOutcome
	Control            json.RawMessage
	SurfacedTools      []SurfacedTool
	LoadedInstructions []string
	PinnedRun          json.RawMessage
}

type ToolEntryTransitionResult struct {
	Entry  *SessionEntry
	Events []*ActivityEvent
}

type SessionEntryFilter struct {
	Scope           Scope
	SessionID       string // one of SessionID, RunID, ActionCallID, StepID is required
	RunID           string
	ActionCallID    string
	StepID          string
	Types           []SessionEntryType
	States          []ToolEntryState // implies type tool
	ToolNames       []string         // implies type tool
	NoteKinds       []NoteKind       // implies type note
	AfterSequence   int64            // only with SessionID
	AfterCompaction bool             // only with SessionID: entries after the latest compaction of that session
	Descending      bool
	Limit           int // default 200, max 1000
}

const (
	defaultSessionEntryListLimit = 200
	maxSessionEntryListLimit     = 1000
)

type AgentSessionPath struct {
	Session    *AgentSession
	Chain      []*AgentSession // root ... leaf (len = Depth+1)
	Compaction *SessionEntry   // latest compaction on the path, nil if none
	Entries    []*SessionEntry // render order; never contains compaction entries
	Tokens     int64           // Compaction.Tokens + sum(Entries.Tokens)
}

// ---------------------------------------------------------------------------
// Request validation shared by every backend

// validateAppendAgentSessionEntries checks an append request before the
// transaction. In-transaction checks (lease, session binding, replay, source
// keys, compaction cut against the path) are the backend's.
func validateAppendAgentSessionEntries(req AppendAgentSessionEntriesRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSessionEntry, err)
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.RunID) == "" {
		return invalidEntry("append needs sessionId and runId")
	}
	if len(req.Entries) > MaxSessionAppendEntries {
		return entryTooLarge("append has %d entries (max %d)", len(req.Entries), MaxSessionAppendEntries)
	}
	if len(req.Entries) == 0 && req.ConsumeInterventionsThrough == nil && req.ExpectedChannelWatermark == nil {
		return invalidEntry("an empty append needs a cursor or a watermark")
	}
	if req.ConsumeInterventionsThrough != nil && *req.ConsumeInterventionsThrough < 0 {
		return invalidEntry("negative intervention cursor")
	}
	if req.ExpectedChannelWatermark != nil && (*req.ExpectedChannelWatermark < 0 || req.ChannelWatermark < *req.ExpectedChannelWatermark) {
		return invalidEntry("channel watermark must not move backwards")
	}
	if req.Lease == nil {
		if len(req.Entries) == 0 || req.ConsumeInterventionsThrough != nil || req.ExpectedChannelWatermark != nil {
			return invalidEntry("a lease-less append holds only notes")
		}
		for _, entry := range req.Entries {
			if entry == nil || entry.Type != SessionEntryNote {
				return invalidEntry("a lease-less append holds only notes")
			}
		}
	} else if strings.TrimSpace(req.Lease.WorkerID) == "" {
		return invalidEntry("lease needs a worker id")
	}

	ids := make(map[string]bool, len(req.Entries))
	sourceKeys := make(map[string]bool)
	// assistant call ids by step, and the tool entries seen for each
	type stepCalls struct {
		calls map[string]bool
		seen  map[string]bool
	}
	steps := make(map[string]*stepCalls)
	// tool call ids of Directive (deterministic) tool entries by step
	directiveSteps := make(map[string]map[string]bool)
	total := 0
	for i, entry := range req.Entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if (entry.Scope != Scope{}) && entry.Scope != req.Scope {
			return invalidEntry("entry %s belongs to another scope", entry.ID)
		}
		if entry.SessionID != "" && entry.SessionID != req.SessionID {
			return invalidEntry("entry %s belongs to another session", entry.ID)
		}
		if entry.RunID != "" && entry.RunID != req.RunID {
			return invalidEntry("entry %s belongs to another run", entry.ID)
		}
		if ids[entry.ID] {
			return invalidEntry("duplicate entry id %s in batch", entry.ID)
		}
		ids[entry.ID] = true
		if entry.SourceKey != "" {
			if sourceKeys[entry.SourceKey] {
				return invalidEntry("duplicate source key %s in batch", entry.SourceKey)
			}
			sourceKeys[entry.SourceKey] = true
		}
		switch entry.Type {
		case SessionEntryAssistant:
			if _, dup := steps[entry.StepID]; dup {
				return invalidEntry("two assistant entries for step %s in batch", entry.StepID)
			}
			if _, mixed := directiveSteps[entry.StepID]; mixed {
				return invalidEntry("step %s has both directive tool entries and an assistant entry", entry.StepID)
			}
			calls := &stepCalls{calls: map[string]bool{}, seen: map[string]bool{}}
			for _, call := range entry.Assistant.ToolCalls {
				calls.calls[call.ID] = true
			}
			steps[entry.StepID] = calls
		case SessionEntryTool:
			switch entry.State {
			case ToolEntryPending, ToolEntryCompleted, ToolEntryError:
			default:
				return invalidEntry("tool entry %s is appended as pending, completed or error, not %s", entry.ID, entry.State)
			}
			if len(entry.Tool.Directive) > 0 {
				// Deterministic producer: its step has no assistant entry.
				if _, mixed := steps[entry.StepID]; mixed {
					return invalidEntry("step %s has both directive tool entries and an assistant entry", entry.StepID)
				}
				seen := directiveSteps[entry.StepID]
				if seen == nil {
					seen = make(map[string]bool)
					directiveSteps[entry.StepID] = seen
				}
				if seen[entry.ToolCallID] {
					return invalidEntry("tool call %s has two tool entries", entry.ToolCallID)
				}
				seen[entry.ToolCallID] = true
				break
			}
			calls := steps[entry.StepID]
			if calls == nil || !calls.calls[entry.ToolCallID] {
				return invalidEntry("tool entry %s has no assistant tool call %s earlier in the batch", entry.ID, entry.ToolCallID)
			}
			if calls.seen[entry.ToolCallID] {
				return invalidEntry("tool call %s has two tool entries", entry.ToolCallID)
			}
			calls.seen[entry.ToolCallID] = true
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return invalidEntry("entry %d does not encode: %v", i, err)
		}
		total += len(encoded)
	}
	if total > MaxSessionAppendBytes {
		return entryTooLarge("append is %d bytes (max %d)", total, MaxSessionAppendBytes)
	}
	for stepID, calls := range steps {
		for callID := range calls.calls {
			if !calls.seen[callID] {
				return invalidEntry("tool call %s of step %s has no tool entry in the batch", callID, stepID)
			}
		}
	}
	return nil
}

// stampAppendedSessionEntry returns the stored form of a validated request
// entry: envelope fields filled, sequence assigned, revision 1, tokens
// estimated when 0. Terminal tool entries get CompletedAt.
func stampAppendedSessionEntry(in *SessionEntry, req AppendAgentSessionEntriesRequest, sequence int64) *SessionEntry {
	out := cloneSessionEntry(in)
	out.Scope = req.Scope
	out.SessionID = req.SessionID
	out.RunID = req.RunID
	out.Sequence = sequence
	out.Revision = 1
	out.CreatedAt = req.Now
	out.UpdatedAt = req.Now
	if out.Tokens == 0 {
		out.Tokens = EstimateSessionEntryTokens(out)
	}
	if out.Tool != nil && out.State.Terminal() && out.Tool.CompletedAt == nil {
		now := req.Now
		out.Tool.CompletedAt = &now
	}
	return out
}

// validateToolEntryTransitionRequest checks the request alone (edge table,
// wait/result rules) before any I/O.
func validateToolEntryTransitionRequest(req ToolEntryTransitionRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidToolEntryTransition, err)
	}
	if strings.TrimSpace(req.EntryID) == "" || strings.TrimSpace(req.RunID) == "" {
		return fmt.Errorf("%w: entryId and runId are required", ErrInvalidToolEntryTransition)
	}
	if strings.TrimSpace(req.Lease.WorkerID) == "" && req.To != ToolEntryError && req.To != ToolEntryInterrupted {
		return fmt.Errorf("%w: lease needs a worker id", ErrInvalidToolEntryTransition)
	}
	if !ValidToolEntryTransition(req.From, req.To) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidToolEntryTransition, req.From, req.To)
	}
	if req.ExpectedRevision < 0 || req.Tokens < 0 {
		return fmt.Errorf("%w: negative revision or tokens", ErrInvalidToolEntryTransition)
	}
	if (req.Wait != nil) != req.To.awaiting() {
		return fmt.Errorf("%w: wait is required exactly for awaiting states", ErrInvalidToolEntryTransition)
	}
	if (req.Result != nil) != req.To.Terminal() {
		return fmt.Errorf("%w: result is required exactly for terminal states", ErrInvalidToolEntryTransition)
	}
	if req.Result != nil {
		if err := validateToolResultForState(req.To, req.Result, ErrInvalidToolEntryTransition); err != nil {
			return err
		}
	}
	return nil
}

// applyToolEntryTransition returns the entry after req, or the fence/CAS error.
// The lease is the backend's to check; the result is validated (caps).
func applyToolEntryTransition(current *SessionEntry, req ToolEntryTransitionRequest) (*SessionEntry, error) {
	if current == nil {
		return nil, ErrSessionEntryNotFound
	}
	if current.Type != SessionEntryTool || current.Tool == nil {
		return nil, fmt.Errorf("%w: entry %s is not a tool entry", ErrInvalidToolEntryTransition, current.ID)
	}
	if current.RunID != req.RunID {
		return nil, ErrToolEntryNotOwned
	}
	if current.State != req.From || (req.ExpectedRevision != 0 && current.Revision != req.ExpectedRevision) {
		return nil, ErrToolEntryStateConflict
	}
	if req.ActionCallID != "" && current.ActionCallID != "" && current.ActionCallID != req.ActionCallID {
		return nil, fmt.Errorf("%w: entry %s already has action call %s", ErrInvalidToolEntryTransition, current.ID, current.ActionCallID)
	}
	next := cloneSessionEntry(current)
	next.State = req.To
	if req.ActionCallID != "" {
		next.ActionCallID = req.ActionCallID
	}
	next.Tool.Wait = cloneToolEntryWait(req.Wait)
	next.Tool.Result = cloneToolResult(req.Result)
	now := req.Now
	if req.To == ToolEntryRunning {
		next.Tool.StartedAt = &now
	}
	if req.To.Terminal() {
		completed := now
		next.Tool.CompletedAt = &completed
	}
	if patch := req.Kernel; patch != nil {
		if patch.ResultDigest != "" {
			next.Tool.ResultDigest = patch.ResultDigest
		}
		if patch.OriginRef != "" {
			next.Tool.OriginRef = patch.OriginRef
		}
		if patch.DuplicateOfEntryID != "" {
			next.Tool.DuplicateOfEntryID = patch.DuplicateOfEntryID
		}
		if patch.Parked != nil {
			next.Tool.Parked = *patch.Parked
		}
		if patch.Guard != nil {
			next.Tool.Guard = cloneToolGuardOutcome(patch.Guard)
		}
		if patch.Control != nil {
			next.Tool.Control = cloneRaw(patch.Control)
		}
		if patch.SurfacedTools != nil {
			next.Tool.SurfacedTools = cloneSurfacedTools(patch.SurfacedTools)
		}
		if patch.LoadedInstructions != nil {
			next.Tool.LoadedInstructions = cloneStrings(patch.LoadedInstructions)
		}
		if patch.PinnedRun != nil {
			next.Tool.PinnedRun = cloneRaw(patch.PinnedRun)
		}
	}
	next.Revision = current.Revision + 1
	next.UpdatedAt = now
	switch {
	case req.Tokens > 0:
		next.Tokens = req.Tokens
	case req.Result != nil || req.Kernel != nil:
		next.Tokens = EstimateSessionEntryTokens(next)
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// normalizeSessionEntryFilter validates a list filter and applies the limit defaults.
func normalizeSessionEntryFilter(filter SessionEntryFilter) (SessionEntryFilter, error) {
	if err := filter.Scope.Validate(); err != nil {
		return filter, fmt.Errorf("%w: %v", ErrInvalidSessionEntry, err)
	}
	if filter.SessionID == "" && filter.RunID == "" && filter.ActionCallID == "" && filter.StepID == "" {
		return filter, invalidEntry("list needs sessionId, runId, actionCallId or stepId")
	}
	if filter.SessionID == "" && (filter.AfterSequence != 0 || filter.AfterCompaction) {
		return filter, invalidEntry("afterSequence and afterCompaction need sessionId")
	}
	if filter.AfterSequence < 0 || filter.Limit < 0 {
		return filter, invalidEntry("negative afterSequence or limit")
	}
	for _, kind := range filter.Types {
		if !kind.valid() {
			return filter, invalidEntry("unknown type %q", kind)
		}
	}
	for _, state := range filter.States {
		if !state.Valid() {
			return filter, invalidEntry("unknown state %q", state)
		}
	}
	if filter.Limit == 0 {
		filter.Limit = defaultSessionEntryListLimit
	}
	if filter.Limit > maxSessionEntryListLimit {
		filter.Limit = maxSessionEntryListLimit
	}
	return filter, nil
}

// sessionEntryMatchesFilter is the row predicate of a normalized filter, minus
// AfterCompaction, ordering and Limit (the backend applies those).
func sessionEntryMatchesFilter(e *SessionEntry, filter SessionEntryFilter) bool {
	if e == nil || e.Scope != filter.Scope {
		return false
	}
	if filter.SessionID != "" && e.SessionID != filter.SessionID {
		return false
	}
	if filter.RunID != "" && e.RunID != filter.RunID {
		return false
	}
	if filter.ActionCallID != "" && e.ActionCallID != filter.ActionCallID {
		return false
	}
	if filter.StepID != "" && e.StepID != filter.StepID {
		return false
	}
	if e.Sequence <= filter.AfterSequence {
		return false
	}
	if len(filter.Types) > 0 && !containsEntryType(filter.Types, e.Type) {
		return false
	}
	if len(filter.States) > 0 || len(filter.ToolNames) > 0 {
		if e.Type != SessionEntryTool || e.Tool == nil {
			return false
		}
		if len(filter.States) > 0 && !containsToolState(filter.States, e.State) {
			return false
		}
		if len(filter.ToolNames) > 0 && !containsString(filter.ToolNames, e.Tool.Name) {
			return false
		}
	}
	if len(filter.NoteKinds) > 0 {
		if e.Type != SessionEntryNote || e.Note == nil || !containsNoteKind(filter.NoteKinds, e.Note.Kind) {
			return false
		}
	}
	return true
}

func containsEntryType(list []SessionEntryType, v SessionEntryType) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsToolState(list []ToolEntryState, v ToolEntryState) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsNoteKind(list []NoteKind, v NoteKind) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Path builder

// buildAgentSessionPath is the pure merge used by all backends. chain is root
// ... leaf; segments[i] holds entries of chain[i] (any order, may include
// entries beyond the segment bound and compaction entries; both are dropped).
// compaction is the latest compaction on the path, or nil to find it in the
// segments. Leaf bound = NextSequence-1; ancestor i bound = chain[i+1].ForkSequence.
func buildAgentSessionPath(chain []*AgentSession, segments [][]*SessionEntry, compaction *SessionEntry) (*AgentSessionPath, error) {
	if len(chain) == 0 {
		return nil, ErrAgentSessionNotFound
	}
	if len(segments) != len(chain) {
		return nil, fmt.Errorf("%w: %d segments for %d sessions", ErrInvalidAgentSession, len(segments), len(chain))
	}
	if len(chain) > MaxAgentSessionDepth+1 {
		return nil, ErrAgentSessionForkTooDeep
	}
	bounds := make([]int64, len(chain))
	index := make(map[string]int, len(chain))
	for i, session := range chain {
		if session == nil {
			return nil, fmt.Errorf("%w: nil session in chain", ErrInvalidAgentSession)
		}
		if i == 0 && session.ParentSessionID != "" {
			return nil, fmt.Errorf("%w: chain does not start at the root", ErrInvalidAgentSession)
		}
		if i > 0 && session.ParentSessionID != chain[i-1].ID {
			return nil, fmt.Errorf("%w: session %s is not a child of %s", ErrInvalidAgentSession, session.ID, chain[i-1].ID)
		}
		index[session.ID] = i
		if i == len(chain)-1 {
			bounds[i] = session.NextSequence - 1
		} else {
			bounds[i] = chain[i+1].ForkSequence
		}
	}

	type position struct {
		segment  int
		sequence int64
	}
	bounded := make([][]*SessionEntry, len(chain))
	var latest *SessionEntry
	latestPos := position{segment: -1}
	positions := make(map[string]position)
	for i, segment := range segments {
		for _, entry := range segment {
			if entry == nil {
				continue
			}
			if entry.SessionID != chain[i].ID {
				return nil, fmt.Errorf("%w: entry %s is not in session %s", ErrInvalidSessionEntry, entry.ID, chain[i].ID)
			}
			if entry.Sequence < 1 || entry.Sequence > bounds[i] {
				continue
			}
			positions[entry.ID] = position{segment: i, sequence: entry.Sequence}
			if entry.Type == SessionEntryCompaction {
				if entry.Compaction == nil {
					return nil, fmt.Errorf("%w: compaction %s has no payload", ErrInvalidSessionEntry, entry.ID)
				}
				if latestPos.segment < i || (latestPos.segment == i && entry.Sequence > latestPos.sequence) {
					latest, latestPos = entry, position{segment: i, sequence: entry.Sequence}
				}
				continue
			}
			bounded[i] = append(bounded[i], entry)
		}
		sort.SliceStable(bounded[i], func(a, b int) bool { return bounded[i][a].Sequence < bounded[i][b].Sequence })
	}
	if compaction != nil {
		seg, ok := index[compaction.SessionID]
		if !ok || compaction.Type != SessionEntryCompaction || compaction.Compaction == nil ||
			compaction.Sequence < 1 || compaction.Sequence > bounds[seg] {
			return nil, fmt.Errorf("%w: compaction %s is not on the path", ErrInvalidSessionEntry, compaction.ID)
		}
		latest, latestPos = compaction, position{segment: seg, sequence: compaction.Sequence}
	}

	start := position{segment: 0, sequence: 1}
	if latest != nil {
		if kept := latest.Compaction.FirstKeptEntryID; kept == "" {
			start = position{segment: latestPos.segment, sequence: latestPos.sequence + 1}
		} else {
			pos, ok := positions[kept]
			if !ok {
				return nil, fmt.Errorf("%w: compaction %s keeps %s, which is not on the path", ErrInvalidSessionEntry, latest.ID, kept)
			}
			start = pos
		}
	}

	path := &AgentSessionPath{Session: chain[len(chain)-1], Chain: chain, Compaction: latest}
	if latest != nil {
		path.Tokens = latest.Tokens
	}
	for i := start.segment; i < len(chain); i++ {
		for _, entry := range bounded[i] {
			if i == start.segment && entry.Sequence < start.sequence {
				continue
			}
			path.Entries = append(path.Entries, entry)
			path.Tokens += entry.Tokens
		}
	}
	if len(path.Entries) > MaxAgentSessionPathEntries {
		return nil, ErrAgentSessionPathTooLarge
	}
	return path, nil
}

// ---------------------------------------------------------------------------
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

// validCompactionCut reports whether e may be the first kept entry of a
// compaction (SPEC 08 G2): a user, assistant or note entry. Never a tool entry,
// whose result must follow its call, nor a model_change.
func validCompactionCut(e *SessionEntry) bool {
	if e == nil {
		return false
	}
	switch e.Type {
	case SessionEntryUser, SessionEntryAssistant, SessionEntryNote:
		return true
	}
	return false
}

// validCompactionCutAt reports whether entries[cut:] may be kept verbatim after
// a summary of entries[:cut] (FirstKeptEntryID = entries[cut].ID; cut ==
// len(entries) keeps nothing). entries[cut] passes validCompactionCut, and the
// cut is at or before the earliest assistant entry with an open tool entry
// (compactionOpenToolBound), so no in-flight call is summarized away.
func validCompactionCutAt(entries []*SessionEntry, cut int) bool {
	if cut < 0 || cut > len(entries) {
		return false
	}
	if cut > compactionOpenToolBound(entries) {
		return false
	}
	if cut == len(entries) {
		return true
	}
	return validCompactionCut(entries[cut])
}

// validateCompactionAgainstPath is the in-transaction append check of a
// compaction entry against the session's current path.
func validateCompactionAgainstPath(path *AgentSessionPath, entry *SessionEntry) error {
	if entry == nil || entry.Compaction == nil {
		return invalidEntry("not a compaction entry")
	}
	if path == nil {
		return invalidEntry("compaction needs the current path")
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
			return invalidEntry("compaction keeps %s, which is not on the current path", kept)
		}
	}
	if !validCompactionCutAt(path.Entries, cut) {
		return invalidEntry("compaction cut at %d is not allowed", cut)
	}
	return nil
}
