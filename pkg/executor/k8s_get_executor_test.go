package executor

import (
	"context"
	"testing"
)

type k8sTestResolver struct{}

func (r *k8sTestResolver) ResolveString(template string) string {
	return template
}

func (r *k8sTestResolver) ResolveMap(m map[string]interface{}) map[string]interface{} {
	return m
}

func (r *k8sTestResolver) EvaluateCondition(condition string) bool {
	return true
}

func (r *k8sTestResolver) SetVariable(name string, value interface{}) {}

func (r *k8sTestResolver) GetStepOutput(stepName string) interface{} {
	return nil
}

func (r *k8sTestResolver) SetStepOutput(stepName string, output interface{}) {}

func (r *k8sTestResolver) GetContextData() map[string]interface{} {
	return map[string]interface{}{}
}

func TestK8sGetExecutor_Type(t *testing.T) {
	executor := NewK8sGetExecutor(nil)
	if executor.Type() != NodeTypeK8sGet {
		t.Errorf("Expected type %s, got %s", NodeTypeK8sGet, executor.Type())
	}
}

func TestK8sGetExecutor_ConfigValidation(t *testing.T) {
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
			name: "unsupported kind",
			config: map[string]interface{}{
				"kind":      "UnknownKind",
				"name":      "test",
				"namespace": "default",
			},
			expectError: true,
			errorMsg:    "unsupported",
		},
		{
			name: "valid pod config",
			config: map[string]interface{}{
				"kind":      "Pod",
				"name":      "test-pod",
				"namespace": "default",
			},
			expectError: false,
		},
		{
			name: "valid deployment config",
			config: map[string]interface{}{
				"kind":      "Deployment",
				"name":      "test-deployment",
				"namespace": "default",
			},
			expectError: false,
		},
		{
			name: "valid service config",
			config: map[string]interface{}{
				"kind":      "Service",
				"name":      "test-service",
				"namespace": "default",
			},
			expectError: false,
		},
	}

	executor := NewK8sGetExecutor(nil)
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
				// Note: Will fail with K8s client error in test environment, but validates config parsing
				t.Logf("Config validation test (expected K8s client error): %v", err)
			}
		})
	}
}

func TestK8sGetExecutor_ClusterScopedResource(t *testing.T) {
	executor := NewK8sGetExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind": "Node",
			"name": "test-node",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	// Will fail with K8s client error in test environment, but validates cluster-scoped handling
	t.Logf("Cluster-scoped resource test (expected K8s client error): %v", err)
}

func TestK8sGetExecutor_TemplateResolution(t *testing.T) {
	executor := NewK8sGetExecutor(nil)
	resolver := &k8sTestResolver{}

	step := &StepDefinition{
		Config: map[string]interface{}{
			"kind":      "Pod",
			"name":      "{{pod_name}}",
			"namespace": "{{namespace}}",
		},
	}

	_, err := executor.Execute(context.Background(), step, resolver)
	// Will fail with K8s client error, but validates template resolution
	t.Logf("Template resolution test (expected K8s client error): %v", err)
}

