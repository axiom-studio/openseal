package runtime

import "github.com/axiom-studio/openseal/pkg/models"

// ToolEntryStates lists every state, open ones first. It is a copy, so
// writing to it never changes ToolEntryState.Valid.
var ToolEntryStates = models.ToolEntryStateValues()

// OpenToolEntryStates lists the non-terminal states (D14 resume filter).
var OpenToolEntryStates = []ToolEntryState{
	ToolEntryPending, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent, ToolEntryRunning,
}
