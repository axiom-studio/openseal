package trigger

import (
	"context"
	"sync"
	"time"
)

// ManualTrigger handles manual/API-triggered runs
type ManualTrigger struct {
	mu        sync.RWMutex
	instances map[int]*manualInstance
	handler   TriggerHandler
}

type manualInstance struct {
	instanceId int
	config     map[string]interface{}
}

// NewManualTrigger creates a new manual trigger
func NewManualTrigger() *ManualTrigger {
	return &ManualTrigger{
		instances: make(map[int]*manualInstance),
	}
}

func (t *ManualTrigger) Type() string {
	return "manual"
}

func (t *ManualTrigger) ValidateConfig(config map[string]interface{}) error {
	// Manual trigger has no required config
	return nil
}

func (t *ManualTrigger) Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.instances[instanceId] = &manualInstance{
		instanceId: instanceId,
		config:     config,
	}

	return &TriggerMetadata{}, nil
}

func (t *ManualTrigger) Teardown(ctx context.Context, instanceId int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.instances, instanceId)
	return nil
}

// Start implements ActiveTrigger (manual doesn't need active listening)
func (t *ManualTrigger) Start(ctx context.Context, handler TriggerHandler) error {
	t.handler = handler
	return nil
}

// Stop implements ActiveTrigger
func (t *ManualTrigger) Stop(ctx context.Context) error {
	t.handler = nil
	return nil
}

// Trigger manually triggers an agent instance
func (t *ManualTrigger) Trigger(ctx context.Context, instanceId int, payload map[string]interface{}, userId int) error {
	t.mu.RLock()
	_, ok := t.instances[instanceId]
	handler := t.handler
	t.mu.RUnlock()

	if !ok {
		// Manual trigger is always allowed even if not explicitly configured
	}

	if handler == nil {
		return nil // No handler, nothing to do
	}

	event := TriggerEvent{
		Type:   "manual",
		Source: "api",
		Payload: map[string]interface{}{
			"triggeredAt": time.Now().UTC().Format(time.RFC3339),
			"triggeredBy": userId,
			"data":        payload,
		},
		Timestamp: time.Now().Unix(),
	}

	return handler(ctx, instanceId, event)
}

// IsConfigured checks if an instance has manual trigger configured
func (t *ManualTrigger) IsConfigured(instanceId int) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.instances[instanceId]
	return ok
}
