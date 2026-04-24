package executor

import (
	"context"
	"testing"
)

func TestK8sLogsExecutor_Type(t *testing.T) {
	executor := NewK8sLogsExecutor(nil)
	if executor.Type() != NodeTypeK8sLogs {
		t.Errorf("Expected type %s, got %s", NodeTypeK8sLogs, executor.Type())
	}
}

func TestK8sLogsExecutor_ConfigValidation(t *testing.T) {
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
			name:        "missing podName",
			config:      map[string]interface{}{},
			expectError: true,
			errorMsg:    "podName",
		},
		{
			name: "empty podName",
			config: map[string]interface{}{
				"podName": "",
			},
			expectError: true,
			errorMsg:    "podName",
		},
		{
			name: "valid config",
			config: map[string]interface{}{
				"podName":   "test-pod",
				"namespace": "default",
			},
			expectError: false,
		},
		{
			name: "with container",
			config: map[string]interface{}{
				"podName":   "test-pod",
				"namespace": "default",
				"container": "nginx",
			},
			expectError: false,
		},
		{
			name: "with tailLines",
			config: map[string]interface{}{
				"podName":   "test-pod",
				"namespace": "default",
				"tailLines": float64(100),
			},
			expectError: false,
		},
		{
			name: "with sinceSeconds",
			config: map[string]interface{}{
				"podName":      "test-pod",
				"namespace":    "default",
				"sinceSeconds": float64(3600),
			},
			expectError: false,
		},
	}

	executor := NewK8sLogsExecutor(nil)
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

func TestK8sLogsExecutor_AllOptions(t *testing.T) {
	executor := NewK8sLogsExecutor(nil)
	resolver := &k8sTestResolver{}

	tests := []struct {
		name   string
		config map[string]interface{}
	}{
		{
			name: "basic logs",
			config: map[string]interface{}{
				"podName":   "test-pod",
				"namespace": "default",
			},
		},
		{
			name: "specific container",
			config: map[string]interface{}{
				"podName":   "multi-container-pod",
				"namespace": "default",
				"container": "sidecar",
			},
		},
		{
			name: "tail 50 lines",
			config: map[string]interface{}{
				"podName":   "test-pod",
				"namespace": "default",
				"tailLines": float64(50),
			},
		},
		{
			name: "last hour logs",
			config: map[string]interface{}{
				"podName":      "test-pod",
				"namespace":    "default",
				"sinceSeconds": float64(3600),
			},
		},
		{
			name: "all options combined",
			config: map[string]interface{}{
				"podName":      "test-pod",
				"namespace":    "default",
				"container":    "main",
				"tailLines":    float64(100),
				"sinceSeconds": float64(300),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{Config: tt.config}
			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Options test %s (expected K8s client error): %v", tt.name, err)
		})
	}
}

func TestK8sLogsExecutor_TemplateResolution(t *testing.T) {
	executor := NewK8sLogsExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"podName":   "{{pod_name}}",
			"namespace": "{{namespace}}",
			"container": "{{container_name}}",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Template resolution test (expected K8s client error): %v", err)
}

func TestK8sLogsExecutor_DefaultNamespace(t *testing.T) {
	executor := NewK8sLogsExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"podName": "test-pod",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Default namespace test (expected K8s client error): %v", err)
}
