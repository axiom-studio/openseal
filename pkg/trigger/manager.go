package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/repository"
	"go.uber.org/zap"
)

// Manager coordinates all triggers and routes events to the orchestrator
type Manager struct {
	logger         *zap.SugaredLogger
	registry       *Registry
	handler        TriggerHandler
	mu             sync.RWMutex
	started        bool
	reloadInterval int // seconds between trigger reloads (0 = disabled)
	stopReload     chan struct{}
	triggerRepo    repository.AgentTriggerRepository
}

// NewManager creates a new trigger manager
func NewManager(logger *zap.SugaredLogger, registry *Registry) *Manager {
	if logger == nil {
		// Create a no-op logger for testing
		l, _ := zap.NewDevelopment()
		logger = l.Sugar()
	}
	return &Manager{
		logger:   logger,
		registry: registry,
	}
}

// Start starts all active triggers
func (m *Manager) Start(ctx context.Context, handler TriggerHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return nil
	}

	m.handler = handler

	// Start all active triggers
	for _, trigger := range m.registry.GetAll() {
		if active, ok := trigger.(ActiveTrigger); ok {
			if err := active.Start(ctx, m.handleTriggerEvent); err != nil {
				m.logger.Errorw("failed to start trigger", "type", trigger.Type(), "error", err)
				return fmt.Errorf("failed to start trigger %s: %w", trigger.Type(), err)
			}
			m.logger.Infow("started trigger", "type", trigger.Type())
		}
	}

	m.started = true

	// Start auto-reload goroutine if enabled
	if m.reloadInterval > 0 && m.triggerRepo != nil {
		m.stopReload = make(chan struct{})
		go m.reloadLoop(ctx)
		m.logger.Infow("trigger auto-reload enabled", "intervalSeconds", m.reloadInterval)
	}

	return nil
}

// Stop stops all active triggers
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil
	}

	// Stop reload loop
	if m.stopReload != nil {
		close(m.stopReload)
		m.stopReload = nil
	}

	var lastErr error
	for _, trigger := range m.registry.GetAll() {
		if active, ok := trigger.(ActiveTrigger); ok {
			if err := active.Stop(ctx); err != nil {
				m.logger.Errorw("failed to stop trigger", "type", trigger.Type(), "error", err)
				lastErr = err
			} else {
				m.logger.Infow("stopped trigger", "type", trigger.Type())
			}
		}
	}

	m.started = false
	m.handler = nil
	return lastErr
}

// SetupTrigger configures a trigger for an agent instance
func (m *Manager) SetupTrigger(ctx context.Context, instanceId int, triggerConfig TriggerConfig) (*TriggerMetadata, error) {
	trigger, err := m.registry.Get(triggerConfig.Type)
	if err != nil {
		return nil, err
	}

	if err := trigger.ValidateConfig(triggerConfig.Config); err != nil {
		return nil, fmt.Errorf("invalid trigger config: %w", err)
	}

	metadata, err := trigger.Setup(ctx, instanceId, triggerConfig.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to setup trigger: %w", err)
	}

	m.logger.Infow("trigger configured", "type", triggerConfig.Type, "instanceId", instanceId)
	return metadata, nil
}

// TeardownTrigger removes trigger configuration for an agent instance
func (m *Manager) TeardownTrigger(ctx context.Context, instanceId int, triggerType string) error {
	trigger, err := m.registry.Get(triggerType)
	if err != nil {
		return err
	}

	if err := trigger.Teardown(ctx, instanceId); err != nil {
		return fmt.Errorf("failed to teardown trigger: %w", err)
	}

	m.logger.Infow("trigger removed", "type", triggerType, "instanceId", instanceId)
	return nil
}

