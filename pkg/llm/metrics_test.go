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
	"net/http/httptest"
	"testing"
)

func TestMetrics_RecordAndSuccessRate(t *testing.T) {
	mc := NewMetricsCollector()

	mc.RecordAttempt()
	mc.RecordSuccess()
	mc.RecordAttempt()
	mc.RecordSuccess()
	mc.RecordAttempt()
	mc.RecordFailure()

	if rate := mc.SuccessRate(); rate != 66.66666666666666 {
		t.Errorf("expected success rate ~66.67, got %f", rate)
	}

	snapshot := mc.GetSnapshot()
	if snapshot.TotalAttempts != 3 {
		t.Errorf("expected 3 attempts, got %d", snapshot.TotalAttempts)
	}
	if snapshot.TotalSuccesses != 2 {
		t.Errorf("expected 2 successes, got %d", snapshot.TotalSuccesses)
	}
	if snapshot.TotalFailures != 1 {
		t.Errorf("expected 1 failure, got %d", snapshot.TotalFailures)
	}
}

func TestMetrics_PerToolMetrics(t *testing.T) {
	mc := NewMetricsCollector()

	mc.RecordToolCall("add_node", true, 100)
	mc.RecordToolCall("add_node", true, 200)
	mc.RecordToolCall("add_node", false, 50)
	mc.RecordToolCall("get_workflow", true, 300)
	mc.RecordToolCall("get_workflow", false, 150)

	snapshot := mc.GetSnapshot()

	if len(snapshot.PerTool) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(snapshot.PerTool))
	}

	addNode := snapshot.PerTool["add_node"]
	if addNode == nil {
		t.Fatal("add_node metrics not found")
	}
	if addNode.SuccessCount != 2 {
		t.Errorf("add_node: expected 2 successes, got %d", addNode.SuccessCount)
	}
	if addNode.FailureCount != 1 {
		t.Errorf("add_node: expected 1 failure, got %d", addNode.FailureCount)
	}
	if addNode.CallCount != 3 {
		t.Errorf("add_node: expected 3 calls, got %d", addNode.CallCount)
	}

	getWorkflow := snapshot.PerTool["get_workflow"]
	if getWorkflow == nil {
		t.Fatal("get_workflow metrics not found")
	}
	if getWorkflow.SuccessCount != 1 {
		t.Errorf("get_workflow: expected 1 success, got %d", getWorkflow.SuccessCount)
	}
}

func TestMetrics_LatencyTracking(t *testing.T) {
	mc := NewMetricsCollector()

	mc.RecordFirstToken(50)
	mc.RecordFirstToken(100)
	mc.RecordFirstToken(150)

	mc.RecordToolCallLatency(200)
	mc.RecordToolCallLatency(300)

	mc.RecordConversationLatency(1000)
	mc.RecordConversationLatency(2000)

	snapshot := mc.GetSnapshot()

	if snapshot.Latency.AvgFirstTokenMs != 100 {
		t.Errorf("expected avg first token 100ms, got %f", snapshot.Latency.AvgFirstTokenMs)
	}
	if snapshot.Latency.AvgToolCallRoundTripMs != 250 {
		t.Errorf("expected avg tool call 250ms, got %f", snapshot.Latency.AvgToolCallRoundTripMs)
	}
	if snapshot.Latency.AvgConversationTotalMs != 1500 {
		t.Errorf("expected avg conversation 1500ms, got %f", snapshot.Latency.AvgConversationTotalMs)
	}
	if snapshot.Latency.Samples != 2 {
		t.Errorf("expected 2 samples, got %d", snapshot.Latency.Samples)
	}
}

