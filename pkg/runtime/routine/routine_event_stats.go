package routine

import (
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// routineEventStatsCounters are the matcher's and dispatcher's counters. Every
// field is atomic, so the ingress path updates them without a lock or an
// allocation.
type routineEventStatsCounters struct {
	in, matched, dropped [routineEventKindCount]atomic.Uint64
	runsStarted          atomic.Uint64
	testRunsStarted      atomic.Uint64
	queueDepth           atomic.Int64
	latencyCounts        [routineEventLatencyBucketCount]atomic.Uint64 // per bucket, not cumulative
	latencySumNanos      atomic.Uint64
}

const (
	routineEventKindCount          = 6
	routineEventLatencyBucketCount = 9
)

// routineEventKindSlot maps a kind to its counter slot; -1 for unknown kinds.
func routineEventKindSlot(kind models.RoutineEventKind) int {
	switch kind {
	case models.RoutineEventSlackMessage:
		return 0
	case models.RoutineEventTelegramMessage:
		return 1
	case models.RoutineEventGitHub:
		return 2
	case models.RoutineEventGmailMessage:
		return 3
	case models.RoutineEventCalendar:
		return 4
	case models.RoutineEventWebhook:
		return 5
	}
	return -1
}

func (c *routineEventStatsCounters) countIn(kind models.RoutineEventKind) {
	if slot := routineEventKindSlot(kind); slot >= 0 {
		c.in[slot].Add(1)
	}
}

func (c *routineEventStatsCounters) countMatched(kind models.RoutineEventKind, matched, dropped int) {
	slot := routineEventKindSlot(kind)
	if slot < 0 {
		return
	}
	if matched > 0 {
		c.matched[slot].Add(uint64(matched))
	}
	if dropped > 0 {
		c.dropped[slot].Add(uint64(dropped))
	}
}

func (c *routineEventStatsCounters) observeLatency(elapsed time.Duration) {
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := elapsed.Seconds()
	for index, bound := range RoutineEventLatencyBuckets {
		if seconds <= bound {
			c.latencyCounts[index].Add(1)
			break
		}
	}
	c.latencySumNanos.Add(uint64(elapsed))
}

func (c *routineEventStatsCounters) countRunStarted(test bool) {
	if test {
		c.testRunsStarted.Add(1)
	} else {
		c.runsStarted.Add(1)
	}
}

// snapshot copies the counters. Latency counts are cumulative per bucket, as
// a Prometheus histogram expects.
func (c *routineEventStatsCounters) snapshot() RoutineEventStats {
	stats := RoutineEventStats{
		In:                  make(map[models.RoutineEventKind]uint64, routineEventKindCount),
		Matched:             make(map[models.RoutineEventKind]uint64, routineEventKindCount),
		Dropped:             make(map[models.RoutineEventKind]uint64, routineEventKindCount),
		RunsStarted:         c.runsStarted.Load(),
		TestRunsStarted:     c.testRunsStarted.Load(),
		QueueDepth:          int(c.queueDepth.Load()),
		MatchLatencyBuckets: append([]float64(nil), RoutineEventLatencyBuckets...),
		MatchLatencyCounts:  make([]uint64, len(RoutineEventLatencyBuckets)),
		MatchLatencySum:     time.Duration(c.latencySumNanos.Load()).Seconds(),
	}
	for _, kind := range RoutineEventKinds {
		slot := routineEventKindSlot(kind)
		stats.In[kind] = c.in[slot].Load()
		stats.Matched[kind] = c.matched[slot].Load()
		stats.Dropped[kind] = c.dropped[slot].Load()
	}
	var total uint64
	for index := range stats.MatchLatencyCounts {
		total += c.latencyCounts[index].Load()
		stats.MatchLatencyCounts[index] = total
	}
	return stats
}

// RoutineEventBatchView is the folded "what started it" view of one
// occurrence: the batch and its events' facts, never their payloads.
type RoutineEventBatchView struct {
	BatchID      string             `json:"batchId"`
	ActivationID string             `json:"activationId"`
	RunID        string             `json:"runId,omitempty"`
	Test         bool               `json:"test"`
	Dropped      int                `json:"dropped"`
	Events       []RoutineEventView `json:"events"`
}

// RoutineEventView is one event of a RoutineEventBatchView.
type RoutineEventView struct {
	ID         string                  `json:"id"`
	Kind       models.RoutineEventKind `json:"kind"`
	OccurredAt time.Time               `json:"occurredAt"`
	Summary    string                  `json:"summary"`
	Fields     map[string][]string     `json:"fields"`
}

// NewRoutineEventBatchView builds the view of a batch from the records
// GetRoutineEventBatch returns, in the batch's event order.
func NewRoutineEventBatchView(batch *models.RoutineEventBatch, records []*models.RoutineEventRecord) *RoutineEventBatchView {
	if batch == nil {
		return nil
	}
	view := &RoutineEventBatchView{
		BatchID: batch.ID, ActivationID: batch.ActivationID, RunID: batch.RunID, Test: batch.Test, Dropped: batch.Dropped,
		Events: make([]RoutineEventView, 0, len(batch.EventIDs)),
	}
	byID := make(map[string]*models.RoutineEventRecord, len(records))
	for _, record := range records {
		if record != nil {
			byID[record.Event.ID] = record
		}
	}
	for _, id := range batch.EventIDs {
		record := byID[id]
		if record == nil {
			continue
		}
		fields := make(map[string][]string, len(record.Event.Fields))
		for key, values := range record.Event.Fields {
			fields[key] = append([]string(nil), values...)
		}
		view.Events = append(view.Events, RoutineEventView{
			ID: record.Event.ID, Kind: record.Event.Kind, OccurredAt: record.Event.OccurredAt.UTC(),
			Summary: record.Event.Summary, Fields: fields,
		})
	}
	return view
}
