package resolver

import (
	"context"
	"fmt"
	"os"
)

// VaultService defines the minimal interface needed for vault resolution
type VaultService interface {
	ResolveCredential(ctx context.Context, name string, projectId *int) (map[string]interface{}, error)
}

// BindingResolver resolves agent bindings to actual values
type BindingResolver interface {
	// ResolveBindings resolves all bindings for an agent instance
	ResolveBindings(ctx context.Context, bindings []*BindingDefinition) (map[string]interface{}, error)
}

// BindingDefinition defines a single binding to resolve
type BindingDefinition struct {
	InputName           string                 `json:"inputName"`
	SourceType          string                 `json:"sourceType"` // blueprint, static, env
	BlueprintName       string                 `json:"blueprintName,omitempty"`
	NodeName            string                 `json:"nodeName,omitempty"`
	NodeType            string                 `json:"nodeType,omitempty"`       // helm-chart or virtual
	VirtualOutputs      map[string]interface{} `json:"virtualOutputs,omitempty"` // static outputs for virtual nodes
	OutputKey           string                 `json:"outputKey,omitempty"`
	StaticValue         string                 `json:"staticValue,omitempty"`
	EnvVar              string                 `json:"envVar,omitempty"`
	VaultCredentialName string                 `json:"vaultCredentialName,omitempty"`
	VaultFieldName      string                 `json:"vaultFieldName,omitempty"`
}

// LocalResolver implements BindingResolver for runtime (uses K8s DNS)
type LocalResolver struct {
	namespace    string
	domain       string
	vaultService VaultService
	projectId    *int
}

// NewLocalResolver creates a resolver that uses K8s DNS for blueprint resolution
func NewLocalResolver(namespace string) *LocalResolver {
	return &LocalResolver{
		namespace:    namespace,
		domain:       "svc.cluster.local",
		vaultService: nil,
		projectId:    nil,
	}
}

// NewLocalResolverWithVault creates a resolver with vault support
func NewLocalResolverWithVault(namespace string, vaultService VaultService, projectId *int) *LocalResolver {
	return &LocalResolver{
		namespace:    namespace,
		domain:       "svc.cluster.local",
		vaultService: vaultService,
		projectId:    projectId,
	}
}

// ResolveBindings resolves bindings using K8s DNS
func (r *LocalResolver) ResolveBindings(ctx context.Context, bindings []*BindingDefinition) (map[string]interface{}, error) {
	resolved := make(map[string]interface{})

	for _, binding := range bindings {
		switch binding.SourceType {
		case "blueprint":
			// Build K8s DNS URL for blueprint node
			value, err := r.resolveBlueprintBinding(binding)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve blueprint binding %s: %w", binding.InputName, err)
			}
			resolved[binding.InputName] = value

		case "static":
			// Static values are used as-is
			resolved[binding.InputName] = binding.StaticValue

		case "env":
			// Environment variables are resolved at runtime
			// Return placeholder that will be resolved when used
			resolved[binding.InputName] = fmt.Sprintf("${%s}", binding.EnvVar)

		case "vault":
			// Resolve vault credential
			if r.vaultService == nil {
				return nil, fmt.Errorf("vault binding requested but vault service not available")
			}
			if binding.VaultCredentialName == "" {
				return nil, fmt.Errorf("vault binding requires vaultCredentialName")
			}

			// Resolve the credential from vault
			credentialFields, err := r.vaultService.ResolveCredential(ctx, binding.VaultCredentialName, r.projectId)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve vault credential %s: %w", binding.VaultCredentialName, err)
			}

			// If a specific field is requested, extract it
			if binding.VaultFieldName != "" {
				value, ok := credentialFields[binding.VaultFieldName]
				if !ok {
					return nil, fmt.Errorf("field %s not found in vault credential %s", binding.VaultFieldName, binding.VaultCredentialName)
				}
				resolved[binding.InputName] = value
			} else {
				// Return all fields as a map
				resolved[binding.InputName] = credentialFields
			}

		default:
			return nil, fmt.Errorf("unknown binding source type: %s", binding.SourceType)
		}
	}

	return resolved, nil
}

