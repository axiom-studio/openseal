package executor

import (
	"context"
	"fmt"
)

type K8sRestartExecutor struct {
	k8sClient K8sClient
}

func NewK8sRestartExecutor(k8sClient K8sClient) *K8sRestartExecutor {
	return &K8sRestartExecutor{
		k8sClient: k8sClient,
	}
}

func (e *K8sRestartExecutor) Type() string {
	return NodeTypeK8sRestart
}

func (e *K8sRestartExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, wrapK8sError("k8s-restart", fmt.Errorf("config is required"))
	}

	kind, err := extractKind(config)
	if err != nil {
		return nil, wrapK8sError("k8s-restart", err)
	}

	if kind != "Deployment" && kind != "StatefulSet" && kind != "DaemonSet" {
		return nil, wrapK8sError("k8s-restart", fmt.Errorf("kind must be Deployment, StatefulSet, or DaemonSet"))
	}

	name, namespace, err := extractNameNamespace(config, resolver)
	if err != nil {
		return nil, wrapK8sError("k8s-restart", err)
	}

	clusterId := extractClusterId(config)

	if e.k8sClient == nil {
		return nil, wrapK8sError("k8s-restart", fmt.Errorf("k8s client not configured"))
	}

	err = e.k8sClient.RestartResource(ctx, clusterId, namespace, name, kind)
	if err != nil {
		return nil, wrapK8sError("k8s-restart", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"success": true,
			"message": "Rollout restart initiated",
			"name":    name,
			"kind":    kind,
		},
	}, nil
}
