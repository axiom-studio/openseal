package routine

import (
	"context"
	"math"

	"github.com/axiom-studio/openseal/pkg/models"
)

// EventSourceCatalog lists host-owned sources (Google) for event_sources and create_task.
type EventSourceCatalog interface {
	ListRoutineEventSources(ctx context.Context, scope models.Scope, owner models.ObjectiveOwner, actingUser string) ([]EventSourceOption, error)
}

// Reasons a EventSourceOption is not available.
const (
	EventSourceNeedsSignIn            = "needs_sign_in"
	EventSourceNotConfigured          = "not_configured"
	EventSourceSharedSignInNotAllowed = "shared_sign_in_not_allowed"
)

// EventSourceNew is EventSourceOption.Source for a webhook routine that gets a new hook.
const EventSourceNew = "new"

type EventSourceOption struct {
	Kind       models.RoutineEventKind `json:"kind"`
	Source     string                  `json:"source"` // the SourceRef, or "new" for webhook
	Label      string                  `json:"label"`
	Available  bool                    `json:"available"`
	Reason     string                  `json:"reason,omitempty"`     // needs_sign_in | not_configured | shared_sign_in_not_allowed
	SkillID    string                  `json:"skillId,omitempty"`    // the skill whose sign-in is needed
	Fields     []string                `json:"fields"`               // RoutineEventFilterFields(Kind)
	EventTypes []string                `json:"eventTypes,omitempty"` // github: subscribed types without "github."
}

// IndexPublisher tells other processes that a scope's event routines changed. Best effort; the
// receiving host calls Engine.RefreshRoutineIndex(scope). Nil in single-process hosts (desktop daemon, tests).
type IndexPublisher interface {
	PublishRoutineIndexChange(scope models.Scope)
}

// EventLatencyBuckets are the upper bounds in seconds of the match latency histogram.
var EventLatencyBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, math.Inf(1)}

// EventStats is a snapshot of the matcher's and dispatcher's lock-free counters.
type EventStats struct {
	In, Matched, Dropped         map[models.RoutineEventKind]uint64 // cumulative; Dropped counts overflow rows
	RunsStarted, TestRunsStarted uint64
	QueueDepth                   int       // pending events at the dispatcher's last pass
	MatchLatencyBuckets          []float64 // upper bounds in seconds, EventLatencyBuckets
	MatchLatencyCounts           []uint64  // cumulative per bucket, index hits only
	MatchLatencySum              float64
}
