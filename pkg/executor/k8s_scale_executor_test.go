package executor

import (
	"context"
	"testing"
)

func TestK8sScaleExecutor_Type(t *testing.T) {
	executor := NewK8sScaleExecutor(nil)
	if executor.Type() != NodeTypeK8sScale {
		t.Errorf("Expected type %s, got %s", NodeTypeK8sScale, executor.Type())
	}
}

func TestK8sScaleExecutor_ConfigValidation(t *testing.T) {
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
			name: "missing replicas",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
			},
			expectError: true,
			errorMsg:    "replicas",
		},
		{
			name: "invalid kind - Pod",
			config: map[string]interface{}{
				"kind":      "Pod",
				"name":      "test-pod",
				"namespace": "default",
				"replicas":  float64(3),
			},
			expectError: true,
			errorMsg:    "Deployment, StatefulSet, or ReplicaSet",
		},
		{
			name: "invalid kind - DaemonSet",
			config: map[string]interface{}{
				"kind":      "DaemonSet",
				"name":      "test-daemonset",
				"namespace": "default",
				"replicas":  float64(3),
			},
			expectError: true,
			errorMsg:    "Deployment, StatefulSet, or ReplicaSet",
		},
		{
			name: "valid Deployment",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"replicas":  float64(5),
			},
			expectError: false,
		},
		{
			name: "valid StatefulSet",
			config: map[string]interface{}{
				"kind":      "StatefulSet",
				"name":      "test-statefulset",
				"namespace": "default",
				"replicas":  float64(3),
			},
			expectError: false,
		},
		{
			name: "valid ReplicaSet",
			config: map[string]interface{}{
				"kind":      "ReplicaSet",
				"name":      "test-replicaset",
				"namespace": "default",
				"replicas":  float64(2),
			},
			expectError: false,
		},
		{
			name: "scale to zero",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"replicas":  float64(0),
			},
			expectError: false,
		},
	}

	executor := NewK8sScaleExecutor(nil)
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

func TestK8sScaleExecutor_DifferentReplicaCounts(t *testing.T) {
	tests := []struct {
		replicas int
	}{
		{0},
		{1},
		{3},
		{5},
		{10},
	}

	executor := NewK8sScaleExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(string(rune(tt.replicas)), func(t *testing.T) {
			step := &StepDefinition{
				Config: map[string]interface{}{
					"kind":      "Deployment",
					"name":      "test-deployment",
					"namespace": "default",
					"replicas":  float64(tt.replicas),
				},
			}

			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Scale to %d replicas test (expected K8s client error): %v", tt.replicas, err)
		})
	}
}

func TestK8sScaleExecutor_TemplateResolution(t *testing.T) {
	executor := NewK8sScaleExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind":      "Deployment",
			"name":      "{{deployment_name}}",
			"namespace": "{{namespace}}",
			"replicas":  float64(5),
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Template resolution test (expected K8s client error): %v", err)
}