// TeardownAllTriggers removes all triggers for an instance
func (m *Manager) TeardownAllTriggers(ctx context.Context, instanceId int) error {
	var lastErr error
	for _, trigger := range m.registry.GetAll() {
		if err := trigger.Teardown(ctx, instanceId); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// handleTriggerEvent is called when any trigger fires
func (m *Manager) handleTriggerEvent(ctx context.Context, instanceId int, event TriggerEvent) error {
	m.mu.RLock()
	handler := m.handler
	m.mu.RUnlock()

	if handler == nil {
		return fmt.Errorf("no handler registered")
	}

	m.logger.Infow("trigger event received",
		"type", event.Type,
		"instanceId", instanceId,
		"source", event.Source,
	)

	return handler(ctx, instanceId, event)
}

// HandleWebhook routes a webhook request to the appropriate trigger
func (m *Manager) HandleWebhook(ctx context.Context, path string, payload map[string]interface{}, headers map[string]string) error {
	_, _, err := m.HandleWebhookWithResult(ctx, path, payload, headers)
	return err
}

// HandleWebhookWithResult routes a webhook request and returns detailed result for sync mode
func (m *Manager) HandleWebhookWithResult(ctx context.Context, path string, payload map[string]interface{}, headers map[string]string) (*WebhookResult, int, error) {
	trigger, err := m.registry.Get("webhook")
	if err != nil {
		return nil, 0, err
	}

	webhookTrigger, ok := trigger.(*WebhookTrigger)
	if !ok {
		return nil, 0, fmt.Errorf("webhook trigger not properly configured")
	}

	return webhookTrigger.HandleWebhookWithResult(ctx, path, payload, headers)
}

// GetTrigger returns a trigger by type
func (m *Manager) GetTrigger(triggerType string) (Trigger, error) {
	return m.registry.Get(triggerType)
}

// TriggerManual manually triggers an agent instance
func (m *Manager) TriggerManual(ctx context.Context, instanceId int, payload map[string]interface{}, userId int, triggerNodeId string) error {
	// Manual trigger always works, even if not explicitly configured
	m.mu.RLock()
	handler := m.handler
	m.mu.RUnlock()

	if handler == nil {
		return fmt.Errorf("no handler registered")
	}

	event := TriggerEvent{
		Type:          "manual",
		Source:        "api",
		TriggerNodeId: triggerNodeId,
		Payload: map[string]interface{}{
			"triggeredBy": userId,
			"data":        payload,
		},
	}

	return handler(ctx, instanceId, event)
}

// GetTriggerMetadata returns metadata for a configured trigger
func (m *Manager) GetTriggerMetadata(instanceId int, triggerType string) (*TriggerMetadata, error) {
	trigger, err := m.registry.Get(triggerType)
	if err != nil {
		return nil, err
	}

	// Re-setup to get metadata (idempotent)
	return trigger.Setup(context.Background(), instanceId, nil)
}

// ParseTriggerConfig parses trigger configuration from JSON
func ParseTriggerConfig(data string) (*TriggerConfig, error) {
	if data == "" {
		return &TriggerConfig{Type: "manual"}, nil
	}

	var config TriggerConfig
	if err := json.Unmarshal([]byte(data), &config); err != nil {
		return nil, err
	}

	if config.Type == "" {
		config.Type = "manual"
	}

	return &config, nil
}

// LoadCronTriggersFromDB loads all cron triggers from the database and registers them
// This should be called on startup before Start() to ensure cron triggers survive restarts
func (m *Manager) LoadCronTriggersFromDB(ctx context.Context, triggerRepo repository.AgentTriggerRepository) error {
	cronTriggers, err := triggerRepo.FindAllCronTriggers()
	if err != nil {
		m.logger.Errorw("failed to load cron triggers from database", "error", err)
		return err
	}

	if len(cronTriggers) == 0 {
		m.logger.Info("no cron triggers found in database")
		return nil
	}

	cronTrigger, err := m.registry.Get("cron")
	if err != nil {
		m.logger.Errorw("cron trigger not registered", "error", err)
		return err
	}

	loaded := 0
	for _, trigger := range cronTriggers {
		config := map[string]interface{}{
			"expression": trigger.CronExpression,
		}

		_, err := cronTrigger.Setup(ctx, trigger.AgentInstanceId, config)
		if err != nil {
			m.logger.Warnw("failed to setup cron trigger",
				"instanceId", trigger.AgentInstanceId,
				"expression", trigger.CronExpression,
				"error", err)
			continue
		}
		loaded++
	}

	m.logger.Infow("loaded cron triggers from database", "count", loaded, "total", len(cronTriggers))
	return nil
}

func (m *Manager) LoadK8sTriggersFromDB(ctx context.Context, triggerRepo repository.AgentTriggerRepository) error {
	k8sTriggers, err := triggerRepo.FindAllK8sTriggers()
	if err != nil {
		m.logger.Errorw("failed to load k8s triggers from database", "error", err)
		return err
	}

	if len(k8sTriggers) == 0 {
		m.logger.Info("no k8s triggers found in database")
		return nil
	}

	loadedEvent := 0
	loadedWatch := 0
	for _, trig := range k8sTriggers {
		if trig.TriggerType != "k8s-event" && trig.TriggerType != "k8s-watch" {
			continue
		}

		trigger, err := m.registry.Get(trig.TriggerType)
		if err != nil {
			m.logger.Warnw("trigger type not registered", "triggerType", trig.TriggerType, "error", err)
			continue
		}

		var configMap map[string]interface{}
		if err := json.Unmarshal([]byte(trig.Config), &configMap); err != nil {
			m.logger.Warnw("failed to parse trigger config",
				"instanceId", trig.AgentInstanceId,
				"triggerType", trig.TriggerType,
				"error", err)
			continue
		}

		_, err = trigger.Setup(ctx, trig.AgentInstanceId, configMap)
		if err != nil {
			m.logger.Warnw("failed to setup k8s trigger",
				"instanceId", trig.AgentInstanceId,
				"triggerType", trig.TriggerType,
				"error", err)
			continue
		}

		if trig.TriggerType == "k8s-event" {
			loadedEvent++
		} else {
			loadedWatch++
		}
	}

	m.logger.Infow("loaded k8s triggers from database", "k8s-event", loadedEvent, "k8s-watch", loadedWatch)
	return nil
}

// SetReloadInterval configures automatic trigger reloading
// interval: seconds between reloads (0 to disable)
// repo: repository for fetching trigger data
func (m *Manager) SetReloadInterval(interval int, repo repository.AgentTriggerRepository) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloadInterval = interval
	m.triggerRepo = repo
}

// reloadLoop periodically checks for trigger changes and reloads them
func (m *Manager) reloadLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(m.reloadInterval) * time.Second)
	defer ticker.Stop()

	m.logger.Infow("trigger reload loop started", "intervalSeconds", m.reloadInterval)

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("trigger reload loop stopped (context cancelled)")
			return
		case <-m.stopReload:
			m.logger.Info("trigger reload loop stopped")
			return
		case <-ticker.C:
			if err := m.reloadTriggers(ctx); err != nil {
				m.logger.Errorw("failed to reload triggers", "error", err)
			}
		}
	}
}

