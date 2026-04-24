/*
 * Copyright (c) 2026. Axiom Studio
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

package events

import "time"

const (
	EventRunStarted    = "run_started"
	EventRunCompleted  = "run_completed"
	EventRunFailed     = "run_failed"
	EventNodeError     = "node_error"
	EventHeartbeatMiss = "heartbeat_miss"
)

type AgentEvent struct {
	AgentID   int                    `json:"agent_id"`
	Type      string                 `json:"type"`
	RunID     string                 `json:"run_id,omitempty"`
	Message   string                 `json:"message"`
	UISpec    map[string]interface{} `json:"ui_spec,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
}
