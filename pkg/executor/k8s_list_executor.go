package executor

import (
	"context"
	"fmt"
)

type K8sListExecutor struct {
	k8sClient K8sClient
}

func NewK8sListExecutor(k8sClient K8sClient) *K8sListExecutor {
	return &K8sListExecutor{
		k8sClient: k8sClient,
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

	if e.k8sClient == nil {
		return nil, wrapK8sError("k8s-list", fmt.Errorf("k8s client not configured"))
	}

	list, err := e.k8sClient.ListResources(ctx, clusterId, namespace, kind, labelSelector, fieldSelector)
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
