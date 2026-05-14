package trigger

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func newTestK8sEventTrigger() *K8sEventTrigger {
	logger, _ := zap.NewDevelopment()
	return NewK8sEventTrigger(nil, nil, nil, nil, logger.Sugar())
}

func TestK8sEventTrigger_Type(t *testing.T) {
	trigger := newTestK8sEventTrigger()
	if trigger.Type() != "k8s-event" {
		t.Errorf("Expected type 'k8s-event', got %s", trigger.Type())
	}
}

func TestK8sEventTrigger_ValidateConfig(t *testing.T) {
	trigger := newTestK8sEventTrigger()

	configs := []map[string]interface{}{
		{},
		{"namespace": "default"},
		{"reason": "BackOff"},
		{"type": "Warning"},
		{"involvedObjectKind": "Pod"},
	}

	for _, config := range configs {
		err := trigger.ValidateConfig(config)
		if err != nil {
			t.Errorf("ValidateConfig failed for %+v: %v", config, err)
		}
	}
}

func TestK8sEventTrigger_Setup(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]interface{}
	}{
		{
			name:   "minimal config",
			config: map[string]interface{}{},
		},
		{
			name: "with namespace",
			config: map[string]interface{}{
				"namespace": "default",
			},
		},
		{
			name: "with reason filter",
			config: map[string]interface{}{
				"reason": "BackOff",
			},
		},
		{
			name: "with type filter",
			config: map[string]interface{}{
				"type": "Warning",
			},
		},
		{
			name: "with involved object kind",
			config: map[string]interface{}{
				"involvedObjectKind": "Pod",
			},
		},
		{
			name: "with label selector",
			config: map[string]interface{}{
				"labelSelector": "app=nginx",
			},
		},
		{
			name: "with field selector",
			config: map[string]interface{}{
				"fieldSelector": "reason=BackOff",
			},
		},
		{
			name: "all filters",
			config: map[string]interface{}{
				"namespace":          "default",
				"reason":             "BackOff",
				"type":               "Warning",
				"involvedObjectKind": "Pod",
				"labelSelector":      "app=nginx",
				"fieldSelector":      "reason=BackOff",
			},
		},
	}

	trigger := newTestK8sEventTrigger()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata, err := trigger.Setup(context.Background(), 1, tt.config)
			if err != nil {
				t.Errorf("Setup failed: %v", err)
			}
			if metadata == nil {
				t.Error("Expected metadata, got nil")
			}
		})
	}
}

func TestK8sEventTrigger_SetupTeardown(t *testing.T) {
	trigger := newTestK8sEventTrigger()

	config := map[string]interface{}{
		"namespace": "default",
		"reason":    "BackOff",
	}

	_, err := trigger.Setup(context.Background(), 1, config)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	trigger.mu.RLock()
	if _, exists := trigger.instances[1]; !exists {
		t.Error("Instance should exist after setup")
	}
	trigger.mu.RUnlock()

	err = trigger.Teardown(context.Background(), 1)
	if err != nil {
		t.Errorf("Teardown failed: %v", err)
	}

	trigger.mu.RLock()
	if _, exists := trigger.instances[1]; exists {
		t.Error("Instance should be removed after teardown")
	}
	trigger.mu.RUnlock()
}

func TestK8sEventTrigger_MultipleInstances(t *testing.T) {
	trigger := newTestK8sEventTrigger()

	configs := []map[string]interface{}{
		{"namespace": "default", "reason": "BackOff"},
		{"namespace": "kube-system", "type": "Warning"},
		{"involvedObjectKind": "Pod"},
	}

	for i, config := range configs {
		_, err := trigger.Setup(context.Background(), i+1, config)
		if err != nil {
			t.Errorf("Setup instance %d failed: %v", i+1, err)
		}
	}

	trigger.mu.RLock()
	if len(trigger.instances) != 3 {
		t.Errorf("Expected 3 instances, got %d", len(trigger.instances))
	}
	trigger.mu.RUnlock()

	for i := range configs {
		err := trigger.Teardown(context.Background(), i+1)
		if err != nil {
			t.Errorf("Teardown instance %d failed: %v", i+1, err)
		}
	}

	trigger.mu.RLock()
	if len(trigger.instances) != 0 {
		t.Errorf("Expected 0 instances after teardown, got %d", len(trigger.instances))
	}
	trigger.mu.RUnlock()
}

func TestK8sEventTrigger_ReplaceInstance(t *testing.T) {
	trigger := newTestK8sEventTrigger()

	config1 := map[string]interface{}{
		"namespace": "default",
	}

	_, err := trigger.Setup(context.Background(), 1, config1)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	config2 := map[string]interface{}{
		"namespace": "kube-system",
		"reason":    "BackOff",
	}

	_, err = trigger.Setup(context.Background(), 1, config2)
	if err != nil {
		t.Fatalf("Replace setup failed: %v", err)
	}

	trigger.mu.RLock()
	if len(trigger.instances) != 1 {
		t.Errorf("Expected 1 instance, got %d", len(trigger.instances))
	}
	if trigger.instances[1].namespace != "kube-system" {
		t.Errorf("Expected namespace 'kube-system', got %q", trigger.instances[1].namespace)
	}
	if trigger.instances[1].reason != "BackOff" {
		t.Errorf("Expected reason 'BackOff', got %q", trigger.instances[1].reason)
	}
	trigger.mu.RUnlock()
}

func TestK8sEventTrigger_InstanceConfig(t *testing.T) {
	trigger := newTestK8sEventTrigger()

	config := map[string]interface{}{
		"namespace":          "test-ns",
		"reason":             "TestReason",
		"type":               "Warning",
		"involvedObjectKind": "Pod",
		"labelSelector":      "app=test",
		"fieldSelector":      "reason=TestReason",
	}

	_, err := trigger.Setup(context.Background(), 1, config)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	trigger.mu.RLock()
	instance := trigger.instances[1]
	trigger.mu.RUnlock()

	if instance.namespace != "test-ns" {
		t.Errorf("Expected namespace 'test-ns', got %q", instance.namespace)
	}
	if instance.reason != "TestReason" {
		t.Errorf("Expected reason 'TestReason', got %q", instance.reason)
	}
	if instance.eventType != "Warning" {
		t.Errorf("Expected type 'Warning', got %q", instance.eventType)
	}
	if instance.involvedObjectKind != "Pod" {
		t.Errorf("Expected involvedObjectKind 'Pod', got %q", instance.involvedObjectKind)
	}
	if instance.labelSelector != "app=test" {
		t.Errorf("Expected labelSelector 'app=test', got %q", instance.labelSelector)
	}
	if instance.fieldSelector != "reason=TestReason" {
		t.Errorf("Expected fieldSelector 'reason=TestReason', got %q", instance.fieldSelector)
	}
}

func TestK8sEventTrigger_DefaultValues(t *testing.T) {
	trigger := newTestK8sEventTrigger()

	config := map[string]interface{}{}

	_, err := trigger.Setup(context.Background(), 1, config)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	trigger.mu.RLock()
	instance := trigger.instances[1]
	trigger.mu.RUnlock()

	if instance.namespace != "" {
		t.Errorf("Expected empty namespace, got %q", instance.namespace)
	}
	if instance.reason != "" {
		t.Errorf("Expected empty reason, got %q", instance.reason)
	}
	if instance.eventType != "" {
		t.Errorf("Expected empty eventType, got %q", instance.eventType)
	}
	if instance.involvedObjectKind != "" {
		t.Errorf("Expected empty involvedObjectKind, got %q", instance.involvedObjectKind)
	}
}
