package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/axiom-studio/openseal/pkg/trigger"
	"go.uber.org/zap"
)

// TriggerManager wraps the trigger.Registry and coordinates trigger
// lifecycles for the daemon. It is a standalone version that does NOT
// import cortex repositories — all state comes from DaemonConfig.
type TriggerManager struct {
	logger   *zap.SugaredLogger
	registry *trigger.Registry
	cron     *trigger.CronTrigger
	webhook  *trigger.WebhookTrigger

	// nameToWorkflow maps trigger names (from config) → workflow filenames.
	nameToWorkflow map[string]string

	// nameToInstanceID maps trigger names → synthetic instance IDs.
	// Because the trigger package uses instanceId int, we assign
	// stable IDs starting from 1.
	nameToInstanceID map[string]int
	nextID           int

	mu sync.Mutex
}

// NewTriggerManager builds a registry with cron + webhook triggers ready.
func NewTriggerManager(logger *zap.SugaredLogger, webhookBaseURL string) *TriggerManager {
	tm := &TriggerManager{
		logger:           logger,
		registry:         trigger.NewRegistry(),
		cron:             trigger.NewCronTrigger(),
		webhook:          trigger.NewWebhookTrigger(webhookBaseURL),
		nameToWorkflow:   make(map[string]string),
		nameToInstanceID: make(map[string]int),
		nextID:           1,
	}

	if err := tm.registry.Register(tm.cron); err != nil {
		logger.Warnw("cron trigger already registered", "error", err)
	}
	if err := tm.registry.Register(tm.webhook); err != nil {
		logger.Warnw("webhook trigger already registered", "error", err)
	}

	return tm
}

// RegisterAll sets up every trigger defined in cfg and returns a mapping
// of instanceId → workflow filename for the event loop to use.
func (tm *TriggerManager) RegisterAll(ctx context.Context, cfg *DaemonConfig) (map[int]string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	instanceToWorkflow := make(map[int]string)

	for name, ct := range cfg.Triggers.Cron {
		instanceID := tm.nextID
		tm.nextID++

		config := map[string]interface{}{
			"expression": ct.Expression,
		}

		if _, err := tm.cron.Setup(ctx, instanceID, config); err != nil {
			return nil, fmt.Errorf("setup cron trigger %q: %w", name, err)
		}

		tm.nameToInstanceID[name] = instanceID
		tm.nameToWorkflow[name] = ct.Workflow
		instanceToWorkflow[instanceID] = ct.Workflow

		tm.logger.Infow("registered cron trigger",
			"name", name,
			"expression", ct.Expression,
			"workflow", ct.Workflow,
			"instanceId", instanceID,
		)
	}

	for name, wt := range cfg.Triggers.Webhook {
		instanceID := tm.nextID
		tm.nextID++

		config := map[string]interface{}{}
		if wt.Path != "" {
			config["path"] = wt.Path
		}

		meta, err := tm.webhook.Setup(ctx, instanceID, config)
		if err != nil {
			return nil, fmt.Errorf("setup webhook trigger %q: %w", name, err)
		}

		tm.nameToInstanceID[name] = instanceID
		tm.nameToWorkflow[name] = wt.Workflow
		instanceToWorkflow[instanceID] = wt.Workflow

		tm.logger.Infow("registered webhook trigger",
			"name", name,
			"workflow", wt.Workflow,
			"instanceId", instanceID,
			"url", meta.WebhookURL,
		)
	}

	return instanceToWorkflow, nil
}

// Start activates all registered triggers with the given handler.
func (tm *TriggerManager) Start(ctx context.Context, handler trigger.TriggerHandler) error {
	for _, t := range tm.registry.GetAll() {
		if active, ok := t.(trigger.ActiveTrigger); ok {
			if err := active.Start(ctx, handler); err != nil {
				return fmt.Errorf("start trigger %s: %w", t.Type(), err)
			}
			tm.logger.Infow("started trigger", "type", t.Type())
		}
	}
	return nil
}

// Stop gracefully stops all active triggers.
func (tm *TriggerManager) Stop(ctx context.Context) error {
	var lastErr error
	for _, t := range tm.registry.GetAll() {
		if active, ok := t.(trigger.ActiveTrigger); ok {
			if err := active.Stop(ctx); err != nil {
				tm.logger.Errorw("failed to stop trigger", "type", t.Type(), "error", err)
				lastErr = err
			} else {
				tm.logger.Infow("stopped trigger", "type", t.Type())
			}
		}
	}
	return lastErr
}

// HandleWebhook routes an incoming webhook by path to its handler.
// This is called from the HTTP server when a request arrives.
func (tm *TriggerManager) HandleWebhook(ctx context.Context, path string, payload map[string]interface{}, headers map[string]string) error {
	return tm.webhook.HandleWebhook(ctx, path, payload, headers)
}

// GetWebhookTrigger exposes the underlying webhook trigger for the HTTP server.
func (tm *TriggerManager) GetWebhookTrigger() *trigger.WebhookTrigger {
	return tm.webhook
}

// Registry returns the underlying trigger registry.
func (tm *TriggerManager) Registry() *trigger.Registry {
	return tm.registry
}