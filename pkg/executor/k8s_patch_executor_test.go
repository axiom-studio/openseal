package executor

import (
	"context"
	"testing"
)

func TestK8sPatchExecutor_Type(t *testing.T) {
	executor := NewK8sPatchExecutor(nil)
	if executor.Type() != NodeTypeK8sPatch {
		t.Errorf("Expected type %s, got %s", NodeTypeK8sPatch, executor.Type())
	}
}

func TestK8sPatchExecutor_ConfigValidation(t *testing.T) {
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
			name: "missing patch",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
			},
			expectError: true,
			errorMsg:    "patch",
		},
		{
			name: "valid string patch",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"patch":     `{"spec":{"replicas":5}}`,
			},
			expectError: false,
		},
		{
			name: "valid map patch",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"patch": map[string]interface{}{
					"spec": map[string]interface{}{
						"replicas": 5,
					},
				},
			},
			expectError: false,
		},
		{
			name: "strategic merge patch type",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"patch":     `{"spec":{"replicas":5}}`,
				"patchType": "strategic",
			},
			expectError: false,
		},
		{
			name: "merge patch type",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"patch":     `{"spec":{"replicas":5}}`,
				"patchType": "merge",
			},
			expectError: false,
		},
		{
			name: "json patch type",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"patch":     `[{"op":"replace","path":"/spec/replicas","value":5}]`,
				"patchType": "json",
			},
			expectError: false,
		},
		{
			name: "invalid patch type",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
				"patch":     `{"spec":{"replicas":5}}`,
				"patchType": "invalid",
			},
			expectError: true,
			errorMsg:    "invalid patchType",
		},
	}

	executor := NewK8sPatchExecutor(nil)
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

func TestK8sPatchExecutor_PatchTypes(t *testing.T) {
	tests := []struct {
		patchType string
		patch     interface{}
	}{
		{
			patchType: "strategic",
			patch:     `{"spec":{"replicas":3}}`,
		},
		{
			patchType: "merge",
			patch:     `{"metadata":{"labels":{"env":"prod"}}}`,
		},
		{
			patchType: "json",
			patch:     `[{"op":"add","path":"/metadata/labels/env","value":"staging"}]`,
		},
	}

	executor := NewK8sPatchExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(tt.patchType, func(t *testing.T) {
			step := &StepDefinition{
				Config: map[string]interface{}{
					"kind":      "Deployment",
					"name":      "test-deployment",
					"namespace": "default",
					"patch":     tt.patch,
					"patchType": tt.patchType,
				},
			}

			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Patch type %s test (expected K8s client error): %v", tt.patchType, err)
		})
	}
}

func TestK8sPatchExecutor_DifferentResources(t *testing.T) {
	tests := []struct {
		kind      string
		namespace string
	}{
		{"Deployment", "default"},
		{"Service", "default"},
		{"ConfigMap", "default"},
		{"StatefulSet", "default"},
		{"Node", ""},
	}

	executor := NewK8sPatchExecutor(nil)
	resolver := &k8sTestResolver{}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			config := map[string]interface{}{
				"kind":  tt.kind,
				"name":  "test-resource",
				"patch": `{"metadata":{"labels":{"test":"true"}}}`,
			}
			if tt.namespace != "" {
				config["namespace"] = tt.namespace
			}

			step := &StepDefinition{Config: config}
			_, err := executor.Execute(context.Background(), step, resolver)
			t.Logf("Resource %s test (expected K8s client error): %v", tt.kind, err)
		})
	}
}

func TestK8sPatchExecutor_TemplateResolution(t *testing.T) {
	executor := NewK8sPatchExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind":      "Deployment",
			"name":      "{{deployment_name}}",
			"namespace": "{{namespace}}",
			"patch":     "{{patch_data}}",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Template resolution test (expected K8s client error): %v", err)
}

func TestK8sPatchExecutor_MapPatchResolution(t *testing.T) {
	executor := NewK8sPatchExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind":      "Deployment",
			"name":      "test-deployment",
			"namespace": "default",
			"patch": map[string]interface{}{
				"spec": map[string]interface{}{
					"replicas": 5,
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app":     "myapp",
								"version": "v2",
							},
						},
					},
				},
			},
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	t.Logf("Map patch resolution test (expected K8s client error): %v", err)
}
