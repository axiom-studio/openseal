package resolver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveVirtualNodeBinding tests resolving bindings to virtual node outputs
func TestResolveVirtualNodeBinding(t *testing.T) {
	resolver := NewLocalResolver("production")

	tests := []struct {
		name     string
		binding  *BindingDefinition
		expected interface{}
		wantErr  bool
	}{
		{
			name: "resolve virtual node host output",
			binding: &BindingDefinition{
				InputName:     "dbHost",
				SourceType:    "blueprint",
				BlueprintName: "data-stack",
				NodeName:      "rds-postgres",
				OutputKey:     "host",
			},
			expected: "rds-postgres.production.svc.cluster.local",
			wantErr:  false,
		},
		{
			name: "resolve virtual node serviceUrl output",
			binding: &BindingDefinition{
				InputName:     "apiEndpoint",
				SourceType:    "blueprint",
				BlueprintName: "external-services",
				NodeName:      "payment-api",
				OutputKey:     "serviceUrl",
			},
			expected: "payment-api.production.svc.cluster.local",
			wantErr:  false,
		},
		{
			name: "resolve virtual node port output",
			binding: &BindingDefinition{
				InputName:     "dbPort",
				SourceType:    "blueprint",
				BlueprintName: "data-stack",
				NodeName:      "rds-postgres",
				OutputKey:     "port",
			},
			expected: 5432,
			wantErr:  false,
		},
		{
			name: "resolve virtual node namespace output",
			binding: &BindingDefinition{
				InputName:     "namespace",
				SourceType:    "blueprint",
				BlueprintName: "data-stack",
				NodeName:      "redis",
				OutputKey:     "namespace",
			},
			expected: "production",
			wantErr:  false,
		},
		{
			name: "resolve virtual node serviceName output",
			binding: &BindingDefinition{
				InputName:     "serviceName",
				SourceType:    "blueprint",
				BlueprintName: "data-stack",
				NodeName:      "mongodb",
				OutputKey:     "serviceName",
			},
			expected: "mongodb",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := resolver.ResolveBindings(context.Background(), []*BindingDefinition{tt.binding})
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result[tt.binding.InputName])
			}
		})
	}
}

// TestResolveHelmChartBinding tests resolving bindings to helm-chart nodes (existing behavior)
func TestResolveHelmChartBinding(t *testing.T) {
	resolver := NewLocalResolver("staging")

	binding := &BindingDefinition{
		InputName:     "webappUrl",
		SourceType:    "blueprint",
		BlueprintName: "app-stack",
		NodeName:      "frontend",
		OutputKey:     "serviceUrl",
	}

	result, err := resolver.ResolveBindings(context.Background(), []*BindingDefinition{binding})
	assert.NoError(t, err)
	assert.Equal(t, "frontend.staging.svc.cluster.local", result["webappUrl"])
}

// TestResolveBindingErrorWhenOutputKeyNotFound tests error when output key is missing
func TestResolveBindingErrorWhenOutputKeyNotFound(t *testing.T) {
	resolver := NewLocalResolver("production")

	binding := &BindingDefinition{
		InputName:     "customValue",
		SourceType:    "blueprint",
		BlueprintName: "data-stack",
		NodeName:      "postgres",
		OutputKey:     "unknownKey",
	}

	result, err := resolver.ResolveBindings(context.Background(), []*BindingDefinition{binding})
	assert.NoError(t, err)
	assert.Equal(t, "postgres.production.svc.cluster.local", result["customValue"])
}

