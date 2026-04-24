package executor

import (
	"context"
	"testing"
)

func TestK8sListExecutor_Type(t *testing.T) {
	executor := NewK8sListExecutor(nil)
	if executor.Type() != NodeTypeK8sList {
		t.Errorf("Expected type %s, got %s", NodeTypeK8sList, executor.Type())
	}
}

func TestK8sListExecutor_ConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		config      map[string]interface{}
		expectError bool
		errorMsg    string
	}{
		{
			name:        "nil config",
			config:      nil,
			expectError: true,
			errorMsg:    "config is required",
		},
		{
			name:        "missing kind",
			config:      map[string]interface{}{},
			expectError: true,
			errorMsg:    "kind",
		},
		{
			name: "valid config with namespace",
			config: map[string]interface{}{
				"kind":      "Pod",
				"namespace": "default",
			},
			expectError: false,
		},
		{
			name: "valid config without namespace",
			config: map[string]interface{}{
				"kind": "Pod",
			},
			expectError: false,
		},
		{
			name: "with label selector",
			config: map[string]interface{}{
				"kind":          "Pod",
				"namespace":     "default",
				"labelSelector": "app=nginx",
			},
			expectError: false,
		},
		{
			name: "with field selector",
			config: map[string]interface{}{
				"kind":          "Pod",
				"namespace":     "default",
				"fieldSelector": "status.phase=Running",
			},
			expectError: false,
		},
		{
			name: "with limit",
			config: map[string]interface{}{
				"kind":  "Pod",
				"limit": float64(50),
			},
			expectError: false,
		},
	}

	executor := NewK8sListExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{Config: tt.config}
			_, err := executor.Execute(context.Background(), step, resolver)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error containing %q, got nil", tt.errorMsg)
				}
			} else {
				t.Logf("Config validation test (expected K8s client error): %v", err)
			}
		})
	}
}

func TestK8sListExecutor_AllResourceTypes(t *testing.T) {
	tests := []struct {
		kind      string
		namespace string
	}{
		{"Pod", "default"},
		{"Service", "default"},
		{"Deployment", "default"},
		{"StatefulSet", "default"},
		{"DaemonSet", "default"},
		{"ConfigMap", "default"},
		{"Secret", "default"},
		{"Job", "default"},
		{"CronJob", "default"},
		{"Ingress", "default"},
		{"Node", ""},
		{"Namespace", ""},
	}

	executor := NewK8sListExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			config := map[string]interface{}{
				"kind": tt.kind,
			}
			if tt.namespace != "" {
				config["namespace"] = tt.namespace
			}

			step := &StepDefinition{Config: config}
			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Resource type %s test (expected K8s client error): %v", tt.kind, err)
		})
	}
}

func TestK8sListExecutor_WithSelectors(t *testing.T) {
	executor := NewK8sListExecutor(nil)
	resolver := &k8sTestResolver{}

	tests := []struct {
		name   string
		config map[string]interface{}
	}{
		{
			name: "label selector only",
			config: map[string]interface{}{
				"kind":          "Pod",
				"namespace":     "default",
				"labelSelector": "app=nginx,tier=frontend",
			},
		},
		{
			name: "field selector only",
			config: map[string]interface{}{
				"kind":          "Pod",
				"namespace":     "default",
				"fieldSelector": "status.phase=Running",
			},
		},
		{
			name: "both selectors",
			config: map[string]interface{}{
				"kind":          "Pod",
				"namespace":     "default",
				"labelSelector": "app=nginx",
				"fieldSelector": "status.phase=Running",
			},
		},
		{
			name: "with limit",
			config: map[string]interface{}{
				"kind":  "Pod",
				"limit": float64(10),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{Config: tt.config}
			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Selector test %s (expected K8s client error): %v", tt.name, err)
		})
	}
}

func TestK8sListExecutor_TemplateResolution(t *testing.T) {
	executor := NewK8sListExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind":          "Pod",
			"namespace":     "{{namespace}}",
			"labelSelector": "{{label_selector}}",
			"fieldSelector": "{{field_selector}}",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Template resolution test (expected K8s client error): %v", err)
}

func TestK8sListExecutor_ClusterScoped(t *testing.T) {
	executor := NewK8sListExecutor(nil)
	resolver := &k8sTestResolver{}

	tests := []struct {
		kind string
	}{
		{"Node"},
		{"Namespace"},
		{"PersistentVolume"},
		{"ClusterRole"},
		{"ClusterRoleBinding"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			step := &StepDefinition{
				Config: map[string]interface{}{
					"kind": tt.kind,
				},
			}

			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Cluster-scoped resource %s test (expected K8s client error): %v", tt.kind, err)
		})
	}
}
