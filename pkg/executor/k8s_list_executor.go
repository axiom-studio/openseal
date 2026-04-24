package executor

import (
	"context"
	"fmt"
)

type K8sListExecutor struct {
	cortexClient CortexK8sClient
}

func NewK8sListExecutor(cortexClient CortexK8sClient) *K8sListExecutor {
	return &K8sListExecutor{
		cortexClient: cortexClient,
	}
}

func (e *K8sListExecutor) Type() string {
	return NodeTypeK8sList
}

func (e *K8sListExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-list", fmt.Errorf("config is required"))
	}

	kind, err := extractKind(config)
	if err != nil {
		return nil, wrapK8sError("k8s-list", err)
	}

	namespaceTemplate, _ := config["namespace"].(string)
	namespace := resolver.ResolveString(namespaceTemplate)

	clusterId := extractClusterId(config)
	labelSelector := extractLabelSelector(config, resolver)
	fieldSelector := extractFieldSelector(config, resolver)

	// Use Cortex API to list resources - works for any cluster Cortex manages
	list, err := e.cortexClient.ListResources(ctx, clusterId, namespace, kind, labelSelector, fieldSelector)
	if err != nil {
		return nil, wrapK8sError("k8s-list", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"list":  list,
			"count": len(list),
		},
	}, nil
}
