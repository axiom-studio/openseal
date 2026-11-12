package runtime

// ToolEntryStates lists every state, open ones first.
var ToolEntryStates = []ToolEntryState{
	ToolEntryPending, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent,
	ToolEntryRunning, ToolEntryCompleted, ToolEntryError, ToolEntryInterrupted,
}

// OpenToolEntryStates lists the non-terminal states (D14 resume filter).
var OpenToolEntryStates = []ToolEntryState{
	ToolEntryPending, ToolEntryAwaitingApproval, ToolEntryAwaitingChild, ToolEntryAwaitingEvent, ToolEntryRunning,
}
