package executor

import (
	"context"
	"fmt"
)

type K8sDeleteExecutor struct {
	k8sClient K8sClient
}

func NewK8sDeleteExecutor(k8sClient K8sClient) *K8sDeleteExecutor {
	return &K8sDeleteExecutor{
		k8sClient: k8sClient,
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

	if e.k8sClient == nil {
		return nil, wrapK8sError("k8s-delete", fmt.Errorf("k8s client not configured"))
	}

	err = e.k8sClient.DeleteResource(ctx, clusterId, namespace, name, kind)
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
