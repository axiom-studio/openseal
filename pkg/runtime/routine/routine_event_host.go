package routine

import (
	"context"
	"math"

	"github.com/axiom-studio/openseal/pkg/models"
)

// RoutineEventSourceCatalog lists host-owned sources (Google) for event_sources and create_task.
type RoutineEventSourceCatalog interface {
	ListRoutineEventSources(ctx context.Context, scope models.Scope, owner models.ObjectiveOwner, actingUser string) ([]RoutineEventSourceOption, error)
}

// Reasons a RoutineEventSourceOption is not available.
const (
	RoutineEventSourceNeedsSignIn            = "needs_sign_in"
	RoutineEventSourceNotConfigured          = "not_configured"
	RoutineEventSourceSharedSignInNotAllowed = "shared_sign_in_not_allowed"
)

// RoutineEventSourceNew is RoutineEventSourceOption.Source for a webhook routine that gets a new hook.
const RoutineEventSourceNew = "new"

type RoutineEventSourceOption struct {
	Kind       models.RoutineEventKind `json:"kind"`
	Source     string                  `json:"source"` // the SourceRef, or "new" for webhook
	Label      string                  `json:"label"`
	Available  bool                    `json:"available"`
	Reason     string                  `json:"reason,omitempty"`     // needs_sign_in | not_configured | shared_sign_in_not_allowed
	SkillID    string                  `json:"skillId,omitempty"`    // the skill whose sign-in is needed
	Fields     []string                `json:"fields"`               // RoutineEventFilterFields(Kind)
	EventTypes []string                `json:"eventTypes,omitempty"` // github: subscribed types without "github."
}

// RoutineIndexPublisher tells other processes that a scope's event routines changed. Best effort; the
// receiving host calls Engine.RefreshRoutineIndex(scope). Nil in single-process hosts (desktop daemon, tests).
type RoutineIndexPublisher interface {
	PublishRoutineIndexChange(scope models.Scope)
}

// RoutineEventLatencyBuckets are the upper bounds in seconds of the match latency histogram.
var RoutineEventLatencyBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, math.Inf(1)}

// RoutineEventStats is a snapshot of the matcher's and dispatcher's lock-free counters.
type RoutineEventStats struct {
	In, Matched, Dropped         map[models.RoutineEventKind]uint64 // cumulative; Dropped counts overflow rows
	RunsStarted, TestRunsStarted uint64
	QueueDepth                   int       // pending events at the dispatcher's last pass
	MatchLatencyBuckets          []float64 // upper bounds in seconds, RoutineEventLatencyBuckets
	MatchLatencyCounts           []uint64  // cumulative per bucket, index hits only
	MatchLatencySum              float64
}