// reloadTriggers checks for changes and reloads all triggers
func (m *Manager) reloadTriggers(ctx context.Context) error {
	m.logger.Debug("checking for trigger changes...")

	// Reload cron triggers
	if err := m.reloadCronTriggers(ctx); err != nil {
		m.logger.Errorw("failed to reload cron triggers", "error", err)
	}

	// Reload K8s triggers
	if err := m.reloadK8sTriggers(ctx); err != nil {
		m.logger.Errorw("failed to reload k8s triggers", "error", err)
	}

	return nil
}

// reloadCronTriggers reloads cron triggers from database
func (m *Manager) reloadCronTriggers(ctx context.Context) error {
	cronTriggers, err := m.triggerRepo.FindAllCronTriggers()
	if err != nil {
		return fmt.Errorf("failed to fetch cron triggers: %w", err)
	}

	cronTrigger, err := m.registry.Get("cron")
	if err != nil {
		return fmt.Errorf("cron trigger not registered: %w", err)
	}

	// Build map of current triggers from DB
	dbTriggers := make(map[int]string) // instanceId -> cronExpression
	for _, trig := range cronTriggers {
		dbTriggers[trig.AgentInstanceId] = trig.CronExpression
	}

	// Get existing instances from the cron trigger
	cronTriggerImpl, ok := cronTrigger.(*CronTrigger)
	if !ok {
		return fmt.Errorf("cron trigger type assertion failed")
	}

	cronTriggerImpl.mu.RLock()
	existingInstances := make(map[int]string) // instanceId -> cronExpression
	for instanceId, instance := range cronTriggerImpl.instances {
		existingInstances[instanceId] = instance.expression
	}
	cronTriggerImpl.mu.RUnlock()

	// Detect changes
	added := 0
	updated := 0
	removed := 0

	// Check for new or updated triggers
	for instanceId, expression := range dbTriggers {
		existing, exists := existingInstances[instanceId]
		if !exists {
			// New trigger - setup
			config := map[string]interface{}{"expression": expression}
			if _, err := cronTrigger.Setup(ctx, instanceId, config); err != nil {
				m.logger.Warnw("failed to setup new cron trigger", "instanceId", instanceId, "error", err)
			} else {
				added++
				m.logger.Infow("added cron trigger", "instanceId", instanceId, "expression", expression)
			}
		} else if existing != expression {
			// Updated trigger - teardown and re-setup
			if err := cronTrigger.Teardown(ctx, instanceId); err != nil {
				m.logger.Warnw("failed to teardown cron trigger for update", "instanceId", instanceId, "error", err)
			}
			config := map[string]interface{}{"expression": expression}
			if _, err := cronTrigger.Setup(ctx, instanceId, config); err != nil {
				m.logger.Warnw("failed to re-setup cron trigger", "instanceId", instanceId, "error", err)
			} else {
				updated++
				m.logger.Infow("updated cron trigger", "instanceId", instanceId, "expression", expression)
			}
		}
	}

	// Check for removed triggers
	for instanceId := range existingInstances {
		if _, exists := dbTriggers[instanceId]; !exists {
			if err := cronTrigger.Teardown(ctx, instanceId); err != nil {
				m.logger.Warnw("failed to teardown removed cron trigger", "instanceId", instanceId, "error", err)
			} else {
				removed++
				m.logger.Infow("removed cron trigger", "instanceId", instanceId)
			}
		}
	}

	if added > 0 || updated > 0 || removed > 0 {
		m.logger.Infow("cron triggers reloaded", "added", added, "updated", updated, "removed", removed)
	}

	return nil
}