// TestResolveMixedBindings tests resolving both virtual and helm-chart node bindings
func TestResolveMixedBindings(t *testing.T) {
	resolver := NewLocalResolver("production")

	bindings := []*BindingDefinition{
		{
			InputName:     "internalDbHost",
			SourceType:    "blueprint",
			BlueprintName: "app-stack",
			NodeName:      "postgres-internal",
			OutputKey:     "host",
		},
		{
			InputName:     "externalDbHost",
			SourceType:    "blueprint",
			BlueprintName: "external-resources",
			NodeName:      "rds-external",
			OutputKey:     "host",
		},
		{
			InputName:   "apiKey",
			SourceType:  "static",
			StaticValue: "secret-key-123",
		},
	}

	result, err := resolver.ResolveBindings(context.Background(), bindings)
	assert.NoError(t, err)
	assert.Len(t, result, 3)

	assert.Equal(t, "postgres-internal.production.svc.cluster.local", result["internalDbHost"])
	assert.Equal(t, "rds-external.production.svc.cluster.local", result["externalDbHost"])
	assert.Equal(t, "secret-key-123", result["apiKey"])
}

// TestResolveVirtualNodeWithCustomOutputs tests virtual nodes with custom output keys
func TestResolveVirtualNodeWithCustomOutputs(t *testing.T) {
	mockResolver := NewMockResolver(map[string]interface{}{
		"dbEndpoint": "prod-rds.us-east-1.rds.amazonaws.com:5432",
		"apiKey":     "rds-access-key",
		"database":   "myapp_production",
	})

	bindings := []*BindingDefinition{
		{
			InputName:     "dbEndpoint",
			SourceType:    "blueprint",
			BlueprintName: "aws-resources",
			NodeName:      "rds",
			OutputKey:     "endpoint",
		},
		{
			InputName:     "apiKey",
			SourceType:    "blueprint",
			BlueprintName: "aws-resources",
			NodeName:      "rds",
			OutputKey:     "accessKey",
		},
		{
			InputName:     "database",
			SourceType:    "blueprint",
			BlueprintName: "aws-resources",
			NodeName:      "rds",
			OutputKey:     "database",
		},
	}

	result, err := mockResolver.ResolveBindings(context.Background(), bindings)
	assert.NoError(t, err)

	assert.Equal(t, "prod-rds.us-east-1.rds.amazonaws.com:5432", result["dbEndpoint"])
	assert.Equal(t, "rds-access-key", result["apiKey"])
	assert.Equal(t, "myapp_production", result["database"])
}

// TestResolveBindingWithDifferentNamespaces tests bindings across namespaces
func TestResolveBindingWithDifferentNamespaces(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		nodeName  string
		outputKey string
		expected  interface{}
	}{
		{
			name:      "production namespace",
			namespace: "production",
			nodeName:  "database",
			outputKey: "serviceUrl",
			expected:  "database.production.svc.cluster.local",
		},
		{
			name:      "staging namespace",
			namespace: "staging",
			nodeName:  "database",
			outputKey: "serviceUrl",
			expected:  "database.staging.svc.cluster.local",
		},
		{
			name:      "development namespace",
			namespace: "dev",
			nodeName:  "api",
			outputKey: "host",
			expected:  "api.dev.svc.cluster.local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := NewLocalResolver(tt.namespace)

			binding := &BindingDefinition{
				InputName:     "endpoint",
				SourceType:    "blueprint",
				BlueprintName: "test-blueprint",
				NodeName:      tt.nodeName,
				OutputKey:     tt.outputKey,
			}

			result, err := resolver.ResolveBindings(context.Background(), []*BindingDefinition{binding})
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, result["endpoint"])
		})
	}
}

