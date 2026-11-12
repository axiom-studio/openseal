package models

import (
	"context"
	"encoding/json"
	"time"
)

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
	MaxSessionEntryListLimit     = 1000
)

type AgentSessionPath struct {
	Session    *AgentSession
	Chain      []*AgentSession // root ... leaf (len = Depth+1)
	Compaction *SessionEntry   // latest compaction on the path, nil if none
	Entries    []*SessionEntry // render order; never contains compaction entries
	Tokens     int64           // Compaction.Tokens + sum(Entries.Tokens)
}