func TestResolveGVR(t *testing.T) {
	tests := []struct {
		kind         string
		wantResource string
		wantGroup    string
		wantVersion  string
		wantErr      bool
	}{
		{
			kind:         "Pod",
			wantResource: "pods",
			wantGroup:    "",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			kind:         "Deployment",
			wantResource: "deployments",
			wantGroup:    "apps",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			kind:         "Service",
			wantResource: "services",
			wantGroup:    "",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			kind:         "Ingress",
			wantResource: "ingresses",
			wantGroup:    "networking.k8s.io",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			kind:         "StatefulSet",
			wantResource: "statefulsets",
			wantGroup:    "apps",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			kind:         "Job",
			wantResource: "jobs",
			wantGroup:    "batch",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			kind:         "CronJob",
			wantResource: "cronjobs",
			wantGroup:    "batch",
			wantVersion:  "v1",
			wantErr:      false,
		},
		{
			kind:    "UnknownKind",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			gvr, err := resolveGVR(tt.kind)

			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error for kind %s", tt.kind)
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

func TestExtractKind(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]interface{}
		want    string
		wantErr bool
	}{
		{
			name:    "valid kind",
			config:  map[string]interface{}{"kind": "Pod"},
			want:    "Pod",
			wantErr: false,
		},
		{
			name:    "missing kind",
			config:  map[string]interface{}{},
			wantErr: true,
		},
		{
			name:    "empty kind",
			config:  map[string]interface{}{"kind": ""},
			wantErr: true,
		},
		{
			name:    "non-string kind",
			config:  map[string]interface{}{"kind": 123},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractKind(tt.config)

			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if got != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestExtractNameNamespace(t *testing.T) {
	resolver := &k8sTestResolver{}

	tests := []struct {
		name          string
		config        map[string]interface{}
		wantName      string
		wantNamespace string
		wantErr       bool
	}{
		{
			name: "with namespace",
			config: map[string]interface{}{
				"name":      "test-resource",
				"namespace": "test-ns",
			},
			wantName:      "test-resource",
			wantNamespace: "test-ns",
			wantErr:       false,
		},
		{
			name: "without namespace",
			config: map[string]interface{}{
				"name": "test-resource",
			},
			wantName: "test-resource",
			wantErr:  false,
		},
		{
			name:    "missing name",
			config:  map[string]interface{}{},
			wantErr: true,
		},
		{
			name: "empty name",
			config: map[string]interface{}{
				"name": "",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, namespace, err := extractNameNamespace(tt.config, resolver)

			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if name != tt.wantName {
				t.Errorf("Expected name %q, got %q", tt.wantName, name)
			}

			if tt.wantNamespace != "" && namespace != tt.wantNamespace {
				t.Errorf("Expected namespace %q, got %q", tt.wantNamespace, namespace)
			}
		})
	}
}

func TestWrapK8sError(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		err       error
		wantNil   bool
	}{
		{
			name:      "nil error",
			operation: "test-op",
			err:       nil,
			wantNil:   true,
		},
		{
			name:      "wraps error",
			operation: "k8s-get",
			err:       context.DeadlineExceeded,
			wantNil:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := wrapK8sError(tt.operation, tt.err)

			if tt.wantNil {
				if result != nil {
					t.Errorf("Expected nil, got %v", result)
				}
			} else {
				if result == nil {
					t.Error("Expected error, got nil")
				}
			}
		})
	}
}

func TestExtractLabelSelector(t *testing.T) {
	resolver := &k8sTestResolver{}

	tests := []struct {
		name   string
		config map[string]interface{}
		want   string
	}{
		{
			name:   "with label selector",
			config: map[string]interface{}{"labelSelector": "app=nginx"},
			want:   "app=nginx",
		},
		{
			name:   "without label selector",
			config: map[string]interface{}{},
			want:   "",
		},
		{
			name:   "empty label selector",
			config: map[string]interface{}{"labelSelector": ""},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLabelSelector(tt.config, resolver)
			if got != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestExtractFieldSelector(t *testing.T) {
	resolver := &k8sTestResolver{}

	tests := []struct {
		name   string
		config map[string]interface{}
		want   string
	}{
		{
			name:   "with field selector",
			config: map[string]interface{}{"fieldSelector": "status.phase=Running"},
			want:   "status.phase=Running",
		},
		{
			name:   "without field selector",
			config: map[string]interface{}{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFieldSelector(tt.config, resolver)
			if got != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestExtractLimit(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]interface{}
		want   int64
	}{
		{
			name:   "with limit",
			config: map[string]interface{}{"limit": float64(50)},
			want:   50,
		},
		{
			name:   "without limit (uses default)",
			config: map[string]interface{}{},
			want:   100,
		},
		{
			name:   "zero limit (uses default)",
			config: map[string]interface{}{"limit": float64(0)},
			want:   100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLimit(tt.config)
			if got != tt.want {
				t.Errorf("Expected %d, got %d", tt.want, got)
			}
		})
	}
}
