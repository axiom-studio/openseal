package executor

import (
	"context"
	"testing"
)

func TestK8sRestartExecutor_Type(t *testing.T) {
	executor := NewK8sRestartExecutor(nil)
	if executor.Type() != NodeTypeK8sRestart {
		t.Errorf("Expected type %s, got %s", NodeTypeK8sRestart, executor.Type())
	}
}

func TestK8sRestartExecutor_ConfigValidation(t *testing.T) {
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
			name: "missing name",
			config: map[string]interface{}{
				"kind": "Deployment",
			},
			expectError: true,
			errorMsg:    "name",
		},
		{
			name: "invalid kind - Pod",
			config: map[string]interface{}{
				"kind":      "Pod",
				"name":      "test-pod",
				"namespace": "default",
			},
			expectError: true,
			errorMsg:    "Deployment, StatefulSet, or DaemonSet",
		},
		{
			name: "invalid kind - Service",
			config: map[string]interface{}{
				"kind":      "Service",
				"name":      "test-svc",
				"namespace": "default",
			},
			expectError: true,
			errorMsg:    "Deployment, StatefulSet, or DaemonSet",
		},
		{
			name: "valid Deployment",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
			},
			expectError: false,
		},
		{
			name: "valid StatefulSet",
			config: map[string]interface{}{
				"kind":      "StatefulSet",
				"name":      "test-statefulset",
				"namespace": "default",
			},
			expectError: false,
		},
		{
			name: "valid DaemonSet",
			config: map[string]interface{}{
				"kind":      "DaemonSet",
				"name":      "test-daemonset",
				"namespace": "default",
			},
			expectError: false,
		},
	}

	executor := NewK8sRestartExecutor(nil)
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

func TestK8sRestartExecutor_SupportedKinds(t *testing.T) {
	tests := []struct {
		kind string
	}{
		{"Deployment"},
		{"StatefulSet"},
		{"DaemonSet"},
	}

	executor := NewK8sRestartExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			step := &StepDefinition{
				Config: map[string]interface{}{
					"kind":      tt.kind,
					"name":      "test-resource",
					"namespace": "default",
				},
			}

			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Supported kind %s test (expected K8s client error): %v", tt.kind, err)
		})
	}
}

func TestK8sRestartExecutor_TemplateResolution(t *testing.T) {
	executor := NewK8sRestartExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind":      "Deployment",
			"name":      "{{deployment_name}}",
			"namespace": "{{namespace}}",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Template resolution test (expected K8s client error): %v", err)
}
