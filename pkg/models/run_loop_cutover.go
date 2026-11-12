package models

import "context"

// RunLoopCutoverRef names one Run the cutover has to look at.
type RunLoopCutoverRef struct {
	Scope Scope
	ID    string
}

// RunLoopCutoverStore is the store the cutover needs. The SQLite and
// PostgreSQL stores implement it; a wrapper that embeds one of them does too.
type RunLoopCutoverStore interface {
	RunCommandStore
	AgentTurnStore
	// ListRunLoopCutoverCandidates lists, across every scope, the
	// non-terminal Runs without an Engine marker, oldest first.
	ListRunLoopCutoverCandidates(ctx context.Context) ([]RunLoopCutoverRef, error)
	// WithRunLoopCutoverLock runs fn under the lock that serializes the
	// cutover of concurrently starting processes.
	WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error
}
