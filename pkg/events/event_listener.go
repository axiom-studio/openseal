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

import (
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// HistoryRepository defines the interface for persisting chat history
type HistoryRepository interface {
	SaveMessage(msg *ChatHistoryMessage) (int, error)
}

// ChatHistoryMessage represents a message in chat history
type ChatHistoryMessage struct {
	AgentID int
	Role    string
	Content string
	UISpec  map[string]interface{}
	Metadata map[string]interface{}
}

type EventListener struct {
	nc     *nats.Conn
	repo   HistoryRepository
	logger *zap.SugaredLogger
	subs   []*nats.Subscription
}

func NewEventListener(nc *nats.Conn, repo HistoryRepository, logger *zap.SugaredLogger) *EventListener {
	return &EventListener{nc: nc, repo: repo, logger: logger}
}

func (el *EventListener) Start() error {
	sub, err := el.nc.Subscribe("agent.events.>", func(m *nats.Msg) {
		el.handleEvent(m)
	})
	if err != nil {
		return fmt.Errorf("subscribe to agent events: %w", err)
	}
	el.subs = append(el.subs, sub)
	el.logger.Info("agent event listener started, subscribed to agent.events.>")
	return nil
}

func (el *EventListener) Stop() {
	for _, sub := range el.subs {
		sub.Unsubscribe()
	}
}

func (el *EventListener) handleEvent(m *nats.Msg) {
	var evt AgentEvent
	if err := json.Unmarshal(m.Data, &evt); err != nil {
		el.logger.Errorf("unmarshal agent event: %v", err)
		return
	}

	el.logger.Debugf("received agent event: agent=%d type=%s", evt.AgentID, evt.Type)

	msg := &ChatHistoryMessage{
		AgentID: evt.AgentID,
		Role:    "system",
		Content: evt.Message,
		UISpec:  evt.UISpec,
		Metadata: map[string]interface{}{
			"event_type": evt.Type,
			"run_id":     evt.RunID,
			"delivered":  "false",
		},
	}

	if _, err := el.repo.SaveMessage(msg); err != nil {
		el.logger.Errorf("save proactive event: %v", err)
		return
	}
}
