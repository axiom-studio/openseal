package models

import "context"

// ConversationActiveRunsReadStore is an optional indexed read boundary for
// active child work whose canonical root belongs to one exact conversation.
// Embedding stores without this optimization retain the existing fallback.
type ConversationActiveRunsReadStore interface {
	ListConversationActiveRuns(context.Context, Scope, ObjectiveOwner, string, int) ([]*AgentRun, error)
}

func ValidateConversationActiveRunsQuery(scope Scope, owner ObjectiveOwner, conversationID string, limit int) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	if !IsValidOpaqueIdentifier(conversationID, 128) || limit < 1 || limit > 100 {
		return ErrInvalidConversationTask
	}
	return nil
}
