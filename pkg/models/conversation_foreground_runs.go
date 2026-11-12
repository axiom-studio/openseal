package models

import "context"

// ConversationForegroundRunsReadStore lists a conversation's foreground Runs
// (the channel lane, explicit threads and Team members' lanes) without
// scanning unrelated Runs. Every store matches lanes exactly as
// ConversationRunLane builds them.
type ConversationForegroundRunsReadStore interface {
	ListConversationForegroundRuns(context.Context, Scope, ObjectiveOwner, string, int, int) ([]*AgentRun, error)
}

func ValidateConversationForegroundRunsQuery(scope Scope, owner ObjectiveOwner, conversationID string, limit, offset int) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(conversationID, 128) || limit < 1 || limit > 500 || offset < 0 {
		return ErrInvalidConversationTask
	}
	return nil
}
