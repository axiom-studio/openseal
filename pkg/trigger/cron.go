package trigger

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// CronTrigger handles scheduled triggers
type CronTrigger struct {
	mu        sync.RWMutex
	scheduler *cron.Cron
	instances map[int]*cronInstance // instanceId -> cron config
	handler   TriggerHandler
	started   bool
}

type cronInstance struct {
	instanceId int
	expression string
	entryId    cron.EntryID
	config     map[string]interface{}
}

// NewCronTrigger creates a new cron trigger
func NewCronTrigger() *CronTrigger {
	return &CronTrigger{
		scheduler: cron.New(cron.WithSeconds()),
		instances: make(map[int]*cronInstance),
	}
}

func (t *CronTrigger) Type() string {
	return "cron"
}

func (t *CronTrigger) ValidateConfig(config map[string]interface{}) error {
	expression, ok := config["expression"].(string)
	if !ok || expression == "" {
		return fmt.Errorf("cron trigger requires 'expression'")
	}

	// Validate cron expression
	parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(expression); err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}

	return nil
}

func (t *CronTrigger) Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error) {
	if err := t.ValidateConfig(config); err != nil {
		return nil, err
	}

	expression := config["expression"].(string)

	t.mu.Lock()
	defer t.mu.Unlock()

	// Remove existing if any
	if existing, ok := t.instances[instanceId]; ok {
		t.scheduler.Remove(existing.entryId)
	}

	instance := &cronInstance{
		instanceId: instanceId,
		expression: expression,
		config:     config,
	}

	// Schedule if handler is set (i.e., trigger is started)
	if t.handler != nil {
		entryId, err := t.scheduler.AddFunc(expression, func() {
			t.fireTrigger(instanceId)
		})
		if err != nil {
			return nil, fmt.Errorf("failed to schedule: %w", err)
		}
		instance.entryId = entryId
	}

	t.instances[instanceId] = instance

	// Calculate next run
	var nextRun string
	if t.started {
		entry := t.scheduler.Entry(instance.entryId)
		if !entry.Next.IsZero() {
			nextRun = entry.Next.Format(time.RFC3339)
		}
	}

	return &TriggerMetadata{
		CronExpression: expression,
		NextRun:        nextRun,
	}, nil
}

func (t *CronTrigger) Teardown(ctx context.Context, instanceId int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	instance, ok := t.instances[instanceId]
	if !ok {
		return nil
	}

	t.scheduler.Remove(instance.entryId)
	delete(t.instances, instanceId)
	return nil
}

// Start implements ActiveTrigger
func (t *CronTrigger) Start(ctx context.Context, handler TriggerHandler) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.handler = handler

	// Schedule all existing instances
	for instanceId, instance := range t.instances {
		id := instanceId // Capture for closure
		entryId, err := t.scheduler.AddFunc(instance.expression, func() {
			t.fireTrigger(id)
		})
		if err != nil {
			return fmt.Errorf("failed to schedule instance %d: %w", instanceId, err)
		}
		instance.entryId = entryId
	}

	t.scheduler.Start()
	t.started = true
	return nil
}

// Stop implements ActiveTrigger
func (t *CronTrigger) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	cronCtx := t.scheduler.Stop()
	select {
	case <-cronCtx.Done():
	case <-ctx.Done():
		return ctx.Err()
	}

	t.started = false
	t.handler = nil
	return nil
}

// GetSchedule implements ScheduledTrigger
func (t *CronTrigger) GetSchedule(instanceId int) string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	instance, ok := t.instances[instanceId]
	if !ok {
		return ""
	}
	return instance.expression
}

func (t *CronTrigger) fireTrigger(instanceId int) {
	t.mu.RLock()
	handler := t.handler
	instance := t.instances[instanceId]
	t.mu.RUnlock()

	if handler == nil || instance == nil {
		return
	}

	event := TriggerEvent{
		Type:   "cron",
		Source: instance.expression,
		Payload: map[string]interface{}{
			"scheduledAt": time.Now().UTC().Format(time.RFC3339),
			"expression":  instance.expression,
		},
		Timestamp: time.Now().Unix(),
	}

	ctx := context.Background()
	if err := handler(ctx, instanceId, event); err != nil {
		// Log error - in production this would use a logger
		fmt.Printf("cron trigger failed for instance %d: %v\n", instanceId, err)
	}
}

// GetNextRun returns the next scheduled run time for an instance
func (t *CronTrigger) GetNextRun(instanceId int) (time.Time, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	instance, ok := t.instances[instanceId]
	if !ok {
		return time.Time{}, false
	}

	entry := t.scheduler.Entry(instance.entryId)
	return entry.Next, !entry.Next.IsZero()
}
