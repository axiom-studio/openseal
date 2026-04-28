package executor

import (
	"context"
	"encoding/json"
	"fmt"
)

type K8sPatchExecutor struct {
	k8sClient K8sClient
}

func NewK8sPatchExecutor(k8sClient K8sClient) *K8sPatchExecutor {
	return &K8sPatchExecutor{
		k8sClient: k8sClient,
	}
}

func (e *K8sPatchExecutor) Type() string {
	return NodeTypeK8sPatch
}

func (e *K8sPatchExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-patch", fmt.Errorf("config is required"))
	}

	kind, err := extractKind(config)
	if err != nil {
		return nil, wrapK8sError("k8s-patch", err)
	}

	name, namespace, err := extractNameNamespace(config, resolver)
	if err != nil {
		return nil, wrapK8sError("k8s-patch", err)
	}

	patchData, ok := config["patch"]
	if !ok {
		return nil, wrapK8sError("k8s-patch", fmt.Errorf("'patch' is required"))
	}

	// Note: patchType is currently not used in Cortex API call
	// The API uses strategic merge by default

	var patchMap map[string]interface{}
	switch p := patchData.(type) {
	case string:
		resolvedPatch := resolver.ResolveString(p)
		if err := json.Unmarshal([]byte(resolvedPatch), &patchMap); err != nil {
			return nil, wrapK8sError("k8s-patch", fmt.Errorf("failed to parse patch JSON: %w", err))
		}
	case map[string]interface{}:
		patchMap = resolver.ResolveMap(p)
	default:
		return nil, wrapK8sError("k8s-patch", fmt.Errorf("patch must be a string or object"))
	}

	clusterId := extractClusterId(config)

	if e.k8sClient == nil {
		return nil, wrapK8sError("k8s-patch", fmt.Errorf("k8s client not configured"))
	}

	result, err := e.k8sClient.UpdateResource(ctx, clusterId, namespace, name, kind, patchMap)
	if err != nil {
		return nil, wrapK8sError("k8s-patch", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"success": true,
			"message": "Resource patched successfully",
			"name":    name,
			"kind":    kind,
			"object":  result,
		},
	}, nil
}
