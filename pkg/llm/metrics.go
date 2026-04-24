/*
 * Copyright (c) 2025. Axiom Studio
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package llm

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ErrorCategory classifies the type of error that occurred
type ErrorCategory string

const (
	ErrorCategoryValidation ErrorCategory = "validation"
	ErrorCategoryTimeout    ErrorCategory = "timeout"
	ErrorCategoryLLM        ErrorCategory = "llm_error"
	ErrorCategoryNetwork    ErrorCategory = "network"
	ErrorCategoryUnknown    ErrorCategory = "unknown"
)

// ToolMetrics tracks success/failure for a specific tool
type ToolMetrics struct {
	SuccessCount   uint64 `json:"successCount"`
	FailureCount   uint64 `json:"failureCount"`
	TotalLatencyMs uint64 `json:"totalLatencyMs"`
	CallCount      uint64 `json:"callCount"`
}

// AvgLatencyMs returns the average latency in milliseconds
func (t *ToolMetrics) AvgLatencyMs() float64 {
	calls := atomic.LoadUint64(&t.CallCount)
	if calls == 0 {
		return 0
	}
	total := atomic.LoadUint64(&t.TotalLatencyMs)
	return float64(total) / float64(calls)
}

// SuccessRate returns the success rate as a percentage (0-100)
func (t *ToolMetrics) SuccessRate() float64 {
	success := atomic.LoadUint64(&t.SuccessCount)
	failure := atomic.LoadUint64(&t.FailureCount)
	total := success + failure
	if total == 0 {
		return 0
	}
	return float64(success) / float64(total) * 100
}

// LatencyMetrics tracks timing information
type LatencyMetrics struct {
	FirstTokenTimes   []int64 `json:"firstTokenTimesMs"`
	ToolCallTimes     []int64 `json:"toolCallRoundTripMs"`
	ConversationTimes []int64 `json:"conversationTotalMs"`
}

// AvgFirstTokenMs returns average time to first token
func (l *LatencyMetrics) AvgFirstTokenMs() float64 {
	if len(l.FirstTokenTimes) == 0 {
		return 0
	}
	var sum int64
	for _, t := range l.FirstTokenTimes {
		sum += t
	}
	return float64(sum) / float64(len(l.FirstTokenTimes))
}

// AvgToolCallRoundTripMs returns average tool call round-trip time
func (l *LatencyMetrics) AvgToolCallRoundTripMs() float64 {
	if len(l.ToolCallTimes) == 0 {
		return 0
	}
	var sum int64
	for _, t := range l.ToolCallTimes {
		sum += t
	}
	return float64(sum) / float64(len(l.ToolCallTimes))
}

// AvgConversationTotalMs returns average total conversation time
func (l *LatencyMetrics) AvgConversationTotalMs() float64 {
	if len(l.ConversationTimes) == 0 {
		return 0
	}
	var sum int64
	for _, t := range l.ConversationTimes {
		sum += t
	}
	return float64(sum) / float64(len(l.ConversationTimes))
}

// MetricsSnapshot is the JSON-serializable metrics response
type MetricsSnapshot struct {
	SuccessRate    float64                 `json:"successRate"`
	TotalAttempts  uint64                  `json:"totalAttempts"`
	TotalSuccesses uint64                  `json:"totalSuccesses"`
	TotalFailures  uint64                  `json:"totalFailures"`
	PerTool        map[string]*ToolMetrics `json:"perTool"`
	Latency        *LatencySnapshot        `json:"latency"`
	Errors         *ErrorCounts            `json:"errors"`
	Uptime         string                  `json:"uptime"`
}

// LatencySnapshot provides human-readable latency metrics
type LatencySnapshot struct {
	AvgFirstTokenMs        float64 `json:"avgFirstTokenMs"`
	AvgToolCallRoundTripMs float64 `json:"avgToolCallRoundTripMs"`
	AvgConversationTotalMs float64 `json:"avgConversationTotalMs"`
	Samples                int     `json:"samples"`
}

// ErrorCounts tracks errors by category
type ErrorCounts struct {
	Validation uint64 `json:"validation"`
	Timeout    uint64 `json:"timeout"`
	LLMError   uint64 `json:"llmError"`
	Network    uint64 `json:"network"`
	Unknown    uint64 `json:"unknown"`
	Total      uint64 `json:"total"`
}

// MetricsCollector collects and exposes LLM interaction metrics
type MetricsCollector struct {
	mu sync.RWMutex

	totalAttempts  uint64
	totalSuccesses uint64
	totalFailures  uint64

	perTool   map[string]*ToolMetrics
	latency   *LatencyMetrics
	errors    *ErrorCounts
	startTime time.Time
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		perTool:   make(map[string]*ToolMetrics),
		latency:   &LatencyMetrics{},
		errors:    &ErrorCounts{},
		startTime: time.Now(),
	}
}

// RecordAttempt records a new LLM interaction attempt
func (m *MetricsCollector) RecordAttempt() {
	atomic.AddUint64(&m.totalAttempts, 1)
}

// RecordSuccess records a successful LLM interaction
func (m *MetricsCollector) RecordSuccess() {
	atomic.AddUint64(&m.totalSuccesses, 1)
}

// RecordFailure records a failed LLM interaction
func (m *MetricsCollector) RecordFailure() {
	atomic.AddUint64(&m.totalFailures, 1)
}

// RecordToolCall records a tool execution with its outcome and latency
func (m *MetricsCollector) RecordToolCall(toolName string, success bool, latencyMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tm, exists := m.perTool[toolName]
	if !exists {
		tm = &ToolMetrics{}
		m.perTool[toolName] = tm
	}

	atomic.AddUint64(&tm.CallCount, 1)
	atomic.AddUint64(&tm.TotalLatencyMs, uint64(latencyMs))
	if success {
		atomic.AddUint64(&tm.SuccessCount, 1)
	} else {
		atomic.AddUint64(&tm.FailureCount, 1)
	}
}

const maxMetricSamples = 1000

// appendSample adds a sample to a slice, capping at maxMetricSamples.
// When at capacity, drops the oldest 10% before appending.
func appendSample(slice []int64, sample int64) []int64 {
	if len(slice) >= maxMetricSamples {
		dropCount := maxMetricSamples / 10
		slice = slice[dropCount:]
	}
	return append(slice, sample)
}

// RecordFirstToken records the time to first token
func (m *MetricsCollector) RecordFirstToken(elapsedMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency.FirstTokenTimes = appendSample(m.latency.FirstTokenTimes, elapsedMs)
}

// RecordToolCallLatency records a tool call round-trip time
func (m *MetricsCollector) RecordToolCallLatency(elapsedMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency.ToolCallTimes = appendSample(m.latency.ToolCallTimes, elapsedMs)
}

// RecordConversationLatency records total conversation time
func (m *MetricsCollector) RecordConversationLatency(elapsedMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency.ConversationTimes = appendSample(m.latency.ConversationTimes, elapsedMs)
}

// RecordError records an error by category
func (m *MetricsCollector) RecordError(category ErrorCategory) {
	m.mu.Lock()
	defer m.mu.Unlock()

	atomic.AddUint64(&m.errors.Total, 1)
	switch category {
	case ErrorCategoryValidation:
		atomic.AddUint64(&m.errors.Validation, 1)
	case ErrorCategoryTimeout:
		atomic.AddUint64(&m.errors.Timeout, 1)
	case ErrorCategoryLLM:
		atomic.AddUint64(&m.errors.LLMError, 1)
	case ErrorCategoryNetwork:
		atomic.AddUint64(&m.errors.Network, 1)
	default:
		atomic.AddUint64(&m.errors.Unknown, 1)
	}
}

// GetSnapshot returns a point-in-time metrics snapshot
func (m *MetricsCollector) GetSnapshot() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	totalSuccesses := atomic.LoadUint64(&m.totalSuccesses)
	totalFailures := atomic.LoadUint64(&m.totalFailures)
	totalAttempts := atomic.LoadUint64(&m.totalAttempts)

	var successRate float64
	if totalAttempts > 0 {
		successRate = float64(totalSuccesses) / float64(totalAttempts) * 100
	}

	// Copy per-tool metrics
	perToolCopy := make(map[string]*ToolMetrics, len(m.perTool))
	for name, tm := range m.perTool {
		perToolCopy[name] = &ToolMetrics{
			SuccessCount:   atomic.LoadUint64(&tm.SuccessCount),
			FailureCount:   atomic.LoadUint64(&tm.FailureCount),
			TotalLatencyMs: atomic.LoadUint64(&tm.TotalLatencyMs),
			CallCount:      atomic.LoadUint64(&tm.CallCount),
		}
	}

	// Copy latency data
	latencyCopy := &LatencyMetrics{
		FirstTokenTimes:   make([]int64, len(m.latency.FirstTokenTimes)),
		ToolCallTimes:     make([]int64, len(m.latency.ToolCallTimes)),
		ConversationTimes: make([]int64, len(m.latency.ConversationTimes)),
	}
	copy(latencyCopy.FirstTokenTimes, m.latency.FirstTokenTimes)
	copy(latencyCopy.ToolCallTimes, m.latency.ToolCallTimes)
	copy(latencyCopy.ConversationTimes, m.latency.ConversationTimes)

	// Copy error counts
	errorsCopy := &ErrorCounts{
		Validation: atomic.LoadUint64(&m.errors.Validation),
		Timeout:    atomic.LoadUint64(&m.errors.Timeout),
		LLMError:   atomic.LoadUint64(&m.errors.LLMError),
		Network:    atomic.LoadUint64(&m.errors.Network),
		Unknown:    atomic.LoadUint64(&m.errors.Unknown),
		Total:      atomic.LoadUint64(&m.errors.Total),
	}

	return MetricsSnapshot{
		SuccessRate:    successRate,
		TotalAttempts:  totalAttempts,
		TotalSuccesses: totalSuccesses,
		TotalFailures:  totalFailures,
		PerTool:        perToolCopy,
		Latency: &LatencySnapshot{
			AvgFirstTokenMs:        latencyCopy.AvgFirstTokenMs(),
			AvgToolCallRoundTripMs: latencyCopy.AvgToolCallRoundTripMs(),
			AvgConversationTotalMs: latencyCopy.AvgConversationTotalMs(),
			Samples:                len(latencyCopy.ConversationTimes),
		},
		Errors: errorsCopy,
		Uptime: time.Since(m.startTime).Round(time.Second).String(),
	}
}

// SuccessRate returns the current success rate as a percentage
func (m *MetricsCollector) SuccessRate() float64 {
	totalSuccesses := atomic.LoadUint64(&m.totalSuccesses)
	totalFailures := atomic.LoadUint64(&m.totalFailures)
	total := totalSuccesses + totalFailures
	if total == 0 {
		return 0
	}
	return float64(totalSuccesses) / float64(total) * 100
}

// MetricsHandler returns an HTTP handler that serves metrics as JSON
func (m *MetricsCollector) MetricsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snapshot := m.GetSnapshot()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(snapshot)
	}
}