// TestVirtualNodeBindingWithComplexOutputs tests virtual nodes with complex output structures
func TestVirtualNodeBindingWithComplexOutputs(t *testing.T) {
	complexOutputs := map[string]interface{}{
		"s3BucketName": "my-app-assets-prod",
		"s3Region":     "us-east-1",
		"s3Endpoint":   "https://s3.us-east-1.amazonaws.com",
		"sqsQueueUrl":  "https://sqs.us-east-1.amazonaws.com/123456789/myqueue",
		"snsTopicArn":  "arn:aws:sns:us-east-1:123456789:mytopic",
	}

	mockResolver := NewMockResolver(complexOutputs)

	bindings := []*BindingDefinition{
		{
			InputName:     "s3BucketName",
			SourceType:    "blueprint",
			BlueprintName: "aws-storage",
			NodeName:      "s3-bucket",
			OutputKey:     "bucketName",
		},
		{
			InputName:     "s3Region",
			SourceType:    "blueprint",
			BlueprintName: "aws-storage",
			NodeName:      "s3-bucket",
			OutputKey:     "region",
		},
		{
			InputName:     "sqsQueueUrl",
			SourceType:    "blueprint",
			BlueprintName: "aws-messaging",
			NodeName:      "sqs-queue",
			OutputKey:     "queueUrl",
		},
	}

	result, err := mockResolver.ResolveBindings(context.Background(), bindings)
	assert.NoError(t, err)

	assert.Equal(t, "my-app-assets-prod", result["s3BucketName"])
	assert.Equal(t, "us-east-1", result["s3Region"])
	assert.Equal(t, "https://sqs.us-east-1.amazonaws.com/123456789/myqueue", result["sqsQueueUrl"])
}

// TestResolveBindingErrorScenarios tests various error conditions
func TestResolveBindingErrorScenarios(t *testing.T) {
	resolver := NewLocalResolver("production")

	tests := []struct {
		name    string
		binding *BindingDefinition
		wantErr bool
		errMsg  string
	}{
		{
			name: "missing blueprint name",
			binding: &BindingDefinition{
				InputName:  "endpoint",
				SourceType: "blueprint",
				NodeName:   "postgres",
				OutputKey:  "host",
			},
			wantErr: true,
			errMsg:  "blueprint binding requires blueprintName and nodeName",
		},
		{
			name: "missing node name",
			binding: &BindingDefinition{
				InputName:     "endpoint",
				SourceType:    "blueprint",
				BlueprintName: "data-stack",
				OutputKey:     "host",
			},
			wantErr: true,
			errMsg:  "blueprint binding requires blueprintName and nodeName",
		},
		{
			name: "unknown source type",
			binding: &BindingDefinition{
				InputName:  "value",
				SourceType: "unknown-type",
			},
			wantErr: true,
			errMsg:  "unknown binding source type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolver.ResolveBindings(context.Background(), []*BindingDefinition{tt.binding})
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestVirtualNodeOutputResolutionInAgentContext tests how agent bindings would resolve virtual node outputs
func TestVirtualNodeOutputResolutionInAgentContext(t *testing.T) {
	mockResolver := NewMockResolver(map[string]interface{}{
		"internalDbUrl": "postgres.production.svc.cluster.local:5432",
		"externalDbUrl": "prod-rds.us-east-1.rds.amazonaws.com:5432",
		"apiKey":        "external-api-key",
		"cacheHost":     "redis.production.svc.cluster.local",
	})

	bindings := []*BindingDefinition{
		{
			InputName:     "internalDbUrl",
			SourceType:    "blueprint",
			BlueprintName: "internal-infra",
			NodeName:      "postgres",
			OutputKey:     "serviceUrl",
		},
		{
			InputName:     "externalDbUrl",
			SourceType:    "blueprint",
			BlueprintName: "external-resources",
			NodeName:      "rds",
			OutputKey:     "endpoint",
		},
		{
			InputName:     "apiKey",
			SourceType:    "blueprint",
			BlueprintName: "external-resources",
			NodeName:      "rds",
			OutputKey:     "accessKey",
		},
		{
			InputName:     "cacheHost",
			SourceType:    "blueprint",
			BlueprintName: "internal-infra",
			NodeName:      "redis",
			OutputKey:     "host",
		},
	}

	result, err := mockResolver.ResolveBindings(context.Background(), bindings)
	assert.NoError(t, err)
	assert.Len(t, result, 4)

	assert.Equal(t, "postgres.production.svc.cluster.local:5432", result["internalDbUrl"])
	assert.Equal(t, "prod-rds.us-east-1.rds.amazonaws.com:5432", result["externalDbUrl"])
	assert.Equal(t, "external-api-key", result["apiKey"])
	assert.Equal(t, "redis.production.svc.cluster.local", result["cacheHost"])
}
