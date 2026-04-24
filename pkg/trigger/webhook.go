package trigger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/axiom-studio/openseal/pkg/repository"
)

// WebhookTrigger handles HTTP webhook triggers
type WebhookTrigger struct {
	mu               sync.RWMutex
	baseURL          string
	instances        map[int]*webhookInstance // instanceId -> webhook config (cache)
	paths            map[string]int           // path -> instanceId (cache)
	handler          TriggerHandler
	handlerWithRunId TriggerHandlerWithRunId
	repository       repository.AgentTriggerRepository
}

// TriggerHandlerWithRunId is a handler that returns the runId
type TriggerHandlerWithRunId func(ctx context.Context, instanceId int, event TriggerEvent) (int, error)

type webhookInstance struct {
	instanceId    int
	nodeId        string
	path          string
	secret        string
	config        map[string]interface{}
}

// NewWebhookTrigger creates a new webhook trigger (in-memory only)
func NewWebhookTrigger(baseURL string) *WebhookTrigger {
	return &WebhookTrigger{
		baseURL:   baseURL,
		instances: make(map[int]*webhookInstance),
		paths:     make(map[string]int),
	}
}

// NewWebhookTriggerWithRepository creates a webhook trigger with database persistence
func NewWebhookTriggerWithRepository(baseURL string, repo repository.AgentTriggerRepository) *WebhookTrigger {
	return &WebhookTrigger{
		baseURL:    baseURL,
		instances:  make(map[int]*webhookInstance),
		paths:      make(map[string]int),
		repository: repo,
	}
}

func (t *WebhookTrigger) Type() string {
	return "webhook"
}

func (t *WebhookTrigger) ValidateConfig(config map[string]interface{}) error {
	// Webhook config is optional - path will be auto-generated if not provided
	return nil
}

func (t *WebhookTrigger) Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Check if already set up
	if existing, ok := t.instances[instanceId]; ok {
		return &TriggerMetadata{
			WebhookURL: fmt.Sprintf("%s/api/v1/agent/webhook/%s", t.baseURL, existing.path),
		}, nil
	}

	// Get or generate path
	path, _ := config["path"].(string)
	if path == "" {
		path = generateWebhookPath()
	}

	// Check for path collision
	if existingId, ok := t.paths[path]; ok && existingId != instanceId {
		return nil, fmt.Errorf("webhook path already in use: %s", path)
	}

	// Generate secret for validation
	secret := generateSecret()

	instance := &webhookInstance{
		instanceId: instanceId,
		path:       path,
		secret:     secret,
		config:     config,
	}

	t.instances[instanceId] = instance
	t.paths[path] = instanceId

	return &TriggerMetadata{
		WebhookURL: fmt.Sprintf("%s/api/v1/agent/webhook/%s", t.baseURL, path),
	}, nil
}

func (t *WebhookTrigger) Teardown(ctx context.Context, instanceId int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	instance, ok := t.instances[instanceId]
	if !ok {
		return nil // Already removed
	}

	delete(t.paths, instance.path)
	delete(t.instances, instanceId)
	return nil
}

// Start implements ActiveTrigger - webhook server is managed externally
func (t *WebhookTrigger) Start(ctx context.Context, handler TriggerHandler) error {
	t.handler = handler
	return nil
}

// Stop implements ActiveTrigger
func (t *WebhookTrigger) Stop(ctx context.Context) error {
	t.handler = nil
	return nil
}

// HandleWebhook processes an incoming webhook request
// Called by the HTTP router
func (t *WebhookTrigger) HandleWebhook(ctx context.Context, path string, payload map[string]interface{}, headers map[string]string) error {
	_, _, err := t.HandleWebhookWithResult(ctx, path, payload, headers)
	return err
}

// WebhookResult contains the result of handling a webhook
type WebhookResult struct {
	InstanceId   int
	RunId        int
	ResponseMode string // "async" or "sync"
	Timeout      int    // seconds for sync mode
}

// HandleWebhookWithResult processes an incoming webhook request and returns detailed result
// This is used for sync webhook mode
func (t *WebhookTrigger) HandleWebhookWithResult(ctx context.Context, path string, payload map[string]interface{}, headers map[string]string) (*WebhookResult, int, error) {
	var instanceId int
	var nodeId string
	var config map[string]interface{}
	var found bool

	// Try to find from repository first (persistent storage)
	if t.repository != nil {
		trigger, err := t.repository.FindByWebhookPath(path)
		if err == nil && trigger != nil && trigger.Enabled {
			instanceId = trigger.AgentInstanceId
			nodeId = trigger.NodeId
			if trigger.Config != "" {
				json.Unmarshal([]byte(trigger.Config), &config)
			}
			found = true
		}
	}

	// Fall back to in-memory cache
	if !found {
		t.mu.RLock()
		instanceId, found = t.paths[path]
		if found {
			if inst := t.instances[instanceId]; inst != nil {
				nodeId = inst.nodeId
				config = inst.config
			}
		}
		t.mu.RUnlock()
	}

	if !found {
		return nil, 0, fmt.Errorf("webhook not found: %s", path)
	}

	if t.handler == nil {
		return nil, 0, fmt.Errorf("no handler registered")
	}

	// Include nodeId in the payload for graph-based execution
	eventPayload := payload
	if eventPayload == nil {
		eventPayload = make(map[string]interface{})
	}
	eventPayload["__triggerNodeId"] = nodeId

	event := TriggerEvent{
		Type:    "webhook",
		Source:  path,
		Payload: eventPayload,
		Headers: headers,
	}

	// Update trigger statistics if repository is available
	if t.repository != nil {
		t.repository.UpdateLastTriggered(path)
	}

	// Get response mode from config
	responseMode := "async"
	timeout := 30
	if config != nil {
		if rm, ok := config["responseMode"].(string); ok && rm == "sync" {
			responseMode = "sync"
		}
		if to, ok := config["timeout"].(int); ok && to > 0 {
			timeout = to
		}
		if to, ok := config["timeout"].(float64); ok && to > 0 {
			timeout = int(to)
		}
	}

	result := &WebhookResult{
		InstanceId:   instanceId,
		ResponseMode: responseMode,
		Timeout:      timeout,
	}

	// Call the handler - this triggers the workflow
	// The handler should return the runId somehow
	// For now, we need to modify the handler interface
	if t.handlerWithRunId == nil {
		return nil, 0, fmt.Errorf("handler with runId not configured")
	}
	runId, err := t.handlerWithRunId(ctx, instanceId, event)
	if err != nil {
		return nil, 0, err
	}

	result.RunId = runId
	return result, runId, nil
}

// SetHandlerWithRunId sets the handler that returns runId
func (t *WebhookTrigger) SetHandlerWithRunId(h TriggerHandlerWithRunId) {
	t.handlerWithRunId = h
}

// GetInstanceByPath returns the instance ID for a webhook path
func (t *WebhookTrigger) GetInstanceByPath(path string) (int, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.paths[path]
	return id, ok
}

// GetPath returns the webhook path for an instance
func (t *WebhookTrigger) GetPath(instanceId int) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	instance, ok := t.instances[instanceId]
	if !ok {
		return "", false
	}
	return instance.path, true
}

func generateWebhookPath() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func generateSecret() string {
	bytes := make([]byte, 32)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
