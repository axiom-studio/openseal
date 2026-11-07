package openseal

import "github.com/axiom-studio/openseal/pkg/runtime"

// Session log types: the kernel-owned, append-only agent session that the run
// loop reads and writes. They are part of KernelStore; the Engine adds no read
// API for them.
type (
	AgentSession      = runtime.AgentSession
	AgentSessionKind  = runtime.AgentSessionKind
	SessionEntry      = runtime.SessionEntry
	SessionEntryType  = runtime.SessionEntryType
	ToolEntryState    = runtime.ToolEntryState
	ToolResult        = runtime.ToolResult
	ToolResultStatus  = runtime.ToolResultStatus
	AgentSessionStore = runtime.AgentSessionStore
	ToolOutputKeeper  = runtime.ToolOutputKeeper
)

var (
	ErrInvalidAgentSession        = runtime.ErrInvalidAgentSession
	ErrAgentSessionNotFound       = runtime.ErrAgentSessionNotFound
	ErrAgentSessionConflict       = runtime.ErrAgentSessionConflict
	ErrAgentSessionForkTooDeep    = runtime.ErrAgentSessionForkTooDeep
	ErrInvalidSessionEntry        = runtime.ErrInvalidSessionEntry
	ErrSessionEntryTooLarge       = runtime.ErrSessionEntryTooLarge
	ErrSessionEntryNotFound       = runtime.ErrSessionEntryNotFound
	ErrInvalidToolEntryTransition = runtime.ErrInvalidToolEntryTransition
	ErrToolEntryStateConflict     = runtime.ErrToolEntryStateConflict
	ErrToolEntryNotOwned          = runtime.ErrToolEntryNotOwned
	ErrAgentSessionPathTooLarge   = runtime.ErrAgentSessionPathTooLarge
	ErrSessionMoved               = runtime.ErrSessionMoved
	ErrChannelWatermarkMoved      = runtime.ErrChannelWatermarkMoved
	ErrToolOutputNoChat           = runtime.ErrToolOutputNoChat
)
