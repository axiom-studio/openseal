package trigger

import (
	"context"
	"testing"
)

func TestK8sWatchTrigger_Type(t *testing.T) {
	trigger := NewK8sWatchTrigger()
	if trigger.Type() != "k8s-watch" {
		t.Errorf("Expected type 'k8s-watch', got %s", trigger.Type())
	}
}

func TestK8sWatchTrigger_ValidateConfig(t *testing.T) {
	tests := []struct {
		name        string
		config      map[string]interface{}
		expectError bool
		errorMsg    string
	}{
		{
			name:        "missing resource",
			config:      map[string]interface{}{},
			expectError: true,
			errorMsg:    "resource",
		},
		{
			name: "empty resource",
			config: map[string]interface{}{
				"resource": "",
			},
			expectError: true,
			errorMsg:    "resource",
		},
		{
			name: "valid minimal config",
			config: map[string]interface{}{
				"resource": "pods",
			},
			expectError: false,
		},
		{
			name: "valid with events",
			config: map[string]interface{}{
				"resource": "deployments",
				"events":   []interface{}{"ADDED", "MODIFIED"},
			},
			expectError: false,
		},
		{
			name: "invalid event type",
			config: map[string]interface{}{
				"resource": "pods",
				"events":   []interface{}{"INVALID_EVENT"},
			},
			expectError: true,
			errorMsg:    "invalid event type",
		},
		{
			name: "non-string event",
			config: map[string]interface{}{
				"resource": "pods",
				"events":   []interface{}{123},
			},
			expectError: true,
			errorMsg:    "array of strings",
		},
		{
			name: "valid ADDED event",
			config: map[string]interface{}{
				"resource": "services",
				"events":   []interface{}{"ADDED"},
			},
			expectError: false,
		},
		{
			name: "valid MODIFIED event",
			config: map[string]interface{}{
				"resource": "configmaps",
				"events":   []interface{}{"MODIFIED"},
			},
			expectError: false,
		},
		{
			name: "valid DELETED event",
			config: map[string]interface{}{
				"resource": "secrets",
				"events":   []interface{}{"DELETED"},
			},
			expectError: false,
		},
	}

	trigger := NewK8sWatchTrigger()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := trigger.ValidateConfig(tt.config)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error containing %q, got nil", tt.errorMsg)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestK8sWatchTrigger_Setup(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]interface{}
	}{
		{
			name: "basic setup",
			config: map[string]interface{}{
				"resource": "pods",
			},
		},
		{
			name: "with namespace",
			config: map[string]interface{}{
				"resource":  "deployments",
				"namespace": "default",
			},
		},
		{
			name: "with label selector",
			config: map[string]interface{}{
				"resource":      "pods",
				"namespace":     "default",
				"labelSelector": "app=nginx",
			},
		},
		{
			name: "with field selector",
			config: map[string]interface{}{
				"resource":      "pods",
				"namespace":     "default",
				"fieldSelector": "status.phase=Running",
			},
		},
		{
			name: "with events",
			config: map[string]interface{}{
				"resource": "services",
				"events":   []interface{}{"ADDED", "DELETED"},
			},
		},
		{
			name: "with debounce",
			config: map[string]interface{}{
				"resource":   "deployments",
				"debounceMs": float64(5000),
			},
		},
	}

	trigger := NewK8sWatchTrigger()

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