// resolveBlueprintBinding resolves a blueprint binding
// For virtual nodes, returns static outputs from VirtualOutputs
// For helm-chart nodes, uses K8s DNS
func (r *LocalResolver) resolveBlueprintBinding(binding *BindingDefinition) (interface{}, error) {
	if binding.BlueprintName == "" || binding.NodeName == "" {
		return nil, fmt.Errorf("blueprint binding requires blueprintName and nodeName")
	}

	// For virtual nodes, return the value directly from VirtualOutputs
	if binding.NodeType == "virtual" {
		if binding.VirtualOutputs == nil {
			return nil, fmt.Errorf("virtual node binding requires virtualOutputs")
		}

		// If OutputKey is specified, return that specific output
		if binding.OutputKey != "" {
			if value, ok := binding.VirtualOutputs[binding.OutputKey]; ok {
				return value, nil
			}
			return nil, fmt.Errorf("output key '%s' not found in virtual node outputs", binding.OutputKey)
		}

		// If no OutputKey specified, return all outputs as a map
		return binding.VirtualOutputs, nil
	}

	// For helm-chart nodes, build K8s DNS URL
	serviceName := binding.NodeName
	serviceUrl := fmt.Sprintf("%s.%s.%s", serviceName, r.namespace, r.domain)

	// Return the appropriate value based on OutputKey
	switch binding.OutputKey {
	case "serviceUrl":
		return serviceUrl, nil
	case "serviceName":
		return serviceName, nil
	case "namespace":
		return r.namespace, nil
	case "host":
		return serviceUrl, nil
	case "port":
		// Default port - this should ideally come from blueprint metadata
		return 5432, nil
	default:
		// Return full URL by default
		return serviceUrl, nil
	}
}

// MockResolver implements BindingResolver for testing
type MockResolver struct {
	mockValues map[string]interface{}
}

// NewMockResolver creates a resolver with predefined mock values
func NewMockResolver(mockValues map[string]interface{}) *MockResolver {
	return &MockResolver{
		mockValues: mockValues,
	}
}

// ResolveBindings returns mock values for testing
func (r *MockResolver) ResolveBindings(ctx context.Context, bindings []*BindingDefinition) (map[string]interface{}, error) {
	resolved := make(map[string]interface{})

	for _, binding := range bindings {
		// Check if we have a mock value
		if mockValue, ok := r.mockValues[binding.InputName]; ok {
			resolved[binding.InputName] = mockValue
		} else {
			// Return a default mock value based on source type
			switch binding.SourceType {
			case "blueprint":
				// For virtual nodes, use the virtual outputs if available
				if binding.NodeType == "virtual" && binding.VirtualOutputs != nil {
					if binding.OutputKey != "" {
						if value, ok := binding.VirtualOutputs[binding.OutputKey]; ok {
							resolved[binding.InputName] = value
						} else {
							resolved[binding.InputName] = fmt.Sprintf("mock-virtual-%s", binding.OutputKey)
						}
					} else {
						resolved[binding.InputName] = binding.VirtualOutputs
					}
				} else {
					resolved[binding.InputName] = fmt.Sprintf("mock-%s-%s", binding.BlueprintName, binding.NodeName)
				}
			case "static":
				resolved[binding.InputName] = binding.StaticValue
			case "env":
				resolved[binding.InputName] = fmt.Sprintf("mock-env-%s", binding.EnvVar)
			case "vault":
				if binding.VaultFieldName != "" {
					resolved[binding.InputName] = fmt.Sprintf("mock-vault-%s-%s", binding.VaultCredentialName, binding.VaultFieldName)
				} else {
					resolved[binding.InputName] = map[string]interface{}{
						"mock_field": fmt.Sprintf("mock-vault-%s", binding.VaultCredentialName),
					}
				}
			}
		}
	}

	return resolved, nil
}

// EnvResolver implements Resolver for CLI standalone mode.
// It reads values from environment variables and resolves ${VAR} style placeholders.
type EnvResolver struct{}

// NewEnvResolver creates a resolver that reads from environment variables
func NewEnvResolver() *EnvResolver {
	return &EnvResolver{}
}

// Resolve resolves a binding by looking up environment variables.
// Supports ${VAR_NAME} syntax - extracts the var name and looks it up.
// The context parameter is ignored in this implementation.
func (r *EnvResolver) Resolve(binding string, context map[string]any) (string, error) {
	// Handle ${VAR} style bindings
	if len(binding) >= 3 && binding[0] == '$' && binding[1] == '{' && binding[len(binding)-1] == '}' {
		varName := binding[2 : len(binding)-1]
		value, exists := os.LookupEnv(varName)
		if !exists {
			return "", fmt.Errorf("environment variable %s not found", varName)
		}
		return value, nil
	}

	// If not a ${VAR} pattern, return as-is
	return binding, nil
}
