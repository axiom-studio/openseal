package trigger

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()

	webhook := NewWebhookTrigger("http://localhost")
	if err := r.Register(webhook); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	got, err := r.Get("webhook")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Type() != "webhook" {
		t.Errorf("Expected type 'webhook', got %q", got.Type())
	}

	// Duplicate registration should fail
	if err := r.Register(webhook); err == nil {
		t.Error("Expected error for duplicate registration")
	}
}

func TestRegistryListTypes(t *testing.T) {
	r, _ := NewDefaultRegistry("http://localhost")

	types := r.ListTypes()
	if len(types) != 3 {
		t.Errorf("Expected 3 types, got %d", len(types))
	}

	typeMap := make(map[string]bool)
	for _, typ := range types {
		typeMap[typ] = true
	}

	if !typeMap["webhook"] || !typeMap["cron"] || !typeMap["manual"] {
		t.Error("Missing expected trigger types")
	}
}

func TestWebhookTriggerSetup(t *testing.T) {
	webhook := NewWebhookTrigger("http://localhost:8080")

	metadata, err := webhook.Setup(context.Background(), 1, map[string]interface{}{})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	if metadata.WebhookURL == "" {
		t.Error("Expected webhook URL")
	}

	// Verify path was generated
	path, ok := webhook.GetPath(1)
	if !ok {
		t.Error("Expected path to be set")
	}
	if path == "" {
		t.Error("Expected non-empty path")
	}

	// Verify instance lookup
	instanceId, ok := webhook.GetInstanceByPath(path)
	if !ok || instanceId != 1 {
		t.Errorf("Expected instance 1, got %d", instanceId)
	}
}

func TestWebhookTriggerCustomPath(t *testing.T) {
	webhook := NewWebhookTrigger("http://localhost:8080")

	metadata, err := webhook.Setup(context.Background(), 1, map[string]interface{}{
		"path": "my-custom-path",
	})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	if metadata.WebhookURL != "http://localhost:8080/api/v1/agent/webhook/my-custom-path" {
		t.Errorf("Unexpected webhook URL: %s", metadata.WebhookURL)
	}

	path, _ := webhook.GetPath(1)
	if path != "my-custom-path" {
		t.Errorf("Expected 'my-custom-path', got %q", path)
	}
}

func TestWebhookTriggerTeardown(t *testing.T) {
	webhook := NewWebhookTrigger("http://localhost:8080")

	webhook.Setup(context.Background(), 1, map[string]interface{}{})

	if err := webhook.Teardown(context.Background(), 1); err != nil {
		t.Fatalf("Teardown failed: %v", err)
	}

	_, ok := webhook.GetPath(1)
	if ok {
		t.Error("Expected path to be removed")
	}
}

func TestWebhookTriggerHandleWebhook(t *testing.T) {
	webhook := NewWebhookTrigger("http://localhost:8080")

	var receivedEvent TriggerEvent
	var receivedInstanceId int
	var wg sync.WaitGroup
	wg.Add(1)

	webhook.Start(context.Background(), func(ctx context.Context, instanceId int, event TriggerEvent) error {
		receivedInstanceId = instanceId
		receivedEvent = event
		wg.Done()
		return nil
	})

	webhook.Setup(context.Background(), 42, map[string]interface{}{
		"path": "test-hook",
	})

	err := webhook.HandleWebhook(context.Background(), "test-hook", map[string]interface{}{
		"data": "test",
	}, map[string]string{
		"Content-Type": "application/json",
	})
	if err != nil {
		t.Fatalf("HandleWebhook failed: %v", err)
	}

	wg.Wait()

	if receivedInstanceId != 42 {
		t.Errorf("Expected instance 42, got %d", receivedInstanceId)
	}
	if receivedEvent.Type != "webhook" {
		t.Errorf("Expected type 'webhook', got %q", receivedEvent.Type)
	}
	if receivedEvent.Payload["data"] != "test" {
		t.Error("Payload not passed correctly")
	}
}

