package models

import (
	"context"
	"time"
)

// WorkItem asks for one pass over a family's durable work in a scope at or
// after DueAt. Items are hints: claims, leases and fairness are always decided
// by the store, so a duplicate or early item costs one empty pass.
type WorkItem struct {
	Family WorkFamily
	Scope  Scope
	DueAt  time.Time
}

// WorkItemKey identifies the family and scope an item schedules.
type WorkItemKey struct {
	Family WorkFamily
	Scope  Scope
}

// WorkFamily names the class of durable work whose claimability changed.
// Families keep wake fan-out narrow: an ActionCall change never wakes Run
// schedulers, and a Run change never wakes action claimers.
type WorkFamily string

const (
	// WorkFamilyAction covers action_calls and Runs leaving an action pause.
	WorkFamilyAction WorkFamily = "action"
	// WorkFamilyRun covers agent_runs and agent_requests.
	WorkFamilyRun WorkFamily = "run"
	// WorkFamilyMaintenance covers Skill runtime maintenance, which gates both
	// ActionCalls and Runs waiting for the maintenance to finish.
	WorkFamilyMaintenance WorkFamily = "maintenance"
	// WorkFamilyApproval covers approval_checkpoints.
	WorkFamilyApproval WorkFamily = "approval"
	// WorkFamilyConversation covers conversations, channel messages and the
	// in-chat Skill setup and credential requests.
	WorkFamilyConversation WorkFamily = "conversation"
	// WorkFamilyExternal covers the external conversation inbox, deliveries,
	// endpoints and callback events.
	WorkFamilyExternal WorkFamily = "external"
	// WorkFamilyActivity covers channel-visible Run commentary.
	WorkFamilyActivity WorkFamily = "activity"

	// Due families are scheduled by the loops themselves for time-based work:
	// retries, lease expiries, timers and spaced or backed-off passes. Each
	// wakes only the loop that scheduled it.

	// WorkFamilyRunDue wakes Run pools only.
	WorkFamilyRunDue WorkFamily = "run-due"
	// WorkFamilyConversationDue wakes the conversation Run reconciler only.
	WorkFamilyConversationDue WorkFamily = "conversation-due"
	// WorkFamilyExternalDue wakes the external conversation supervisor only.
	WorkFamilyExternalDue WorkFamily = "external-due"
)

// ActionDeadlineStore reports when persisted ActionCalls next become
// claimable without any further write: a retry's available_at or a running
// attempt's lease expiry. Nil means no time-based work is pending.
type ActionDeadlineStore interface {
	NextActionDeadline(ctx context.Context, scope Scope, now time.Time) (*time.Time, error)
}

// AgentRunDeadlineStore reports when persisted Runs of one pool next become
// claimable without any further write: a queued Run's available_at or a
// running Run's lease expiry. Sleeping timers are reported by WakeDueTimers.
type AgentRunDeadlineStore interface {
	NextAgentRunDeadline(ctx context.Context, scope Scope, kind RunKind, assignedAgentID string, now time.Time) (*time.Time, error)
}

// ExternalConversationDeadlineStore reports when external conversation work
// next becomes due: inbox, delivery and callback retries or lease expiries,
// reply retries and pending approval expiries.
type ExternalConversationDeadlineStore interface {
	NextExternalConversationDeadline(ctx context.Context, scope Scope, now time.Time) (*time.Time, error)
}