// reloadK8sTriggers reloads K8s triggers from database
func (m *Manager) reloadK8sTriggers(ctx context.Context) error {
	k8sTriggers, err := m.triggerRepo.FindAllK8sTriggers()
	if err != nil {
		return fmt.Errorf("failed to fetch k8s triggers: %w", err)
	}

	eventAdded, eventUpdated, eventRemoved := 0, 0, 0
	watchAdded, watchUpdated, watchRemoved := 0, 0, 0

	// Process each trigger type
	for _, trigType := range []string{"k8s-event", "k8s-watch"} {
		trigger, err := m.registry.Get(trigType)
		if err != nil {
			continue
		}

		// Build map of current triggers from DB for this type
		dbTriggers := make(map[int]string) // instanceId -> config JSON
		for _, trig := range k8sTriggers {
			if trig.TriggerType == trigType {
				dbTriggers[trig.AgentInstanceId] = trig.Config
			}
		}

		// Get existing instances
		existingInstances := make(map[int]string)
		if trigType == "k8s-event" {
			if eventTrigger, ok := trigger.(*K8sEventTriggerHTTP); ok {
				eventTrigger.mu.RLock()
				for instanceId, instance := range eventTrigger.instances {
					configJSON, _ := json.Marshal(instance.config)
					existingInstances[instanceId] = string(configJSON)
				}
				eventTrigger.mu.RUnlock()
			}
		} else if trigType == "k8s-watch" {
			if watchTrigger, ok := trigger.(*K8sWatchTriggerHTTP); ok {
				watchTrigger.mu.RLock()
				for instanceId, instance := range watchTrigger.instances {
					configJSON, _ := json.Marshal(instance.config)
					existingInstances[instanceId] = string(configJSON)
				}
				watchTrigger.mu.RUnlock()
			}
		}

		// Detect changes
		for instanceId, configJSON := range dbTriggers {
			var configMap map[string]interface{}
			if err := json.Unmarshal([]byte(configJSON), &configMap); err != nil {
				m.logger.Warnw("failed to parse trigger config", "instanceId", instanceId, "error", err)
				continue
			}

			existing, exists := existingInstances[instanceId]
			if !exists {
				// New trigger
				if _, err := trigger.Setup(ctx, instanceId, configMap); err != nil {
					m.logger.Warnw("failed to setup new k8s trigger", "instanceId", instanceId, "type", trigType, "error", err)
				} else {
					if trigType == "k8s-event" {
						eventAdded++
					} else {
						watchAdded++
					}
					m.logger.Infow("added k8s trigger", "instanceId", instanceId, "type", trigType)
				}
			} else if existing != configJSON {
				// Updated trigger
				if err := trigger.Teardown(ctx, instanceId); err != nil {
					m.logger.Warnw("failed to teardown k8s trigger for update", "instanceId", instanceId, "error", err)
				}
				if _, err := trigger.Setup(ctx, instanceId, configMap); err != nil {
					m.logger.Warnw("failed to re-setup k8s trigger", "instanceId", instanceId, "type", trigType, "error", err)
				} else {
					if trigType == "k8s-event" {
						eventUpdated++
					} else {
						watchUpdated++
					}
					m.logger.Infow("updated k8s trigger", "instanceId", instanceId, "type", trigType)
				}
			}
		}

		// Check for removed triggers
		for instanceId := range existingInstances {
			if _, exists := dbTriggers[instanceId]; !exists {
				if err := trigger.Teardown(ctx, instanceId); err != nil {
					m.logger.Warnw("failed to teardown removed k8s trigger", "instanceId", instanceId, "error", err)
				} else {
					if trigType == "k8s-event" {
						eventRemoved++
					} else {
						watchRemoved++
					}
					m.logger.Infow("removed k8s trigger", "instanceId", instanceId, "type", trigType)
				}
			}
		}
	}

	if eventAdded+eventUpdated+eventRemoved+watchAdded+watchUpdated+watchRemoved > 0 {
		m.logger.Infow("k8s triggers reloaded",
			"k8s-event-added", eventAdded, "k8s-event-updated", eventUpdated, "k8s-event-removed", eventRemoved,
			"k8s-watch-added", watchAdded, "k8s-watch-updated", watchUpdated, "k8s-watch-removed", watchRemoved)
	}

	return nil
}