func TestMetrics_ErrorCategorization(t *testing.T) {
	mc := NewMetricsCollector()

	mc.RecordError(ErrorCategoryValidation)
	mc.RecordError(ErrorCategoryValidation)
	mc.RecordError(ErrorCategoryTimeout)
	mc.RecordError(ErrorCategoryLLM)
	mc.RecordError(ErrorCategoryNetwork)
	mc.RecordError(ErrorCategoryUnknown)

	snapshot := mc.GetSnapshot()

	if snapshot.Errors.Validation != 2 {
		t.Errorf("expected 2 validation errors, got %d", snapshot.Errors.Validation)
	}
	if snapshot.Errors.Timeout != 1 {
		t.Errorf("expected 1 timeout error, got %d", snapshot.Errors.Timeout)
	}
	if snapshot.Errors.LLMError != 1 {
		t.Errorf("expected 1 LLM error, got %d", snapshot.Errors.LLMError)
	}
	if snapshot.Errors.Network != 1 {
		t.Errorf("expected 1 network error, got %d", snapshot.Errors.Network)
	}
	if snapshot.Errors.Unknown != 1 {
		t.Errorf("expected 1 unknown error, got %d", snapshot.Errors.Unknown)
	}
	if snapshot.Errors.Total != 6 {
		t.Errorf("expected 6 total errors, got %d", snapshot.Errors.Total)
	}
}

func TestMetrics_EndpointReturnsJSON(t *testing.T) {
	mc := NewMetricsCollector()

	mc.RecordAttempt()
	mc.RecordSuccess()
	mc.RecordToolCall("add_node", true, 100)
	mc.RecordFirstToken(50)
	mc.RecordError(ErrorCategoryTimeout)

	req := httptest.NewRequest(http.MethodGet, "/orchestrator/llm/metrics", nil)
	rr := httptest.NewRecorder()

	handler := mc.MetricsHandler()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}

	contentType := rr.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected content-type application/json, got %s", contentType)
	}

	var snapshot MetricsSnapshot
	decoder := json.NewDecoder(rr.Body)
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if snapshot.SuccessRate != 100 {
		t.Errorf("expected 100%% success rate, got %f", snapshot.SuccessRate)
	}
	if snapshot.TotalAttempts != 1 {
		t.Errorf("expected 1 attempt, got %d", snapshot.TotalAttempts)
	}
	if len(snapshot.PerTool) != 1 {
		t.Errorf("expected 1 tool, got %d", len(snapshot.PerTool))
	}
	if snapshot.Errors.Timeout != 1 {
		t.Errorf("expected 1 timeout error, got %d", snapshot.Errors.Timeout)
	}
}

func TestMetrics_SuccessRateZeroWhenNoData(t *testing.T) {
	mc := NewMetricsCollector()

	if rate := mc.SuccessRate(); rate != 0 {
		t.Errorf("expected 0%% success rate with no data, got %f", rate)
	}

	snapshot := mc.GetSnapshot()
	if snapshot.SuccessRate != 0 {
		t.Errorf("expected 0%% success rate in snapshot, got %f", snapshot.SuccessRate)
	}
}

func TestMetrics_ToolMetricsSuccessRate(t *testing.T) {
	mc := NewMetricsCollector()

	mc.RecordToolCall("test_tool", true, 100)
	mc.RecordToolCall("test_tool", true, 100)
	mc.RecordToolCall("test_tool", false, 100)
	mc.RecordToolCall("test_tool", false, 100)

	snapshot := mc.GetSnapshot()
	testTool := snapshot.PerTool["test_tool"]

	expectedRate := 50.0
	if testTool.SuccessRate() != expectedRate {
		t.Errorf("expected tool success rate %f, got %f", expectedRate, testTool.SuccessRate())
	}
}

func TestMetrics_LatencyZeroWhenNoData(t *testing.T) {
	mc := NewMetricsCollector()

	snapshot := mc.GetSnapshot()

	if snapshot.Latency.AvgFirstTokenMs != 0 {
		t.Errorf("expected 0 avg first token, got %f", snapshot.Latency.AvgFirstTokenMs)
	}
	if snapshot.Latency.AvgToolCallRoundTripMs != 0 {
		t.Errorf("expected 0 avg tool call, got %f", snapshot.Latency.AvgToolCallRoundTripMs)
	}
	if snapshot.Latency.AvgConversationTotalMs != 0 {
		t.Errorf("expected 0 avg conversation, got %f", snapshot.Latency.AvgConversationTotalMs)
	}
}

func TestMetrics_UptimePresent(t *testing.T) {
	mc := NewMetricsCollector()

	snapshot := mc.GetSnapshot()
	if snapshot.Uptime == "" {
		t.Error("expected uptime to be non-empty")
	}
}