func TestK8sWatchTrigger_SetupTeardown(t *testing.T) {
	trigger := NewK8sWatchTrigger()

	config := map[string]interface{}{
		"resource":  "pods",
		"namespace": "default",
	}

	_, err := trigger.Setup(context.Background(), 1, config)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

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

func TestK8sWatchTrigger_MultipleInstances(t *testing.T) {
	trigger := NewK8sWatchTrigger()

	configs := []map[string]interface{}{
		{"resource": "pods", "namespace": "default"},
		{"resource": "deployments", "namespace": "kube-system"},
		{"resource": "services", "namespace": "default"},
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

func TestK8sWatchTrigger_ReplaceInstance(t *testing.T) {
	trigger := NewK8sWatchTrigger()

	config1 := map[string]interface{}{
		"resource":  "pods",
		"namespace": "default",
	}

	_, err := trigger.Setup(context.Background(), 1, config1)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	config2 := map[string]interface{}{
		"resource":  "deployments",
		"namespace": "kube-system",
	}

	_, err = trigger.Setup(context.Background(), 1, config2)
	if err != nil {
		t.Fatalf("Replace setup failed: %v", err)
	}

	trigger.mu.RLock()
	if len(trigger.instances) != 1 {
		t.Errorf("Expected 1 instance, got %d", len(trigger.instances))
	}
	if trigger.instances[1].resource != "deployments" {
		t.Errorf("Expected resource 'deployments', got %q", trigger.instances[1].resource)
	}
	trigger.mu.RUnlock()
}

func TestK8sWatchTrigger_ShouldFireEvent(t *testing.T) {
	trigger := NewK8sWatchTrigger()

	instance := &k8sWatchInstance{
		events: []string{"ADDED", "MODIFIED"},
	}

	tests := []struct {
		eventType string
		want      bool
	}{
		{"ADDED", true},
		{"MODIFIED", true},
		{"DELETED", false},
		{"UNKNOWN", false},
	}

	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			got := trigger.shouldFireEvent(instance, tt.eventType)
			if got != tt.want {
				t.Errorf("shouldFireEvent(%q) = %v, want %v", tt.eventType, got, tt.want)
			}
		})
	}
}

func TestK8sWatchTrigger_DefaultEvents(t *testing.T) {
	trigger := NewK8sWatchTrigger()

	config := map[string]interface{}{
		"resource": "pods",
	}

	_, err := trigger.Setup(context.Background(), 1, config)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	trigger.mu.RLock()
	instance := trigger.instances[1]
	trigger.mu.RUnlock()

	if len(instance.events) != 3 {
		t.Errorf("Expected 3 default events, got %d", len(instance.events))
	}

	expectedEvents := map[string]bool{
		"ADDED":    false,
		"MODIFIED": false,
		"DELETED":  false,
	}

	for _, event := range instance.events {
		if _, ok := expectedEvents[event]; ok {
			expectedEvents[event] = true
		}
	}

	for event, found := range expectedEvents {
		if !found {
			t.Errorf("Expected default event %q not found", event)
		}
	}
}

func TestResourceToGVR(t *testing.T) {
	tests := []struct {
		resource     string
		wantResource string
		wantGroup    string
		wantVersion  string
		wantErr      bool
	}{
		{
			resource:     "pods",
			wantResource: "pods",
			wantGroup:    "",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			resource:     "deployments",
			wantResource: "deployments",
			wantGroup:    "apps",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			resource:     "services",
			wantResource: "services",
			wantGroup:    "",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			resource:     "configmaps",
			wantResource: "configmaps",
			wantGroup:    "",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			resource:     "ingresses",
			wantResource: "ingresses",
			wantGroup:    "networking.k8s.io",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			resource:     "horizontalpodautoscalers",
			wantResource: "horizontalpodautoscalers",
			wantGroup:    "autoscaling",
			wantVersion:  "v2",
			wantErr:      false,
		},
		{
			resource: "unsupported-resource",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			gvr, err := resourceToGVR(tt.resource)

			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error for resource %q", tt.resource)
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if gvr.Resource != tt.wantResource {
				t.Errorf("Expected resource %q, got %q", tt.wantResource, gvr.Resource)
			}
			if gvr.Group != tt.wantGroup {
				t.Errorf("Expected group %q, got %q", tt.wantGroup, gvr.Group)
			}
			if gvr.Version != tt.wantVersion {
				t.Errorf("Expected version %q, got %q", tt.wantVersion, gvr.Version)
			}
		})
	}
}

func TestExtractMetadata(t *testing.T) {
	tests := []struct {
		name          string
		obj           interface{}
		wantNamespace string
		wantName      string
	}{
		{
			name:          "nil object",
			obj:           nil,
			wantNamespace: "",
			wantName:      "",
		},
		{
			name: "map object",
			obj: map[string]interface{}{
				"metadata": map[string]interface{}{
					"namespace": "default",
					"name":      "test-pod",
				},
			},
			wantNamespace: "default",
			wantName:      "test-pod",
		},
		{
			name: "map without metadata",
			obj: map[string]interface{}{
				"spec": map[string]interface{}{},
			},
			wantNamespace: "",
			wantName:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractMetadata(tt.obj)

			if result["namespace"] != tt.wantNamespace {
				t.Errorf("Expected namespace %q, got %q", tt.wantNamespace, result["namespace"])
			}
			if result["name"] != tt.wantName {
				t.Errorf("Expected name %q, got %q", tt.wantName, result["name"])
			}
		})
	}
}