func TestCronTriggerValidateConfig(t *testing.T) {
	cron := NewCronTrigger()

	// Missing expression
	if err := cron.ValidateConfig(map[string]interface{}{}); err == nil {
		t.Error("Expected error for missing expression")
	}

	// Invalid expression
	if err := cron.ValidateConfig(map[string]interface{}{
		"expression": "invalid",
	}); err == nil {
		t.Error("Expected error for invalid expression")
	}

	// Valid expression
	if err := cron.ValidateConfig(map[string]interface{}{
		"expression": "0 * * * * *",
	}); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestCronTriggerSetup(t *testing.T) {
	cron := NewCronTrigger()

	metadata, err := cron.Setup(context.Background(), 1, map[string]interface{}{
		"expression": "0 0 * * * *",
	})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	if metadata.CronExpression != "0 0 * * * *" {
		t.Errorf("Expected expression '0 0 * * * *', got %q", metadata.CronExpression)
	}

	schedule := cron.GetSchedule(1)
	if schedule != "0 0 * * * *" {
		t.Errorf("Expected schedule '0 0 * * * *', got %q", schedule)
	}
}

func TestCronTriggerFires(t *testing.T) {
	cron := NewCronTrigger()

	var fired bool
	var firedInstanceId int
	var mu sync.Mutex

	cron.Start(context.Background(), func(ctx context.Context, instanceId int, event TriggerEvent) error {
		mu.Lock()
		fired = true
		firedInstanceId = instanceId
		mu.Unlock()
		return nil
	})
	defer cron.Stop(context.Background())

	// Schedule to run every second
	cron.Setup(context.Background(), 99, map[string]interface{}{
		"expression": "* * * * * *",
	})

	// Wait for it to fire
	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if !fired {
		t.Error("Expected cron to fire")
	}
	if firedInstanceId != 99 {
		t.Errorf("Expected instance 99, got %d", firedInstanceId)
	}
}

func TestManualTrigger(t *testing.T) {
	manual := NewManualTrigger()

	var receivedEvent TriggerEvent
	var receivedInstanceId int

	manual.Start(context.Background(), func(ctx context.Context, instanceId int, event TriggerEvent) error {
		receivedInstanceId = instanceId
		receivedEvent = event
		return nil
	})

	manual.Setup(context.Background(), 1, nil)

	err := manual.Trigger(context.Background(), 1, map[string]interface{}{
		"key": "value",
	}, 42)
	if err != nil {
		t.Fatalf("Trigger failed: %v", err)
	}

	if receivedInstanceId != 1 {
		t.Errorf("Expected instance 1, got %d", receivedInstanceId)
	}
	if receivedEvent.Type != "manual" {
		t.Errorf("Expected type 'manual', got %q", receivedEvent.Type)
	}
}

func TestManagerSetupAndTeardown(t *testing.T) {
	registry, _ := NewDefaultRegistry("http://localhost")
	manager := NewManager(nil, registry)

	// Setup webhook trigger
	metadata, err := manager.SetupTrigger(context.Background(), 1, TriggerConfig{
		Type:   "webhook",
		Config: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("SetupTrigger failed: %v", err)
	}
	if metadata.WebhookURL == "" {
		t.Error("Expected webhook URL")
	}

	// Teardown
	if err := manager.TeardownTrigger(context.Background(), 1, "webhook"); err != nil {
		t.Fatalf("TeardownTrigger failed: %v", err)
	}
}

func TestManagerHandleWebhook(t *testing.T) {
	registry, _ := NewDefaultRegistry("http://localhost")
	manager := NewManager(nil, registry)

	var received bool
	manager.Start(context.Background(), func(ctx context.Context, instanceId int, event TriggerEvent) error {
		received = true
		return nil
	})
	defer manager.Stop(context.Background())

	manager.SetupTrigger(context.Background(), 1, TriggerConfig{
		Type: "webhook",
		Config: map[string]interface{}{
			"path": "test-path",
		},
	})

	err := manager.HandleWebhook(context.Background(), "test-path", map[string]interface{}{}, nil)
	if err != nil {
		t.Fatalf("HandleWebhook failed: %v", err)
	}

	if !received {
		t.Error("Expected handler to be called")
	}
}

func TestParseTriggerConfig(t *testing.T) {
	// Empty defaults to manual
	config, err := ParseTriggerConfig("")
	if err != nil {
		t.Fatalf("ParseTriggerConfig failed: %v", err)
	}
	if config.Type != "manual" {
		t.Errorf("Expected 'manual', got %q", config.Type)
	}

	// Valid JSON
	config, err = ParseTriggerConfig(`{"type": "webhook", "config": {"path": "test"}}`)
	if err != nil {
		t.Fatalf("ParseTriggerConfig failed: %v", err)
	}
	if config.Type != "webhook" {
		t.Errorf("Expected 'webhook', got %q", config.Type)
	}
	if config.Config["path"] != "test" {
		t.Error("Config not parsed correctly")
	}
}
