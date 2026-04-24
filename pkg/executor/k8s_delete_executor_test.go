package executor

import (
	"context"
	"testing"
)

func TestK8sDeleteExecutor_Type(t *testing.T) {
	executor := NewK8sDeleteExecutor(nil)
	if executor.Type() != NodeTypeK8sDelete {
		t.Errorf("Expected type %s, got %s", NodeTypeK8sDelete, executor.Type())
	}
}

func TestK8sDeleteExecutor_ConfigValidation(t *testing.T) {
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
				"kind": "Pod",
			},
			expectError: true,
			errorMsg:    "name",
		},
		{
			name: "valid config",
			config: map[string]interface{}{
				"kind":      "Pod",
				"name":      "test-pod",
				"namespace": "default",
			},
			expectError: false,
		},
		{
			name: "with grace period",
			config: map[string]interface{}{
				"kind":               "Pod",
				"name":               "test-pod",
				"namespace":          "default",
				"gracePeriodSeconds": float64(30),
			},
			expectError: false,
		},
		{
			name: "zero grace period (immediate delete)",
			config: map[string]interface{}{
				"kind":               "Pod",
				"name":               "test-pod",
				"namespace":          "default",
				"gracePeriodSeconds": float64(0),
			},
			expectError: false,
		},
	}

	executor := NewK8sDeleteExecutor(nil)
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

func TestK8sDeleteExecutor_DifferentResources(t *testing.T) {
	tests := []struct {
		kind      string
		namespace string
	}{
		{"Pod", "default"},
		{"Service", "default"},
		{"Deployment", "default"},
		{"ConfigMap", "default"},
		{"Secret", "default"},
		{"StatefulSet", "default"},
		{"DaemonSet", "default"},
		{"Job", "default"},
		{"Ingress", "default"},
		{"Node", ""},
		{"Namespace", ""},
	}

	executor := NewK8sDeleteExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			config := map[string]interface{}{
				"kind": tt.kind,
				"name": "test-resource",
			}
			if tt.namespace != "" {
				config["namespace"] = tt.namespace
			}

			step := &StepDefinition{Config: config}
			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Delete %s test (expected K8s client error): %v", tt.kind, err)
		})
	}
}

func TestK8sDeleteExecutor_GracePeriods(t *testing.T) {
	tests := []struct {
		name        string
		gracePeriod float64
	}{
		{"immediate delete", 0},
		{"10 seconds", 10},
		{"30 seconds", 30},
		{"60 seconds", 60},
	}

	executor := NewK8sDeleteExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &StepDefinition{
				Config: map[string]interface{}{
					"kind":               "Pod",
					"name":               "test-pod",
					"namespace":          "default",
					"gracePeriodSeconds": tt.gracePeriod,
				},
			}

			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Grace period %s test (expected K8s client error): %v", tt.name, err)
		})
	}
}

func TestK8sDeleteExecutor_TemplateResolution(t *testing.T) {
	executor := NewK8sDeleteExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind":      "Pod",
			"name":      "{{pod_name}}",
			"namespace": "{{namespace}}",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Template resolution test (expected K8s client error): %v", err)
}

func TestCreateDeleteOptions(t *testing.T) {
	tests := []struct {
		name            string
		config          map[string]interface{}
		wantGracePeriod bool
		gracePeriodVal  int64
	}{
		{
			name:            "no grace period",
			config:          map[string]interface{}{},
			wantGracePeriod: false,
		},
		{
			name: "with grace period",
			config: map[string]interface{}{
				"gracePeriodSeconds": float64(30),
			},
			wantGracePeriod: true,
			gracePeriodVal:  30,
		},
		{
			name: "zero grace period",
			config: map[string]interface{}{
				"gracePeriodSeconds": float64(0),
			},
			wantGracePeriod: true,
			gracePeriodVal:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := createDeleteOptions(tt.config)

			if tt.wantGracePeriod {
				if opts.GracePeriodSeconds == nil {
					t.Error("Expected GracePeriodSeconds to be set")
				} else if *opts.GracePeriodSeconds != tt.gracePeriodVal {
					t.Errorf("Expected grace period %d, got %d", tt.gracePeriodVal, *opts.GracePeriodSeconds)
				}
			} else {
				if opts.GracePeriodSeconds != nil {
					t.Error("Expected GracePeriodSeconds to be nil")
				}
			}
		})
	}
}
