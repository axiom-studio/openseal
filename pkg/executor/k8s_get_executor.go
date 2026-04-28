package executor

import (
	"context"
	"fmt"
)

// K8sClient is the interface for K8s operations.
// OpenSeal ships with a direct-K8s implementation for standalone mode.
// Cortex/Atlas injects a remote implementation that proxies to the Cortex API
type K8sClient interface {
	GetResource(ctx context.Context, clusterId int, namespace, name, kind string) (map[string]interface{}, error)
	ListResources(ctx context.Context, clusterId int, namespace, kind, labelSelector, fieldSelector string) ([]map[string]interface{}, error)
	DeleteResource(ctx context.Context, clusterId int, namespace, name, kind string) error
	UpdateResource(ctx context.Context, clusterId int, namespace, name, kind string, patch map[string]interface{}) (map[string]interface{}, error)
	GetPodLogs(ctx context.Context, clusterId int, namespace, podName, containerName string, tailLines int, sinceSeconds int) (string, error)
	ListEvents(ctx context.Context, clusterId int, namespace, resourceKind, resourceName string) ([]map[string]interface{}, error)
	RestartResource(ctx context.Context, clusterId int, namespace, name, kind string) error
}

type K8sGetExecutor struct {
	k8sClient K8sClient
}

func NewK8sGetExecutor(k8sClient K8sClient) *K8sGetExecutor {
	return &K8sGetExecutor{
		k8sClient: k8sClient,
	}
}

func (e *K8sGetExecutor) Type() string {
	return NodeTypeK8sGet
}

func (e *K8sGetExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-get", fmt.Errorf("config is required"))
	}

	kind, err := extractKind(config)
	if err != nil {
		return nil, wrapK8sError("k8s-get", err)
	}

	name, namespace, err := extractNameNamespace(config, resolver)
	if err != nil {
		return nil, wrapK8sError("k8s-get", err)
	}

	clusterId := extractClusterId(config)

	if e.k8sClient == nil {
		return nil, wrapK8sError("k8s-get", fmt.Errorf("k8s client not configured"))
	}

	obj, err := e.k8sClient.GetResource(ctx, clusterId, namespace, name, kind)
	if err != nil {
		return nil, wrapK8sError("k8s-get", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"object": obj,
		},
	}, nil
}
