package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// RetryConversationRun requeues the original reply identity, never reposts the
// user message. This first recovery path deliberately excludes task trees and
// any tool execution; those require their own recovery protocol.
func (s *RunCommandService) RetryConversationRun(ctx context.Context, req AgentRunCommandRequest) (*AgentRunCommandResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.RunID == "" || req.ExpectedRevision <= 0 {
		return nil, ErrInvalidRunCommand
	}
	if req.Instruction != "" || req.InterventionID != "" || req.HumanInterventionID != "" {
		return nil, fmt.Errorf("%w: retry cannot change the saved request", ErrInvalidRunCommand)
	}
	current, err := s.store.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrRunNotFound
	}
	if current.Scope != req.Scope || current.ID != req.RunID {
		return nil, ErrRunNotFound
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	reason, err := s.conversationRetryBlocker(ctx, current)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRunCommand, reason)
	}
	run := cloneAgentRun(current)
	now := s.now()
	run.Status, run.Error, run.CompletedAt = AgentRunStatusQueued, "", nil
	run.LeaseOwner, run.LeaseExpiresAt = "", nil
	run.AvailableAt, run.QueueEnteredAt, run.UpdatedAt = now, now, now
	run.Revision++
	// Usage, turn cursor, plan, checkpoint and action identity remain intact.
	// The normal worker budget admission still applies to the next attempt.
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: req.Scope, RunID: run.ID, AgentID: run.AssignedAgentID,
		EventType: "run.retried", Severity: ActivitySeverityInfo, Visibility: visibility,
		Actor: req.Actor, Summary: "Retrying the saved reply", CreatedAt: now,
		Payload: map[string]interface{}{"previousRevision": current.Revision},
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	persisted, err := s.store.UpdateAgentRunWithEvent(ctx, run, current.Revision, event, nil)
	if err != nil {
		return nil, err
	}
	return &AgentRunCommandResult{Run: run, Event: persisted}, nil
}

type RunRetryEligibility struct {
	RunID     string `json:"runId"`
	Revision  int64  `json:"revision"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// ConversationRetryEligibility performs the same checks as execution, without
// modifying the run, budget, message or audit. Execution rechecks everything.
func (s *RunCommandService) ConversationRetryEligibility(ctx context.Context, scope Scope, id string) (*RunRetryEligibility, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	run, err := s.store.GetAgentRun(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if run == nil || run.ID != id || run.Scope != scope {
		return nil, ErrRunNotFound
	}
	reason, err := s.conversationRetryBlocker(ctx, run)
	if err != nil {
		return nil, err
	}
	return &RunRetryEligibility{RunID: id, Revision: run.Revision, Available: reason == "", Reason: reason}, nil
}

func (s *RunCommandService) conversationRetryBlocker(ctx context.Context, run *AgentRun) (string, error) {
	if run.Status != AgentRunStatusFailed {
		return "not_failed", nil
	}
	if run.Kind != RunKindConversation || run.ParentRunID != "" {
		return "task_recovery_required", nil
	}
	children, err := s.store.ListAgentRuns(ctx, AgentRunFilter{Scope: run.Scope, ParentRunID: run.ID, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(children) != 0 {
		return "task_recovery_required", nil
	}
	actions, err := s.store.ListActionCalls(ctx, ActionFilter{Scope: run.Scope, RunID: run.ID, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(actions) != 0 {
		return "action_recovery_required", nil
	}
	turns, ok := s.store.(AgentTurnStore)
	if !ok {
		return "recovery_unavailable", nil
	}
	pending, err := turns.ListAgentTurns(ctx, AgentTurnFilter{Scope: run.Scope, RunID: run.ID, AfterSequence: run.LastAppliedTurn, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(pending) != 0 {
		return "turn_recovery_required", nil
	}
	return "", nil
}
