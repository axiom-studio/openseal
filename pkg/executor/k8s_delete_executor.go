package executor

import (
	"context"
	"fmt"
)

type K8sDeleteExecutor struct {
	cortexClient CortexK8sClient
}

func NewK8sDeleteExecutor(cortexClient CortexK8sClient) *K8sDeleteExecutor {
	return &K8sDeleteExecutor{
		cortexClient: cortexClient,
	}
}

func (e *K8sDeleteExecutor) Type() string {
	return NodeTypeK8sDelete
}

func (e *K8sDeleteExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-delete", fmt.Errorf("config is required"))
	}

	kind, err := extractKind(config)
	if err != nil {
		return nil, wrapK8sError("k8s-delete", err)
	}

	name, namespace, err := extractNameNamespace(config, resolver)
	if err != nil {
		return nil, wrapK8sError("k8s-delete", err)
	}

	clusterId := extractClusterId(config)

	// Use Cortex API to delete resource - works for any cluster Cortex manages
	err = e.cortexClient.DeleteResource(ctx, clusterId, namespace, name, kind)
	if err != nil {
		return nil, wrapK8sError("k8s-delete", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"success": true,
			"message": "Resource deleted successfully",
			"name":    name,
			"kind":    kind,
		},
	}, nil
}
