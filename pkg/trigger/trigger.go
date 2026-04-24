package trigger

import (
	"context"
	"fmt"
	"sync"
)

const (
	TriggerTypeWebhook  = "webhook"
	TriggerTypeCron     = "cron"
	TriggerTypeManual   = "manual"
	TriggerTypeK8sWatch = "k8s-watch"
	TriggerTypeK8sEvent = "k8s-event"
)

// TriggerConfig holds configuration for a trigger
type TriggerConfig struct {
	Type   string                 `json:"type"`
	Config map[string]interface{} `json:"config"`
}

// TriggerMetadata holds metadata returned after trigger setup
type TriggerMetadata struct {
	WebhookURL     string `json:"webhookUrl,omitempty"`
	CronExpression string `json:"cronExpression,omitempty"`
	NextRun        string `json:"nextRun,omitempty"`
}

// TriggerEvent represents an incoming trigger event
type TriggerEvent struct {
	Type           string                 // Trigger type that fired
	Source         string                 // Source identifier (e.g., webhook path, cron job id)
	TriggerNodeId  string                 // Specific trigger node ID that fired
	Payload        map[string]interface{} // Event payload
	Headers        map[string]string      // HTTP headers (for webhook)
	Timestamp      int64                  // Unix timestamp
}

// TriggerHandler is called when a trigger fires
type TriggerHandler func(ctx context.Context, instanceId int, event TriggerEvent) error

// Trigger defines the interface for pipeline triggers
type Trigger interface {
	// Type returns the trigger type (e.g., "webhook", "cron", "gmail")
	Type() string

	// Setup is called when an agent instance is deployed
	// Returns metadata about the configured trigger
	Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error)

	// Teardown is called when an agent instance is undeployed
	Teardown(ctx context.Context, instanceId int) error

	// ValidateConfig validates trigger configuration
	ValidateConfig(config map[string]interface{}) error
}

// ActiveTrigger is a trigger that actively listens for events
// (e.g., webhook server, email poller)
type ActiveTrigger interface {
	Trigger

	// Start begins listening for events
	Start(ctx context.Context, handler TriggerHandler) error

	// Stop stops listening
	Stop(ctx context.Context) error
}

// ScheduledTrigger is a trigger based on time schedules
type ScheduledTrigger interface {
	Trigger

	// GetSchedule returns the cron expression for an instance
	GetSchedule(instanceId int) string
}

// Registry manages trigger implementations
type Registry struct {
	mu       sync.RWMutex
	triggers map[string]Trigger
}

// NewRegistry creates a new trigger registry
func NewRegistry() *Registry {
	return &Registry{
		triggers: make(map[string]Trigger),
	}
}

// Register adds a trigger to the registry
func (r *Registry) Register(trigger Trigger) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	triggerType := trigger.Type()
	if _, exists := r.triggers[triggerType]; exists {
		return fmt.Errorf("trigger already registered: %s", triggerType)
	}

	r.triggers[triggerType] = trigger
	return nil
}

// Get returns a trigger by type
func (r *Registry) Get(triggerType string) (Trigger, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	trigger, ok := r.triggers[triggerType]
	if !ok {
		return nil, fmt.Errorf("no trigger registered: %s", triggerType)
	}
	return trigger, nil
}

// Has checks if a trigger type is registered
func (r *Registry) Has(triggerType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.triggers[triggerType]
	return ok
}

// ListTypes returns all registered trigger types
func (r *Registry) ListTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.triggers))
	for t := range r.triggers {
		types = append(types, t)
	}
	return types
}

// GetAll returns all registered triggers
func (r *Registry) GetAll() []Trigger {
	r.mu.RLock()
	defer r.mu.RUnlock()

	triggers := make([]Trigger, 0, len(r.triggers))
	for _, t := range r.triggers {
		triggers = append(triggers, t)
	}
	return triggers
}
