package executor

import (
	"context"
	"fmt"
)

type K8sRestartExecutor struct {
	cortexClient CortexK8sClient
}

func NewK8sRestartExecutor(cortexClient CortexK8sClient) *K8sRestartExecutor {
	return &K8sRestartExecutor{
		cortexClient: cortexClient,
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

	// Use Cortex API to restart resource - works for any cluster Cortex manages
	// Cortex has a dedicated rotate endpoint for this operation
	err = e.cortexClient.RestartResource(ctx, clusterId, namespace, name, kind)
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
