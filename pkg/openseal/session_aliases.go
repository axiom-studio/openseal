package openseal

import (
	"github.com/axiom-studio/openseal/pkg/models"
)

// Session log types: the kernel-owned, append-only agent session that the run
// loop reads and writes. They are part of KernelStore; the Engine adds no read
// API for them.
type (
	AgentSession      = models.AgentSession
	AgentSessionKind  = models.AgentSessionKind
	SessionEntry      = models.SessionEntry
	SessionEntryType  = models.SessionEntryType
	ToolEntryState    = models.ToolEntryState
	ToolResult        = models.ToolResult
	ToolResultStatus  = models.ToolResultStatus
	AgentSessionStore = models.AgentSessionStore
	ToolOutputKeeper  = models.ToolOutputKeeper
)

var (
	ErrInvalidAgentSession        = models.ErrInvalidAgentSession
	ErrAgentSessionNotFound       = models.ErrAgentSessionNotFound
	ErrAgentSessionConflict       = models.ErrAgentSessionConflict
	ErrAgentSessionForkTooDeep    = models.ErrAgentSessionForkTooDeep
	ErrInvalidSessionEntry        = models.ErrInvalidSessionEntry
	ErrSessionEntryTooLarge       = models.ErrSessionEntryTooLarge
	ErrSessionEntryNotFound       = models.ErrSessionEntryNotFound
	ErrInvalidToolEntryTransition = models.ErrInvalidToolEntryTransition
	ErrToolEntryStateConflict     = models.ErrToolEntryStateConflict
	ErrToolEntryNotOwned          = models.ErrToolEntryNotOwned
	ErrAgentSessionPathTooLarge   = models.ErrAgentSessionPathTooLarge
	ErrSessionMoved               = models.ErrSessionMoved
	ErrChannelWatermarkMoved      = models.ErrChannelWatermarkMoved
	ErrToolOutputNoChat           = models.ErrToolOutputNoChat
)
